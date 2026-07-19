package failure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// JournalFormat is the journal file's schema version, stamped into every
// header this package writes. Independent of the application version —
// bumped only when the on-disk record shapes change. Replay fails closed on
// any other stamp (and on the absent stamp of a pre-versioning journal):
// there is no auto-migration; a mismatched run is finished on the faber that
// wrote it or restarted with --fresh.
const JournalFormat = 1

// Journal record kinds — the "kind" discriminator on every JSONL line.
// Unknown kinds are skipped (and logged) on replay: a same-format journal
// only ever gains additive kinds, so skipping is forward-compatible within
// one format.
const (
	KindHeader  = "header"
	KindResult  = "result"
	KindCost    = "cost"
	KindCleanup = "cleanup"
	KindDefer   = "defer"
	KindRunEnd  = "run-end"
)

// Header is the journal's first line, written at run start. Resume
// compatibility is defined against it: the config pipeline is deterministic,
// so "same IR hash" means "same graph", and a mismatch is detected before any
// skip decision is trusted. Params is the supplied params in string (--param
// k=v) form, sufficient to re-derive the typed params. Format is the journal
// schema stamp; IRVersion the IR schema the hash was computed under (so an
// engine-side IR change is distinguishable from operator config drift); and
// Images the per-template image tags resolved at run start — engine-compiled
// inputs the IR hash cannot see, compared on resume so a pin or engine
// upgrade cannot silently invalidate every journal key while the guard
// passes.
type Header struct {
	Kind       string            `json:"kind"`
	Format     int               `json:"format,omitempty"`
	RunID      string            `json:"run_id"`
	ConfigPath string            `json:"config_path"`
	ConfigHash string            `json:"config_hash"`
	Workflow   string            `json:"workflow"`
	Params     map[string]string `json:"params"`
	IRHash     string            `json:"ir_hash"`
	IRVersion  int               `json:"ir_version,omitempty"`
	Images     map[string]string `json:"images,omitempty"`
	Started    time.Time         `json:"started"`
}

// ResultRecord journals one settled step attempt sequence: the final Result
// (attempt history inside) under its (step-id, input-hash) key.
type ResultRecord struct {
	Kind      string `json:"kind"`
	StepID    string `json:"step_id"`
	InputHash string `json:"input_hash"`
	Result    Result `json:"result"`
}

// CostRecord is the home for a step's actual-cost accounting (the metering
// module's actual(result) output), keyed like the result record so a resumed
// run's cost fold does not double-count skipped steps. The cost body is
// opaque to this module.
type CostRecord struct {
	Kind      string          `json:"kind"`
	StepID    string          `json:"step_id"`
	InputHash string          `json:"input_hash"`
	Cost      json.RawMessage `json:"cost"`
}

// CleanupRecord reports an on_failure hook's outcome, attached to the step it
// cleaned up after — reported alongside, never replacing, the failure it
// followed.
type CleanupRecord struct {
	Kind      string `json:"kind"`
	StepID    string `json:"step_id"`
	InputHash string `json:"input_hash"`
	Attempt   int    `json:"attempt"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
}

// DeferRecord journals one defer decision at the moment it happens — a
// waiting state is durable state (the spec requires every defer journaled
// before the scheduler re-queues), so a crash mid-defer leaves a timeline the
// report and resume can see rather than losing the wait entirely. Zero Until
// is the re-check-on-next-settlement shape.
type DeferRecord struct {
	Kind   string    `json:"kind"`
	StepID string    `json:"step_id"`
	Until  time.Time `json:"until,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Run-end statuses.
const (
	RunEndSettled = "settled" // every node reached a terminal state
	RunEndAborted = "aborted" // the run stopped early (cancel, journal failure)
)

// RunEndRecord marks the end of one execution of the run: the executor
// appends it after the scheduler returns, settled or aborted. Its absence is
// the durable signature of an interrupted run — what the pre-upgrade guard
// looks for. A resumed run appends a fresh run-end when it finishes; replay
// is last-wins.
type RunEndRecord struct {
	Kind     string    `json:"kind"`
	Status   string    `json:"status"`
	Failed   int       `json:"failed"` // failed steps this execution settled
	Detail   string    `json:"detail,omitempty"`
	Finished time.Time `json:"finished"`
}

// Key is the journal's result key: (step-id, input-hash).
type Key struct {
	StepID    string
	InputHash string
}

// maxJournalLine bounds a single journal line on both sides: append refuses
// a record that replay's line scanner could not read back, so an oversized
// record fails loudly at write time instead of poisoning every later Load.
const maxJournalLine = 64 * 1024 * 1024

// Journal is one run's append-only JSONL file. The mutex serializes
// concurrent step goroutines; one Write per line plus Sync means a crash
// loses at most the in-flight line and never interleaves two. A journal
// opened through the Store additionally owns the run's advisory lock
// (single-appender invariant); Close releases it after the file.
type Journal struct {
	mu    sync.Mutex
	f     *os.File
	lock  *RunLock // run-level exclusivity; nil for direct OpenJournal callers
	limit int      // max line bytes; 0 ⇒ maxJournalLine (lowerable in tests)
}

// OpenJournal opens (creating if absent) a journal file for appending. A
// pre-existing file is first repaired: a torn final line (crash mid-append)
// is truncated away — matching Load's drop semantics — so a subsequent append
// can never merge onto the fragment and corrupt the line framing. Callers
// must hold the run's exclusivity lock (the Store paths do) — repair against
// a live appender would truncate a line it just committed.
func OpenJournal(path string) (*Journal, error) {
	if err := repairTornTail(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failure: open journal: %w", err)
	}
	return &Journal{f: f}, nil
}

// repairTornTail truncates a trailing partial line (one not terminated by
// '\n') left by a crash mid-append. The one-write-per-line invariant means
// everything before the last newline is intact; the fragment after it is
// exactly what Load would drop as torn.
func repairTornTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to repair; append will create the file
		}
		return fmt.Errorf("failure: repair journal %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failure: repair journal %s: %w", path, err)
	}
	size := st.Size()
	if size == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return fmt.Errorf("failure: repair journal %s: %w", path, err)
	}
	if last[0] == '\n' {
		return nil // clean tail
	}
	// Scan backwards in chunks for the last newline; truncate just past it
	// (or to zero if the file never completed a single line).
	keep := int64(0)
	buf := make([]byte, 4096)
	for off := size; off > 0; {
		n := int64(len(buf))
		if off < n {
			n = off
		}
		off -= n
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			return fmt.Errorf("failure: repair journal %s: %w", path, err)
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			keep = off + int64(i) + 1
			break
		}
	}
	if err := f.Truncate(keep); err != nil {
		return fmt.Errorf("failure: repair journal %s: truncate torn line: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failure: repair journal %s: %w", path, err)
	}
	return nil
}

// Close closes the underlying file, then releases the run lock (if this
// journal owns one) — in that order, so the lock outlives the last append.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	err := j.f.Close()
	lerr := j.lock.Release()
	if err != nil {
		return fmt.Errorf("failure: close journal: %w", err)
	}
	return lerr
}

// AppendResult validates and appends one result record. Exactly one result
// record per step per (resumed) run reaches the journal; a resumed run's
// re-run appends a fresh record rather than editing history.
func (j *Journal) AppendResult(rec ResultRecord) error {
	if err := rec.Result.Validate(); err != nil {
		return fmt.Errorf("failure: journal append %s: %w", rec.StepID, err)
	}
	rec.Kind = KindResult
	return j.append(rec)
}

// AppendCost appends one actual-cost record.
func (j *Journal) AppendCost(rec CostRecord) error {
	rec.Kind = KindCost
	return j.append(rec)
}

// AppendCleanup appends one cleanup-outcome record.
func (j *Journal) AppendCleanup(rec CleanupRecord) error {
	rec.Kind = KindCleanup
	return j.append(rec)
}

// AppendDefer appends one defer decision.
func (j *Journal) AppendDefer(rec DeferRecord) error {
	rec.Kind = KindDefer
	return j.append(rec)
}

// AppendRunEnd appends the run-end marker for this execution.
func (j *Journal) AppendRunEnd(rec RunEndRecord) error {
	rec.Kind = KindRunEnd
	return j.append(rec)
}

// appendHeader writes the run header; it must be the file's first line. The
// journal format stamp is owned here — callers cannot write another format.
func (j *Journal) appendHeader(h Header) error {
	h.Kind = KindHeader
	h.Format = JournalFormat
	if h.Params == nil {
		h.Params = map[string]string{}
	}
	return j.append(h)
}

func (j *Journal) append(v any) error {
	line, err := marshalLine(v)
	if err != nil {
		return fmt.Errorf("failure: journal encode: %w", err)
	}
	limit := j.limit
	if limit == 0 {
		limit = maxJournalLine
	}
	if len(line) > limit {
		return fmt.Errorf("failure: journal append: record is %d bytes, over the %d-byte line limit (replay could not read it back)", len(line), limit)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(line); err != nil {
		return fmt.Errorf("failure: journal append: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("failure: journal sync: %w", err)
	}
	return nil
}

// marshalLine encodes one record as a single JSONL line (trailing newline
// included, HTML escaping off).
func marshalLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Replay is a journal's replayed state — everything resume, reporting, and
// interactive re-entry read. Results is last-wins per key, so a resumed run's
// re-runs supersede naturally while the file remains append-only history.
type Replay struct {
	Header Header
	// Results maps (step-id, input-hash) to the last result record journaled
	// under that key.
	Results map[Key]ResultRecord
	// LastByStep maps a step id to its last result record in journal order,
	// regardless of input hash — what interactive re-entry looks up.
	LastByStep map[string]ResultRecord
	Costs      []CostRecord
	Cleanups   []CleanupRecord
	Defers     []DeferRecord
	// End is the last run-end marker, nil when the last execution of this
	// run never finished (the pre-upgrade guard's signal).
	End *RunEndRecord
}

// Load replays a journal file. A torn final line (crash mid-append, so the
// file does not end in a newline) is dropped with a warning — everything
// before it is intact by the one-write-per-line invariant. A malformed line
// anywhere else, including a newline-terminated final line, is a hard error:
// termination proves the write completed, so the corruption is real, not a
// crash artifact. Unknown record kinds are skipped with a log line. A
// journal whose header carries a different format stamp (or none) fails
// closed — no auto-migration, never silent misinterpretation.
func Load(path string, log *slog.Logger) (*Replay, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failure: open journal: %w", err)
	}
	defer f.Close()

	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("failure: read journal %s: %w", path, err)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("failure: journal %s is empty (no header)", path)
	}
	terminated, err := endsWithNewline(f)
	if err != nil {
		return nil, fmt.Errorf("failure: read journal %s: %w", path, err)
	}

	rp := &Replay{
		Results:    map[Key]ResultRecord{},
		LastByStep: map[string]ResultRecord{},
	}
	sawHeader := false
	for i, line := range lines {
		if i == len(lines)-1 && !terminated {
			// An unterminated final line is an incomplete write by the
			// one-write-per-line invariant — torn even when it happens to
			// parse. Dropping it unconditionally keeps replay byte-symmetric
			// with repairTornTail's truncation, so a record can never be
			// folded by one resume and silently gone by the next.
			log.Warn("dropping torn final journal line", "path", path, "line", i+1)
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// A newline-terminated malformed line completed its write and is
			// genuine corruption — a hard error wherever it sits.
			return nil, fmt.Errorf("failure: journal %s: line %d: %w", path, i+1, err)
		}
		if i == 0 && probe.Kind != KindHeader {
			return nil, fmt.Errorf("failure: journal %s: first record is %q, want %q", path, probe.Kind, KindHeader)
		}
		switch probe.Kind {
		case KindHeader:
			if sawHeader {
				return nil, fmt.Errorf("failure: journal %s: line %d: duplicate header", path, i+1)
			}
			if err := json.Unmarshal(line, &rp.Header); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: header: %w", path, i+1, err)
			}
			if rp.Header.Format != JournalFormat {
				return nil, formatError(path, rp.Header.Format)
			}
			sawHeader = true
		case KindResult:
			var rec ResultRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: result record: %w", path, i+1, err)
			}
			if err := rec.Result.Validate(); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: %w", path, i+1, err)
			}
			rp.Results[Key{rec.StepID, rec.InputHash}] = rec
			rp.LastByStep[rec.StepID] = rec
		case KindCost:
			var rec CostRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: cost record: %w", path, i+1, err)
			}
			rp.Costs = append(rp.Costs, rec)
		case KindCleanup:
			var rec CleanupRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: cleanup record: %w", path, i+1, err)
			}
			rp.Cleanups = append(rp.Cleanups, rec)
		case KindDefer:
			var rec DeferRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: defer record: %w", path, i+1, err)
			}
			rp.Defers = append(rp.Defers, rec)
		case KindRunEnd:
			var rec RunEndRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return nil, fmt.Errorf("failure: journal %s: line %d: run-end record: %w", path, i+1, err)
			}
			rp.End = &rec // last wins: a resumed run appends a fresh marker
		default:
			// Forward compatibility within one format: additive kinds are
			// skipped, but never silently — replay names what it ignored.
			log.Warn("skipping unknown journal record kind", "path", path, "line", i+1, "kind", probe.Kind)
		}
	}
	if !sawHeader {
		return nil, fmt.Errorf("failure: journal %s: missing header", path)
	}
	return rp, nil
}

// formatError is the fail-closed refusal for a journal whose format stamp is
// not this package's. There is no auto-migration in either direction.
func formatError(path string, format int) error {
	if format > JournalFormat {
		return fmt.Errorf(
			"failure: journal %s: written by a newer faber (journal format %d, this faber reads %d) — use the faber that wrote it, or start over with --fresh",
			path, format, JournalFormat)
	}
	return fmt.Errorf(
		"failure: journal %s: journaled under schema v%d, current v%d; no auto-migration — finish the run on the faber that wrote it, or start over with --fresh",
		path, format, JournalFormat)
}

// endsWithNewline reports whether the file's final byte is '\n' — the
// discriminator between a crash-torn final line (unterminated) and a
// terminated-but-corrupt one.
func endsWithNewline(f *os.File) (bool, error) {
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return false, err
	}
	return last[0] == '\n', nil
}
