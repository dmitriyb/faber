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
    Mounts    []Mount           // engine mounts only (result dir, hook scripts, box entry binary)
    Env       map[string]string // engine env; contract values, never secrets
    Bindings  []string          // ordered argv fragment from security.BindingSet
    Entry     []string          // in-container entry argv
}

type Mount struct {
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
        v := m.Host + ":" + m.Container
        if m.ReadOnly { v += ":ro" }
        args = append(args, "-v", v)
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
byte-for-byte. The mounts a spec may carry are exactly the engine's two (result
dir rw, hook scripts ro); everything else — agent socket, secret file, cache
volume, `--network`, `--runtime` — arrives inside `Bindings`. There is no code
path that could emit a docker-socket mount or `--privileged`: no field of
`RunSpec` maps to them.

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
