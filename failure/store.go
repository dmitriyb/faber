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

// Store satisfies the config CLI's journal seam.
var _ config.JournalStore = (*Store)(nil)

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
// header as the first line. It refuses an already-begun run id.
func (s *Store) Begin(hdr Header) (*Journal, error) {
	if hdr.RunID == "" {
		return nil, fmt.Errorf("failure: begin run: empty run id")
	}
	if err := os.MkdirAll(s.RunDir(hdr.RunID), 0o755); err != nil {
		return nil, fmt.Errorf("failure: begin run %s: %w", hdr.RunID, err)
	}
	f, err := os.OpenFile(s.journalPath(hdr.RunID), os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failure: begin run %s: %w", hdr.RunID, err)
	}
	j := &Journal{f: f}
	if err := j.appendHeader(hdr); err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

// Reopen opens an existing run's journal for further appends (resume).
func (s *Store) Reopen(runID string) (*Journal, error) {
	path := s.journalPath(runID)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("failure: reopen run %s: %w", runID, err)
	}
	return OpenJournal(path)
}

// Load replays a run's journal.
func (s *Store) Load(runID string) (*Replay, error) {
	return Load(s.journalPath(runID), s.log)
}

// LoadHeader reads only a run journal's header line, in the shape the config
// CLI's resume guard consumes.
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
	}, nil
}
