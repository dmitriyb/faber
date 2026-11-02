package pipeline

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/metering"
)

// recordingSink captures events in emission order; safe from any goroutine.
type recordingSink struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recordingSink) Emit(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *recordingSink) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.evs))
	for _, ev := range r.evs {
		s := string(ev.Kind)
		if ev.Step != "" {
			s += ":" + ev.Step
		}
		out = append(out, s)
	}
	return out
}

// Verifies 8964ddc94332: the progress stream carries one started and one
// settled event per executed step in dispatch order, then the run end; and
// af1a4ddf11e0: every box attempt leaves a start record in the journal.
func TestEvents_SerializedDiamondOrderAndJournalStarts(t *testing.T) {
	h := newHarness(t)
	sink := &recordingSink{}
	h.exec.Events = sink
	if err := h.run(t, diamondIR(), config.RunOptions{MaxParallel: 1}); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{
		"run-started",
		"step-started:w/a", "step-settled:w/a",
		"step-waiting:w/c", // b took the only slot; c says why it waits, once
		"step-started:w/b", "step-settled:w/b",
		"step-started:w/c", "step-settled:w/c",
		"step-started:w/d", "step-settled:w/d",
		"run-ended",
	}
	if got := sink.kinds(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("event order\n got %v\nwant %v", got, want)
	}
	last := sink.evs[len(sink.evs)-1]
	if last.RunID != "run-test" || last.RunStatus != failure.RunEndSettled || last.Failed != 0 {
		t.Errorf("run-ended event %+v, want settled run-test with 0 failed", last)
	}
	if first := sink.evs[0]; first.RunID != "run-test" || first.Workflow != "w" {
		t.Errorf("run-started event %+v, want run-test / workflow w", first)
	}
	settled := sink.evs[2]
	if settled.Status != StateOK || settled.Duration != 2*time.Second || settled.Attempt != 0 {
		t.Errorf("settled event %+v, want ok with the record's 2s span", settled)
	}
	if w := sink.evs[3]; w.Detail != "slot 1 of 1" {
		t.Errorf("waiting event %+v, want detail \"slot 1 of 1\"", w)
	}

	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Starts) != 4 {
		t.Fatalf("journaled starts %d, want 4", len(rp.Starts))
	}
	wantStarts := []string{"w/a", "w/b", "w/c", "w/d"}
	for i, rec := range rp.Starts {
		if rec.Attempt != 1 || rec.Started.IsZero() || rec.StepID != wantStarts[i] {
			t.Errorf("start record %d = %+v, want attempt 1 for %s", i, rec, wantStarts[i])
		}
	}
}

// Verifies 8964ddc94332 and af1a4ddf11e0 (edge): a rate-limit defer emits a
// deferred event between the two dispatches' starts, and both leave start
// records — numbered attempt 1 twice, because a defer-converted dispatch
// consumes no attempt (the failure policy's own rule).
func TestEvents_DeferAndRetryStarts(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	reset := testBase.Add(90 * time.Second)
	h := newHarness(t)
	sink := &recordingSink{}
	h.exec.Events = sink
	h.boxes.script("w/x",
		failedResult(metering.ReasonRateLimit, `{"reset": `+timestamp(reset)+`}`),
		okPayload(map[string]any{"out": "done"}),
	)
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "run-started step-started:w/x step-deferred:w/x step-started:w/x step-settled:w/x run-ended"
	if got := strings.Join(sink.kinds(), " "); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	if d := sink.evs[2]; !d.Until.Equal(reset) {
		t.Errorf("deferred until %v, want %v", d.Until, reset)
	}
	if a1, a2 := sink.evs[1].Attempt, sink.evs[3].Attempt; a1 != 1 || a2 != 1 {
		t.Errorf("start attempts %d,%d, want 1,1 (a defer consumes no attempt)", a1, a2)
	}
	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Starts) != 2 || rp.Starts[0].Attempt != 1 || rp.Starts[1].Attempt != 1 {
		t.Errorf("start records %+v, want two starts both at attempt 1", rp.Starts)
	}
}

// Verifies 8964ddc94332: a failed step's settled event carries the reason,
// and dependents settle through the same stream as not-reached skips.
func TestEvents_FailureReasonAndSkipAncestor(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")}, []config.Edge{orderEdge("w/a", "w/b")})
	h := newHarness(t)
	sink := &recordingSink{}
	h.exec.Events = sink
	h.boxes.script("w/a", failedResult("agent-failed", "agent exited 1"))
	if err := h.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatal("run with a failed step must return an error")
	}
	var a, b Event
	for _, ev := range sink.evs {
		if ev.Kind == EventStepSettled && ev.Step == "w/a" {
			a = ev
		}
		if ev.Kind == EventStepSettled && ev.Step == "w/b" {
			b = ev
		}
	}
	if a.Status != StateFailed || a.Reason != "agent-failed" {
		t.Errorf("a settled %+v, want failed/agent-failed", a)
	}
	if b.Status != StateSkippedDependency || b.Reason != "w/a" {
		t.Errorf("b settled %+v, want skipped-dependency after w/a", b)
	}
}
