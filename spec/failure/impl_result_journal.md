# Implementation: Result and journal encoding

Covers ResultContract and Journal.

## Result types (internal/failure/result.go)

```go
type Status string

const (
    StatusOK     Status = "ok"
    StatusFailed Status = "failed"
    StatusHalted Status = "halted"
)

type Result struct {
    Status   Status          `json:"status"`
    Payload  json.RawMessage `json:"payload,omitempty"` // schema-validated upstream (agent module)
    Error    *ErrorRecord    `json:"error,omitempty"`
    Halt     *HaltRecord     `json:"halt,omitempty"`     // status halted's record arm
    Timing   Timing          `json:"timing"`
    Attempt  int             `json:"attempt"`            // 1-based, the attempt this record describes
    Attempts []AttemptInfo   `json:"attempts,omitempty"` // prior attempts, oldest first
    // AgentSkipped marks an attempt whose agent phase was skipped by the
    // prelude (an opted-in template): annotation metadata, valid on any
    // status, additive within the journal format.
    AgentSkipped bool `json:"agent_skipped,omitempty"`
}

type HaltRecord struct {
    Reason string `json:"reason"`           // stable machine word chosen by the halter
    Detail string `json:"detail,omitempty"` // human elaboration
    Phase  string `json:"phase,omitempty"`  // box phase that requested the halt
}

type ErrorRecord struct {
    Reason  string `json:"reason"`            // stable machine word (hook, agent, result-schema, ...)
    Detail  string `json:"detail"`            // human text, or JSON by per-reason convention (e.g. rate-limit reset)
    Handoff string `json:"handoff,omitempty"` // path relative to the run dir
}

type Timing struct {
    Started  time.Time `json:"started"`
    Finished time.Time `json:"finished"`
}

type AttemptInfo struct {
    Attempt int          `json:"attempt"`
    Timing  Timing       `json:"timing"`
    Error   *ErrorRecord `json:"error"`
}
```

`(*Result).Validate()` enforces the union: `ok` ⇒ Payload set, Error and Halt
nil; `failed` ⇒ Error set, Payload and Halt nil; `halted` ⇒ Halt set (with a
reason), Payload and Error nil; Attempt ≥ 1. Every boundary that accepts a
Result (journal append, threading, metering) calls it — cheap defense against
a hand-edited `result.json`.

## Input hash (internal/failure/hash.go)

```go
// InputHash keys journal reuse: resolved slot values + template identity + image tag.
func InputHash(inputs map[string]any, template, imageTag string) (string, error) {
    h := sha256.New()
    enc := json.NewEncoder(h)
    enc.SetEscapeHTML(false)
    // canonical: fixed envelope, sorted slot keys (json.Marshal on a map sorts,
    // but values are re-marshaled through a sort-keys walker for nested objects)
    err := enc.Encode(struct {
        Inputs   json.RawMessage `json:"inputs"` // canonicalized
        Template string          `json:"template"`
        Image    string          `json:"image"`
    }{canonicalJSON(inputs), template, imageTag})
    return hex.EncodeToString(h.Sum(nil)), err
}
```

Same canonicalization discipline as the config module's IR emission: sorted
keys, no HTML escaping, no floats introduced by round-tripping (inputs are the
already-typed slot values, not re-parsed YAML).

## Journal records (internal/failure/journal.go)

One JSONL file per run: `<runDir>/journal.jsonl`, opened `O_APPEND|O_CREATE`.
The store paths (`Begin`, `Reopen`, `Resume`) first take the run directory's
advisory `flock(2)` lock (`<runDir>/run.lock`, non-blocking, held for the
process lifetime and attached to the returned Journal — `Close` releases it),
so there is exactly one appender per run and torn-tail repair never runs
against a live writer. Every line is one record with a `kind` discriminator:

```go
type Header struct {
    Kind       string            `json:"kind"`   // "header"
    Format     int               `json:"format"` // journal schema stamp (JournalFormat = 1)
    RunID      string            `json:"run_id"`
    Name       string            `json:"name,omitempty"` // optional operator-given run name (--name)
    ConfigPath string            `json:"config_path"`
    ConfigHash string            `json:"config_hash"`
    Workflow   string            `json:"workflow"`
    Params     map[string]string `json:"params"` // --param k=v form, re-derivable to typed params
    IRHash     string            `json:"ir_hash"`
    IRVersion  int               `json:"ir_version"` // IR schema the hash was computed under
    Images     map[string]string `json:"images"`     // template -> resolved image tag at run start
    Started    time.Time         `json:"started"`
}

type ResultRecord struct {
    Kind      string `json:"kind"` // "result"
    StepID    string `json:"step_id"`
    InputHash string `json:"input_hash"`
    Result    Result `json:"result"`
}
// CostRecord ("cost": StepID, InputHash, metering.Cost),
// CleanupRecord ("cleanup": StepID, InputHash, OK bool, Detail) and
// RunEndRecord ("run-end": Status settled|aborted|paused, Failed, Halted,
// Finished) mirror the shape. appendHeader owns the Format stamp; Load
// refuses any other stamp (fail closed, no auto-migration). The name and the
// paused status arm are additive within format 1: an older faber replays the
// header unchanged (unknown JSON fields are dropped) and never meets a
// paused run-end mid-replay decision — run-end is informational to replay.
```

```go
type Journal struct {
    mu sync.Mutex
    f  *os.File
}

func (j *Journal) Append(rec any) error // marshal, single Write of line+"\n", then Sync
```

The mutex serializes concurrent step goroutines; one `Write` per line plus
`Sync` means a crash loses at most the in-flight line, never interleaves two.

## Replay (resume-side lookup)

```go
type Key struct{ StepID, InputHash string }

// Load replays a journal into the Replay view: header, last-wins result map
// (plus a per-step last-record index), cost and cleanup lists, and the
// last run-end marker. The logger receives the torn-tail and unknown-kind
// warnings.
func Load(path string, log *slog.Logger) (*Replay, error)
```

## Pause marker and the runs store surface (internal/failure/pause.go, store.go)

```go
const pauseFile = "pause" // beside journal.jsonl and run.lock in the run dir

// RequestPause writes the durable pause marker for a LIVE run (the run-lock
// flock probe gates it; a non-live run has nothing to drain and errors).
func (s *Store) RequestPause(runID string) error
// PauseRequested reports whether runDir carries the marker (the scheduler's
// scheduling-point probe — one stat, no lock).
func PauseRequested(runDir string) bool
// ClearPause removes a stale marker; a missing marker is not an error.
// Called by Resume and Begin under the freshly acquired run lock.
func ClearPause(runDir string) error
```

The liveness gate on RequestPause is advisory (the run can exit between probe
and write); the leftover marker is harmless — resume clears it on start, and
the scheduler that already appended its run-end never reads it again.

```go
// ResolveRunRef maps a run reference (id or header name) to a run id by
// enumerating run directories and reading headers. An existing run id wins
// over an equal name; an ambiguous name errors naming every matching id.
func (s *Store) ResolveRunRef(ref string) (string, error)

// PruneRuns deletes finished, non-live run directories and reports what it
// removed. Default: run-end present and status != paused. all=true widens to
// paused and incomplete non-live runs. Each candidate's run lock is acquired
// non-blocking immediately before deletion (refusal ⇒ skip), and the
// journal is os.Remove'd first under the lock: RemoveAll unlinks run.lock
// at an unspecified point, letting a racing resume flock a fresh inode —
// with the journal already gone that resume fails loudly at its journal
// read instead of losing appends.
func (s *Store) PruneRuns(all bool) ([]string, error)
```

`AuditRuns` (the upgrade guard's tolerant kind-probe scan) is enriched for
the `faber runs` listing: the header probe additionally captures `name`,
`workflow`, and `started`, and the trailing run-end probe captures its
`status` — still never interpretive replay, so any-format journals list.

`Load` scans line-by-line (`bufio.Scanner`, generous max token size),
dispatching on `kind`; unknown kinds are skipped with a log line (additive
forward compatibility within one format). A torn final line (crash
mid-append) is detected as a JSON parse error on the last line only, **and
only when the file does not end in a newline** — an unterminated fragment is
the crash artifact the one-write-per-line invariant predicts, and is dropped
with a warning; a newline-terminated malformed final line completed its
write, is genuine corruption, and is a hard error like any interior line.
Later result records for the same Key replace earlier ones, so a resumed
run's re-runs supersede naturally while the file itself remains append-only
history.
