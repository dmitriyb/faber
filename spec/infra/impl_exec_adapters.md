# Implementation: Exec adapter contracts

Covers ExecAdapters.

## Interfaces (internal/infra/exec.go)

```go
type DockerClient interface {
    ImageExists(ctx context.Context, tag string) (bool, error)
    Load(ctx context.Context, tarball string) (string, error) // returns loaded tag
    NetworkExists(ctx context.Context, name string) (bool, error)
    ContainerRun(ctx context.Context, args []string, stdin io.Reader, output io.Writer) (int, error)
    Kill(ctx context.Context, name string) error
}

type GitClient interface {
    LsRemote(ctx context.Context, url, ref string) (string, error) // sha or ErrRefAbsent
}

type NixClient interface {
    Eval(ctx context.Context, exprFile string, args []string) (json.RawMessage, error)
    Build(ctx context.Context, exprFile string) ([]string, error) // store out paths
}

type CommandRunner interface {
    Run(ctx context.Context, spec CmdSpec) (CmdResult, error)
}

type CmdSpec struct {
    Path  string   // the user's command file, executed directly (no shell)
    Args  []string
    Stdin []byte
    Env   []string // appended to a minimal base env, never the full host env
    Dir   string
}

type CmdResult struct {
    Stdout   []byte // typed by the caller; may be secret — never logged here
    Stderr   []byte // bounded tail
    ExitCode int
}
```

Interfaces live in the infra package; consumers depend on the interface value
handed to them at construction (no package-level defaults, no globals). The
real implementations are private structs (`dockerCLI`, `gitCLI`, `nixCLI`,
`userCmd`) constructed by one factory each.

## Structured invocation

Every docker query verb pins the output format and parses exactly one shape:

```go
func (d *dockerCLI) ImageExists(ctx context.Context, tag string) (bool, error) {
    out, err := d.run(ctx, "image", "inspect", "--format", "{{json .Id}}", tag)
    if isNotFound(err) { return false, nil }
    if err != nil { return false, err }
    var id string
    if err := json.Unmarshal(out, &id); err != nil {
        return false, fmt.Errorf("infra: docker image inspect %s: parse: %w", tag, err)
    }
    return id != "", nil
}
```

`nix eval`/`nix build` always carry `--json`; their raw messages are decoded by
ImageBuilder against the expression it rendered. `git ls-remote` output is
plumbing (`<sha>\t<ref>`), split once, with `--exit-code` distinguishing
absent-ref from failure. No adapter contains a second parser for the same verb.

## Error contract (internal/infra/exec_error.go)

```go
type ExecError struct {
    Cmd      string   // "docker", "nix", or "user-command" for CommandRunner
    Args     []string // redacted to Path-only for user commands
    ExitCode int
    Stderr   string   // trimmed tail, bounded (4 KiB)
    Err      error    // underlying exec error, %w-wrapped
}
```

Every non-zero exit or spawn failure returns `*ExecError` wrapped with the
module prefix (`fmt.Errorf("infra: docker load: %w", execErr)`); callers use
`errors.As` for exit codes and `errors.Is(err, context.Canceled)` for kills.
For `CommandRunner`, stdout never appears in the error or any log record —
resolver output is a potential credential; stderr and exit code must be
sufficient diagnosis, and the argv is reduced to the command path.

## Cancellation

All adapters use `exec.CommandContext` with `cmd.Cancel` sending SIGTERM to the
process group and `cmd.WaitDelay` (10s) escalating to SIGKILL, so a hung user
command or wedged CLI cannot outlive its step. `ContainerRun` is the exception
handled one level up: killing the docker *client* process detaches rather than
stops the container, so ContainerRunner pairs cancellation with an explicit
`Kill(ctx2, name)` on a fresh short-deadline context.

When `stdin != nil`, `ContainerRun` wires it to the docker client's standard
input (the caller has already put `-i` in `args`), copies it to EOF, and closes
the pipe — a clean EOF the box reads as end-of-payload. `stdin == nil` leaves
stdin unattached, unchanged from the no-secrets path. The stdin bytes are a
potential credential, so they are never logged here, exactly like
`CommandRunner` stdout.

## Fakes

Tests use a single recording fake per interface: it appends each call's argv to
a slice and pops canned results from a queue. Because components receive
interfaces at construction, the fakes need no build tags or indirection — the
integration suite (test_infra.md) is the only place real CLIs run.
