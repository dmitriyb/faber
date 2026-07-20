// Integration wiring: this file is the one place the real modules meet. The
// config CLI stays testable in-process behind config.Deps; here the seams are
// filled with infra's builders, the failure store, and a pipeline executor
// assembled per invocation from the RunOptions wiring context.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/pipeline"
	"github.com/dmitriyb/faber/security"
)

// stateDir is where faber keeps host-side state: run journals under runs/,
// the infra image manifest under infra/. Overridable for tests and multi-root
// setups; a relative value resolves against the working directory (a run's
// journal lives beside the config that produced it) but is absolutized HERE —
// state-dir paths end up as docker -v host paths, and docker reads a relative
// host path as a named volume, silently detaching every result mount.
func stateDir() string {
	d := os.Getenv("FABER_STATE_DIR")
	if d == "" {
		d = ".faber"
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return d // Abs fails only on an unreadable cwd; the run will fail loudly
	}
	return abs
}

// boxBinary locates the faber-box sequencer to bind-mount into containers:
// next to the faber executable unless FABER_BOX_BIN overrides. Absolutized
// for the same docker -v reason as stateDir.
func boxBinary() string {
	b := os.Getenv("FABER_BOX_BIN")
	if b == "" {
		exe, err := os.Executable()
		if err != nil {
			b = "faber-box"
		} else {
			b = filepath.Join(filepath.Dir(exe), "faber-box")
		}
	}
	abs, err := filepath.Abs(b)
	if err != nil {
		return b
	}
	return abs
}

// wireDeps builds the config.Deps injection for the real binary. The wiring
// logger covers construction-time components only; every command passes its
// own flag-configured logger through the seams.
func wireDeps(stdout, stderr io.Writer) config.Deps {
	wlog := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	docker := infra.NewDockerCLI(wlog)
	builder := infra.NewImageBuilder(docker, infra.NewNixCLI(wlog), infra.DefaultNixpkgsPin(),
		filepath.Join(stateDir(), "infra"), wlog)
	store := failure.NewStore(filepath.Join(stateDir(), "runs"), wlog)
	return config.Deps{
		Prover:   builder.PackageProver(),
		Builder:  builder.ConfigBuilder(),
		Journal:  store,
		Audit:    store,
		Executor: &wiredExecutor{stdout: stdout, docker: docker, builder: builder, store: store},
		Registry: registryController{},
	}
}

// registryController adapts the security RoleRegistry to the config CLI's
// RegistryController seam so the config package never imports security. It
// reads and writes only roles.json under faber's config home; it never touches
// key material.
type registryController struct{}

// AddKey is a load-modify-write over roles.json (Load → AddKey → Save). It is
// deliberately not file-locked: SaveRegistry's temp-file-plus-rename keeps the
// file itself always intact, so the only race is two concurrent `faber add-key`
// invocations where the later rename wins and drops the earlier update. For an
// interactive init-flow CLI that window is acceptable; hardening it would mean
// an advisory lock on the config dir, which this scope does not add.
func (registryController) AddKey(role, fingerprint, comment string, force bool) error {
	path := security.RegistryPath()
	reg, err := security.LoadRegistry(path)
	if err != nil {
		return err
	}
	reg, changed, err := security.AddKey(reg, role, fingerprint, comment, force)
	if err != nil {
		var ve *security.ValidationError
		if errors.As(err, &ve) {
			return &config.RegistryUsageError{Err: err}
		}
		return err
	}
	if changed {
		return security.SaveRegistry(path, reg)
	}
	return nil
}

func (registryController) ListKeys(stdout, stderr io.Writer) error {
	reg, err := security.LoadRegistry(security.RegistryPath())
	if err != nil {
		return err
	}
	security.WriteRegistryList(reg, stdout, stderr)
	return nil
}

// wiredExecutor satisfies the config CLI's Executor seam by assembling a
// pipeline.Executor per invocation from the RunOptions wiring context. The
// pipeline core never reads Config; this adapter resolves the security
// sections and generate targets for it.
type wiredExecutor struct {
	stdout  io.Writer
	docker  infra.DockerClient
	builder *infra.ImageBuilder
	store   *failure.Store
}

func (w *wiredExecutor) Execute(ctx context.Context, ir *config.IR, params config.Params, opts config.RunOptions, logger *slog.Logger) error {
	cfg := opts.Config
	if cfg == nil || opts.Targets == nil {
		return fmt.Errorf("faber: internal: executor invoked without the CLI wiring context")
	}
	configHash, err := failure.HashFile(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("faber: hash config for the journal header: %w", err)
	}

	var resolver security.Resolver
	if cfg.Credentials.Resolver != "" {
		resolver = security.NewExecResolver(cfg.Credentials.Resolver, infra.NewCommandRunner(logger))
	}
	netBinding := security.NewNetworkBinding(w.docker, logger)
	remoteBinding := security.NewRemoteBinding(logger)
	identityBinding := security.NewIdentityBinding(security.NewAgentController(logger), logger)
	// The registry+locator resolve a template `identity: <name>` that carries
	// no explicit key path. A path-form identity (every current config) never
	// consults them, so a missing or empty registry leaves existing runs
	// byte-identical; a malformed registry is a hard, loud error.
	reg, err := security.LoadRegistry(security.RegistryPath())
	if err != nil {
		return err
	}
	identityBinding.Registry = reg
	identityBinding.Locator = security.NewKeyLocator(logger)
	bindings := security.NewBindingSet(
		netBinding,
		remoteBinding,
		identityBinding,
		security.NewCredentialBroker(resolver, logger),
		logger)
	// Interactive re-entry composes no credential broker: the debug shell only
	// observes a failed step, never runs the agent, and cannot materialize a
	// stdin secrets payload — so it resolves no tokens (no GetToken side effect)
	// and streams none. Network/remote/identity stay so clone and observe still
	// work; an operator who needs a secret sets it by hand inside the shell.
	reentryBindings := security.NewBindingSetWithoutCredentials(netBinding, remoteBinding, identityBinding, logger)

	network := &cfg.Network
	if cfg.Network.Name == "" && cfg.Network.Proxy == "" && len(cfg.Network.NoProxy) == 0 && !cfg.Network.Nftables {
		network = nil
	}
	remote := &cfg.Remote
	if cfg.Remote.URL == "" {
		remote = nil
	}

	entry := boxBinary()
	images := imageTagger{b: w.builder}
	ex := &pipeline.Executor{
		Store: w.store,
		Boxes: &pipeline.AgentBoxes{
			Containers:  infra.NewContainerRunner(w.docker, logger),
			Bindings:    bindings,
			EntryBinary: entry,
			Network:     network,
			Remote:      remote,
			Identities:  cfg.Identities,
			Services:    cfg.Credentials.Services,
			GitName:     os.Getenv("FABER_GIT_NAME"),
			GitEmail:    os.Getenv("FABER_GIT_EMAIL"),
			Log:         logger,
		},
		Hooks:      &failure.ExecHookRunner{Log: logger},
		Source:     infra.NewCommandRunner(logger),
		Workflows:  opts.Targets,
		Images:     images,
		ImageCheck: w.docker,
		Reentry: &pipeline.Reentry{
			IR:          ir,
			Images:      images,
			Bindings:    reentryBindings,
			Interactive: terminalRunner{},
			EntryBinary: entry,
			Network:     network,
			Remote:      remote,
			Identities:  cfg.Identities,
		},
		Meta: pipeline.RunMeta{
			ConfigPath: opts.ConfigPath,
			ConfigHash: configHash,
			Supplied:   opts.Supplied,
		},
		Out: w.stdout,
	}
	// The machine-readable report goes wherever --report-json points; "-"
	// means stdout (after the human report, which also writes there). A file
	// target buffers in memory and is written only if a report was actually
	// produced, so an interactive session or a pre-scheduler failure leaves
	// no stale/empty file behind, and the close error is surfaced.
	var jsonBuf *bytes.Buffer
	switch opts.ReportJSON {
	case "":
	case "-":
		ex.JSONOut = w.stdout
	default:
		jsonBuf = &bytes.Buffer{}
		ex.JSONOut = jsonBuf
	}
	runErr := ex.Execute(ctx, ir, params, opts, logger)
	if jsonBuf != nil && jsonBuf.Len() > 0 {
		if werr := os.WriteFile(opts.ReportJSON, jsonBuf.Bytes(), 0o644); werr != nil && runErr == nil {
			return fmt.Errorf("faber: write --report-json target: %w", werr)
		}
	}
	return runErr
}

// imageTagger adapts infra's deterministic tag derivation to the pipeline's
// journal-hash seam. Tags are computable without docker or nix.
type imageTagger struct {
	b *infra.ImageBuilder
}

func (t imageTagger) Tag(template *config.ResolvedTemplate) (string, error) {
	// Carry template.Pin into the reconstructed BuildDef so the run/resume tag
	// resolves the SAME pin as `faber build`. Drop it and a pinned toolset's tag
	// falls back to the default pin and diverges from the built image's tag.
	return t.b.ImageTag(template.Name, config.BuildDef{Packages: template.Packages, Overlay: template.Overlay, Pin: template.Pin})
}

// terminalRunner is the interactive TTY variant of the container run: the
// same argv infra assembles, plus -it, attached to the operator's terminal.
type terminalRunner struct{}

func (terminalRunner) RunInteractive(ctx context.Context, spec infra.RunSpec) error {
	args := infra.RunArgs(spec)
	argv := append([]string{args[0], "-it"}, args[1:]...)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
