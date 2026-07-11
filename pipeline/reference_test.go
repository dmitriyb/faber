package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// taskParams supplies the reference task workflow's declared params.
func taskParams() config.Params {
	return config.Params{
		"item": {Type: "string", Value: "W-1"},
		"repo": {Type: "string", Value: "gw/repo"},
	}
}

// Verifies ae796d2a1503 and 595a2a6fcc5b: the reference task IR's unrolled
// review/fix chain settles on the approval condition at iteration 2 — fix@1
// runs, iteration 3 skips via the skipped-dependency short-circuit, the
// selector adopts review@2's payload, and merge runs.
func TestReference_TaskLoopSettlesOnIterationTwo(t *testing.T) {
	ir := loadReferenceIR(t, "reference_task.ir.json")
	h := newHarness(t)
	h.params = taskParams()
	h.boxes.script("task/implement", prPayload())
	h.boxes.script("task/review-cycle@1/review", verdict("changes"))
	h.boxes.script("task/review-cycle@2/review", verdict("approved"))
	h.boxes.script("task/review-cycle@1/fix", okPayload(map[string]any{"status": "pushed"}))
	h.boxes.script("task/merge", okPayload(map[string]any{"merged": true}))

	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"task/implement":             StateOK,
		"task/review-cycle@1/review": StateOK,
		"task/review-cycle@1/fix":    StateOK,
		"task/review-cycle@2/review": StateOK,
		"task/review-cycle@2/fix":    StateSkippedCondition,
		"task/review-cycle@3/review": StateSkippedCondition,
		"task/review-cycle@3/fix":    StateSkippedCondition,
		"task/review":                StateOK,
		"task/merge":                 StateOK,
	})
	if len(states) != 9 {
		t.Errorf("journal has %d step records, want 9: %v", len(states), states)
	}
	// The selector adopted review@2's payload.
	sel := h.record(t, "run-test", "task/review")
	if !strings.Contains(string(sel.Result.Payload), "approved") {
		t.Errorf("selector payload %s does not carry review@2's verdict", sel.Result.Payload)
	}
	// Iteration 3 never launched a box.
	if h.boxes.attempts("task/review-cycle@3/review") != 0 || h.boxes.attempts("task/review-cycle@3/fix") != 0 {
		t.Errorf("iteration 3 launched boxes despite the settled loop")
	}
}

// Verifies a0f44481f57b and ae796d2a1503: three unfavorable verdicts (each
// status ok — unfavorable is not failure) exhaust the loop: the selector
// settles failed (loop-exhausted), merge settles skipped-dependency naming
// the selector, and the report's failure block names the exhausted bound.
func TestReference_LoopExhaustion(t *testing.T) {
	ir := loadReferenceIR(t, "reference_task.ir.json")
	h := newHarness(t)
	h.params = taskParams()
	h.boxes.script("task/implement", prPayload())
	h.boxes.deflt = func(box BoxAttempt) failure.Result {
		if strings.HasSuffix(box.NodeID, "/review") {
			return verdict("changes")
		}
		return okPayload(map[string]any{"status": "pushed"})
	}

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want a failed-run error, got %v", err)
	}
	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"task/review-cycle@1/review": StateOK,
		"task/review-cycle@2/review": StateOK,
		"task/review-cycle@3/review": StateOK,
		"task/review-cycle@3/fix":    StateOK,
		"task/review":                StateFailed,
		"task/merge":                 StateSkippedDependency,
	})
	sel := h.record(t, "run-test", "task/review")
	if sel.Result.Error.Reason != failure.ReasonLoopExhausted {
		t.Errorf("selector failed with reason %q, want %q", sel.Result.Error.Reason, failure.ReasonLoopExhausted)
	}
	if !strings.Contains(sel.Result.Error.Detail, "3 iterations") {
		t.Errorf("failure detail does not name the exhausted bound: %s", sel.Result.Error.Detail)
	}
	merge := h.record(t, "run-test", "task/merge")
	if merge.Result.Error.Detail != "task/review" {
		t.Errorf("merge skip names ancestor %q, want task/review", merge.Result.Error.Detail)
	}
	if !strings.Contains(h.text.String(), failure.ReasonLoopExhausted) {
		t.Errorf("report failure block does not carry the loop-exhausted reason:\n%s", h.text.String())
	}
}

// Verifies 595a2a6fcc5b: a condition evaluation error against a well-typed
// activation (a payload lacking the read field) fails the node — never
// silently false.
func TestReference_ConditionEvaluationErrorFailsNode(t *testing.T) {
	ir := loadReferenceIR(t, "reference_task.ir.json")
	h := newHarness(t)
	h.params = taskParams()
	h.boxes.script("task/implement", prPayload())
	// Hostile payload: status ok but no verdict field, so fix@1's condition
	// steps[...review].verdict cannot evaluate.
	h.boxes.script("task/review-cycle@1/review", okPayload(map[string]any{"unexpected": "shape"}))

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want a failed-run error, got %v", err)
	}
	states := h.states(t, "run-test")
	if states["task/review-cycle@1/fix"] != StateFailed {
		t.Fatalf("fix@1 settled %s, want failed on condition error", states["task/review-cycle@1/fix"])
	}
	rec := h.record(t, "run-test", "task/review-cycle@1/fix")
	if rec.Result.Error.Reason != reasonCondition {
		t.Errorf("failure reason %q, want %q", rec.Result.Error.Reason, reasonCondition)
	}
}

// Verifies 595a2a6fcc5b: a false condition is a distinct terminal state
// (skipped-condition), the skip short-circuits every condition reading the
// skipped step, and ordering-only dependents of a condition-skip still run.
func TestReference_SkipSemantics(t *testing.T) {
	nodes := []config.Node{
		agentNode("w/a", "out"),
		agentNode("w/b", "out"),
		agentNode("w/c", "out"),
		agentNode("w/d", "out"),
	}
	nodes[1].When = &config.CondSpec{CEL: `steps["w/a"].out == "no"`, Deps: []string{"w/a"}}
	nodes[2].When = &config.CondSpec{CEL: `steps["w/b"].out == "whatever"`, Deps: []string{"w/b"}}
	ir := testIR("w", nodes, []config.Edge{orderEdge("w/c", "w/d")})

	h := newHarness(t)
	h.boxes.script("w/a", okPayload(map[string]any{"out": "yes"}))

	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run (condition skips are success): %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/a": StateOK,
		"w/b": StateSkippedCondition, // condition false
		"w/c": StateSkippedCondition, // short-circuit: reads skipped w/b
		"w/d": StateOK,               // ordering dependent of a condition skip still runs
	})
	if h.boxes.attempts("w/b") != 0 || h.boxes.attempts("w/c") != 0 {
		t.Errorf("skipped nodes launched boxes")
	}
}

// Verifies 8879dc1597d6 and a0f44481f57b: resume replays the journal — a
// settled step is a cached hit whose box never re-runs and whose report line
// is marked cached; --no-cache (fresh) re-runs everything under a new run id.
func TestReference_ResumeFromJournal(t *testing.T) {
	ir := loadReferenceIR(t, "reference_task.ir.json")
	h := newHarness(t)
	h.params = taskParams()
	h.exec.Meta.Supplied = map[string]string{"item": "W-1", "repo": "gw/repo"}

	// Run 1: kill the scheduler (context cancel) right after implement's box
	// finishes; implement settles into the journal, nothing else runs.
	ctx, cancel := context.WithCancel(context.Background())
	h.boxes.script("task/implement", prPayload()).onAttempt = func(int) { cancel() }
	err := h.runCtx(ctx, t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("want an aborted-run error, got %v", err)
	}
	if got := h.states(t, "run-test"); got["task/implement"] != StateOK {
		t.Fatalf("implement settled %q before the abort, want ok", got["task/implement"])
	}

	// Run 2: resume the same journal. implement is a cached hit — the box
	// runner is never invoked for it — and execution restarts at review@1.
	h.boxes.scripts["task/implement"].onAttempt = nil
	h.boxes.script("task/review-cycle@1/review", verdict("approved"))
	h.boxes.script("task/merge", okPayload(map[string]any{"merged": true}))
	h.text.Reset()
	if err := h.run(t, ir, config.RunOptions{Mode: "resume", RunID: "run-test"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := h.boxes.attempts("task/implement"); got != 1 {
		t.Fatalf("implement ran %d boxes across run+resume, want 1 (cached hit)", got)
	}
	if got := h.boxes.attempts("task/review-cycle@1/review"); got != 1 {
		t.Fatalf("review@1 ran %d boxes on resume, want 1", got)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"task/implement": StateOK,
		"task/review":    StateOK,
		"task/merge":     StateOK,
	})
	rec := h.record(t, "run-test", "task/implement")
	if _, _, cached, _ := decodeAnnotations(rec.Result.Attempts); !cached {
		t.Errorf("resumed implement record carries no cached annotation")
	}
	if !strings.Contains(h.text.String(), "(cached)") {
		t.Errorf("report does not mark implement cached:\n%s", h.text.String())
	}

	// Run 3: --no-cache. A fresh run id, an empty lookup, every node re-runs.
	if err := h.run(t, ir, config.RunOptions{Mode: "fresh"}); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if got := h.boxes.attempts("task/implement"); got != 2 {
		t.Errorf("implement ran %d boxes after --no-cache, want 2", got)
	}
}

// Verifies a0f44481f57b: an inlined sub-workflow entry that skips takes its
// whole inner graph with it, and a failure inside the sub-workflow skips the
// outer dependents that wait on it.
func TestReference_SubWorkflowScopes(t *testing.T) {
	subIR := func(scope string) *config.IR {
		inner := agentNode(scope+"/inner", "out")
		inner.Bindings = map[string]config.BindingDesc{}
		return testIR("subwf", []config.Node{inner}, nil)
	}
	sub := config.Node{
		ID:       "w/child",
		Kind:     config.KindSubWorkflow,
		Sub:      subIR("w/child"),
		Bindings: map[string]config.BindingDesc{},
		When:     &config.CondSpec{CEL: `steps["w/a"].out == "go"`, Deps: []string{"w/a"}},
	}
	nodes := []config.Node{agentNode("w/a", "out"), sub, agentNode("w/z", "out")}
	edges := []config.Edge{orderEdge("w/child", "w/z")}
	ir := testIR("w", nodes, edges)

	// Condition false: the entry and its whole inner graph settle
	// skipped-condition; the ordering dependent still runs.
	h := newHarness(t)
	h.boxes.script("w/a", okPayload(map[string]any{"out": "halt"}))
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/a":           StateOK,
		"w/child":       StateSkippedCondition,
		"w/child/inner": StateSkippedCondition,
		"w/z":           StateOK,
	})

	// Inner failure: the outer dependent (wired to the sub graph's sinks)
	// settles skipped-dependency.
	h2 := newHarness(t)
	h2.boxes.script("w/a", okPayload(map[string]any{"out": "go"}))
	h2.boxes.script("w/child/inner", failedResult("agent", "inner died"))
	if err := h2.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatalf("want a failed-run error")
	}
	wantStates(t, h2.states(t, "run-test"), map[string]string{
		"w/child":       StateOK,
		"w/child/inner": StateFailed,
		"w/z":           StateSkippedDependency,
	})
}
