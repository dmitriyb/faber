package config

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Cross-module seams. The CLI composes module entry points; the modules behind
// build/run/resume (infra, pipeline, failure) are injected so this package
// never imports them. A nil seam yields a clear structured error (or, for the
// validate-time package proof, a logged skip) — final wiring happens at
// integration.

// PackageProver proves every template's package list resolves (infra's
// validate-time nix eval proof).
type PackageProver interface {
	ProvePackages(ctx context.Context, cfg *Config, logger *slog.Logger) error
}

// ImageBuilder builds one template's immutable image (infra module).
type ImageBuilder interface {
	BuildImage(ctx context.Context, cfg *Config, template string, logger *slog.Logger) error
}

// RunOptions carries run policy from the CLI to the executor. Measurement is
// run policy, so metering config lives beside the run, not in orchestrator.yaml.
type RunOptions struct {
	RunID           string
	Mode            string // "" (fresh run) | "resume" | "fresh" | "interactive"
	InteractiveStep string
	MaxParallel     int
	Budgets         map[string]float64
	MeteringPath    string

	// Wiring context: the CLI fills these so the integration-level Executor
	// can assemble per-run capabilities (journal header identity, security
	// bindings, generate targets) without re-reading or re-validating the
	// YAML. The executor core never reads Config — only the cmd wiring does.
	ConfigPath string
	Workflow   string
	Supplied   map[string]string // the --param k=v bindings as given
	Targets    map[string]*IR    // desugared, wiring-checked IRs of every reachable workflow
	Config     *Config           // the loaded, validated config
}

// Executor executes a validated IR (pipeline module).
type Executor interface {
	Execute(ctx context.Context, ir *IR, params Params, opts RunOptions, logger *slog.Logger) error
}

// JournalHeader is what resume needs from a run journal: enough to re-derive
// the config, workflow, and params, plus the IR hash that defines resume
// compatibility ("same bytes out of the desugaring pipeline").
type JournalHeader struct {
	RunID      string
	ConfigPath string
	Workflow   string
	Params     map[string]string
	IRHash     string
}

// JournalStore opens journaled runs (failure module).
type JournalStore interface {
	LoadHeader(runID string) (JournalHeader, error)
}

// Deps injects the future modules' capabilities into the CLI.
type Deps struct {
	Prover   PackageProver
	Builder  ImageBuilder
	Executor Executor
	Journal  JournalStore
}

// Run is the faber CLI: subcommand dispatch, exit-code contract, and logging
// initialization, testable in-process. Exit codes: 0 success; 1 validation or
// run failure (details already reported on stderr); 2 usage error.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithDeps(args, stdout, stderr, Deps{})
}

// RunWithDeps is Run with cross-module seams injected.
func RunWithDeps(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "validate":
		return cmdValidate(rest, stdout, stderr, deps)
	case "build":
		return cmdBuild(rest, stderr, deps)
	case "run":
		return cmdRun(rest, stderr, deps)
	case "resume":
		return cmdResume(rest, stderr, deps)
	default:
		fmt.Fprintf(stderr, "faber: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: faber <command> [flags]

commands:
  validate   load, desugar, and check every workflow; --emit-ir prints the IR
  build      build template images
  run        execute a workflow: faber run <workflow> --param k=v ...
  resume     re-enter a journaled run: faber resume <run-id>

common flags: --config path (default orchestrator.yaml), --log-level level, --log-format auto|json|text
`)
}

type commonFlags struct {
	config    string
	logLevel  string
	logFormat string
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.config, "config", "orchestrator.yaml", "path to orchestrator.yaml")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug|info|warn|error")
	fs.StringVar(&c.logFormat, "log-format", "auto", "auto|json|text")
	return c
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func (c *commonFlags) logger(stderr io.Writer) (*slog.Logger, error) {
	return InitLogging(c.logLevel, c.logFormat, stderr)
}

func cmdValidate(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := addCommonFlags(fs)
	emitIR := fs.Bool("emit-ir", false, "print the canonical IR to stdout")
	workflow := fs.String("workflow", "", "validate only this workflow")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger, err := common.logger(stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logger = logger.With("component", "cli")

	cfg, viols, err := Load(common.config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := Validate(cfg, viols); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	names, err := workflowNames(cfg, *workflow)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// Desugar + wiring + package-proof errors are one wave, reported together.
	var errs []error
	irs := map[string]*IR{}
	for _, name := range names {
		ir, derr := Desugar(cfg, name)
		if derr != nil {
			errs = append(errs, derr)
			continue
		}
		if werr := CheckWiring(ir, cfg); werr != nil {
			errs = append(errs, werr)
			continue
		}
		irs[name] = ir
	}
	if deps.Prover != nil {
		if perr := deps.Prover.ProvePackages(context.Background(), cfg, logger.With("component", "infra")); perr != nil {
			errs = append(errs, perr)
		}
	} else {
		logger.Debug("package resolution proof skipped", "reason", "infra module not wired")
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *emitIR {
		for _, name := range names {
			b, err := EncodeIR(irs[name])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if _, err := stdout.Write(b); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
	}
	return 0
}

func cmdBuild(args []string, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := addCommonFlags(fs)
	template := fs.String("template", "", "build only this template")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger, err := common.logger(stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg, viols, err := Load(common.config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := Validate(cfg, viols); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	names := sortedKeys(cfg.Templates)
	if *template != "" {
		if _, ok := cfg.Templates[*template]; !ok {
			fmt.Fprintf(stderr, "faber build: unknown template %q\n", *template)
			return 1
		}
		names = []string{*template}
	}
	if deps.Builder == nil {
		fmt.Fprintln(stderr, "faber build: image builds require the infra module, which is not wired into this binary yet")
		return 1
	}
	blog := logger.With("component", "infra")
	for _, name := range names {
		if err := deps.Builder.BuildImage(context.Background(), cfg, name, blog); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

func cmdRun(args []string, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: faber run <workflow> [--param k=v ...] [flags]")
		return 2
	}
	workflow, rest := args[0], args[1:]

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := addCommonFlags(fs)
	var paramFlags, budgetFlags stringList
	fs.Var(&paramFlags, "param", "workflow param binding k=v (repeatable)")
	fs.Var(&budgetFlags, "budget", "budget bound unit=n (repeatable)")
	maxParallel := fs.Int("max-parallel", 0, "maximum concurrently running steps (0 = unlimited)")
	metering := fs.String("metering", "", "path to run-time metering config")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	logger, err := common.logger(stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	supplied, err := parsePairs(paramFlags, "--param")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	budgets, err := parseBudgets(budgetFlags)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	cfg, ir, targets, params, code := runEntry(common.config, workflow, supplied, stderr)
	if code != 0 {
		return code
	}
	if deps.Executor == nil {
		fmt.Fprintln(stderr, "faber run: execution requires the pipeline module, which is not wired into this binary yet (validation passed)")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := RunOptions{
		MaxParallel: *maxParallel, Budgets: budgets, MeteringPath: *metering,
		ConfigPath: common.config, Workflow: workflow, Supplied: supplied,
		Targets: targets, Config: cfg,
	}
	if err := deps.Executor.Execute(ctx, ir, params, opts, logger.With("component", "pipeline")); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runEntry is the shared run/resume entry pipeline: the full validate flow for
// the entry workflow and everything reachable from it, then run-entry param
// checking. There is no code path that executes an IR that did not just pass
// full validation in the same process.
func runEntry(configPath, workflow string, supplied map[string]string, stderr io.Writer) (*Config, *IR, map[string]*IR, Params, int) {
	cfg, viols, err := Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, nil, nil, 1
	}
	if err := Validate(cfg, viols); err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, nil, nil, 1
	}
	wf, ok := cfg.Workflows[workflow]
	if !ok {
		fmt.Fprintf(stderr, "faber run: unknown workflow %q\n", workflow)
		return nil, nil, nil, nil, 1
	}
	var errs []error
	var entryIR *IR
	targets := map[string]*IR{}
	for _, name := range reachableWorkflows(cfg, workflow) {
		ir, derr := Desugar(cfg, name)
		if derr != nil {
			errs = append(errs, derr)
			continue
		}
		if werr := CheckWiring(ir, cfg); werr != nil {
			errs = append(errs, werr)
			continue
		}
		targets[name] = ir
		if name == workflow {
			entryIR = ir
		}
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, nil, nil, 1
	}
	params, err := CheckRunParams(wf, supplied)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, nil, nil, 1
	}
	return cfg, entryIR, targets, params, 0
}

func cmdResume(args []string, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: faber resume <run-id> [--fresh] [--interactive <step-id>] [flags]")
		return 2
	}
	runID, rest := args[0], args[1:]

	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := addCommonFlags(fs)
	fresh := fs.Bool("fresh", false, "restart from the journal's config without reusing step results")
	interactive := fs.String("interactive", "", "re-enter interactively at this step id")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	logger, err := common.logger(stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if deps.Journal == nil {
		fmt.Fprintln(stderr, "faber resume: journaled runs require the failure module, which is not wired into this binary yet")
		return 1
	}
	header, err := deps.Journal.LoadHeader(runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// Re-derive config path, workflow, and params from the journal header.
	cfg, ir, targets, params, code := runEntry(header.ConfigPath, header.Workflow, header.Params, stderr)
	if code != 0 {
		return code
	}
	hash, err := HashIR(ir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if hash != header.IRHash && !*fresh {
		fmt.Fprintf(stderr, "faber resume: run %s was journaled against a different IR (config has changed since the run; journal %s, current %s) — fix the config back or pass --fresh to restart\n",
			runID, header.IRHash, hash)
		return 1
	}
	if deps.Executor == nil {
		fmt.Fprintln(stderr, "faber resume: execution requires the pipeline module, which is not wired into this binary yet (resume guard passed)")
		return 1
	}
	mode := "resume"
	if *fresh {
		mode = "fresh"
	}
	if *interactive != "" {
		mode = "interactive"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := RunOptions{
		RunID: runID, Mode: mode, InteractiveStep: *interactive,
		ConfigPath: header.ConfigPath, Workflow: header.Workflow, Supplied: header.Params,
		Targets: targets, Config: cfg,
	}
	if err := deps.Executor.Execute(ctx, ir, params, opts, logger.With("component", "pipeline")); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func workflowNames(cfg *Config, only string) ([]string, error) {
	if only == "" {
		return sortedKeys(cfg.Workflows), nil
	}
	if _, ok := cfg.Workflows[only]; !ok {
		return nil, fmt.Errorf("faber validate: unknown workflow %q", only)
	}
	return []string{only}, nil
}

// reachableWorkflows returns the entry workflow plus every workflow reachable
// from it via use: reuse and generate: fan-out, sorted, entry first.
func reachableWorkflows(cfg *Config, entry string) []string {
	seen := map[string]bool{entry: true}
	queue := []string{entry}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		var walk func(steps []StepDef)
		walk = func(steps []StepDef) {
			for _, s := range steps {
				var target string
				switch {
				case s.Loop != nil:
					walk(s.Loop.Steps)
					continue
				case s.Generate != nil:
					target = s.Generate.Workflow
				case s.Use != "":
					if _, ok := cfg.Workflows[s.Use]; ok {
						target = s.Use
					}
				}
				if target != "" && !seen[target] {
					seen[target] = true
					queue = append(queue, target)
				}
			}
		}
		walk(cfg.Workflows[name].Steps)
	}
	others := make([]string, 0, len(seen)-1)
	for name := range seen {
		if name != entry {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	return append([]string{entry}, others...)
}

func parsePairs(pairs []string, flagName string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("faber: %s %q: expected k=v", flagName, p)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("faber: %s %q: duplicate key %q", flagName, p, k)
		}
		out[k] = v
	}
	return out, nil
}

func parseBudgets(pairs []string) (map[string]float64, error) {
	raw, err := parsePairs(pairs, "--budget")
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(raw))
	for k, v := range raw {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("faber: --budget %s=%s: value is not a number", k, v)
		}
		out[k] = n
	}
	return out, nil
}
