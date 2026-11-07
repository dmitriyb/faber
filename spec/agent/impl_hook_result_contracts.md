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

## The frame channel

The box's standard output is not a log: it is the channel the records
travel on. One binary record shape, read and written by the contract
package (`frame.go`) so both halves link the same code:

```
\x00faber\x00<kind>\x00<name>\x00<mtime>\x00<len>\n<len bytes>
```

- `kind` is `event`, `file`, `link` or `end` (`FrameEvent`, `FrameFile`,
  `FrameLink`, `FrameEnd`); anything else is a stream error.
- `name` is a relative slash path with no empty, `.` or `..` segment, no
  leading slash, no backslash and no NUL (`ValidFrameName`) — the same
  containment rule the handoff pointer obeys. `end` carries no name.
- `mtime` is the file's modification time in unix nanoseconds, `0` where it
  has no meaning; a reader applies it to the file it writes, because a
  harness finds its latest session by mtime.
- `len` is the body length in decimal, at most `MaxRecordBytes`; the header
  itself is bounded (`MaxFrameHeaderBytes`, 4 KiB). The leading NUL is what
  keeps a text line from ever parsing as a header.

```go
type FrameWriter struct{ w io.Writer; mu sync.Mutex }
func NewFrameWriter(w io.Writer) *FrameWriter
func (w *FrameWriter) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error
func (w *FrameWriter) Event(line []byte) error       // one events.jsonl line, kind event, name hooks/events.jsonl
func (w *FrameWriter) End() error

// FrameHandler receives each frame's body as a bounded reader it must drain,
// and the bytes that arrive outside any frame (a foreign writer on stdout).
type FrameHandler interface {
    Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error
    Foreign(p []byte) error
}
func ReadFrames(r io.Reader, h FrameHandler) error
```

`ReadFrames` is a streaming reader: it never buffers a body, it hands the
handler an `io.LimitReader` over the stream and drains whatever the handler
left. Bytes before the next magic that are not a header go to `Foreign`
instead of failing the stream — a foreign writer on the container's stdout
(a toolset binary that ignores the convention) must not cost the attempt
its record. A header that parses but violates a bound, an unknown kind or an
invalid name is a stream error and ends the read; the demuxer records it.

What the box sends, in order, on every exit path of `Main`:

1. during the user phases, each complete new line of
   `$FABER_RESULT_DIR/hooks/events.jsonl` as an `event` frame, so the
   operator's view of a hook's account is live;
2. after the result phase (or the fail-stop, or the halt), every regular
   file under `$FABER_RESULT_DIR` as `result/<rel>` (`file`),
   `result/result.json` last; a file over the frame bound is skipped with a
   log line, never cut, and so is a symlink — none may go among the records,
   and the host refuses one there, so a record can never be read through a
   link the box planted;
3. with `FABER_SESSIONS_DIR` set, every entry under it as `sessions/<rel>`
   with its mtime, symlinks as `link` with the target in the body (a
   harness's latest-pointer is a real link), the same way;
4. `end`.

The host side (pipeline's demuxer, spec/pipeline/impl_scheduling.md) roots
`result/` and `sessions/` under the attempt directory and refuses any other
first segment, appends `event` frames to the host's `hooks/events.jsonl`,
writes every file to a temporary name and renames it into place through an
`os.Root` of its tree, so a truncated frame is an absent file, never a
half-written record, and a link the box planted cannot redirect a later
frame outside the tree. Because `result.json` is sent after the other result
files, a stream that breaks before it extracts as `box-vanished`, with the
stream error in the record's detail; a break among the sessions after it
leaves the record standing.

## Contract version handshake

`contract.ContractVersion` (currently 2) is the faber↔faber-box result
contract's schema version, independent of the application version. The host
stamps it into the box env as `FABER_CONTRACT_VERSION`; the box's env phase
refuses a mismatching value (absence is tolerated for direct sequencer
invocations), and `WriteResultFile` stamps the writer's version into
`result.json` (`contract` field), which the host asserts on extract. Since
faber-box ships from the host as the same build, any mismatch is a
stale-image detector (the baked faber-box predates or postdates this host:
run `faber build`) — not a migration path. Version 2 is the frame channel:
a v1 box writes its records into a mount the v2 host no longer provides, a
v2 box's stdout is frames a v1 host would file as output, so neither pair
may run.

`FABER_INVOKE_PROFILE` (the concrete invocation-profile JSON) is the worked
example of the additive rule: an env variable whose absence the box tolerates
with a defined default (the built-in dialect) does **not** bump the version —
an old box under a new host ignores the unknown variable, and a new box under
an old host falls back to the default. `FABER_SESSIONS_DIR` rides the same
rule: the host sets it (a container path under `HOME`) exactly when capture
is on and the profile names a `session_dir`, the box ships that directory
at the end as `sessions/<rel>` frames, and absence simply means capture is
off, so nothing is shipped.

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
  atomically (temp file + `os.Rename` within the result dir, a tmpfs); the
  shipping step then sends it last, so the host never sees a record ahead of
  the files it points at.
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
Hooks write it (creating `hooks/` themselves); the box never interprets or
creates it, and nothing fails over it — the box only tails it, sending each
complete new line to the host as an `event` frame while the user phases run,
so the host's copy under the attempt's result directory grows as the hooks
write. The host-side on_failure hook is
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

Re-parses `result.json` from the attempt's result directory on the host —
the file the demuxer wrote from the box's last frame — re-validates the
payload against the same schema (defense against a tampering agent —
mis-shaped data must not thread), and synthesizes `failed / box-vanished`
when the file is missing or unparseable.
This function is the only agent-module code the host executes; everything
else in this section runs inside the box.
