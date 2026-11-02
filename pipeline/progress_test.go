package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/failure"
)

// epicStream is a scripted progress stream in the shape of the real epic
// workflow: one cycle that implements, reviews once, converges, then fails
// at landing, so two later cycles are not reached.
func epicStream() []Event {
	t0 := time.Date(2026, 9, 2, 19, 4, 8, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	anc := "epic/cycles@1/land@1/merge"
	return []Event{
		{Kind: EventRunStarted, Time: at(0), RunID: "r1", Workflow: "epic", Loops: map[string]int{
			"epic/cycles": 45, "epic/cycles@1/review-cycle": 5, "epic/cycles@1/land": 2, "epic/cycles@2/review-cycle": 5, "epic/cycles@2/land": 2}},
		{Kind: EventStepStarted, Time: at(0), Step: "epic/cycles@1/implement", Attempt: 1},
		{Kind: EventStepSettled, Time: at(3), Step: "epic/cycles@1/implement", Status: StateOK, Duration: 2*time.Minute + 53*time.Second},
		{Kind: EventStepStarted, Time: at(3), Step: "epic/cycles@1/review-cycle@1/review", Attempt: 1},
		{Kind: EventStepSettled, Time: at(7), Step: "epic/cycles@1/review-cycle@1/review", Status: StateOK, Duration: 4 * time.Minute},
		{Kind: EventStepSettled, Time: at(7), Step: "epic/cycles@1/review-cycle@1/fix", Status: StateSkippedCondition},
		{Kind: EventStepSettled, Time: at(7), Step: "epic/cycles@1/review-cycle@2/review", Status: StateSkippedCondition},
		{Kind: EventStepSettled, Time: at(7), Step: "epic/cycles@1/review-cycle@2/fix", Status: StateSkippedCondition},
		{Kind: EventStepSettled, Time: at(7), Step: "epic/cycles@1/review-cycle@3/review", Status: StateSkippedCondition},
		{Kind: EventStepStarted, Time: at(7), Step: "epic/cycles@1/land@1/merge", Attempt: 1},
		{Kind: EventStepDeferred, Time: at(8), Step: "epic/cycles@1/land@1/merge", Until: at(9), Detail: "rate-limit"},
		{Kind: EventStepStarted, Time: at(9), Step: "epic/cycles@1/land@1/merge", Attempt: 1},
		{Kind: EventStepSettled, Time: at(10), Step: "epic/cycles@1/land@1/merge", Status: StateFailed, Reason: "agent-\x1bfailed", Duration: time.Minute},
		{Kind: EventStepSettled, Time: at(10), Step: "epic/cycles@1/land@2/merge", Status: StateSkippedDependency, Reason: anc},
		{Kind: EventStepSettled, Time: at(10), Step: "epic/cycles@2/implement", Status: StateSkippedDependency, Reason: anc},
		{Kind: EventStepSettled, Time: at(10), Step: "epic/cycles@2/review-cycle@1/review", Status: StateSkippedDependency, Reason: anc},
		{Kind: EventStepSettled, Time: at(10), Step: "epic/cycles@3/implement", Status: StateSkippedDependency, Reason: anc},
		{Kind: EventRunEnded, Time: at(10), RunID: "r1", RunStatus: failure.RunEndSettled, Failed: 1},
	}
}

// Verifies 46b463750d77: the text sink renders breadcrumbs with iteration
// and cap, aligned status words, a guard-false step inside a live iteration
// as "not needed", a loop's convergence as one line, nothing for the unused
// cap, and not-reached iterations as one range per loop at run end; box
// text is terminal-sanitized.
func TestTextSink_EpicStream(t *testing.T) {
	var sb strings.Builder
	sink := NewTextSink(&sb, false)
	for _, ev := range epicStream() {
		sink.Emit(ev)
	}
	want := strings.Join([]string{
		"19:04:08  run          r1  workflow epic",
		"19:04:08  start        cycles 1/45 > implement",
		"19:07:08  ok           cycles 1/45 > implement  2m53s",
		"19:07:08  start        cycles 1/45 > review-cycle 1/5 > review",
		"19:11:08  ok           cycles 1/45 > review-cycle 1/5 > review  4m0s",
		"19:11:08  not needed   cycles 1/45 > review-cycle 1/5 > fix",
		"19:11:08  ok           cycles 1/45 > review-cycle  settled after 1 of 5",
		"19:11:08  start        cycles 1/45 > land 1/2 > merge",
		"19:12:08  WAIT         cycles 1/45 > land 1/2 > merge  until 19:13:08  rate-limit",
		"19:13:08  start        cycles 1/45 > land 1/2 > merge",
		"19:14:08  FAIL         cycles 1/45 > land 1/2 > merge  1m0s  agent-�failed",
		"19:14:08  not reached  cycles 1/45 > land @2  after cycles 1/45 > land 1/2 > merge",
		"19:14:08  not reached  cycles @2..@3  after cycles 1/45 > land 1/2 > merge",
		"19:14:08  end          run r1: settled, 1 failed",
	}, "\n") + "\n"
	if sb.String() != want {
		t.Errorf("text stream\n--- got ---\n%s--- want ---\n%s", sb.String(), want)
	}
}

// Verifies 46b463750d77: colour wraps the status word only, with palette
// slots, and the rest of the line is byte-identical to the plain form.
func TestTextSink_ColourStatusWordOnly(t *testing.T) {
	ev := Event{Kind: EventStepSettled, Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: "w/x", Status: StateFailed, Reason: "agent-failed", Duration: time.Second}
	var plain, colour strings.Builder
	for _, sink := range []*TextSink{NewTextSink(&plain, false), NewTextSink(&colour, true)} {
		sink.Emit(Event{Kind: EventRunStarted, Time: ev.Time, RunID: "r", Workflow: "w"})
		sink.Emit(ev)
	}
	wantPlain := "00:00:00  FAIL         x  1s  agent-failed\n"
	if got := plain.String(); !strings.HasSuffix(got, wantPlain) {
		t.Fatalf("plain %q", got)
	}
	wantColour := "00:00:00  \x1b[1;31mFAIL        \x1b[0m x  1s  agent-failed\n"
	if got := colour.String(); !strings.HasSuffix(got, wantColour) {
		t.Fatalf("colour %q, want %q", colour.String(), wantColour)
	}
	if strings.Contains(colour.String(), "\x1b[38;") {
		t.Fatal("256-colour sequence found: only palette slots are allowed")
	}
}

// Verifies 46b463750d77: the JSON sink writes one object per event with
// the event's kind and fields.
func TestJSONSink_OneObjectPerEvent(t *testing.T) {
	var sb strings.Builder
	sink := NewJSONSink(&sb)
	evs := epicStream()
	for _, ev := range evs {
		sink.Emit(ev)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != len(evs) {
		t.Fatalf("%d lines for %d events", len(lines), len(evs))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["kind"] != "run-started" || first["workflow"] != "epic" || first["loops"].(map[string]any)["epic/cycles"].(float64) != 45 {
		t.Errorf("first line %s", lines[0])
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if last["kind"] != "run-ended" || last["failed"].(float64) != 1 {
		t.Errorf("last line %s", lines[len(lines)-1])
	}
}

// Verifies f8da2202d321: a held node renders as ready with its hold reason.
func TestTextSink_Waiting(t *testing.T) {
	var sb strings.Builder
	sink := NewTextSink(&sb, false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sink.Emit(Event{Kind: EventRunStarted, Time: at, RunID: "r", Workflow: "w", Loops: map[string]int{}})
	sink.Emit(Event{Kind: EventStepWaiting, Time: at, Step: "w/merge", Detail: "lane landing 1 of 1"})
	want := "00:00:00  ready        merge  waiting for lane landing 1 of 1\n"
	if !strings.HasSuffix(sb.String(), want) {
		t.Errorf("got %q", sb.String())
	}
}
