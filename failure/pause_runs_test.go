package failure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedRun begins one run in the given store and drives it to a shape: "live"
// keeps the journal (and the run lock) open and returns it; every other
// shape closes the journal after appending the matching run-end marker
// ("incomplete" appends none).
func seedRun(t *testing.T, store *Store, runID, name, shape string) *Journal {
	t.Helper()
	hdr := diamondHeader(runID)
	hdr.Name = name
	j, err := store.Begin(hdr)
	if err != nil {
		t.Fatal(err)
	}
	switch shape {
	case "live":
		return j
	case "incomplete":
	case RunEndSettled, RunEndAborted, RunEndPaused:
		if err := j.AppendRunEnd(RunEndRecord{Status: shape, Finished: time.Now()}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown run shape %q", shape)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return nil
}

// Verifies bff0f92afc29: the pause marker is the durable stop request — it
// is writable only against a live run, one stat probes it, and the new
// executions (Begin, Resume) clear a stale marker under the run lock.
func TestPauseMarkerLifecycle(t *testing.T) {
	t.Run("request requires a live run", func(t *testing.T) {
		store := NewStore(t.TempDir(), nil)
		seedRun(t, store, "run-stopped", "", RunEndSettled)
		if err := store.RequestPause("run-stopped"); err == nil || !strings.Contains(err.Error(), "not live") {
			t.Fatalf("pausing a stopped run must refuse naming liveness, got %v", err)
		}
		if err := store.RequestPause("run-ghost"); err == nil {
			t.Fatal("pausing an unknown run must error")
		}
		if PauseRequested(store.RunDir("run-stopped")) {
			t.Fatal("a refused request must leave no marker")
		}
	})

	t.Run("request against a live run writes the marker", func(t *testing.T) {
		store := NewStore(t.TempDir(), nil)
		j := seedRun(t, store, "run-live", "", "live")
		defer j.Close()
		if err := store.RequestPause("run-live"); err != nil {
			t.Fatalf("pause a live run: %v", err)
		}
		if !PauseRequested(store.RunDir("run-live")) {
			t.Fatal("the marker must be probeable after RequestPause")
		}
	})

	t.Run("resume clears a stale marker", func(t *testing.T) {
		params := map[string]string{"target": "value"}
		store := journaledRun(t, "run-1", params)
		if err := os.WriteFile(filepath.Join(store.RunDir("run-1"), pauseFile), []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		seed, err := store.Resume(smallIR(false), "run-1", params)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer seed.Journal.Close()
		if PauseRequested(store.RunDir("run-1")) {
			t.Fatal("resume must clear the stale pause marker before any dispatch")
		}
	})

	t.Run("begin clears a marker left in a pre-existing dir", func(t *testing.T) {
		store := NewStore(t.TempDir(), nil)
		if err := os.MkdirAll(store.RunDir("run-new"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.RunDir("run-new"), pauseFile), []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		j, err := store.Begin(diamondHeader("run-new"))
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		if PauseRequested(store.RunDir("run-new")) {
			t.Fatal("begin must clear a pre-existing pause marker")
		}
	})
}

// Verifies 87f006277d2c: run references resolve by id or header name with no
// cross-run index — an id always wins over an equal name, an ambiguous name
// refuses naming every matching id, and headers round-trip the name.
func TestResolveRunRef(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	seedRun(t, store, "run-a", "alpha", RunEndSettled)
	seedRun(t, store, "run-b", "dup", RunEndSettled)
	seedRun(t, store, "run-c", "dup", RunEndSettled)
	// run-d's NAME collides with run-a's ID.
	seedRun(t, store, "run-d", "run-a", RunEndSettled)

	hdr, err := store.LoadHeader("run-a")
	if err != nil || hdr.Name != "alpha" {
		t.Fatalf("header name must round-trip, got %+v (%v)", hdr, err)
	}

	if id, err := store.ResolveRunRef("alpha"); err != nil || id != "run-a" {
		t.Fatalf("name alpha: got %q, %v", id, err)
	}
	if id, err := store.ResolveRunRef("run-a"); err != nil || id != "run-a" {
		t.Fatalf("an id must win over an equal name, got %q, %v", id, err)
	}
	_, err = store.ResolveRunRef("dup")
	if err == nil || !strings.Contains(err.Error(), "run-b") || !strings.Contains(err.Error(), "run-c") {
		t.Fatalf("ambiguous name must list every matching id, got %v", err)
	}
	if _, err := store.ResolveRunRef("ghost"); err == nil {
		t.Fatal("an unknown reference must error")
	}
	if _, err := store.ResolveRunRef(""); err == nil {
		t.Fatal("an empty reference must error")
	}
}

// Verifies 87f006277d2c: prune keeps what is resumable — default removes
// only finished (settled/aborted) non-live runs; --all widens to paused and
// incomplete; a live run survives both sweeps.
func TestPruneRuns(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	seedRun(t, store, "run-settled", "", RunEndSettled)
	seedRun(t, store, "run-aborted", "", RunEndAborted)
	seedRun(t, store, "run-paused", "", RunEndPaused)
	seedRun(t, store, "run-incomplete", "", "incomplete")
	live := seedRun(t, store, "run-live", "", "live")
	defer live.Close()

	removed, err := store.PruneRuns(false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []string{"run-aborted", "run-settled"}; !equalStrings(removed, want) {
		t.Fatalf("default prune removed %v, want %v", removed, want)
	}
	for _, id := range []string{"run-paused", "run-incomplete", "run-live"} {
		if _, err := os.Stat(store.RunDir(id)); err != nil {
			t.Fatalf("default prune must keep %s: %v", id, err)
		}
	}

	removed, err = store.PruneRuns(true)
	if err != nil {
		t.Fatalf("prune --all: %v", err)
	}
	if want := []string{"run-incomplete", "run-paused"}; !equalStrings(removed, want) {
		t.Fatalf("--all prune removed %v, want %v", removed, want)
	}
	if _, err := os.Stat(store.RunDir("run-live")); err != nil {
		t.Fatalf("a live run must never be pruned: %v", err)
	}
}

// Verifies 87f006277d2c: the audit scan carries the listing's identity
// fields — header name/workflow/started and the last run-end's status —
// while staying a tolerant kind probe.
func TestAuditRunsEnriched(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	seedRun(t, store, "run-paused", "nightly", RunEndPaused)
	seedRun(t, store, "run-open", "", "incomplete")

	audits, err := store.AuditRuns()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, a := range audits {
		byID[a.RunID] = true
		switch a.RunID {
		case "run-paused":
			if !a.Complete || a.EndStatus != RunEndPaused {
				t.Errorf("run-paused: Complete=%v EndStatus=%q, want complete paused", a.Complete, a.EndStatus)
			}
			if a.Name != "nightly" || a.Workflow != "main" {
				t.Errorf("run-paused identity: name=%q workflow=%q", a.Name, a.Workflow)
			}
			if !a.Started.Equal(diamondHeader("run-paused").Started) {
				t.Errorf("run-paused started = %v", a.Started)
			}
		case "run-open":
			if a.Complete || a.EndStatus != "" {
				t.Errorf("run-open: Complete=%v EndStatus=%q, want incomplete with no status", a.Complete, a.EndStatus)
			}
		}
	}
	if !byID["run-paused"] || !byID["run-open"] {
		t.Fatalf("audit must list both runs, got %v", byID)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
