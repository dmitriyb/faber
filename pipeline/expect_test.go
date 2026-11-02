package pipeline

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// taskWithExpect is the reference task IR with an expectation declared on
// its review loop (cap 3), served by the harness's fake source.
func taskWithExpect(t *testing.T, h *harness, stdout string, err error) *config.IR {
	t.Helper()
	ir := loadReferenceIR(t, "reference_task.ir.json")
	ir.Loops = []config.LoopSpec{{ID: "task/review-cycle", Max: 3, Command: "./expect", Args: []string{"--repo", "${params.repo}"}}}
	h.params = taskParams()
	h.exec.Source = &fakeSource{stdout: []byte(stdout), err: err}
	h.boxes.script("task/implement", prPayload())
	h.boxes.script("task/review-cycle@1/review", verdict("changes"))
	h.boxes.script("task/review-cycle@2/review", verdict("approved"))
	h.boxes.script("task/review-cycle@1/fix", okPayload(map[string]any{"status": "pushed"}))
	h.boxes.script("task/merge", okPayload(map[string]any{"merged": "yes"}))
	return ir
}

// Verifies 54c79a3cbcba: a count over the cap pauses the run at the loop's
// entry — implement (before the loop) ran, no iteration node dispatched or
// journaled, exit through RunPaused with the loop's message, and the
// run-end record carries it.
func TestExpect_StopsAtEntryWhenCapInsufficient(t *testing.T) {
	h := newHarness(t)
	ir := taskWithExpect(t, h, `{"count": 5}`, nil)
	sink := &recordingSink{}
	h.exec.Events = sink
	err := h.run(t, ir, config.RunOptions{})
	var paused *RunPaused
	if !errors.As(err, &paused) {
		t.Fatalf("want RunPaused, got %v", err)
	}
	want := "task/review-cycle: expected 5, cap 3; raise the cap and resume"
	if paused.Detail != want || !strings.Contains(err.Error(), want) {
		t.Errorf("pause detail %q (err %v), want %q", paused.Detail, err, want)
	}
	states := h.states(t, "run-test")
	if states["task/implement"] != StateOK {
		t.Errorf("implement = %q, want ok (it precedes the loop)", states["task/implement"])
	}
	for id := range states {
		if strings.Contains(id, "review-cycle@") {
			t.Errorf("iteration node %s was journaled; nothing past the gate may run", id)
		}
	}
	if h.boxes.attempts("task/review-cycle@1/review") != 0 {
		t.Error("the first iteration's box ran despite the stop")
	}
	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if rp.End == nil || rp.End.Status != failure.RunEndPaused || rp.End.Detail != want {
		t.Errorf("run-end = %+v, want paused with the loop's message", rp.End)
	}
	kinds := strings.Join(sink.kinds(), " ")
	if !strings.Contains(kinds, "loop-expected:task/review-cycle") || !strings.Contains(kinds, "pause-requested") {
		t.Errorf("events %s lack loop-expected and pause-requested", kinds)
	}
}

// Verifies 54c79a3cbcba: a count within the cap is silent — the loop runs
// to convergence — and the command runs once per iteration actually
// entered (the entry and one boundary here), never for the unused cap.
func TestExpect_WithinCapRunsAndRechecksAtBoundaries(t *testing.T) {
	h := newHarness(t)
	ir := taskWithExpect(t, h, `{"count": 2}`, nil)
	sink := &recordingSink{}
	h.exec.Events = sink
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"task/review-cycle@1/review": StateOK, "task/review-cycle@2/review": StateOK,
		"task/review-cycle@3/review": StateSkippedCondition, "task/merge": StateOK,
	})
	src := h.exec.Source.(*fakeSource)
	if n := len(src.calls); n != 2 {
		t.Errorf("expect command ran %d times, want 2 (entry + one boundary)", n)
	}
	if len(src.calls) > 0 && strings.Join(src.calls[0].Args, " ") != "--repo gw/repo" {
		t.Errorf("expect args %q, want the loop scope's params resolved", src.calls[0].Args)
	}
	n := 0
	for _, ev := range sink.evs {
		if ev.Kind == EventLoopExpected {
			n++
			if ev.Expected != 2 || ev.Cap != 3 || ev.Step != "task/review-cycle" {
				t.Errorf("loop-expected event %+v", ev)
			}
		}
	}
	if n != 2 {
		t.Errorf("loop-expected events %d, want 2", n)
	}
}

// Verifies 54c79a3cbcba (edge): a failing or malformed expect command fails
// the iteration's triggering node with the source-contract reason and
// cascades like any failure; nothing hangs.
func TestExpect_CommandFailureFailsTheIteration(t *testing.T) {
	for name, src := range map[string]*fakeSource{
		"command error":    {err: errors.New("tracker down")},
		"malformed output": {stdout: []byte(`{"items": []}`)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ir := taskWithExpect(t, h, "", nil)
			h.exec.Source = src
			if err := h.run(t, ir, config.RunOptions{}); err == nil {
				t.Fatal("want a failed run")
			}
			rec := h.record(t, "run-test", "task/review-cycle@1/review")
			if rec.Result.Status != failure.StatusFailed || rec.Result.Error.Reason != failure.ReasonSourceContract {
				t.Errorf("review settled %+v, want failed/source-contract", rec.Result)
			}
			if st := h.states(t, "run-test")["task/merge"]; st != StateSkippedDependency {
				t.Errorf("merge = %q, want skipped-dependency after the failed iteration", st)
			}
		})
	}
}

// Verifies 54c79a3cbcba: the text sink renders the expectation and then
// counts iterations against it rather than the cap.
func TestTextSink_ExpectedCount(t *testing.T) {
	var sb strings.Builder
	sink := NewTextSink(&sb, false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sink.Emit(Event{Kind: EventRunStarted, Time: at, RunID: "r", Workflow: "epic", Loops: map[string]int{"epic/cycles": 45}})
	sink.Emit(Event{Kind: EventLoopExpected, Time: at, Step: "epic/cycles", Expected: 12, Cap: 45})
	sink.Emit(Event{Kind: EventStepStarted, Time: at, Step: "epic/cycles@3/implement", Attempt: 1})
	want := "00:00:00  expect       cycles  12 of up to 45\n00:00:00  start        cycles 3/12 (cap 45) > implement\n"
	if !strings.HasSuffix(sb.String(), want) {
		t.Errorf("got %q", sb.String())
	}
}
