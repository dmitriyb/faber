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
    Mounts    []Mount           // engine mounts: result bind, ro hooks + box entry, ro skills (when the template declares them), workspace volume, tmpfs writables
    Env       map[string]string // engine env; contract values, never secrets
    Bindings  []string          // ordered argv fragment from security.BindingSet
    StdinSecrets []byte          // framed JSON secrets payload (security.Assembled.SecretsStdin); empty = no stdin, no -i; non-empty MUST pair with Env[contract.EnvSecretsStdin]="1" — see the pairing invariant below
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

**`StdinSecrets` is half of an atomic pairing.** The assembler MUST set
`StdinSecrets` and `Env[contract.EnvSecretsStdin]="1"` together — never one
without the other. A non-empty `StdinSecrets` with the flag unset makes the box
skip its stdin read (phase 3 is gated on `FABER_SECRETS_STDIN=1`) and silently
lose the credential; the flag set with an empty payload leaves the box waiting
on a stdin that never arrives. ContainerRunner is a pure argv builder: it only
mechanically emits `-i` and streams the bytes when `StdinSecrets` is non-empty,
and it never sets the flag. Enforcing the pairing therefore belongs to whoever
assembles the `RunSpec` from `security.Assembled` and the engine env — the
step-runner seam in the pipeline scheduler (see spec/pipeline/impl_scheduling.md),
which is the only host-side owner of `RunSpec.Env` assembly.

## Assembly

```go
func buildArgs(spec RunSpec) []string {
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
spec too. The engine mounts a spec carries are the result bind (rw), the
read-only hook scripts and box entry binary, the read-only skills directory
(`contract.ContainerSkillsDir` = `/faber/skills`, present only when the template
declares a `skills` leg), the disk-backed `/workspace` volume, and the tmpfs
writables (`/faber/bundle`, `/tmp`, `HOME`); everything else — agent socket,
the secrets tmpfs (`--tmpfs /run/secrets`), cache volume, `--network`,
`--runtime` — arrives inside `Bindings`. The file-mode token is not a mount at
all: it rides `StdinSecrets` on the container's stdin, and faber-box writes it
into that tmpfs.
`/faber/skills` is a sibling of `/faber/hooks`: per-template, read-only, static
capabilities, deliberately not nested under the box-writable per-run bundle
tmpfs. There is no code path that could emit a docker-socket
mount, `--privileged`, or `--user`: no field of `RunSpec` maps to them, and the
non-root drop is the box's own job, driven by the `FABER_RUN_UID`/`FABER_RUN_GID`
engine env, not a docker flag.

## Run lifecycle

```go
func (r *ContainerRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
    out := newTailBuffer(256 << 10) // bounded capture
    sink := io.MultiWriter(out, slogWriter(r.logger, "step", spec.Name))
    var stdin io.Reader // nil unless a secrets payload rides stdin
    if len(spec.StdinSecrets) > 0 {
        stdin = bytes.NewReader(spec.StdinSecrets) // written then closed by the adapter
    }
    start := time.Now()
    code, err := r.docker.ContainerRun(ctx, buildArgs(spec), stdin, sink)
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
  which is what guarantees BindingSet teardown hooks (agent shutdown; file-mode
  secrets need none — they die with the container tmpfs) run after every
  attempt.
