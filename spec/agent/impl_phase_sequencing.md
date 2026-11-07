# Implementation: Phase sequencing

Covers PhaseSequencer and AgentInvoker.

## The binary (cmd/faber-box, internal/agent)

`faber-box` is built `CGO_ENABLED=0 GOOS=linux` from the same module as the
host engine and baked into every template image by the image builder at
`/faber/bin/faber-box`; it shares the config
module's schema types (the output schema) but imports nothing host-only.

```go
type Box struct {
    Env     *BoxEnv
    Runner  CmdRunner     // os/exec behind an interface; faked in tests
    Log     *slog.Logger
    Frames  *contract.FrameWriter // the box's stdout; nil ships nothing (unit tests with a fake runner)
    Workdir string        // set by clone (or scratch)
    Bundle  *Bundle       // set after prelude
    Timing  map[string]time.Duration
}

type BoxEnv struct {
    Skill, Identity      string
    AgentCLI             string            // FABER_AGENT_CLI; the agent binary the box invokes (no vendor default)
    ResultDir, BundleDir string
    RemoteURL            string
    Repo                 string            // reserved input; empty = gateless
    Inputs               map[string]string // slot name -> stringified value
    HostKey              string
    TOFU                 bool
    GitName, GitEmail    string
    Model, Effort, ExtraInstruction, MaxBudget string
    Attempt              int
    OutputSchema         config.OutputSchema // decoded from FABER_OUTPUT_SCHEMA
    RunUID, RunGID       int               // FABER_RUN_UID/GID; the preamble drops to these; 0/0 = no drop
    AgentSocket          string            // SSH_AUTH_SOCK; the box's agent socket the relay serves, waited for before the clone; "" = no identity
    GitCache             string            // FABER_GIT_CACHE; ro object cache for clone --reference-if-able, empty = none
    SkillsLink           string            // FABER_SKILLS_LINK; $HOME-relative path to symlink at /faber/skills, empty = no skills leg
    SecretsStdin         bool              // FABER_SECRETS_STDIN=1; file-mode tokens arrive on stdin for phase 3 to materialize
    AgentOptional        bool              // FABER_AGENT_OPTIONAL=1; the template declares itself agent-skippable (exact "1", like TOFU)
    Invoke               config.ResolvedInvoke // FABER_INVOKE_PROFILE (concrete JSON); absent/empty ⇒ config.DefaultInvoke(); malformed or rule-breaking ⇒ env-phase violation
    SessionsDir          string            // FABER_SESSIONS_DIR; container path under HOME the box ships as sessions/<rel> at the end, "" = capture off
    Slots                []string          // FABER_INPUT_SLOTS; declared slot names, for the slot-keyed handoff
    // FABER_CONTRACT_VERSION is validated by the env phase (see the contract
    // version handshake in impl_hook_result_contracts.md)
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
    User bool // user-filled phase: a halt request is honored after it
}

var phases = []Phase{
    {"skills", (*Box).linkSkills, false},    // $HOME/<link> -> /faber/skills, no-op when unset
    {"env", (*Box).checkEnv, false},         // required slots present, dirs writable
    {"secrets", (*Box).loadSecrets, false},  // stdin payload -> /run/secrets/* (file mode); then /run/secrets/* -> process env
    {"hostkey", (*Box).applyHostKeyPolicy, false},
    {"clone", (*Box).clone, false},          // no-op when Repo == ""
    {"signing", (*Box).configureSigning, false},
    {"context", (*Box).runContextHook, true},
    {"prelude", (*Box).runPreludeHook, true},
    {"agent", (*Box).runAgent, true},
    {"postlude", (*Box).runPostludeHook, true},
    {"result", (*Box).emitResult, false},
}

func Main(ctx context.Context, box *Box) int {
    if err := box.enterRunUser(); err != nil { // phase 0: chown writable mounts, drop root
        box.failStop("preamble", err)
        return 1
    }
    stopTail := box.tailEvents(ctx) // event frames while the user phases run
    defer stopTail()
    for _, p := range phases {
        start := time.Now()
        err := p.Run(box, ctx)
        box.Timing[p.Name] = time.Since(start)
        if err != nil {
            box.failStop(p.Name, err) // handoff.json + failed result.json
            return 1
        }
        if p.User { // context|prelude|agent|postlude: honor a halt request
            switch halted, herr := box.checkHalt(ctx, p.Name); {
            case herr != nil: // halt.json present but malformed: fail loudly
                box.failStop(p.Name, herr) // reason halt-invalid
                return 1
            case halted: // halted result.json written; later phases never run
                return 0
            }
        }
    }
    return 0
}
```

`main` calls `Main` and then, whatever it returned, `box.Ship(ctx)`: the
events tail is stopped and flushed, every regular file under `Env.ResultDir`
goes out as a `result/<rel>` frame (symlinks skipped and logged: none may go
among the records), `result.json` last, then with `Env.SessionsDir` set
every entry under it as `sessions/<rel>` with its mtime, symlinks as
`link`, then `end`. A file over `MaxRecordBytes` is logged and
skipped. `Ship` is the one place the records leave the container, so it
cannot be skipped by any exit path: `Main` returns a code on every path and
never exits the process itself.

```go
func (b *Box) tailEvents(ctx context.Context) (stop func())   // polls hooks/events.jsonl, sends complete new lines as event frames
func (b *Box) Ship(ctx context.Context)                       // result/<rel> (result.json last), sessions/<rel>, end
```

`checkHalt` reads `$FABER_RESULT_DIR/halt.json` (size-bounded like every
container-boundary record): absent ⇒ `(false, nil)`; unparseable or missing
its `reason` ⇒ a `halt-invalid` boxError; else it writes the `halted` attempt
record — `{status: halted, halt: {reason, detail, phase}, timing, attempt}` —
and returns `(true, nil)`. The check runs only after a phase that exited 0
(a failing phase takes the fail-stop funnel regardless of any halt file), and
only for the user-filled phases — engine phases never halt.

`enterRunUser` is the privileged preamble (arch phase 0). When the box is root
and a run uid is set, it chowns exactly the writable mounts and drops:

```go
func (b *Box) enterRunUser() error {
    if os.Getuid() != 0 || b.Env.RunUID == 0 {
        return nil // already non-root, or no drop requested (gateless local)
    }
    home := "/home/box"
    for _, d := range []string{contract.ContainerWorkspace, b.Env.ResultDir, b.Env.BundleDir, "/tmp", home} {
        if err := os.Chown(d, b.Env.RunUID, b.Env.RunGID); err != nil {
            return fmt.Errorf("preamble: chown %s: %w", d, err)
        }
    }
    // /run/secrets is a gated add: the --tmpfs is present only in file mode and
    // is mounted root-owned, so chown it — but only when it exists — so phase 3
    // can write the 0600 files as the dropped run user.
    if _, err := os.Stat(contract.ContainerSecretsDir); err == nil {
        if err := os.Chown(contract.ContainerSecretsDir, b.Env.RunUID, b.Env.RunGID); err != nil {
            return fmt.Errorf("preamble: chown %s: %w", contract.ContainerSecretsDir, err)
        }
    }
    b.setEnv("HOME", home) // b.Environ only — never os.Setenv (no-global-state policy)
    return dropTo(b.Env.RunUID, b.Env.RunGID) // setgroups {gid}, setgid, setuid (all-thread since Go 1.16); shared with the relay
}
```

`linkSkills` is the skills leg (arch phase 1): the one agent-specific
translation, driven entirely by config so faber never hardcodes `.claude`. It
resolves `HOME` from the **box environment** (`b.lookupEnv("HOME")`, which scans
`b.Environ` like `setEnv`), never `os.Getenv`: the preamble sets `HOME=/home/box`
via `b.setEnv`, which mutates only `b.Environ` (the no-global-state policy), so
on the production drop path the process `HOME` still reads `/root` while the box
`HOME` is the writable tmpfs — and the link must land on the box scratch the
agent and hooks below also use. On the no-drop local path (non-root or
`RunUID==0`, e.g. the box-lifecycle tests running the binary as a plain process)
`b.Environ`'s `HOME` is whatever the caller/harness put there. It is a no-op
when no `skills` leg was declared:

```go
// lookupEnv scans b.Environ for key= (mirrors setEnv); the box env, not the
// process env, is authoritative for HOME and every other phase value.
func (b *Box) lookupEnv(key string) string {
    prefix := key + "="
    for _, kv := range b.Environ {
        if strings.HasPrefix(kv, prefix) {
            return kv[len(prefix):]
        }
    }
    return ""
}

func (b *Box) linkSkills(ctx context.Context) error {
    if b.Env.SkillsLink == "" {
        return nil // no skills leg on this template
    }
    link := filepath.Join(b.lookupEnv("HOME"), b.Env.SkillsLink)
    if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
        return fmt.Errorf("skills: mkdir %s: %w", filepath.Dir(link), err)
    }
    // os.Symlink, not a shell command: the image is shell-less. The target is
    // the read-only engine mount; the link name is opaque agent config.
    if err := os.Symlink(contract.ContainerSkillsDir, link); err != nil {
        return fmt.Errorf("skills: symlink %s -> %s: %w", link, contract.ContainerSkillsDir, err)
    }
    return nil
}
```

`failStop` is the single failure funnel (see the hook/result impl section for
the record shapes). `context.Context` threads through every phase; the first
pass sets no deadlines (timeouts are a deferred failure-module seam) but the
cancellation plumbing is in place.

## Setup phases

```go
type CmdRunner interface {
    Run(ctx context.Context, spec CmdSpec) (CmdResult, error) // captured stdout
    Stream(ctx context.Context, spec CmdSpec) (int, error)    // stdout AND stderr to the box's stderr: fd 1 is the frame channel
}
type CmdSpec struct{ Argv []string; Dir string; Env []string }
type CmdResult struct{ Stdout []byte; StderrTail []byte; ExitCode int }
```

- `loadSecrets`: two steps. First, when `b.Env.SecretsStdin` is set (file
  mode), it reads all of `os.Stdin` to EOF (`io.ReadAll`), `json.Unmarshal`s the
  single object into `map[string]string`, `base64.StdEncoding`-decodes each
  value, and `os.WriteFile(filepath.Join(contract.ContainerSecretsDir, name),
  tok, 0o600)` — materializing the container-RAM secret files that the preamble
  already chowned to the run user. A malformed payload or a decode error aborts
  the phase (reason `secrets`). Stdin is read exactly once and only here; the
  headless agent never touches it, and faber closes stdin so the read sees a
  clean EOF. Second (unchanged, and whether or not the stdin step ran):
  `os.ReadDir(contract.ContainerSecretsDir)` — a missing dir is treated as no
  secrets (the common proxy/helper case) — and each regular file is exported as
  `strings.ToUpper(name)` = trimmed contents, `os.Setenv` only — the value
  exists in this process tree, never in the docker argv.
- `applyHostKeyPolicy`: pinned key → write a known-hosts file and export
  `GIT_SSH_COMMAND="ssh -o UserKnownHostsFile=<f> -o StrictHostKeyChecking=yes"`;
  TOFU → `accept-new`; neither, with an ssh remote URL → error before any
  network phase runs.
- `clone`: `git clone [--reference-if-able <GitCache>] <RemoteURL>/<Repo>.git
  <workdir>` via the runner, into `/workspace/<Repo>`; sets `Box.Workdir`. The
  `--reference-if-able` flag is added only when `GitCache` is set, so per-box
  clones borrow objects from the shared read-only cache without duplicating
  history. Gateless steps get `os.MkdirTemp` instead and skip signing.
- `configureSigning`: `ssh-add -L` via the runner; `len(lines) != 1` is an
  error naming the count; empty `Env.GitEmail` is an error naming
  the contract env's committer email, which the host injects per role from
  the registry — the abort names the `add-key --git-email` remedy (gated
  steps require an explicit committer email; no
  synthetic fallback); then four `git config` invocations (`gpg.format
  ssh`, `user.signingkey "key::"+pub`, `commit.gpgsign true`, name with
  default `faber-<identity>`, email verbatim).

- `runPostludeHook`: identical shape to `runPreludeHook` — no-op when the
  hook file is absent (the absence CHECK is per hook; the shared
  `hookDeclared` flag it sets is consumed only by the pre-agent pair, which
  already ran); runs
  the baked `/faber/hooks/postlude` with the box child env, cwd = the
  workdir; nonzero exit fail-stops with phase `postlude` and the stderr
  tail in the handoff. No bundle-existence assertion follows it (that
  gate belongs to the pre-agent pair); the result phase's own validations
  run next regardless.

## AgentInvoker

A pure expander over the concrete invocation profile (see
`arch_agent_invoker.md`) — no vendor literal in the file:

```go
type Invocation struct {
    CLI     string                // agent binary name; must be in the template's package set
    Profile config.ResolvedInvoke // concrete dialect from BoxEnv (default when the env var was absent)
    Skill   string
    Body    string                // CONTEXT.md bytes, verbatim
    Extra, Model, Effort, MaxBudget string
}

// Prompt expands Profile.PromptTemplate over {skill}, {body}, {extra} via
// strings.Replacer — substituted text is never re-scanned, so bundle bytes
// cannot inject into the template. {extra} is the ADDITIONAL INSTRUCTION
// trailer, or empty.
func (i Invocation) Prompt() string

func (i Invocation) Argv() []string {
    p := i.Profile
    argv := append([]string{i.CLI}, p.Subcommand...)
    if p.PromptFlag != "" { argv = append(argv, p.PromptFlag) } // "" ⇒ positional prompt
    argv = append(argv, i.Prompt())
    if p.SkillMode == config.SkillModeFlag { argv = append(argv, p.SkillFlag, i.Skill) }
    argv = append(argv, p.FixedFlags...)
    // A pair is emitted only when BOTH the profile flag and the value are set.
    if p.ModelFlag != "" && i.Model != ""       { argv = append(argv, p.ModelFlag, i.Model) }
    if p.EffortFlag != "" && i.Effort != ""     { argv = append(argv, p.EffortFlag, i.Effort) }
    if p.BudgetFlag != "" && i.MaxBudget != ""  { argv = append(argv, p.BudgetFlag, i.MaxBudget) }
    return argv
}
```

A byte-for-byte table test pins the default profile's argv and prompt to the
historical output (`agent-cli -p /skill\n\nbody --permission-mode
bypassPermissions …`), so the profile mechanism can never drift the default
dialect.

`runAgent` builds the Invocation from `BoxEnv` + `Bundle.Doc`, merges the
bundle sidecar values into the child environment, and calls `Runner.Stream`
(both of the child's streams go to the container's stderr — agent output
belongs to the human log, never to a parser, and never to the frame
channel). A nonzero exit code returns an error carrying the code and the
stderr tail for the handoff; exit 0 falls through to `emitResult`. No output
of this phase is interpreted: the result file is the only channel.

When the prelude requested the skip (`Box.skipAgent`, set by the bundle read
on an opted-in template), `runAgent` logs "agent skipped by prelude" and
returns nil without invoking anything; `emitResult` stamps
`agent_skipped: true` into the record and, when the fallback path then finds
required outputs unsatisfied, prefixes the `missing-output` detail with the
skip so the diagnosis names the prelude, not a silent agent.

No global state anywhere: `Box` is constructed in `main` from `os.Environ()`,
the runner and logger are injected, and every phase is a method taking the
context — the whole sequence unit-tests as a plain value with a fake runner.

## The hold, receive and relay entries

```go
// agent/box, dispatched by cmd/faber-box on its first argument
func Hold(ctx context.Context) int                                              // blocks until the context is cancelled (SIGTERM, SIGINT)
func Receive(ctx context.Context, env *BoxEnv, in io.Reader, log *slog.Logger) error
func Relay(ctx context.Context, env *BoxEnv, in io.Reader, out io.Writer, log *slog.Logger) error
```

`Receive` runs `contract.ReadFrames` over its stdin with a handler that
roots `handoff/` at `env.HandoffDir` (`contract.ContainerHandoffDir`, with
the usual plain-process override) and `sessions/` at `env.SessionsDir`
(refusing `sessions/` when it is unset, and any other first segment), each
tree behind an `os.Root` opened on first use so no frame can resolve outside
it, writes each file through a temporary name, applies the frame's mtime,
recreates links, and then — as root — chowns both trees to
`env.RunUID:env.RunGID` plus what the preamble would have chowned and a held
box lacks: the workspace, `/tmp`, `HOME`, and the components between `HOME`
and the sessions directory (`holdChownPaths`), so the operator's exec, which
drops to that user, owns what it finds and can write beside it. Foreign
bytes on its stdin are a stream error: nothing but faber feeds it.

`Relay` listens on `env.AgentSocket` (`SSH_AUTH_SOCK`; refused when empty),
creating its directory, chmods the socket 0600 and — as root with a run uid
set — chowns the socket and then its directory to `env.RunUID:env.RunGID`
(the socket first: once the run user owns the directory it could replace
the socket with a link before root's chown) and calls `dropTo` before
accepting. One goroutine owns `in`: it loops `readAgentMessage(in)` (4-byte
big-endian length, `1..maxAgentMessage` = 256 KiB, then the body) onto a
replies channel, and its first error — the host's stream ending — is
recorded and ends the relay at once, not at the next exchange. Each accepted
connection is served by a goroutine that loops `readAgentMessage(conn)`
(a bad length closes that connection alone) and, under one mutex for the
whole relay, writes the message to `out` and takes the next reply off the
channel for it. When the listener closes — the host's stream ending, or the
context — every live connection is closed too, so a client mid-conversation
is cut off rather than holding the relay open; an EOF from the host is an
orderly end. The clone phase's `waitAgentSocket` dials `env.AgentSocket`
every 50 ms (a dial, not a stat: the socket exists root-owned for an instant
before the chown) up to `agentSocketWait` (30 s, a package variable the
tests lower) and fails with `ReasonSigning` past it; it runs before the
gateless check, so a gateless step with an identity waits as well.

## Check runs

`FABER_CHECK=1` (`contract.EnvCheck`, parsed by the env phase into
`BoxEnv.Check`; any other value is an env-contract violation) changes two
phases and nothing else in the table: `runAgent` returns after logging
"agent skipped: check run" whatever the template declares — the hooks
before and after it run unchanged — and `emitResult`, when `readOutput`
reports the fallback, threads `contract.PlaceholderPayload(schema)` instead
of the empty payload before validating as usual. Every record of the
attempt, ok or failed, carries `check: true`.
