# Implementation: Result and journal encoding

Covers ResultContract and Journal.

## Result types (internal/failure/result.go)

```go
type Status string

const (
    StatusOK     Status = "ok"
    StatusFailed Status = "failed"
)

type Result struct {
    Status   Status          `json:"status"`
    Payload  json.RawMessage `json:"payload,omitempty"` // schema-validated upstream (agent module)
    Error    *ErrorRecord    `json:"error,omitempty"`
    Timing   Timing          `json:"timing"`
    Attempt  int             `json:"attempt"`            // 1-based, the attempt this record describes
    Attempts []AttemptInfo   `json:"attempts,omitempty"` // prior attempts, oldest first
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

`(*Result).Validate()` enforces the union: `ok` ⇒ Payload set, Error nil;
`failed` ⇒ Error set, Payload nil; Attempt ≥ 1. Every boundary that accepts a
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
// RunEndRecord ("run-end": Status settled|aborted, Failed, Finished) mirror
// the shape. appendHeader owns the Format stamp; Load refuses any other
// stamp (fail closed, no auto-migration).
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
