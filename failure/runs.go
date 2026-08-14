package failure

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dmitriyb/faber/config"
)

// Store satisfies the config CLI's run-administration seam.
var _ config.RunController = (*Store)(nil)

// ResolveRunRef maps a run reference — a run id or an operator-given header
// name — to a run id. An existing run id always wins over an equal name;
// name resolution enumerates run directories and reads headers (there is no
// cross-run index to maintain or corrupt). An ambiguous name refuses,
// naming every matching run id.
func (s *Store) ResolveRunRef(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("failure: empty run reference")
	}
	if _, err := os.Stat(s.journalPath(ref)); err == nil {
		return ref, nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("failure: no run with id or name %q (the run store is empty)", ref)
		}
		return "", fmt.Errorf("failure: resolve run %q: %w", ref, err)
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// An unreadable or headerless journal is simply not a name match —
		// resolution must stay tolerant, like every other store scan.
		hdr, err := s.LoadHeader(e.Name())
		if err != nil {
			continue
		}
		if hdr.Name == ref {
			matches = append(matches, e.Name())
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("failure: no run with id or name %q", ref)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("failure: run name %q is ambiguous: it names runs %s — use a run id",
			ref, strings.Join(matches, ", "))
	}
}

// PruneRuns deletes run directories that are finished and not live,
// returning the removed run ids in store order. Default policy keeps paused
// runs — they are resumable state by design; all widens the sweep to paused
// and incomplete (no run-end marker: crashed or abandoned) non-live runs.
// A live run is never touched: beyond the audit's advisory probe, each
// candidate's run lock is acquired non-blocking and held across the removal,
// so a concurrent resume refuses loudly instead of losing its journal
// mid-append.
func (s *Store) PruneRuns(all bool) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no store root yet: nothing to prune
		}
		return nil, fmt.Errorf("failure: prune runs: %w", err)
	}
	var removed []string
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
			return removed, fmt.Errorf("failure: prune run %s: %w", runID, err)
		}
		if a.Live {
			continue
		}
		if !a.Complete && !all {
			continue
		}
		// Only the literal paused status is kept by default: a complete
		// run-end whose status is unprobeable (hand-edited) or unknown to
		// this binary prunes as finished, so a future resumable-by-design
		// status must also teach prune to keep it.
		if a.EndStatus == RunEndPaused && !all {
			continue
		}
		lock, err := AcquireRunLock(s.RunDir(runID))
		if err != nil {
			continue // became live (or unlockable) since the audit: never force
		}
		// The journal dies first, under the held lock. RemoveAll unlinks
		// run.lock at an unspecified point, and a racing resume could then
		// flock a FRESH lock inode (the hazard lock.go's Release comment
		// names) — but with the journal already gone that resume refuses
		// loudly at its journal read instead of appending into a directory
		// mid-deletion.
		rmErr := os.Remove(s.journalPath(runID))
		if rmErr == nil || os.IsNotExist(rmErr) {
			rmErr = os.RemoveAll(s.RunDir(runID))
		}
		lock.Release()
		if rmErr != nil {
			return removed, fmt.Errorf("failure: prune run %s: %w", runID, rmErr)
		}
		removed = append(removed, runID)
		s.log.Info("pruned run", "run", runID)
	}
	return removed, nil
}
