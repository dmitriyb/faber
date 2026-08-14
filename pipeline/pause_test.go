package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/metering"
)

// pauseDuring makes a node's box attempt write the run's pause marker
// through the real store gate (the run is live while it executes).
func pauseDuring(t *testing.T, h *harness, node, runID string) {
	t.Helper()
	s := h.boxes.scripts[node]
	if s == nil {
		s = h.boxes.script(node, okPayload(map[string]any{"out": node}))
	}
	s.onAttempt = func(int) {
		if err := h.store.RequestPause(runID); err != nil {
			t.Errorf("request pause mid-run: %v", err)
		}
	}
}

// Verifies 8879dc1597d6: a pause request drains the run — the in-flight step
// settles and journals, nothing further dispatches, the run-end record says
// paused, the run error is the typed pause error (exit 4), and the report
// flags the pause. Resume then reuses the settled step and finishes clean.
func TestPauseDrainsAndResumes(t *testing.T) {
	nodes, edges := chain("p", 3)
	ir := testIR("main", nodes, edges)
	h := newHarness(t)
	pauseDuring(t, h, "p/s0", "run-pause")

	err := h.run(t, ir, config.RunOptions{RunID: "run-pause"})
	var paused *RunPaused
	if !errors.As(err, &paused) || paused.RunID != "run-pause" {
		t.Fatalf("want the typed pause error, got %v", err)
	}
	if paused.ExitCode() != 4 {
		t.Fatalf("paused run must map to exit 4, got %d", paused.ExitCode())
	}
	if got := h.boxes.attempts("p/s0"); got != 1 {
		t.Errorf("p/s0 ran %d times, want 1", got)
	}
	for _, id := range []string{"p/s1", "p/s2"} {
		if got := h.boxes.attempts(id); got != 0 {
			t.Errorf("%s ran %d times, want 0 (paused before dispatch)", id, got)
		}
	}

	rp, err := h.store.Load("run-pause")
	if err != nil {
		t.Fatal(err)
	}
	if rp.End == nil || rp.End.Status != failure.RunEndPaused {
		t.Fatalf("run-end = %+v, want status paused", rp.End)
	}
	if rec, ok := rp.LastByStep["p/s0"]; !ok || rec.Result.Status != failure.StatusOK {
		t.Fatalf("the drained step must journal ok, got %+v", rec)
	}
	if _, ok := rp.LastByStep["p/s1"]; ok {
		t.Fatal("an undispatched step must leave no record")
	}

	if !strings.Contains(h.text.String(), "paused: the run stopped on request; resume with: faber resume run-pause") {
		t.Errorf("human report must carry the paused footer:\n%s", h.text.String())
	}
	if !strings.Contains(h.json.String(), `"paused": true`) {
		t.Errorf("JSON report must flag the pause:\n%s", h.json.String())
	}
	if !failure.PauseRequested(h.store.RunDir("run-pause")) {
		t.Fatal("the marker survives the paused run (resume is what clears it)")
	}

	// Resume: the settled step is a reuse hit, the rest run, the fresh
	// run-end says settled, and the stale marker is gone.
	if err := h.run(t, ir, config.RunOptions{RunID: "run-pause", Mode: "resume"}); err != nil {
		t.Fatalf("resume after pause must settle clean: %v", err)
	}
	if got := h.boxes.attempts("p/s0"); got != 1 {
		t.Errorf("p/s0 ran %d times total, want 1 (resume hit)", got)
	}
	for _, id := range []string{"p/s1", "p/s2"} {
		if got := h.boxes.attempts(id); got != 1 {
			t.Errorf("%s ran %d times total, want 1", id, got)
		}
	}
	rp, err = h.store.Load("run-pause")
	if err != nil {
		t.Fatal(err)
	}
	if rp.End == nil || rp.End.Status != failure.RunEndSettled {
		t.Fatalf("resumed run-end = %+v, want settled", rp.End)
	}
	if failure.PauseRequested(h.store.RunDir("run-pause")) {
		t.Fatal("resume must clear the pause marker")
	}
}

// Verifies 8879dc1597d6: with parallel steps in flight, pause lets each
// finish and settle while dispatching nothing new; a step that fails during
// the drain keeps its own status and outranks the pause in the exit
// decision, while the run-end still records the pause-ended execution.
func TestPauseLetsParallelFlightsSettle(t *testing.T) {
	twoChains := func() *config.IR {
		aNodes, aEdges := chain("a", 2)
		bNodes, bEdges := chain("b", 2)
		return testIR("main", append(aNodes, bNodes...), append(aEdges, bEdges...))
	}

	t.Run("both flights settle, tails never dispatch", func(t *testing.T) {
		h := newHarness(t)
		h.boxes.script("b/s0", okPayload(map[string]any{"out": "b"})).latency = 150 * time.Millisecond
		pauseDuring(t, h, "a/s0", "run-par")

		err := h.run(t, twoChains(), config.RunOptions{RunID: "run-par"})
		var paused *RunPaused
		if !errors.As(err, &paused) {
			t.Fatalf("want the typed pause error, got %v", err)
		}
		states := h.states(t, "run-par")
		wantStates(t, states, map[string]string{"a/s0": StateOK, "b/s0": StateOK})
		for _, id := range []string{"a/s1", "b/s1"} {
			if got := h.boxes.attempts(id); got != 0 {
				t.Errorf("%s ran %d times, want 0", id, got)
			}
		}
	})

	t.Run("failure during the drain outranks the pause", func(t *testing.T) {
		h := newHarness(t)
		h.boxes.script("b/s0", failedResult("agent", "boom")).latency = 150 * time.Millisecond
		pauseDuring(t, h, "a/s0", "run-parfail")

		err := h.run(t, twoChains(), config.RunOptions{RunID: "run-parfail"})
		if err == nil {
			t.Fatal("a run with a failed step must error")
		}
		var paused *RunPaused
		if errors.As(err, &paused) {
			t.Fatalf("failure outranks pause, got the pause error: %v", err)
		}
		if !strings.Contains(err.Error(), "1 step(s) failed") {
			t.Fatalf("want the failure error, got %v", err)
		}
		rp, lerr := h.store.Load("run-parfail")
		if lerr != nil {
			t.Fatal(lerr)
		}
		if rp.End == nil || rp.End.Status != failure.RunEndPaused || rp.End.Failed != 1 {
			t.Fatalf("run-end = %+v, want paused with one failure", rp.End)
		}
		// The failed flight's own propagation still runs during the drain.
		states := h.states(t, "run-parfail")
		wantStates(t, states, map[string]string{
			"a/s0": StateOK,
			"b/s0": StateFailed,
			"b/s1": StateSkippedDependency,
		})
	})

	t.Run("halt during the drain outranks the pause", func(t *testing.T) {
		h := newHarness(t)
		h.boxes.script("b/s0", haltedResult("needs-triage", "stop here")).latency = 150 * time.Millisecond
		pauseDuring(t, h, "a/s0", "run-parhalt")

		err := h.run(t, twoChains(), config.RunOptions{RunID: "run-parhalt"})
		var halted *RunHalted
		if !errors.As(err, &halted) {
			t.Fatalf("halt outranks pause: want the typed halt error, got %v", err)
		}
		var paused *RunPaused
		if errors.As(err, &paused) {
			t.Fatalf("the pause error must not surface when a step halted: %v", err)
		}
		rp, lerr := h.store.Load("run-parhalt")
		if lerr != nil {
			t.Fatal(lerr)
		}
		if rp.End == nil || rp.End.Status != failure.RunEndPaused || rp.End.Halted != 1 {
			t.Fatalf("run-end = %+v, want paused with one halt", rp.End)
		}
	})
}

// stuckClock records timed wakes but never fires them, so the run loop
// genuinely idles in its event select — the state the idle re-probe covers.
type stuckClock struct{ *fakeClock }

func (c stuckClock) AfterFunc(d time.Duration, _ func()) {
	c.mu.Lock()
	c.delays = append(c.delays, d)
	c.mu.Unlock()
}

// Verifies 8879dc1597d6: a pause requested while the run sits out a timed
// defer window with nothing in flight — the proposal's driving scenario —
// is observed by the idle re-probe tick, not first at the wake that would
// end the window. Without the tick this run loop blocks on an event that
// never comes and the test times out.
func TestPauseObservedDuringTimedDeferWindow(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	until := testBase.Add(time.Hour)
	meter := &fakeMeter{est: []metering.Estimate{{DeferUntil: &until}}}
	admit := metering.NewAdmitter(map[string][]metering.Meter{"": {meter}}, nil, nil)
	sh := newSchedHarness(t, ir, admit, 0)
	sh.s.clock = stuckClock{sh.clock}
	sh.s.pausePoll = 5 * time.Millisecond
	sh.s.pause = func() bool { return failure.PauseRequested(sh.store.RunDir("run-s")) }

	go func() {
		// Wait until the defer parked with its (never-firing) timed wake,
		// then write the marker — the loop is by then blocked in its select.
		for len(sh.clock.recorded()) == 0 {
			time.Sleep(time.Millisecond)
		}
		if err := sh.store.RequestPause("run-s"); err != nil {
			t.Errorf("request pause: %v", err)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- sh.s.run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause during an idle defer window was never observed (idle re-probe missing)")
	}
	if !sh.s.paused {
		t.Fatal("the pause request must be latched")
	}
	if got := sh.boxes.attempts("w/x"); got != 0 {
		t.Errorf("w/x ran %d times, want 0 (still deferred when paused)", got)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rp.LastByStep["w/x"]; ok {
		t.Fatal("a deferred, never-dispatched step must leave no result record")
	}
}

// Verifies 8879dc1597d6: a pause request that arrives when every node has
// already settled changes nothing — the run-end status is paused only when
// the drain itself is what ended the run.
func TestPauseAfterCompletionSettles(t *testing.T) {
	nodes, edges := chain("solo", 1)
	ir := testIR("main", nodes, edges)
	h := newHarness(t)
	pauseDuring(t, h, "solo/s0", "run-late")

	if err := h.run(t, ir, config.RunOptions{RunID: "run-late"}); err != nil {
		t.Fatalf("a completed run is settled, not paused: %v", err)
	}
	rp, err := h.store.Load("run-late")
	if err != nil {
		t.Fatal(err)
	}
	if rp.End == nil || rp.End.Status != failure.RunEndSettled {
		t.Fatalf("run-end = %+v, want settled", rp.End)
	}
}
