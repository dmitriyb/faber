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
	"path/filepath"
	"sync"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/pipeline"
	"github.com/dmitriyb/faber/security"
	"github.com/dmitriyb/faber/tui"
)

// stateDir is where faber keeps host-side state: run journals under runs/,
// the infra image manifest under infra/. Overridable for tests and multi-root
// setups; a relative value resolves against the working directory (a run's
// journal lives beside the config that produced it) but is absolutized HERE —
// state-dir paths end up as docker -v host paths, and docker reads a relative
// host path as a named volume, silently detaching every result mount.
func stateDir(hc config.HostConfig) string {
	d := hc.StateDir
	if d == "" {
		d = ".faber"
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return d // Abs fails only on an unreadable cwd; the run will fail loudly
	}
	return abs
}

// boxBinary locates the faber-box sequencer the image builder bakes into
// every image: next to the faber executable unless the host config's box_bin
// overrides. The override comes ONLY from the explicit host file — a binary
// that runs as root inside every box must never be swappable through ambient
// process environment.
func boxBinary(hc config.HostConfig) string {
	b := hc.BoxBin
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
func wireDeps(stdout, stderr io.Writer) (config.Deps, error) {
	wlog := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// The host config is loaded exactly once, before anything is constructed:
	// a malformed file refuses the whole invocation — main maps the error to
	// the operational exit code through its single exit path.
	hc, err := config.LoadHostConfig(config.HostConfigPath())
	if err != nil {
		return config.Deps{}, err
	}
	docker := infra.NewDockerCLI(wlog)
	builder := infra.NewImageBuilder(docker, infra.NewNixCLI(wlog), infra.DefaultNixpkgsPin(),
		filepath.Join(stateDir(hc), "infra"), boxBinary(hc), wlog)
	store := failure.NewStore(filepath.Join(stateDir(hc), "runs"), wlog)
	view := tui.New(tui.Options{
		Store:  store,
		Images: imageProbe{b: builder, docker: docker},
		Keys:   keyProbe{locator: security.NewKeyLocator(wlog)},
		LogDir: filepath.Join(stateDir(hc), "logs"),
	})
	return config.Deps{
		UI:        view,
		Prover:    builder.PackageProver(),
		Builder:   builder.ConfigBuilder(),
		Journal:   store,
		Audit:     store,
		Runs:      store,
		Executor:  &wiredExecutor{stdout: stdout, stderr: stderr, docker: docker, builder: builder, store: store, host: hc},
		Registry:  registryController{},
		Volumes:   volumeController{docker: docker},
		BuildInfo: config.BuildInfo{Version: version, Commit: commit, Date: date},
		Installer: config.EmbeddedInstaller{},
		BoxBinary: boxBinary(hc),
	}, nil
}

// volumeController adapts the docker adapter's two volume verbs to the
// config CLI's VolumeController seam; the project prefix is config's.
type volumeController struct{ docker infra.DockerClient }

func (v volumeController) ListVolumes(ctx context.Context, prefix string) ([]string, error) {
	return v.docker.VolumeList(ctx, prefix)
}

func (v volumeController) RemoveVolume(ctx context.Context, name string) error {
	return v.docker.VolumeRemove(ctx, name)
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
func (registryController) AddKey(role, fingerprint, comment, gitName, gitEmail string, force bool) error {
	path := security.RegistryPath()
	reg, err := security.LoadRegistry(path)
	if err != nil {
		return err
	}
	reg, changed, err := security.AddKey(reg, role, fingerprint, comment, gitName, gitEmail, force)
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
	stderr  io.Writer
	docker  infra.DockerClient
	builder *infra.ImageBuilder
	store   *failure.Store
	host    config.HostConfig
}

func (w *wiredExecutor) Execute(ctx context.Context, ir *config.IR, params config.Params, opts config.RunOptions, logger *slog.Logger) error {
	logger.Debug("host config", "effective", w.host.Describe(config.HostConfigPath()))
	cfg := opts.Config
	if cfg == nil || opts.Targets == nil {
		return fmt.Errorf("faber: internal: executor invoked without the CLI wiring context")
	}
	configHash, err := failure.HashFile(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("faber: hash config for the journal header: %w", err)
	}
	capFree, err := config.HashIRCapFree(cfg, opts.Workflow)
	if err != nil {
		return fmt.Errorf("faber: cap-free IR hash for the journal header: %w", err)
	}

	identityBinding := security.NewIdentityBinding(security.NewAgentController(logger), logger)
	identityBinding.Exec = w.docker
	// A project's named volumes are faber-<project>-<name>; a check run gets
	// its own prefix, so its hooks never touch the project's volumes.
	project := config.ProjectID(opts.ConfigPath)
	var bindings, reentryBindings *security.BindingSet
	reg := security.Registry{}
	store := w.store
	services := cfg.Credentials.Services
	// A check run swaps the whole security surface for the sandbox and keeps
	// its journal out of the run store: nothing it does is a run of record.
	var sandbox *checkSandbox
	if opts.Check {
		sb, err := newCheckSandbox(ctx, w.docker, opts.Keep, w.stderr, logger)
		if err != nil {
			return err
		}
		defer sb.close()
		sandbox = sb
		project = sb.project
		bindings = security.NewCheckBindingSet(identityBinding, sb.volume, sb.source, sb.key, infra.NewContainerRunner(w.docker, logger), infra.NewCommandRunner(logger), logger)
		store = failure.NewStore(filepath.Join(sb.dir, "runs"), logger)
		services = nil
	} else {
		var resolver security.Resolver
		if cfg.Credentials.Resolver != "" {
			resolver = security.NewExecResolver(cfg.Credentials.Resolver, infra.NewCommandRunner(logger))
		}
		netBinding := security.NewNetworkBinding(w.docker, logger)
		remoteBinding := security.NewRemoteBinding(logger)
		// The registry+locator resolve a template `identity: <name>` that carries
		// no explicit key path. A path-form identity (every current config) never
		// consults them, so a missing or empty registry leaves existing runs
		// byte-identical; a malformed registry is a hard, loud error.
		var err error
		reg, err = security.LoadRegistry(security.RegistryPath())
		if err != nil {
			return err
		}
		identityBinding.Registry = reg
		identityBinding.Locator = security.NewKeyLocator(logger)
		bindings = security.NewBindingSet(
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
		reentryBindings = security.NewBindingSetWithoutCredentials(netBinding, remoteBinding, identityBinding, logger)
	}

	network := &cfg.Network
	if cfg.Network.Name == "" && cfg.Network.Proxy == "" && len(cfg.Network.NoProxy) == 0 && !cfg.Network.Nftables {
		network = nil
	}
	remote := &cfg.Remote
	if cfg.Remote.URL == "" {
		remote = nil
	}

	// Committer identity is per-role registry state — never process env. The
	// map hands pipeline only what it needs, keeping it free of the security
	// package.
	gitIdentities := map[string]pipeline.GitIdentity{}
	for role, e := range reg {
		gitIdentities[role] = pipeline.GitIdentity{Name: e.GitName, Email: e.GitEmail}
	}
	if sandbox != nil {
		// Every role commits as the check identity: there is no forge to
		// verify against, and the registry's addresses stay out of a sandbox.
		for role := range cfg.Identities {
			gitIdentities[role] = pipeline.GitIdentity{Name: checkGitName, Email: checkGitEmail}
		}
	}

	// One tag memo per run: faber-box and the hooks are read once, and an
	// edit mid-run cannot move a node's input hash.
	images := newImageTagger(w.builder)
	ex := &pipeline.Executor{
		Store: store,
		Boxes: &pipeline.AgentBoxes{
			Containers:    infra.NewContainerRunner(w.docker, logger),
			Bindings:      bindings,
			Network:       network,
			Remote:        remote,
			Identities:    cfg.Identities,
			Services:      services,
			GitIdentities: gitIdentities,
			Check:         opts.Check,
			Project:       project,
			Log:           logger,
		},
		Hooks:      &failure.ExecHookRunner{Log: logger},
		Source:     infra.NewCommandRunner(logger),
		Workflows:  opts.Targets,
		Images:     images,
		ImageCheck: w.docker,
		Meta: pipeline.RunMeta{
			ConfigPath:    opts.ConfigPath,
			ConfigHash:    configHash,
			Supplied:      opts.Supplied,
			IRHashCapFree: capFree,
		},
		Out: w.stdout,
	}
	if reentryBindings != nil {
		ex.Reentry = &pipeline.Reentry{
			IR:          ir,
			Images:      images,
			Bindings:    reentryBindings,
			Interactive: terminalRunner{runner: infra.NewContainerRunner(w.docker, logger)},
			Network:     network,
			Remote:      remote,
			Identities:  cfg.Identities,
			Project:     project,
		}
	}
	// The progress stream is the operator surface, on stderr beside (not
	// inside) the diagnostic log; the CLI resolved which sink and whether
	// colour against the terminal.
	switch opts.Progress {
	case "text":
		ex.Events = pipeline.NewTextSink(w.stderr, opts.Color)
	case "json":
		ex.Events = pipeline.NewJSONSink(w.stderr)
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
	// The role agents outlive every attempt and die with the run; both
	// binding sets share the one identity binding, so one close covers both.
	if cerr := bindings.Close(context.WithoutCancel(ctx)); cerr != nil {
		logger.Warn("close run-scoped bindings", "err", cerr)
	}
	if jsonBuf != nil && jsonBuf.Len() > 0 {
		if werr := os.WriteFile(opts.ReportJSON, jsonBuf.Bytes(), 0o644); werr != nil && runErr == nil {
			return fmt.Errorf("faber: write --report-json target: %w", werr)
		}
	}
	return runErr
}

// imageProbe is the TUI's images pane seam: the tag a template resolves to
// and whether the daemon holds it — the run preflight, shown before a run.
type imageProbe struct {
	b      *infra.ImageBuilder
	docker infra.DockerClient
}

// Tag recomputes on every probe — the view re-probes to pick up edits, so it
// gets a fresh tagger each time; only a run memoizes (newImageTagger per
// Execute). A hook the image cannot bake is reported through the view's
// ErrImageInputs with the slot and reason, so Home shows the image as not
// checked beside the templates row that names the slot, not as a second FAIL.
func (p imageProbe) Tag(t *config.ResolvedTemplate) (string, error) {
	tag, err := newImageTagger(p.b).Tag(t)
	var hook *infra.HookInputError
	if errors.As(err, &hook) {
		return "", fmt.Errorf("%w: hooks.%s %v", tui.ErrImageInputs, hook.Slot, hook.Err)
	}
	return tag, err
}
func (p imageProbe) Exists(ctx context.Context, tag string) (bool, error) {
	return p.docker.ImageExists(ctx, tag)
}

// keyProbe is the TUI's identity seam: the registry and the locator the
// run itself consults before a box starts, read-only, nothing spawned.
type keyProbe struct {
	locator security.KeyLocator
}

func (p keyProbe) Resolve(ctx context.Context, role string, def config.IdentityDef) (string, error) {
	reg, err := security.LoadRegistry(security.RegistryPath())
	if err != nil {
		return "", err
	}
	src, _, err := security.ResolveIdentity(ctx, reg, p.locator, role, def)
	return src, err
}

// imageTagger adapts infra's deterministic tag derivation to the pipeline's
// journal-hash seam. Tags are computable without docker or nix. Each tagger
// memoizes by template name: a run reads faber-box, the hooks and the skills
// once, and an edit mid-run cannot move a node's input hash.
type imageTagger struct {
	b    *infra.ImageBuilder
	mu   *sync.Mutex
	tags map[string]string
}

func newImageTagger(b *infra.ImageBuilder) imageTagger {
	return imageTagger{b: b, mu: &sync.Mutex{}, tags: map[string]string{}}
}

func (t imageTagger) Tag(template *config.ResolvedTemplate) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tag, ok := t.tags[template.Name]; ok {
		return tag, nil
	}
	// Carry template.Pin into the reconstructed BuildDef so the run/resume tag
	// resolves the SAME pin as `faber build`. Drop it and a pinned toolset's tag
	// falls back to the default pin and diverges from the built image's tag.
	// The hooks and skills ride beside it for the same reason: the resolved
	// template carries exactly what `faber build` baked.
	tag, err := t.b.ImageTag(template.Name,
		config.BuildDef{Packages: template.Packages, Overlay: template.Overlay, Pin: template.Pin},
		infra.ImageInputs{Hooks: template.Hooks, Skills: template.Skills})
	if err != nil {
		return "", err
	}
	t.tags[template.Name] = tag
	return tag, nil
}

// terminalRunner is the interactive variant of the container run: infra's
// Enter choreography (hold, receive the stream, attach the operator's
// terminal, kill) over the same docker client the runs use.
type terminalRunner struct{ runner *infra.ContainerRunner }

func (t terminalRunner) RunInteractive(ctx context.Context, s pipeline.InteractiveSession) error {
	return t.runner.Enter(ctx, s.Spec, s.Input, s.Session)
}
