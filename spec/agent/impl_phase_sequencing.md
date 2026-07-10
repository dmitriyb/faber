# Implementation: Phase sequencing

Covers PhaseSequencer and AgentInvoker.

## The binary (cmd/faber-box, internal/agent)

`faber-box` is built `CGO_ENABLED=0 GOOS=linux` from the same module as the
host engine and bind-mounted read-only by the runner; it shares the config
module's schema types (the output schema) but imports nothing host-only.

```go
type Box struct {
    Env     *BoxEnv
    Runner  CmdRunner     // os/exec behind an interface; faked in tests
    Log     *slog.Logger
    Workdir string        // set by clone (or scratch)
    Bundle  *Bundle       // set after prelude
    Timing  map[string]time.Duration
}

type BoxEnv struct {
    Skill, Identity      string
    ResultDir, BundleDir string
    RemoteURL            string
    Repo                 string            // reserved input; empty = gateless
    Inputs               map[string]string // slot name -> stringified value
    HostKey              string
    TOFU                 bool
    GitName, GitEmail    string
    Effort, ExtraInstruction, MaxBudget string
    Attempt              int
    OutputSchema         config.OutputSchema // decoded from FABER_OUTPUT_SCHEMA
}

func ParseEnv(environ []string) (*BoxEnv, error) // collects ALL violations
```

## The phase table

The fixed order is a data structure, not a call chain — the spec's "engine-
owned sequence" is literally this slice:

```go
type Phase struct {
    Name string
    Run  func(*Box, context.Context) error
}

var phases = []Phase{
    {"env", (*Box).checkEnv},         // required slots present, dirs writable
    {"secrets", (*Box).loadSecrets},  // /run/secrets/* -> process env
    {"hostkey", (*Box).applyHostKeyPolicy},
    {"clone", (*Box).clone},          // no-op when Repo == ""
    {"signing", (*Box).configureSigning},
    {"context", (*Box).runContextHook},
    {"prelude", (*Box).runPreludeHook},
    {"agent", (*Box).runAgent},
    {"result", (*Box).emitResult},
}

func Main(ctx context.Context, box *Box) int {
    for _, p := range phases {
        start := time.Now()
        err := p.Run(box, ctx)
        box.Timing[p.Name] = time.Since(start)
        if err != nil {
            box.failStop(p.Name, err) // handoff.json + failed result.json
            return 1
        }
    }
    return 0
}
```

`failStop` is the single failure funnel (see the hook/result impl section for
the record shapes). `context.Context` threads through every phase; the first
pass sets no deadlines (timeouts are a deferred failure-module seam) but the
cancellation plumbing is in place.

## Setup phases

```go
type CmdRunner interface {
    Run(ctx context.Context, spec CmdSpec) (CmdResult, error) // captured output
    Stream(ctx context.Context, spec CmdSpec) (int, error)    // inherited stdio
}
type CmdSpec struct{ Argv []string; Dir string; Env []string }
type CmdResult struct{ Stdout []byte; StderrTail []byte; ExitCode int }
```

- `loadSecrets`: `os.ReadDir("/run/secrets")`; each regular file exported as
  `strings.ToUpper(name)` = trimmed contents, `os.Setenv` only — the value
  exists in this process tree, never in the docker argv.
- `applyHostKeyPolicy`: pinned key → write a known-hosts file and export
  `GIT_SSH_COMMAND="ssh -o UserKnownHostsFile=<f> -o StrictHostKeyChecking=yes"`;
  TOFU → `accept-new`; neither, with an ssh remote URL → error before any
  network phase runs.
- `clone`: `git clone <RemoteURL>/<Repo>.git <workdir>` via the runner; sets
  `Box.Workdir`. Gateless steps get `os.MkdirTemp` instead and skip signing.
- `configureSigning`: `ssh-add -L` via the runner; `len(lines) != 1` is an
  error naming the count; then four `git config` invocations (`gpg.format
  ssh`, `user.signingkey "key::"+pub`, `commit.gpgsign true`, name/email with
  defaults `faber-<identity>` / `faber-<identity>@box.invalid`).

## AgentInvoker

```go
type Invocation struct {
    CLI    string // agent binary name; must be in the template's package set
    Skill  string
    Body   string // CONTEXT.md bytes, verbatim
    Extra, Effort, MaxBudget string
}

func (i Invocation) Prompt() string // "/"+Skill+"\n\n"+Body [+ trailer]

func (i Invocation) Argv() []string {
    argv := []string{i.CLI, "-p", i.Prompt(),
        "--permission-mode", "bypassPermissions"}
    if i.Effort != ""    { argv = append(argv, "--effort", i.Effort) }
    if i.MaxBudget != "" { argv = append(argv, "--max-budget-usd", i.MaxBudget) }
    return argv
}
```

`runAgent` builds the Invocation from `BoxEnv` + `Bundle.Doc`, merges the
bundle sidecar values into the child environment, and calls `Runner.Stream`
(inherited stdio — agent output belongs to the container log, never to a
parser). A nonzero exit code returns an error carrying the code and the
stderr tail for the handoff; exit 0 falls through to `emitResult`. No output
of this phase is interpreted: the result file is the only channel.

No global state anywhere: `Box` is constructed in `main` from `os.Environ()`,
the runner and logger are injected, and every phase is a method taking the
context — the whole sequence unit-tests as a plain value with a fake runner.
