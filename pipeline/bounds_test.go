package pipeline

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// maxConcurrent counts the most spans overlapping at any instant.
func maxConcurrent(spans [][2]time.Time) int {
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, s := range spans {
		edges = append(edges, edge{s[0], 1}, edge{s[1], -1})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].at.Equal(edges[j].at) {
			return edges[i].delta < edges[j].delta // an end before a start at the same instant
		}
		return edges[i].at.Before(edges[j].at)
	})
	cur, best := 0, 0
	for _, e := range edges {
		cur += e.delta
		if cur > best {
			best = cur
		}
	}
	return best
}

// Verifies f8da2202d321: a generate's max_parallel bounds its own instances
// — four independent items under max_parallel 2 never run more than two
// implement boxes at once, while the run-wide bound stays unlimited.
func TestBounds_GenerateMaxParallel(t *testing.T) {
	h := epicHarness(t)
	h.exec.Source = &fakeSource{stdout: itemsJSON(
		map[string]any{"id": "I-1"}, map[string]any{"id": "I-2"}, map[string]any{"id": "I-3"}, map[string]any{"id": "I-4"},
	)}
	ir := loadReferenceIR(t, "reference_epic.ir.json")
	for i := range ir.Nodes {
		if ir.Nodes[i].Kind == config.KindGenerate {
			ir.Nodes[i].Gen.MaxParallel = 2
		}
	}
	for _, item := range []string{"I-1", "I-2", "I-3", "I-4"} {
		h.boxes.script("epic/tasks["+item+"]/implement", prPayload()).latency = 40 * time.Millisecond
	}
	sink := &recordingSink{}
	h.exec.Events = sink
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	var spans [][2]time.Time
	for id, sp := range h.boxes.spans {
		if strings.HasSuffix(id, "/implement") {
			spans = append(spans, sp)
		}
	}
	if len(spans) != 4 {
		t.Fatalf("implement spans %d, want 4", len(spans))
	}
	if got := maxConcurrent(spans); got > 2 {
		t.Errorf("max concurrent implement boxes %d, want <= 2", got)
	}
	// Something waited on the generate pool and said so.
	found := false
	for _, ev := range sink.evs {
		if ev.Kind == EventStepWaiting && strings.HasPrefix(ev.Detail, "generate epic/tasks ") {
			found = true
		}
	}
	if !found {
		t.Errorf("no step-waiting event named the generate pool: %v", sink.kinds())
	}
}

// Verifies f8da2202d321: a declared lane serializes every box of the
// templates that name it, in id order, while boxes outside the lane overlap
// freely; the held node says which lane holds it.
func TestBounds_LaneSerializes(t *testing.T) {
	nodes := []config.Node{agentNode("w/m1", "out"), agentNode("w/m2", "out"), agentNode("w/m3", "out"), agentNode("w/free", "out")}
	for i := range nodes[:3] {
		nodes[i].Template.Lane = "landing"
	}
	ir := testIR("w", nodes, nil)
	ir.Lanes = map[string]int{"landing": 1}
	h := newHarness(t)
	for _, id := range []string{"w/m1", "w/m2", "w/m3", "w/free"} {
		h.boxes.script(id, okPayload(map[string]any{"out": id})).latency = 30 * time.Millisecond
	}
	sink := &recordingSink{}
	h.exec.Events = sink
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	laneSpans := [][2]time.Time{h.boxes.spans["w/m1"], h.boxes.spans["w/m2"], h.boxes.spans["w/m3"]}
	if got := maxConcurrent(laneSpans); got != 1 {
		t.Errorf("lane boxes ran %d at once, want 1", got)
	}
	free := h.boxes.spans["w/free"]
	if !(free[0].Before(laneSpans[0][1]) && laneSpans[0][0].Before(free[1])) {
		t.Errorf("the lane-less box %v did not overlap the first lane box %v", free, laneSpans[0])
	}
	if got := strings.Join(h.boxes.startOrder(), ","); !strings.HasPrefix(got, "w/free,w/m1,w/m2,w/m3") && !strings.HasPrefix(got, "w/m1,w/free,w/m2,w/m3") {
		t.Errorf("start order %q, want the lane in id order with free beside m1", got)
	}
	var reasons []string
	for _, ev := range sink.evs {
		if ev.Kind == EventStepWaiting {
			reasons = append(reasons, ev.Step+":"+ev.Detail)
		}
	}
	want := "w/m2:lane landing 1 of 1,w/m3:lane landing 1 of 1"
	if got := strings.Join(reasons, ","); got != want {
		t.Errorf("waiting events %q, want %q", got, want)
	}
}

// Verifies f8da2202d321 (edge): a lane slot released by a defer lets the
// next lane box in, and the deferred box re-takes it on re-dispatch.
func TestBounds_LaneReleasedOnDefer(t *testing.T) {
	nodes := []config.Node{agentNode("w/m1", "out"), agentNode("w/m2", "out")}
	for i := range nodes {
		nodes[i].Template.Lane = "landing"
	}
	ir := testIR("w", nodes, nil)
	ir.Lanes = map[string]int{"landing": 1}
	h := newHarness(t)
	h.boxes.script("w/m1",
		failedResult("rate-limit", `{"reset": `+timestamp(testBase.Add(time.Second))+`}`),
		okPayload(map[string]any{"out": "m1"}),
	)
	h.boxes.script("w/m2", okPayload(map[string]any{"out": "m2"}))
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{"w/m1": StateOK, "w/m2": StateOK})
	if h.boxes.attempts("w/m1") != 2 || h.boxes.attempts("w/m2") != 1 {
		t.Errorf("attempts m1=%d m2=%d, want 2 and 1", h.boxes.attempts("w/m1"), h.boxes.attempts("w/m2"))
	}
	rec := h.record(t, "run-test", "w/m1")
	if rec.Result.Status != failure.StatusOK {
		t.Errorf("m1 settled %s, want ok after the deferred re-dispatch", rec.Result.Status)
	}
}
