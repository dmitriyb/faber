package failure

import (
	"strings"
	"testing"
)

// Verifies bff0f92afc29 / 87f006277d2c: a run accepts one appending process
// at a time — while a journal is open (Begin), resuming the same run refuses
// loudly instead of creating a second appender.
func TestRunLockRefusesLiveRun(t *testing.T) {
	params := map[string]string{}
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(Header{RunID: "run-1", Workflow: "main", Params: params,
		IRHash: mustHashIR(t, smallIR(false))})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Resume(smallIR(false), "run-1", params); err == nil ||
		!strings.Contains(err.Error(), "is live") {
		t.Fatalf("resume of a live run must refuse naming the holder, got %v", err)
	}
	if _, err := store.Reopen("run-1"); err == nil || !strings.Contains(err.Error(), "is live") {
		t.Fatalf("reopen of a live run must refuse, got %v", err)
	}
	if !store.RunLive("run-1") {
		t.Fatal("RunLive must report a held lock")
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if store.RunLive("run-1") {
		t.Fatal("RunLive must report false after the journal closed")
	}
	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatalf("resume after the holder exits must succeed: %v", err)
	}
	seed.Journal.Close()
}

// Verifies bff0f92afc29: the exclusivity lock guards the whole resume
// sequence — replay and torn-tail repair happen under the same lock the
// appender will hold, and a resumed journal releases it on Close so a later
// process can take over.
func TestRunLockHandoffAcrossResumes(t *testing.T) {
	params := map[string]string{}
	store := journaledRun(t, "run-1", params)

	first, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resume(smallIR(false), "run-1", params); err == nil {
		t.Fatal("second concurrent resume must refuse while the first holds the lock")
	}
	if err := first.Journal.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatalf("resume after release must succeed: %v", err)
	}
	second.Journal.Close()
}

// Verifies 87f006277d2c: a refused resume (config drift) releases the lock —
// a failed guard must never wedge the run against a corrected retry.
func TestRunLockReleasedOnRefusedResume(t *testing.T) {
	params := map[string]string{"target": "v"}
	store := journaledRun(t, "run-1", params)

	if _, err := store.Resume(smallIR(true), "run-1", params); err == nil {
		t.Fatal("drifted IR must refuse")
	}
	if store.RunLive("run-1") {
		t.Fatal("a refused resume must not leave the lock held")
	}
	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatalf("corrected resume must succeed: %v", err)
	}
	seed.Journal.Close()
}

// RunLive on a never-begun run (no dir, no lock file) is simply false.
func TestRunLiveAbsentRun(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	if store.RunLive("no-such-run") {
		t.Fatal("an absent run is not live")
	}
}
