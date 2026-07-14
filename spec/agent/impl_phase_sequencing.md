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
    RunUID, RunGID       int               // FABER_RUN_UID/GID; the preamble drops to these; 0/0 = no drop
    GitCache             string            // FABER_GIT_CACHE; ro object cache for clone --reference-if-able, empty = none
    SkillsLink           string            // FABER_SKILLS_LINK; $HOME-relative path to symlink at /faber/skills, empty = no skills leg
    SecretsStdin         bool              // FABER_SECRETS_STDIN=1; file-mode tokens arrive on stdin for phase 3 to materialize
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
    {"skills", (*Box).linkSkills},    // $HOME/<link> -> /faber/skills, no-op when unset
    {"env", (*Box).checkEnv},         // required slots present, dirs writable
    {"secrets", (*Box).loadSecrets},  // stdin payload -> /run/secrets/* (file mode); then /run/secrets/* -> process env
    {"hostkey", (*Box).applyHostKeyPolicy},
    {"clone", (*Box).clone},          // no-op when Repo == ""
    {"signing", (*Box).configureSigning},
    {"context", (*Box).runContextHook},
    {"prelude", (*Box).runPreludeHook},
    {"agent", (*Box).runAgent},
    {"result", (*Box).emitResult},
}

func Main(ctx context.Context, box *Box) int {
    if err := box.enterRunUser(); err != nil { // phase 0: chown writable mounts, drop root
        box.failStop("preamble", err)
        return 1
    }
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

`enterRunUser` is the privileged preamble (arch phase 0). When the box is root
and a run uid is set, it chowns exactly the writable mounts and drops:

```go
func (b *Box) enterRunUser() error {
    if os.Getuid() != 0 || b.Env.RunUID == 0 {
        return nil // already non-root, or no drop requested (gateless local)
    }
    home := "/home/box"
    for _, d := range []string{contract.ContainerWorkspace, b.Env.BundleDir, "/tmp", home} {
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
    if err := syscall.Setgroups([]int{b.Env.RunGID}); err != nil { return err }
    if err := syscall.Setgid(b.Env.RunGID); err != nil { return err }
    if err := syscall.Setuid(b.Env.RunUID); err != nil { return err } // all-thread since Go 1.16
    return nil
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
    Run(ctx context.Context, spec CmdSpec) (CmdResult, error) // captured output
    Stream(ctx context.Context, spec CmdSpec) (int, error)    // inherited stdio
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
