package failure

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pauseFile is the durable pause marker's name inside a run directory,
// beside journal.jsonl and run.lock. Its presence IS the pause request: no
// signal, no pid trust, no IPC — the scheduler re-derives the request from
// the store at its scheduling points, so it survives races and process
// boundaries. The file's timestamp content is diagnostics only.
const pauseFile = "pause"

// RequestPause records the cooperative stop request for a live run. The
// liveness gate exists because pausing a stopped run is meaningless (there
// is nothing to drain) and would only plant a marker for a future resume to
// clear; it is advisory — the run can exit between probe and write, and the
// leftover marker is harmless for the same reason.
func (s *Store) RequestPause(runID string) error {
	if _, err := os.Stat(s.journalPath(runID)); err != nil {
		return fmt.Errorf("failure: pause run %s: %w", runID, err)
	}
	runDir := s.RunDir(runID)
	if !RunLive(runDir) {
		return fmt.Errorf(
			"failure: pause run %s: the run is not live (no process holds its lock); only a running run can pause — a stopped run is already stopped and resumes with `faber resume`",
			runID)
	}
	stamp := time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(filepath.Join(runDir, pauseFile), []byte(stamp), 0o644); err != nil {
		return fmt.Errorf("failure: pause run %s: %w", runID, err)
	}
	s.log.Info("pause requested", "run", runID)
	return nil
}

// PauseRequested reports whether runDir carries the pause marker — the
// scheduler's scheduling-point probe. One stat, no lock: the marker is
// written whole and never rewritten, so there is nothing to race.
func PauseRequested(runDir string) bool {
	_, err := os.Stat(filepath.Join(runDir, pauseFile))
	return err == nil
}

// ClearPause removes a stale pause marker; a missing marker is not an
// error. Begin and Resume call it under the freshly acquired run lock so a
// marker written while the run was down cannot re-pause the new execution.
func ClearPause(runDir string) error {
	if err := os.Remove(filepath.Join(runDir, pauseFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failure: clear pause marker in %s: %w", runDir, err)
	}
	return nil
}
