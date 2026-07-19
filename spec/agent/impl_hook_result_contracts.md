# Implementation: Hook and result contracts

Covers PreludeHooks and ResultExtractor.

## Hook execution (internal/agent/hooks.go)

```go
type Hook struct {
    Phase string // "context" | "prelude"
    Path  string // /faber/hooks/<phase>, bind-mounted read-only
}

func (b *Box) runHook(ctx context.Context, h Hook) error
```

- Missing hook file for an undeclared hook is not an error — the phase is a
  no-op; the sequencer tracks whether *any* hook was declared.
- Execution via `Runner.Run`: cwd = `b.Workdir`, env = the box environment
  verbatim (the `FABER_INPUT_*` exports are already the typed-inputs
  contract), stdout to the container log, stderr additionally kept in a
  fixed-size tail buffer (last 4 KiB) for the handoff record.
- Nonzero exit wraps into `hookError{Phase, ExitCode, StderrTail}`.
- After the prelude phase, `LoadBundle` enforces the postcondition.

## The bundle

```go
type Bundle struct {
    Dir string
    Doc string            // CONTEXT.md — required, non-empty
    Env map[string]string // bundle.env — optional sidecar
}

func LoadBundle(dir string) (*Bundle, error) // ErrNoBundle on missing/empty Doc
```

`bundle.env` parsing is deliberately dumb: line-oriented `KEY=VALUE`, `#`
comments and blank lines skipped, no quoting, no expansion — values are opaque
bytes. A malformed line is a prelude-phase contract error, not a warning.
When no hook was declared, `synthesizeBundle(inputs)` writes a minimal
`CONTEXT.md` from the sorted input names and values, so the agent phase sees
one shape regardless of template.

## The handoff record

Written by `failStop` to `$FABER_RESULT_DIR/handoff.json`, with the bundle
directory copied to `$FABER_RESULT_DIR/handoff/bundle/`:

```go
type Handoff struct {
    Keying     string            `json:"keying,omitempty"` // "slot" | "" (pre-versioning, token-keyed)
    Phase      string            `json:"phase"`
    Reason     string            `json:"reason"`
    ExitCode   int               `json:"exit_code,omitempty"`
    StderrTail string            `json:"stderr_tail,omitempty"`
    Inputs     map[string]string `json:"inputs"` // bound input values only — never secret env
    Workdir    string            `json:"workdir"`
}
```

`Keying` names the `Inputs` key vocabulary. Whenever the host supplied the
declared slot list (`FABER_INPUT_SLOTS`), the box records inputs keyed by
slot names and stamps `keying: "slot"` — the shape interactive re-entry
feeds straight back into the slot-named run contract. An absent `Keying` is
a pre-versioning record keyed by `FABER_INPUT_*` env tokens; re-entry
translates it forward through the template's declared slots (slot→token is
total; the reverse is lossy, which is why the box records slots now). A
record with no usable value for a required slot is refused at re-entry with
a clear message.

The failed attempt record's `error.handoff` carries the path; the interactive
recovery mode reconstructs the box from exactly this data.

## Contract version handshake

`contract.ContractVersion` (currently 1) is the faber↔faber-box result
contract's schema version, independent of the application version. The host
stamps it into the box env as `FABER_CONTRACT_VERSION`; the box's env phase
refuses a mismatching value (absence is tolerated for direct sequencer
invocations), and `WriteResultFile` stamps the writer's version into
`result.json` (`contract` field), which the host asserts on extract. Since
faber-box ships from the host as the same build, any mismatch is a
`FABER_BOX_BIN`-misconfiguration detector — a stale or foreign sequencer —
not a migration path.

## Result emission (internal/agent/result.go)

```go
func (b *Box) emitResult(ctx context.Context) error {
    payload, fallback, err := readOutput(b.Env.ResultDir) // output.json
    // absent file after agent exit 0 => payload = {}, fallback = true
    verrs := config.ValidateOutput(b.Env.OutputSchema, payload) // all collected
    if len(verrs) == 0 {
        verrs = b.verifySideEffects(ctx) // declared postconditions
    }
    return writeResult(b.Env.ResultDir, failure.Result{ /* status, payload|error,
        timing: b.Timing, attempt: b.Env.Attempt, fallback marker */ })
}
```

- `ValidateOutput` walks the decoded JSON: required fields present, kinds
  exact (a JSON number satisfies an `int` slot only when integral; no
  string/int coercion), enum membership. Undeclared extra fields are kept in
  the payload but flagged `unthreaded` in the record.
- `verifySideEffects`: the declared-effect table has one first-pass row —
  `Bundle.Env["BRANCH"]` with a bound repo runs
  `git ls-remote --exit-code origin refs/heads/<name>` via the runner; a
  nonzero exit yields reason `side-effect-unverified`.
- `writeResult` marshals the failure module's record type and writes it
  atomically (temp file + `os.Rename` within the result dir): the mounted
  directory never exposes a half-written record.
- Every path through `failStop` also ends in `writeResult` with a failed
  record — one record per attempt is an invariant of the binary, not a
  convention.

## The host half

```go
// called by pipeline after the container exits
func ExtractResult(dir string, schema config.OutputSchema) (failure.Result, error)
```

Re-parses `result.json`, re-validates the payload against the same schema
(defense against a tampering agent — mis-shaped data must not thread), and
synthesizes `failed / box-vanished` when the file is missing or unparseable.
This function is the only agent-module code the host executes; everything
else in this section runs inside the box.
