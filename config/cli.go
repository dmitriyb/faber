package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
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
	// ReportJSON is where the machine-readable run report goes: a file path,
	// "-" for stdout, or empty for none.
	ReportJSON string

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
// compatibility ("same bytes out of the desugaring pipeline"). Format and
// IRVersion are the schema stamps the guards branch on: they distinguish an
// engine upgrade from operator config drift.
type JournalHeader struct {
	RunID      string
	ConfigPath string
	Workflow   string
	Params     map[string]string
	IRHash     string
	Format     int // journal schema stamp (0 = pre-versioning journal)
	IRVersion  int // IR schema the journaled hash was computed under
}

// JournalStore opens journaled runs (failure module). SupportedFormat is the
// journal schema this binary reads and writes — the failure module owns the
// constant; the CLI only compares stamps against it for its early guards.
type JournalStore interface {
	LoadHeader(runID string) (JournalHeader, error)
	SupportedFormat() int
}

// RunAudit is one journaled run's upgrade-relevant state, as the pre-upgrade
// guard reports it.
type RunAudit struct {
	RunID    string
	Live     bool // another process currently holds the run's lock
	Complete bool // the journal records a run-end marker
	Format   int  // journal schema stamp (0 = pre-versioning journal)
}

// RunAuditor enumerates journaled runs for `faber upgrade-check` (failure
// module). The scan is read-only and tolerant: it must read journals of any
// format, because the guard's whole job is to look before an upgrade leaps.
type RunAuditor interface {
	AuditRuns() ([]RunAudit, error)
}

// RegistryController manages the global role→fingerprint registry (security
// module). The CLI dispatches add-key/list-keys through this seam so the config
// package never imports security. AddKey returns a *RegistryUsageError for a
// bad flag value (exit 2); any other non-nil error is operational (exit 1).
type RegistryController interface {
	AddKey(role, fingerprint, comment string, force bool) error
	ListKeys(stdout, stderr io.Writer) error
}

// RegistryUsageError wraps an add-key failure that is a usage error (a
// malformed --fingerprint or --role), mapping it to exit 2. Every other
// registry error is operational and maps to exit 1.
type RegistryUsageError struct{ Err error }

func (e *RegistryUsageError) Error() string { return e.Err.Error() }
func (e *RegistryUsageError) Unwrap() error { return e.Err }

// ExitCode marks a RegistryUsageError as a usage error for the dispatcher's
// generic error→exit-code mapping (see cliError below).
func (e *RegistryUsageError) ExitCode() int { return 2 }

// Deps injects the future modules' capabilities into the CLI.
type Deps struct {
	Prover    PackageProver
	Builder   ImageBuilder
	Executor  Executor
	Journal   JournalStore
	Audit     RunAuditor
	Registry  RegistryController
	BuildInfo BuildInfo
	// Installer runs the embedded install.sh in upgrade mode (faber upgrade).
	Installer Installer
	// BoxBinary is the installed faber-box path, resolved by the integration
	// layer with the same FABER_BOX_BIN-or-next-to-faber convention used to
	// bind-mount it (cmd/faber/wire.go). Empty in the in-process test deps.
	BoxBinary string
}

// cliError carries an explicit process exit code alongside the wrapped error,
// the same convention spexmachina's per-command exit errors use. An error
// without this interface defaults to exit 1 (an operational or validation
// failure, already reported); usageErr marks the exit-2 (malformed
// invocation) cases the flag/argument layer detects before any module work
// starts.
type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }
func (e *cliError) ExitCode() int { return e.code }

func usageErr(err error) error { return &cliError{code: 2, err: err} }

// rootUsageText is printed (as the error text, to stderr) when faber is
// invoked with no command at all, or an unrecognized one — the exit-2 usage
// path. `faber --help` / `-h` / `help` take the separate, cobra-generated
// help path (stdout, exit 0) via NewRootCmd's default help machinery.
const rootUsageText = `usage: faber <command> [flags]

commands:
  version    print version, commit, and build date (also: --version, -v)
  validate   load, desugar, and check every workflow; --emit-ir prints the IR
  build      build template images
  run        execute a workflow: faber run <workflow> --param k=v ...
  resume     re-enter a journaled run: faber resume <run-id>
  upgrade-check  read-only pre-upgrade guard: refuses while live or
             unfinished runs exist (faber is not upgraded mid-run);
             --force acknowledges and proceeds
  upgrade    update faber and faber-box to a newer signed release via the
             embedded install.sh; runs upgrade-check first, then self-replaces
             both binaries: --check/--dry-run, --version vX.Y.Z, --rollback, --force
  add-key    register a role→fingerprint in the global registry:
             faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--force]
  list-keys  print the global role→fingerprint registry

common flags: --config path (default orchestrator.yaml), --log-level level, --log-format auto|json|text
add-key/list-keys touch no orchestrator.yaml and take only --log-level/--log-format`

// NewRootCmd constructs the top-level faber command with every subcommand
// attached, the cross-module seams (deps) closed over each subcommand's
// RunE. Mirrors spexmachina's cli.NewRootCmd + cmd/spex newXxxCmd() split,
// kept inside the config package (rather than a separate cli package) so the
// whole CLI stays testable in-process via SetArgs/SetOut/SetErr, per this
// package's existing design.
func NewRootCmd(deps Deps) *cobra.Command {
	var showVersion bool
	cmd := &cobra.Command{
		Use:           "faber",
		Short:         "Generic containerized-agent workflow engine",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				return printVersion(cmd.OutOrStdout(), deps.BuildInfo)
			}
			if len(args) == 0 {
				return usageErr(errors.New(rootUsageText))
			}
			return usageErr(fmt.Errorf("faber: unknown command %q\n%s", args[0], rootUsageText))
		},
	}
	cmd.Flags().BoolVarP(&showVersion, "version", "v", false, "print version, commit, and build date")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageErr(err)
	})
	// The command set is a preserved surface (see task constraints): cobra's
	// autogenerated shell-completion subcommand is not part of it.
	cmd.CompletionOptions.DisableDefaultCmd = true

	cmd.AddCommand(
		newVersionCmd(deps),
		newValidateCmd(deps),
		newBuildCmd(deps),
		newRunCmd(deps),
		newResumeCmd(deps),
		newUpgradeCheckCmd(deps),
		newUpgradeCmd(deps),
		newAddKeyCmd(deps),
		newListKeysCmd(deps),
	)
	return cmd
}

// Run is the faber CLI: subcommand dispatch, exit-code contract, and logging
// initialization, testable in-process. Exit codes: 0 success; 1 validation or
// run failure (details already reported on stderr); 2 usage error.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithDeps(args, stdout, stderr, Deps{})
}

// RunWithDeps is Run with cross-module seams injected.
func RunWithDeps(args []string, stdout, stderr io.Writer, deps Deps) int {
	root := NewRootCmd(deps)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// A nil args is not the same as "no args" to cobra: SetArgs(nil) makes
	// Execute() fall back to reading os.Args[1:] (a workaround for cobra's
	// own test binary), which would leak the calling test binary's own flags
	// into faber's dispatch. Normalize so "no args" is always explicit.
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err)
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}

// commonFlags carries the three flags shared by most subcommands.
type commonFlags struct {
	config    string
	logLevel  string
	logFormat string
}

// addCommonFlags registers --config/--log-level/--log-format on cmd.
func addCommonFlags(cmd *cobra.Command) {
	cmd.Flags().String("config", "orchestrator.yaml", "path to orchestrator.yaml")
	cmd.Flags().String("log-level", "info", "debug|info|warn|error")
	cmd.Flags().String("log-format", "auto", "auto|json|text")
}

// addLogFlags registers only --log-level/--log-format, for the registry
// subcommands that touch no orchestrator.yaml and take no --config. These
// thin-dispatch commands build no logger and do no logging, so the parsed
// values are inert — the flags are accepted for CLI symmetry with the other
// subcommands (a `faber add-key --log-level debug` is not rejected), not
// because they take effect.
func addLogFlags(cmd *cobra.Command) {
	cmd.Flags().String("log-level", "info", "debug|info|warn|error")
	cmd.Flags().String("log-format", "auto", "auto|json|text")
}

// readCommonFlags reads --config/--log-level/--log-format back off cmd.
func readCommonFlags(cmd *cobra.Command) commonFlags {
	config, _ := cmd.Flags().GetString("config")
	logLevel, _ := cmd.Flags().GetString("log-level")
	logFormat, _ := cmd.Flags().GetString("log-format")
	return commonFlags{config: config, logLevel: logLevel, logFormat: logFormat}
}

func (c commonFlags) logger(stderr io.Writer) (*slog.Logger, error) {
	return InitLogging(c.logLevel, c.logFormat, stderr)
}

// runEntry is the shared run/resume entry pipeline: the full validate flow for
// the entry workflow and everything reachable from it, then run-entry param
// checking. There is no code path that executes an IR that did not just pass
// full validation in the same process.
func runEntry(configPath, workflow string, supplied map[string]string) (*Config, *IR, map[string]*IR, Params, error) {
	cfg, viols, err := Load(configPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := Validate(cfg, viols); err != nil {
		return nil, nil, nil, nil, err
	}
	wf, ok := cfg.Workflows[workflow]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("faber run: unknown workflow %q", workflow)
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
		return nil, nil, nil, nil, err
	}
	params, err := CheckRunParams(wf, supplied)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, entryIR, targets, params, nil
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
