# Implementation: Run argv assembly

Covers ContainerRunner.

## Types (internal/infra/run.go)

```go
type ContainerRunner struct {
    docker DockerClient
    logger *slog.Logger
}

type RunSpec struct {
    Name      string            // deterministic: faber-<run-id>-<slug(step-id)>-a<attempt>
    Image     string            // the tag ImageBuilder produced
    Resources config.ResourceDef
    Mounts    []Mount           // engine mounts: result bind, ro hooks + box entry, workspace volume, tmpfs writables
    Env       map[string]string // engine env; contract values, never secrets
    Bindings  []string          // ordered argv fragment from security.BindingSet
    Entry     []string          // in-container entry argv
}

// MountKind selects the docker flag. The container starts as root (image
// default — no baked user); faber-box chowns the writable mounts to the run
// uid:gid (carried in Env as FABER_RUN_UID/GID) and drops privileges before any
// hook or agent. There is no --user flag and no RunSpec field for one.
type MountKind int
const (
    KindBind   MountKind = iota // -v Host:Container[:ro]  — host bind
    KindVolume                  // -v Container[:ro]       — anonymous volume, disk-backed, --rm-discarded
    KindTmpfs                   // --tmpfs Container        — RAM
)

// Mount is one engine mount. Host is set only for KindBind; ReadOnly applies to
// bind and volume (a tmpfs is always writable — it is the box's own scratch).
type Mount struct {
    Kind            MountKind
    Host, Container string
    ReadOnly        bool
}

type RunResult struct {
    ExitCode int
    Output   []byte        // bounded combined stdout+stderr tail
    Started  time.Time
    Duration time.Duration
}

func (r *ContainerRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error)
```

## Assembly

```go
func buildArgs(spec RunSpec) []string {
    args := []string{"run", "--rm", "--name", spec.Name}
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
byte-for-byte. The engine mounts a spec carries are the result bind (rw), the
read-only hook scripts and box entry binary, the disk-backed `/workspace`
volume, and the tmpfs writables (`/faber/bundle`, `/tmp`, `HOME`); everything
else — agent socket, secret file, cache volume, `--network`, `--runtime` —
arrives inside `Bindings`. There is no code path that could emit a docker-socket
mount, `--privileged`, or `--user`: no field of `RunSpec` maps to them, and the
non-root drop is the box's own job, driven by the `FABER_RUN_UID`/`FABER_RUN_GID`
engine env, not a docker flag.

## Run lifecycle

```go
func (r *ContainerRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
    out := newTailBuffer(256 << 10) // bounded capture
    sink := io.MultiWriter(out, slogWriter(r.logger, "step", spec.Name))
    start := time.Now()
    code, err := r.docker.ContainerRun(ctx, buildArgs(spec), sink)
    res := RunResult{ExitCode: code, Output: out.Bytes(),
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
- **Output is capped.** `tailBuffer` keeps the last 256 KiB; the authoritative
  step artifact is the mounted `result.json`, and captured output exists for
  the failure record and debugging, not data threading.
- **Always returns.** Success, failure, or kill, `Run` returns to its caller,
  which is what guarantees BindingSet teardown hooks (agent shutdown, secret
  shredding) run after every attempt.
