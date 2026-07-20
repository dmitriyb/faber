package failure

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dmitriyb/faber/config"
)

// journalFile is the journal's name inside a run directory.
const journalFile = "journal.jsonl"

// Store is the run-journal store: one directory per run under Root, named by
// run id, holding that run's journal (and, beside it, run-scoped artifacts
// like handoff directories). First pass deliberately has no cross-run index
// and no locking — concurrent runs over the same items are the operator's
// responsibility until that seam is designed.
type Store struct {
	root string
	log  *slog.Logger
}

// Store satisfies the config CLI's journal and run-audit seams.
var (
	_ config.JournalStore = (*Store)(nil)
	_ config.RunAuditor   = (*Store)(nil)
)

// NewStore returns a Store rooted at root. A nil logger discards.
func NewStore(root string, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{root: root, log: log.With("component", "failure")}
}

// RunDir is the run's directory: everything durable about the run —
// journal, handoff directories — lives under it.
func (s *Store) RunDir(runID string) string { return filepath.Join(s.root, runID) }

func (s *Store) journalPath(runID string) string {
	return filepath.Join(s.RunDir(runID), journalFile)
}

// NewRunID mints a fresh run id: UTC timestamp plus random suffix, unique
// enough that per-run directories never collide in practice.
func NewRunID() string {
	var b [4]byte
	rand.Read(b[:]) // never fails per crypto/rand contract
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// Begin creates the run directory and journal for a new run and writes the
// header as the first line. It refuses an already-begun run id, and takes
// the run's exclusivity lock before touching the journal — the lock rides
// on the returned Journal for the process lifetime (released by Close).
func (s *Store) Begin(hdr Header) (*Journal, error) {
	if hdr.RunID == "" {
		return nil, fmt.Errorf("failure: begin run: empty run id")
	}
	if err := os.MkdirAll(s.RunDir(hdr.RunID), 0o755); err != nil {
		return nil, fmt.Errorf("failure: begin run %s: %w", hdr.RunID, err)
	}
	lock, err := AcquireRunLock(s.RunDir(hdr.RunID))
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.journalPath(hdr.RunID), os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		lock.Release()
		return nil, fmt.Errorf("failure: begin run %s: %w", hdr.RunID, err)
	}
	j := &Journal{f: f, lock: lock}
	if err := j.appendHeader(hdr); err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

// Reopen opens an existing run's journal for further appends (resume). It
// takes the run's exclusivity lock first — a live run refuses loudly — and
// only then repairs and opens; the lock rides on the returned Journal.
func (s *Store) Reopen(runID string) (*Journal, error) {
	lock, err := AcquireRunLock(s.RunDir(runID))
	if err != nil {
		return nil, err
	}
	j, err := s.reopenLocked(runID, lock)
	if err != nil {
		lock.Release()
		return nil, err
	}
	return j, nil
}

// reopenLocked opens the journal for appends under an already-held run lock,
// attaching the lock to the journal. The caller releases the lock on error.
func (s *Store) reopenLocked(runID string, lock *RunLock) (*Journal, error) {
	path := s.journalPath(runID)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("failure: reopen run %s: %w", runID, err)
	}
	j, err := OpenJournal(path)
	if err != nil {
		return nil, err
	}
	j.lock = lock
	return j, nil
}

// RunLive reports whether another process currently holds the run's
// exclusivity lock (the run is being executed right now).
func (s *Store) RunLive(runID string) bool { return RunLive(s.RunDir(runID)) }

// Load replays a run's journal.
func (s *Store) Load(runID string) (*Replay, error) {
	return Load(s.journalPath(runID), s.log)
}

// LoadHeader reads only a run journal's header line, in the shape the config
// CLI's resume guard consumes. Deliberately format-tolerant: the CLI's own
// guards (and --fresh) need the header of any journal vintage; interpretive
// replay is Load's job and fails closed there.
func (s *Store) LoadHeader(runID string) (config.JournalHeader, error) {
	f, err := os.Open(s.journalPath(runID))
	if err != nil {
		return config.JournalHeader{}, fmt.Errorf("failure: run %s: %w", runID, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return config.JournalHeader{}, fmt.Errorf("failure: run %s: read journal: %w", runID, err)
		}
		return config.JournalHeader{}, fmt.Errorf("failure: run %s: journal is empty (no header)", runID)
	}
	var hdr Header
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return config.JournalHeader{}, fmt.Errorf("failure: run %s: journal header: %w", runID, err)
	}
	if hdr.Kind != KindHeader {
		return config.JournalHeader{}, fmt.Errorf("failure: run %s: first journal record is %q, want %q", runID, hdr.Kind, KindHeader)
	}
	return config.JournalHeader{
		RunID:      hdr.RunID,
		ConfigPath: hdr.ConfigPath,
		Workflow:   hdr.Workflow,
		Params:     hdr.Params,
		IRHash:     hdr.IRHash,
		Format:     hdr.Format,
		IRVersion:  hdr.IRVersion,
	}, nil
}

// SupportedFormat reports the journal schema this binary reads and writes
// (the config CLI's early-guard seam).
func (s *Store) SupportedFormat() int { return JournalFormat }

// AuditRuns enumerates every journaled run's upgrade-relevant state for the
// pre-upgrade guard. The scan is read-only and deliberately tolerant — it
// probes only record kinds, so journals of any format (including formats
// this binary refuses to replay) still report liveness and completeness.
func (s *Store) AuditRuns() ([]config.RunAudit, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no store root yet: no runs
		}
		return nil, fmt.Errorf("failure: audit runs: %w", err)
	}
	var audits []config.RunAudit
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		a, err := s.auditRun(runID)
		if err != nil {
			if os.IsNotExist(err) {
				continue // a run dir without a journal is not a run
			}
			return nil, fmt.Errorf("failure: audit run %s: %w", runID, err)
		}
		audits = append(audits, a)
	}
	sort.Slice(audits, func(i, j int) bool { return audits[i].RunID < audits[j].RunID })
	return audits, nil
}

// auditRun probes one journal for the guard's three facts: lock liveness,
// completeness, and the header's format stamp. Completeness means the LAST
// probeable record is a run-end marker — the journal is append-only, so a
// resumed run appends past its earlier marker and a resumed-then-interrupted
// run must audit unfinished, not complete. An unterminated final line (crash
// artifact) is ignored, matching replay's torn-tail semantics.
func (s *Store) auditRun(runID string) (config.RunAudit, error) {
	f, err := os.Open(s.journalPath(runID))
	if err != nil {
		return config.RunAudit{}, err
	}
	defer f.Close()
	a := config.RunAudit{RunID: runID, Live: s.RunLive(runID)}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return config.RunAudit{}, err
	}
	terminated, err := endsWithNewline(f)
	if err != nil {
		return config.RunAudit{}, err
	}
	if !terminated && len(lines) > 0 {
		lines = lines[:len(lines)-1] // torn fragment: not a record
	}
	lastKind := ""
	for i, line := range lines {
		var probe struct {
			Kind   string `json:"kind"`
			Format int    `json:"format"`
		}
		if json.Unmarshal(line, &probe) != nil {
			// A terminated line the guard cannot probe voids any earlier
			// run-end: replay would hard-error on this journal, and "complete"
			// for an unreplayable journal is the optimistic answer the guard
			// must not give.
			lastKind = ""
			continue
		}
		if i == 0 && probe.Kind == KindHeader {
			a.Format = probe.Format
		}
		lastKind = probe.Kind
	}
	a.Complete = lastKind == KindRunEnd
	return a, nil
}
