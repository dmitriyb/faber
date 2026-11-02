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
		"step-started:w/a", "step-settled:w/a",
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
	settled := sink.evs[1]
	if settled.Status != StateOK || settled.Duration != 2*time.Second || settled.Attempt != 0 {
		t.Errorf("settled event %+v, want ok with the record's 2s span", settled)
	}

	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Starts) != 4 {
		t.Fatalf("journaled starts %d, want 4", len(rp.Starts))
	}
	for i, rec := range rp.Starts {
		if rec.Attempt != 1 || rec.Started.IsZero() || rec.StepID != want[2*i][len("step-started:"):] {
			t.Errorf("start record %d = %+v, want attempt 1 for %s", i, rec, want[2*i])
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
	want := "step-started:w/x step-deferred:w/x step-started:w/x step-settled:w/x run-ended"
	if got := strings.Join(sink.kinds(), " "); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	if d := sink.evs[1]; !d.Until.Equal(reset) {
		t.Errorf("deferred until %v, want %v", d.Until, reset)
	}
	if a1, a2 := sink.evs[0].Attempt, sink.evs[2].Attempt; a1 != 1 || a2 != 1 {
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

// Verifies 8964ddc94332: the line sink renders ASCII, one line per event,
// with box-authored text terminal-sanitized.
func TestLineSink_Format(t *testing.T) {
	at := time.Date(2026, 9, 2, 20, 40, 59, 0, time.UTC)
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{Kind: EventStepStarted, Time: at, Step: "epic/cycles@8/implement", Attempt: 1}, "20:40:59  start   epic/cycles@8/implement"},
		{Event{Kind: EventStepStarted, Time: at, Step: "w/x", Attempt: 2}, "20:40:59  start   w/x  attempt 2"},
		{Event{Kind: EventStepSettled, Time: at, Step: "w/x", Status: StateOK, Duration: 2*time.Minute + 14*time.Second}, "20:40:59  ok      w/x  2m14s"},
		{Event{Kind: EventStepSettled, Time: at, Step: "w/x", Status: StateFailed, Reason: "agent-\x1b[31mfailed", Duration: time.Second}, "20:40:59  FAIL    w/x  1s  agent-\uFFFD[31mfailed"},
		{Event{Kind: EventStepSettled, Time: at, Step: "w/x", Status: StateOK, Cached: true}, "20:40:59  ok      w/x  (cached)"},
		{Event{Kind: EventStepDeferred, Time: at, Step: "w/x", Until: at.Add(90 * time.Second), Detail: "rate-limit"}, "20:40:59  WAIT    w/x  until 20:42:29  rate-limit"},
		{Event{Kind: EventGenerateExpanded, Time: at, Step: "w/gen", Items: 12, Nodes: 36}, "20:40:59  expand  w/gen  12 items, 36 nodes"},
		{Event{Kind: EventRunEnded, Time: at, RunID: "r1", RunStatus: failure.RunEndSettled, Failed: 1}, "20:40:59  end     run r1: settled, 1 failed"},
	}
	for _, c := range cases {
		if got := formatEventLine(c.ev); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.ev.Kind, got, c.want)
		}
	}
	var sb strings.Builder
	NewLineSink(&sb).Emit(cases[0].ev)
	if sb.String() != cases[0].want+"\n" {
		t.Errorf("sink wrote %q", sb.String())
	}
}
