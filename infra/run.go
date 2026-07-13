package infra

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dmitriyb/faber/config"
)

// outputCap bounds captured container output. The box's machine-readable
// artifact is the mounted result.json; captured output exists for the failure
// record and debugging, so only the tail is kept.
const outputCap = 256 << 10

// killGracePeriod bounds the explicit docker kill issued after a context
// cancellation.
const killGracePeriod = 15 * time.Second

// RunSpec is one fully resolved step-container invocation. Engine mounts are
// exactly the declared ones (result dir, hook scripts, box entry binary);
// everything else — agent socket, secret file, cache volume, --network,
// --runtime — arrives inside Bindings, the security module's ordered argv
// fragment, spliced verbatim. No RunSpec field maps to a docker-socket mount,
// --privileged, or host namespaces: the discipline is structural.
type RunSpec struct {
	Name      string             // deterministic: faber-<run-id>-<slug(step-id)>-a<attempt>
	Image     string             // the tag ImageBuilder produced
	Resources config.ResourceDef // template limits; absent fields emit no flags
	Mounts    []Mount            // engine mounts only
	Env       map[string]string  // engine env; step-contract values, never secrets
	Bindings  []string           // ordered argv fragment from the security module, verbatim
	Entry     []string           // in-container entry argv; empty runs the image default
	User      string             // "uid:gid" the container runs as; empty leaves the image default (root)
}

// Mount is one engine-declared bind mount.
type Mount struct {
	Host, Container string
	ReadOnly        bool
}

// RunResult is the mechanical outcome of one container run. Deciding what an
// exit code means belongs to the agent and failure modules.
type RunResult struct {
	ExitCode int
	Output   []byte // bounded combined stdout+stderr tail
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

// Run starts the step container, streams combined output into a bounded
// buffer (and the debug logger), waits, and returns the exit code as data —
// err is reserved for actuation failures (daemon unreachable, cancellation).
// On context cancellation the container is killed by its deterministic name
// on a fresh short-deadline context, and --rm removes it. Run always returns,
// success or kill, so binding teardown hooks always get their turn.
func (r *ContainerRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if spec.Name == "" || spec.Image == "" {
		return RunResult{}, fmt.Errorf("infra: run: spec requires a container name and an image tag")
	}
	out := newTailBuffer(outputCap)
	sink := io.MultiWriter(out, slogWriter(r.logger, "step", spec.Name))
	start := time.Now()
	code, err := r.docker.ContainerRun(ctx, buildArgs(spec), sink)
	res := RunResult{ExitCode: code, Output: out.Bytes(), Started: start, Duration: time.Since(start)}
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

// RunArgs exposes the assembled docker run argv for the one caller that
// cannot go through Run: the integration wiring's interactive TTY variant,
// which must attach the operator's terminal. Argv construction still lives
// only here.
func RunArgs(spec RunSpec) []string {
	return buildArgs(spec)
}

// buildArgs assembles the docker run argv in the fixed, golden-testable
// order: rm/name, resource limits, engine mounts, engine env (sorted keys),
// the binding fragment spliced verbatim (never parsed, reordered, or
// deduplicated — its meaning belongs to the security module), then the image
// and the entry argv. Pure function, no I/O.
func buildArgs(spec RunSpec) []string {
	args := []string{"run", "--rm", "--name", spec.Name}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if m := spec.Resources.Memory; m != "" {
		args = append(args, "--memory="+m)
	}
	if c := spec.Resources.CPUs; c != 0 {
		args = append(args, fmt.Sprintf("--cpus=%g", c))
	}
	for _, m := range spec.Mounts {
		v := m.Host + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
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
