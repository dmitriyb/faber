# Implementation: Run argv assembly

Covers ContainerRunner.

## Types (internal/infra/run.go)

```go
type ContainerRunner struct {
    docker DockerClient
    logger *slog.Logger
}

type ContainerSpec struct {
    Name      string            // deterministic: faber-<run-id>-<slug(step-id)>-a<attempt>
    Image     string            // the tag ImageBuilder produced
    Resources config.ResourceDef
    Mounts    []Mount           // engine mounts: workspace volume, tmpfs writables (result, bundle, tmp, home); never a host bind
    Env       map[string]string // engine env; contract values, never secrets
    Bindings  []string          // ordered argv fragment from security.BindingSet
    StdinSecrets []byte          // JSON secrets payload (security.Assembled.SecretsStdin); empty = no stdin, no -i; non-empty MUST pair with Env[contract.EnvSecretsStdin]="1" — see the pairing invariant below
    Entry     []string          // in-container entry argv
    Stdout    io.Writer         // the container's stdout: the box's frame stream, passed through untouched; nil discards
    Output    io.Writer         // the container's stderr plus docker's own messages, tee'd in full: the attempt's output.log
    Attach    func(ctx context.Context, container string) error // run beside the container (security.Assembled.Attach), cancelled when it ends; nil = none
}

// Session is what Enter runs inside a held box once the input stream is in.
type Session struct {
    Entry   []string          // the operator's entry: a shell, or the harness's resume argv
    Env     map[string]string // exec-level env (HOME for the harness)
    User    string            // uid:gid the exec drops to (the box's run user)
    Workdir string
}

// MountKind selects the docker flag. The container starts as root (image
// default — no baked user); faber-box chowns the writable mounts to the run
// uid:gid (carried in Env as FABER_RUN_UID/GID) and drops privileges before any
// hook or agent. There is no --user flag and no ContainerSpec field for one.
type MountKind int
const (
    KindBind   MountKind = iota // -v Host:Container[:ro]  — host bind
    KindVolume                  // -v Container[:ro]       — anonymous volume, disk-backed, --rm-discarded
    KindTmpfs                   // --tmpfs Container        — RAM
    KindNamed                   // -v Name:Container[:ro]  — docker named volume, created by the daemon on first use, kept across runs
)

// Mount is one engine mount. Host is set only for KindBind, Name only for
// KindNamed; ReadOnly applies to bind and volume (a tmpfs is always writable —
// it is the box's own scratch).
type Mount struct {
    Kind            MountKind
    Host, Container string
    Name            string
    ReadOnly        bool
}

type ContainerResult struct {
    ExitCode int
    Output   []byte        // bounded tail of the human channel (container stderr + docker's messages)
    Started  time.Time
    Duration time.Duration
}

func (r *ContainerRunner) Run(ctx context.Context, spec ContainerSpec) (ContainerResult, error)
func (r *ContainerRunner) Enter(ctx context.Context, spec ContainerSpec, input io.Reader, s Session) error
```

**`StdinSecrets` is half of an atomic pairing.** The assembler MUST set
`StdinSecrets` and `Env[contract.EnvSecretsStdin]="1"` together — never one
without the other. A non-empty `StdinSecrets` with the flag unset makes the box
skip its stdin read (phase 3 is gated on `FABER_SECRETS_STDIN=1`) and silently
lose the credential; the flag set with an empty payload leaves the box waiting
on a stdin that never arrives. ContainerRunner is a pure argv builder: it only
mechanically emits `-i` and streams the bytes when `StdinSecrets` is non-empty,
and it never sets the flag. Enforcing the pairing therefore belongs to whoever
assembles the `ContainerSpec` from `security.Assembled` and the engine env — the
step-runner seam in the pipeline scheduler (see spec/pipeline/impl_scheduling.md),
which is the only host-side owner of `ContainerSpec.Env` assembly.

## Assembly

```go
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
            if m.ReadOnly { v += ":ro" }
            args = append(args, "-v", v)
        case KindNamed:
            v := m.Name + ":" + m.Container
            if m.ReadOnly { v += ":ro" }
            args = append(args, "-v", v)
        default: // KindBind
            v := m.Host + ":" + m.Container
            if m.ReadOnly { v += ":ro" }
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
```

Pure function, no I/O — the golden-argv tests in test_infra.md exercise it
directly. Sorted env keys keep the argv deterministic for a given spec; the
binding fragment's internal order is BindingSet's contract, preserved
byte-for-byte. The lone `-i` flag is emitted only when `StdinSecrets` is
non-empty and sits right after `--rm`, so its presence is a pure function of the
spec too. The engine mounts a spec carries are the
disk-backed `/workspace` volume, the tmpfs writables (`/faber/result`,
`/faber/bundle`, `/tmp`, `HOME`) and the template's named volumes (`-v
faber-<project>-<name>:<path>`, a docker volume the daemon creates on first
use and keeps across runs) — no host bind at all; everything else — the agent
tmpfs (`--tmpfs /faber/agent`), the secrets tmpfs (`--tmpfs /run/secrets`), cache volume, `--network`,
`--runtime` — arrives inside `Bindings`. The file-mode token is not a mount at
all: it rides `StdinSecrets` on the container's stdin, and faber-box writes it
into that tmpfs.
`/faber/bin/faber-box`, `/faber/hooks/<slot>` and `/faber/skills` are image
content the image builder bakes, so no argv ever mounts them. There is no code path that could emit a docker-socket
mount, `--privileged`, or `--user`: no field of `ContainerSpec` maps to them, and the
non-root drop is the box's own job, driven by the `FABER_RUN_UID`/`FABER_RUN_GID`
engine env, not a docker flag.

Two guard layers protect the spec before any argv exists. The run-spec
assembler (agent.BuildContainerSpec) refuses template env claiming engine- or
security-owned names (only `FABER_AGENT_CLI` may be set) and template volumes
overlapping the reserved container paths (`/faber`, `/run/secrets`,
`/workspace`, `/home/box`, `/tmp` — docker's last-mount-wins
would sever the result channel or substitute the engine's mounts); the
config Loader mirrors both refusals at validate time from the same shared
vocabulary. `Run` itself enforces two floors: a relative bind host is
refused (docker would read it as a NAMED VOLUME and silently detach the
mount — absolutizing is the wiring's job; the engine emits no bind and
template volumes are named volumes, so the floor is defense in depth), and any stale holder of the
deterministic container name is force-removed before launch (a crashed
process's abandoned container both blocks the name and keeps writing into
its orphaned attempt dir).

## Run lifecycle

```go
func (r *ContainerRunner) Run(ctx context.Context, spec ContainerSpec) (ContainerResult, error) {
    out := newTailBuffer(256 << 10) // bounded capture of the human channel
    writers := []io.Writer{out, slogWriter(r.logger, "step", spec.Name)}
    if spec.Output != nil { writers = append(writers, spec.Output) } // the attempt's durable output.log
    sink := io.MultiWriter(writers...)
    stdout := spec.Stdout            // the frame stream, untouched
    if stdout == nil { stdout = io.Discard }
    var stdin io.Reader // nil unless a secrets payload rides stdin
    if len(spec.StdinSecrets) > 0 {
        stdin = bytes.NewReader(spec.StdinSecrets) // written then closed by the adapter
    }
    start := time.Now()
    code, err := r.docker.ContainerRun(ctx, buildArgs(spec), stdin, stdout, sink)
    res := ContainerResult{ExitCode: code, Output: out.Bytes(),
        Started: start, Duration: time.Since(start)}
    if ctx.Err() != nil {
        killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
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
```

Points of discipline:

- **Kill is explicit.** Cancelling the `docker run` client process would only
  detach; the runner issues `docker kill <name>` on a fresh short-deadline
  context derived via `context.WithoutCancel`, then relies on `--rm` for
  removal. The deterministic name is what makes the kill (and operator
  inspection of a live step) addressable.
- **Non-zero exit is not a Go error.** The box failing is a *result*
  (`ExitCode != 0`, output attached) for the agent/failure modules to classify;
  `err` is reserved for actuation failures (daemon unreachable, image missing,
  cancellation).
- **Output is capped.** `tailBuffer` keeps the last 256 KiB of the human
  channel; the authoritative step artifact is the `result.json` the box ships
  on its stdout, and captured output exists for the failure record and
  debugging, not data threading. The stdout stream is never buffered here: it
  flows to the caller's writer as it arrives.
- **The streams never mix.** `cliRunner.runStreaming` gives the docker client
  two writers; a merged writer would file frames as output and lose the
  record.
- **Always returns.** Success, failure, or kill, `Run` returns to its caller,
  which is what guarantees BindingSet teardown hooks run after every
  attempt.
- **The attach is bracketed by the run.** With `spec.Attach` set, `Run`
  derives a context, starts `spec.Attach(actx, spec.Name)` in a goroutine
  before `docker run`, and after the run returns cancels that context and
  waits for the goroutine; an attach error that is not the cancellation is
  logged at warn with the container. `ExecWhenRunning(ctx, e Execer, name,
  argv, stdin, stdout, stderr)` is what an attach calls: it polls
  `e.Running(ctx, name)` every 50 ms until true (or returns the context's
  error), then calls `e.Exec` once. `Execer` is the two-verb slice of
  `DockerClient` (`Running`, `Exec`).

## Enter

```go
func (r *ContainerRunner) Enter(ctx context.Context, spec ContainerSpec, input io.Reader, s Session) error {
    _ = r.docker.Remove(ctx, spec.Name)                       // evict a stale holder, as Run does
    spec.Entry = []string{contract.ContainerEntry, "hold"}
    if _, err := r.docker.ContainerRun(ctx, detachArgs(spec), nil, io.Discard, stderr); err != nil { return err }
    defer r.docker.Kill(killCtx, spec.Name)                   // --rm removes it; always runs
    stop := r.startAttach(ctx, spec)                          // the companion exec, when the spec has one
    defer stop()                                              // cancelled and awaited before the kill
    if code, err := r.docker.Exec(ctx, spec.Name, []string{contract.ContainerEntry, "receive"}, input, io.Discard, stderr); err != nil || code != 0 {
        return fmt.Errorf("infra: enter %s: receive exited %d: %w", spec.Name, code, err)
    }
    return r.docker.ExecTerminal(ctx, spec.Name, s.Entry, s.Env, s.User, s.Workdir)
}
```

`detachArgs` is `buildArgs` with `-d` inserted after `--rm` (and never `-i`:
a held box takes no stdin). The container id `docker run -d` prints is
discarded; the deterministic name is the handle for every later verb. The
kill is deferred before the receive so a refused stream or a failed attach
never leaves a held box behind.
