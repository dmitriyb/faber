package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dmitriyb/faber/config"
)

// outputCap bounds the captured human channel (the container's stderr). The
// box's machine-readable artifact is the frame stream on its stdout, passed
// through to the caller untouched; captured output exists for the failure
// record and debugging, so only the tail is kept.
const outputCap = 256 << 10

// killGracePeriod bounds the explicit docker kill issued after a context
// cancellation.
const killGracePeriod = 15 * time.Second

// ContainerSpec is one fully resolved step-container invocation. Engine mounts are
// exactly the declared ones (the workspace volume and the tmpfs writables;
// faber-box, the hooks and the skills tree are image content, the result
// dir a tmpfs the box ships out over its stdout — no host path at all);
// everything else — agent socket, the /run/secrets tmpfs, cache volume,
// --network, --runtime — arrives inside Bindings, the security module's
// ordered argv fragment, spliced verbatim. No ContainerSpec field maps to a
// docker-socket mount, --privileged, or host namespaces: the discipline is
// structural.
type ContainerSpec struct {
	Name      string             // deterministic: faber-<run-id>-<slug(step-id)>-a<attempt>
	Image     string             // the tag ImageBuilder produced
	Resources config.ResourceDef // template limits; absent fields emit no flags
	Mounts    []Mount            // engine mounts: workspace volume, tmpfs writables (result, bundle, tmp, home); template volumes
	Env       map[string]string  // engine env; step-contract values, never secrets
	Bindings  []string           // ordered argv fragment from the security module, verbatim
	// StdinSecrets is the framed JSON file-mode secrets payload
	// (security.Assembled.SecretsStdin): empty = no stdin, no -i; non-empty
	// emits -i (right after --rm) and streams the bytes on the container's
	// stdin. It MUST be paired with Env[contract.EnvSecretsStdin]="1" by the
	// assembler — ContainerRunner never sets that flag (see impl_run_argv.md).
	StdinSecrets []byte
	Entry        []string // in-container entry argv; empty runs the image default
	// Output, when non-nil, receives the container's stderr (plus docker's
	// own messages) as it streams — the human channel; the run-prep seam
	// points it at the attempt's durable output.log. The bounded tail and the
	// debug forwarding happen regardless.
	Output io.Writer
	// Stdout, when non-nil, receives the container's stdout untouched — the
	// box's frame channel, which the run-prep seam demultiplexes. The runner
	// never parses or buffers it; nil discards it.
	Stdout io.Writer
	// Attach, when non-nil, is a host-side companion of the running
	// container (security.Assembled.Attach: the identity relay served over
	// docker exec). Run and Enter start it beside the container and cancel
	// its context when the container ends; its error is logged, never the
	// run's result — what the box did without its companion is in the box's
	// own record.
	Attach func(ctx context.Context, container string) error
}

// Session is what Enter runs inside a held box once the input stream is in:
// the operator's entry (a shell, or the harness's resume argv), exec-level
// env, the uid:gid the exec drops to, and the working directory.
type Session struct {
	Entry   []string
	Env     map[string]string
	User    string
	Workdir string
}

// MountKind selects the docker flag a Mount emits. The container starts as root
// (the image bakes no user); faber-box chowns the writable mounts to the run
// uid:gid (FABER_RUN_UID/GID engine env) and drops privileges before any hook or
// agent, so a fresh root-owned volume/tmpfs still ends up writable by the box.
type MountKind int

const (
	KindBind   MountKind = iota // -v Host:Container[:ro]  — host bind
	KindVolume                  // -v Container[:ro]       — anonymous volume, disk-backed, --rm-discarded
	KindTmpfs                   // --tmpfs Container        — RAM
)

// Mount is one engine mount. Host is set only for KindBind; ReadOnly applies to
// bind and volume (a tmpfs is always writable — the box's own scratch).
type Mount struct {
	Kind            MountKind
	Host, Container string
	ReadOnly        bool
}

// ContainerResult is the mechanical outcome of one container run. Deciding what an
// exit code means belongs to the agent and failure modules.
type ContainerResult struct {
	ExitCode int
	Output   []byte // bounded tail of the human channel (container stderr + docker's messages)
	Started  time.Time
	Duration time.Duration
}

// ContainerRunner is the single-step run primitive — the only place in faber
// a docker run argv is ever constructed. One container per step attempt,
// --rm, never reused.
type ContainerRunner struct {
	docker DockerClient
	logger *slog.Logger
}

// NewContainerRunner constructs the run primitive.
func NewContainerRunner(docker DockerClient, logger *slog.Logger) *ContainerRunner {
	return &ContainerRunner{docker: docker, logger: ensureLogger(logger).With("component", "container-runner")}
}

// Run starts the step container, streams its stderr into a bounded buffer
// (and the debug logger, and the Output writer) and its stdout into the
// Stdout writer, waits, and returns the exit code as data — err is reserved
// for actuation failures (daemon unreachable, cancellation).
// On context cancellation the container is killed by its deterministic name
// on a fresh short-deadline context, and --rm removes it. Run always returns,
// success or kill, so binding teardown hooks always get their turn.
func (r *ContainerRunner) Run(ctx context.Context, spec ContainerSpec) (ContainerResult, error) {
	if spec.Name == "" || spec.Image == "" {
		return ContainerResult{}, fmt.Errorf("infra: run: spec requires a container name and an image tag")
	}
	// A relative bind host would be read by docker as a NAMED VOLUME — the
	// mount silently detaches from the intended host dir (the result channel
	// above all). Absolutizing is the wiring's job; this is the floor.
	for _, m := range spec.Mounts {
		if m.Kind == KindBind && m.Host != "" && !filepath.IsAbs(m.Host) {
			return ContainerResult{}, fmt.Errorf("infra: run %s: bind mount host path %q is relative (docker would read it as a named volume); absolutize it at the wiring", spec.Name, m.Host)
		}
	}
	out := newTailBuffer(outputCap)
	writers := []io.Writer{out, slogWriter(r.logger, "step", spec.Name)}
	if spec.Output != nil {
		writers = append(writers, spec.Output)
	}
	sink := io.MultiWriter(writers...)
	stdout := spec.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	var stdin io.Reader // nil unless a secrets payload rides stdin
	if len(spec.StdinSecrets) > 0 {
		// A *bytes.Reader, deliberately not an *os.File: os/exec then pipes it
		// through a copier that closes the write end on EOF, so the box's
		// io.ReadAll(stdin) sees a clean end-of-payload rather than hanging (see
		// cliRunner.runStreaming). Passing an *os.File here would reintroduce
		// that hang.
		stdin = bytes.NewReader(spec.StdinSecrets)
	}
	// Evict any stale holder of this deterministic name first: a crashed
	// process (docker client killed, container survives) leaves a container
	// that both blocks the name and keeps writing into the old attempt dir.
	// Removal is idempotent; a failure here only foreshadows the run's own.
	if err := r.docker.Remove(ctx, spec.Name); err != nil {
		r.logger.WarnContext(ctx, "evict stale container before launch", "container", spec.Name, "err", err)
	}
	start := time.Now()
	stopAttach := r.startAttach(ctx, spec)
	code, err := r.docker.ContainerRun(ctx, buildArgs(spec), stdin, stdout, sink)
	stopAttach()
	res := ContainerResult{ExitCode: code, Output: out.Bytes(), Started: start, Duration: time.Since(start)}
	if ctx.Err() != nil {
		// Killing the docker client process only detaches; stop the container
		// itself, addressable by its deterministic name.
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killGracePeriod)
		defer cancel()
		if kerr := r.docker.Kill(killCtx, spec.Name); kerr != nil && !isNoSuchContainer(kerr) {
			r.logger.WarnContext(killCtx, "kill after cancel", "container", spec.Name, "err", kerr)
		}
		return res, fmt.Errorf("infra: run %s: %w", spec.Name, ctx.Err())
	}
	if err != nil {
		return res, fmt.Errorf("infra: run %s: %w", spec.Name, err)
	}
	return res, nil
}

// Enter puts the operator inside a reconstructed box without mounting
// anything from the host: the box starts detached on the entry `faber-box
// hold`, the caller's frame stream goes in through `docker exec -i <name>
// faber-box receive`, the operator's terminal attaches through a second
// exec as the session says, and the container is killed by name when the
// terminal exits (--rm removes it). The kill is deferred before the receive,
// so a refused stream or a failed attach never leaves a held box behind. A
// single `docker run -it` cannot take a stream first and a terminal after,
// which is why this is a choreography of four verbs.
func (r *ContainerRunner) Enter(ctx context.Context, spec ContainerSpec, input io.Reader, s Session) error {
	if spec.Name == "" || spec.Image == "" {
		return fmt.Errorf("infra: enter: spec requires a container name and an image tag")
	}
	if err := r.docker.Remove(ctx, spec.Name); err != nil {
		r.logger.WarnContext(ctx, "evict stale container before launch", "container", spec.Name, "err", err)
	}
	spec.Entry = []string{holdEntry, "hold"}
	spec.StdinSecrets = nil
	stderr := slogWriter(r.logger, "container", spec.Name)
	if code, err := r.docker.ContainerRun(ctx, detachArgs(spec), nil, io.Discard, stderr); err != nil || code != 0 {
		return fmt.Errorf("infra: enter %s: start held box: exit %d: %w", spec.Name, code, err)
	}
	defer func() {
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killGracePeriod)
		defer cancel()
		if kerr := r.docker.Kill(killCtx, spec.Name); kerr != nil && !isNoSuchContainer(kerr) {
			r.logger.WarnContext(killCtx, "kill held box", "container", spec.Name, "err", kerr)
		}
	}()
	// Deferred after the kill, so it runs first: the companion ends before
	// the box it served.
	defer r.startAttach(ctx, spec)()
	// The receive's stderr is kept: a refused stream names the frame there,
	// and the operator must see why the box was not entered.
	tail := newTailBuffer(4 << 10)
	if code, err := r.docker.Exec(ctx, spec.Name, []string{holdEntry, "receive"}, input, io.Discard, io.MultiWriter(stderr, tail)); err != nil || code != 0 {
		return fmt.Errorf("infra: enter %s: receive exited %d: %w (%s)", spec.Name, code, err, strings.TrimSpace(string(tail.Bytes())))
	}
	return r.docker.ExecTerminal(ctx, spec.Name, s.Entry, s.Env, s.User, s.Workdir)
}

// startAttach runs the spec's companion beside the container and returns
// what ends it: cancel, then wait. A companion that ends on its own before
// the container does is a warning with the container's name; the
// cancellation at the end is not.
func (r *ContainerRunner) startAttach(ctx context.Context, spec ContainerSpec) (stop func()) {
	if spec.Attach == nil {
		return func() {}
	}
	actx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The cancellation at the end is expected and may already have
		// happened when a quick failure returns, so the error, not the
		// context, tells the two apart.
		if err := spec.Attach(actx, spec.Name); err != nil && !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "attach ended", "container", spec.Name, "err", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runningPollInterval paces ExecWhenRunning's wait for the container.
const runningPollInterval = 50 * time.Millisecond

// ExecWhenRunning polls Running until the container is up, then execs
// once: how a companion catches a container Run has just started, with no
// daemon prose matched and no stdin fed to an exec that never started. The
// context bounds the wait — the runner cancels it when the container ends.
func ExecWhenRunning(ctx context.Context, e Execer, name string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	t := time.NewTicker(runningPollInterval)
	defer t.Stop()
	for {
		up, err := e.Running(ctx, name)
		if err != nil {
			return -1, err
		}
		if up {
			return e.Exec(ctx, name, argv, stdin, stdout, stderr)
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-t.C:
		}
	}
}

// holdEntry is the baked box binary, the entry of a held box and of the
// receive exec. The agent module owns the path; it is restated here so infra
// imports nothing above it.
const holdEntry = "/faber/bin/faber-box"

// detachArgs is buildArgs with -d after --rm: a held box runs detached and
// takes no stdin.
func detachArgs(spec ContainerSpec) []string {
	args := buildArgs(spec)
	return slices.Insert(args, 2, "-d")
}

// buildArgs assembles the docker run argv in the fixed, golden-testable
// order: rm, the lone -i (only when StdinSecrets is non-empty, right after
// --rm), name, resource limits, engine mounts, engine env (sorted keys), the
// binding fragment spliced verbatim (never parsed, reordered, or deduplicated
// — its meaning belongs to the security module, including its --tmpfs
// /run/secrets), then the image and the entry argv. The file-mode token is not
// a mount: it rides StdinSecrets on the container's stdin. Pure function, no
// I/O — -i's presence is a pure function of the spec too.
func buildArgs(spec ContainerSpec) []string {
	args := []string{"run", "--rm"}
	if len(spec.StdinSecrets) > 0 {
		args = append(args, "-i") // attach stdin for the secrets payload
	}
	args = append(args, "--name", spec.Name)
	if m := spec.Resources.Memory; m != "" {
		args = append(args, "--memory="+m)
	}
	if c := spec.Resources.CPUs; c != 0 {
		args = append(args, fmt.Sprintf("--cpus=%g", c))
	}
	for _, m := range spec.Mounts {
		switch m.Kind {
		case KindTmpfs:
			args = append(args, "--tmpfs", m.Container)
		case KindVolume:
			v := m.Container
			if m.ReadOnly {
				v += ":ro"
			}
			args = append(args, "-v", v)
		default: // KindBind
			v := m.Host + ":" + m.Container
			if m.ReadOnly {
				v += ":ro"
			}
			args = append(args, "-v", v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args, spec.Bindings...) // verbatim — never parsed or reordered
	args = append(args, spec.Image)
	return append(args, spec.Entry...)
}

// tailBuffer is a bounded writer keeping the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if keep := t.max - n; len(t.buf) > keep {
		copy(t.buf, t.buf[len(t.buf)-keep:])
		t.buf = t.buf[:keep]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// Bytes returns a copy of the retained tail.
func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf)
}

// slogWriter returns a writer that forwards complete output lines to the
// logger at debug level, tagged with the given attrs.
func slogWriter(logger *slog.Logger, args ...any) io.Writer {
	return &lineLogWriter{logger: logger.With(args...)}
}

// lineLogWriter buffers writes into lines so log records carry whole lines;
// a pathological unterminated line is flushed once it exceeds the bound.
type lineLogWriter struct {
	logger *slog.Logger
	mu     sync.Mutex
	rem    []byte
}

const lineLogBound = 64 << 10

func (w *lineLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rem = append(w.rem, p...)
	for {
		i := bytes.IndexByte(w.rem, '\n')
		if i < 0 {
			break
		}
		w.emit(w.rem[:i])
		w.rem = w.rem[i+1:]
	}
	if len(w.rem) > lineLogBound {
		w.emit(w.rem)
		w.rem = w.rem[:0]
	}
	return len(p), nil
}

func (w *lineLogWriter) emit(line []byte) {
	trimmed := bytes.TrimRight(line, "\r")
	if len(trimmed) == 0 {
		return
	}
	w.logger.Debug("box output", "line", string(trimmed))
}
