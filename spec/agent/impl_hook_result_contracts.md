# Implementation: Hook and result contracts

Covers PreludeHooks and ResultExtractor.

## Hook execution (internal/agent/hooks.go)

```go
type Hook struct {
    Phase string // "context" | "prelude"
    Path  string // /faber/hooks/<phase>, image content baked by the image builder
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

After the bundle loads, the prelude phase pops the one engine-consumed
sidecar key, `FABER_SKIP_AGENT`, BEFORE the reserved-name check (every other
`FABER_*` sidecar key remains a bundle-malformed violation): value `1` on an
`AgentOptional` template sets `Box.skipAgent`; value `1` on a template that
did not opt in logs "prelude requested an agent skip but the template does
not declare agent_optional; ignoring the request" and runs the agent; any
other value is `bundle-malformed` naming the key only — the value is
hook-authored bytes and never enters the record. The key is never merged
into the agent or hook environment.

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
stale-image detector (the baked faber-box predates or postdates this host:
run `faber build`) — not a migration path.

`FABER_INVOKE_PROFILE` (the concrete invocation-profile JSON) is the worked
example of the additive rule: an env variable whose absence the box tolerates
with a defined default (the built-in dialect) does **not** bump the version —
an old box under a new host ignores the unknown variable, and a new box under
an old host falls back to the default. `FABER_SESSIONS_DIR` rides the same
rule: the host sets it (a container path under `HOME`) exactly when it also
mounted the live sessions bind there — the pair-set discipline of
`FABER_SECRETS_STDIN` — and absence simply means capture is off, so the
preamble's gated component-chown never runs.

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

## The halt file

`$FABER_RESULT_DIR/halt.json` is the user phases' operator-stop request:
`{"reason": "<machine word>", "detail": "<human text>"}` — reason required,
detail optional, both opaque to the engine. The sequencer's post-phase check
(see the phase-sequencing section) consumes it into a `halted` attempt record
`{status: halted, halt: {reason, detail, phase}, timing, attempt}`; the read
is size-bounded like every container-boundary record, and a file that exists
but does not parse or lacks a reason converts into a `halt-invalid` failure
instead of a guess.

## The events file

`$FABER_RESULT_DIR/hooks/events.jsonl` (`contract.HooksEventsFile`) is the
hooks' own account of the attempt for the operator — which PR this cycle
is on, whether the push landed, why the hook halted and what to do next.
Hooks write it (creating `hooks/` themselves); the box never reads or
creates it, and nothing fails over it. The host-side on_failure hook is
given the same directory under the same `FABER_RESULT_DIR` name by the
failure module's hook runner, so all four slots share one file per attempt.

One JSON object per line:

```json
{"ts":"2026-11-04T10:00:05Z","hook":"post-review","event":"decision","pr":"17","result":"landable","mss":"clean"}
{"ts":"2026-11-04T10:00:09Z","hook":"post-review","event":"halt","reason":"approval-not-boolean","pr":"17","next":"clear the reservation on the PR, then resume"}
```

- `ts` RFC 3339, `hook` the writer's own name (a script does not know which
  slot mounted it), `event` a non-empty word; every further field a string,
  opaque, shown as `key=value`.
- Reserved words, and only these: an event named `halt` (`contract.EventHalt`)
  is the hook's account of a halt, whose `next` field is the one sentence an
  operator should act on; a field named `result` whose value is `ok` or
  `failed` is a status a view may colour. Nothing else is interpreted.

```go
type Event struct { Line int; TS time.Time; Hook, Name string; Fields map[string]string }
type Malformed struct { Line int; Err string }
type Events struct { Events []Event; Malformed []Malformed; Truncated bool }

func ReadEvents(path string) (Events, error)
```

`ReadEvents` never drops a line: one that is not a JSON object, lacks `ts`,
`hook` or `event`, carries a non-string field, or exceeds
`MaxEventLineBytes` comes back as `Malformed` with its line number. A
missing file is no events and no error. The read is bounded like every
container-boundary record, by `MaxEventsBytes` from the end of the file —
the newest lines, cut at a line boundary, with `Truncated` set and the line
numbers still true to the file. `pipeline.AttemptEvents` locates the file by
the attempt's result directory for the host side.

## Check runs

A check run (`FABER_CHECK=1`) has no agent to write `output.json`. When no
hook wrote it either, the result phase threads `PlaceholderPayload(schema)`
— `""` or an enum's first value for a string, `0` for an int, `false` for a
bool, `{}` for an object — which validates by construction, so the step
settles ok, its dependents bind, and their hooks run against typed values.
A hook that did write `output.json` is held to the schema exactly as in a
run. The record carries `check: true` beside `agent_skipped`; the host's
`ExtractResult` and the pipeline's adapter copy it through to the journal
and the report (`(check)` on the step line).

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
