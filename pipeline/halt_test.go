package pipeline

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// Verifies a0f44481f57b: a halted step stops its chain, not the run — the
// dependent settles skipped-halt naming the halting ancestor (hashless, so
// never a resume hit), the independent branch completes, the run error is
// the typed halt error carrying exit code 3, and the run-end record counts
// the halt.
func TestScheduling_HaltStopsChainNotRun(t *testing.T) {
	ir := diamondIR()
	h := newHarness(t)
	// b halts even with retries declared: a halt consumes no retry budget.
	for i := range ir.Nodes {
		if ir.Nodes[i].ID == "w/b" {
			ir.Nodes[i].Retry = 2
		}
	}
	h.boxes.script("w/b", haltedResult("needs-triage", "ci stuck past the retry budget"))

	err := h.run(t, ir, config.RunOptions{})
	var halted *RunHalted
	if !errors.As(err, &halted) {
		t.Fatalf("want *RunHalted, got %v", err)
	}
	if halted.ExitCode() != 3 {
		t.Fatalf("halted exit code = %d, want 3", halted.ExitCode())
	}
	if len(halted.Steps) != 1 || halted.Steps[0] != (HaltedStep{Step: "w/b", Reason: "needs-triage"}) {
		t.Fatalf("halted steps = %+v", halted.Steps)
	}
	if !strings.Contains(err.Error(), "w/b") || !strings.Contains(err.Error(), "needs-triage") {
		t.Fatalf("halt error must name the step and reason: %v", err)
	}
	if got := h.boxes.attempts("w/b"); got != 1 {
		t.Fatalf("halting step ran %d attempts, want 1 (halt consumes no retry)", got)
	}

	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"w/a": StateOK,
		"w/b": StateHalted,
		"w/c": StateOK,
		"w/d": StateSkippedHalt,
	})
	rec := h.record(t, "run-test", "w/d")
	if rec.Result.Error == nil || rec.Result.Error.Detail != "w/b" {
		t.Fatalf("skip record names ancestor %v, want w/b", rec.Result.Error)
	}
	if rec.InputHash != "" {
		t.Fatalf("halt-skip record carries input hash %q, want null", rec.InputHash)
	}
	bRec := h.record(t, "run-test", "w/b")
	if bRec.InputHash == "" {
		t.Fatal("the halted step executed; its record must carry a real hash")
	}
	if bRec.Result.Halt == nil || bRec.Result.Halt.Reason != "needs-triage" {
		t.Fatalf("journaled halt arm = %+v", bRec.Result.Halt)
	}

	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatal(err)
	}
	if rp.End == nil || rp.End.Halted != 1 || rp.End.Failed != 0 {
		t.Fatalf("run-end = %+v, want halted=1 failed=0", rp.End)
	}

	// The report names the halt without the reader parsing JSON.
	text := h.text.String()
	for _, want := range []string{
		"halted:",
		"w/b: needs-triage: ci stuck past the retry budget",
		"1 halted",
		"1 skipped (halt)",
		"after-halt-of=w/b",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

// Verifies a0f44481f57b: a halted step's skip attribution survives a
// sub-workflow boundary — the entry settles skipped-halt naming the halting
// step, and every inlined member cascades as skipped-halt naming the entry,
// never as a dependency-failure cascade.
func TestScheduling_HaltCascadesThroughSubWorkflow(t *testing.T) {
	inner := agentNode("w/child/inner", "out")
	inner.Bindings = map[string]config.BindingDesc{}
	sub := config.Node{
		ID:       "w/child",
		Kind:     config.KindSubWorkflow,
		Sub:      testIR("subwf", []config.Node{inner}, nil),
		Bindings: map[string]config.BindingDesc{},
	}
	ir := testIR("w",
		[]config.Node{agentNode("w/a", "out"), sub},
		[]config.Edge{orderEdge("w/a", "w/child")},
	)

	h := newHarness(t)
	h.boxes.script("w/a", haltedResult("needs-triage", ""))

	err := h.run(t, ir, config.RunOptions{})
	var halted *RunHalted
	if !errors.As(err, &halted) {
		t.Fatalf("want *RunHalted, got %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/a":           StateHalted,
		"w/child":       StateSkippedHalt,
		"w/child/inner": StateSkippedHalt,
	})
	if rec := h.record(t, "run-test", "w/child"); rec.Result.Error == nil || rec.Result.Error.Detail != "w/a" {
		t.Fatalf("entry skip names %v, want w/a", rec.Result.Error)
	}
	if rec := h.record(t, "run-test", "w/child/inner"); rec.Result.Error == nil || rec.Result.Error.Detail != "w/child" {
		t.Fatalf("member skip names %v, want the entry w/child", rec.Result.Error)
	}
}

// Verifies a0f44481f57b: failure outranks halt — a run with both a failed
// and a halted step is a failure (exit 1 via the plain error), naming both
// counts, never the halt error.
func TestScheduling_FailureOutranksHalt(t *testing.T) {
	aNodes, aEdges := chain("w/a", 2)
	bNodes, bEdges := chain("w/b", 2)
	ir := testIR("w", append(aNodes, bNodes...), append(aEdges, bEdges...))

	h := newHarness(t)
	h.boxes.script("w/a/s0", haltedResult("needs-triage", ""))
	h.boxes.script("w/b/s0", failedResult("agent", "the box died"))

	err := h.run(t, ir, config.RunOptions{})
	var halted *RunHalted
	if errors.As(err, &halted) {
		t.Fatalf("a run with failures must not return the halt error: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "1 step(s) failed, 1 halted") {
		t.Fatalf("want the failure error naming both counts, got %v", err)
	}
	rp, lerr := h.store.Load("run-test")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if rp.End == nil || rp.End.Failed != 1 || rp.End.Halted != 1 {
		t.Fatalf("run-end = %+v, want failed=1 halted=1", rp.End)
	}
}

// Verifies 87f006277d2c + a0f44481f57b: a halted run is resumable at the
// halted step — the halted record is not a reuse hit, so resume re-runs
// exactly the halted step and its skipped dependents; the settled steps hit
// the journal and are never re-executed.
func TestScheduling_ResumeReentersHaltedStep(t *testing.T) {
	ir := diamondIR()
	h := newHarness(t)
	h.boxes.script("w/b",
		haltedResult("needs-triage", "operator, look"),
		okPayload(map[string]any{"out": "resolved"}),
	)

	err := h.run(t, ir, config.RunOptions{RunID: "run-halt"})
	var halted *RunHalted
	if !errors.As(err, &halted) {
		t.Fatalf("first run: want *RunHalted, got %v", err)
	}

	if err := h.run(t, ir, config.RunOptions{RunID: "run-halt", Mode: "resume"}); err != nil {
		t.Fatalf("resume after halt must settle clean: %v", err)
	}
	states := h.states(t, "run-halt")
	wantStates(t, states, map[string]string{
		"w/a": StateOK, "w/b": StateOK, "w/c": StateOK, "w/d": StateOK,
	})
	// a and c were journal hits; b re-ran (the halted record missed), d ran
	// for the first time.
	if got := h.boxes.attempts("w/a"); got != 1 {
		t.Errorf("w/a ran %d times, want 1 (resume hit)", got)
	}
	if got := h.boxes.attempts("w/b"); got != 2 {
		t.Errorf("w/b ran %d times, want 2 (halted, then re-run on resume)", got)
	}
	if got := h.boxes.attempts("w/d"); got != 1 {
		t.Errorf("w/d ran %d times, want 1", got)
	}
	// The resumed run's report marks the cached hits, and its journal's
	// last-wins record for b is the ok re-run.
	rec := h.record(t, "run-halt", "w/b")
	if rec.Result.Status != failure.StatusOK {
		t.Fatalf("b's last record = %+v, want ok after resume", rec.Result)
	}
}

// Verifies a0f44481f57b: a hostile box cannot forge a scheduler skip through
// the halt vocabulary. Unit half: the extract boundary namespaces the
// reserved reason. End-to-end half: even a failed record that reaches the
// journal claiming reason "skipped-halt" carries the executed step's real
// input hash, so the reporter's null-hash gate keeps it a failure — the
// failure totals and exit code never zero out.
func TestScheduling_BoxCannotForgeHaltSkip(t *testing.T) {
	if got := sanitizeBoxReason(reasonSkippedHalt); got != "box:skipped-halt" {
		t.Fatalf("sanitizeBoxReason(skipped-halt) = %q, want box:skipped-halt", got)
	}

	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	h := newHarness(t)
	h.boxes.script("w/x", failedResult(reasonSkippedHalt, "forged"))

	err := h.run(t, ir, config.RunOptions{})
	var halted *RunHalted
	if errors.As(err, &halted) {
		t.Fatalf("a forged skip reason must never read as a halt: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "1 step(s) failed") {
		t.Fatalf("want the failure error, got %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{"w/x": StateFailed})
	rp, lerr := h.store.Load("run-test")
	if lerr != nil {
		t.Fatal(lerr)
	}
	report, rerr := (RunReporter{}).Report(rp, ir, "")
	if rerr != nil {
		t.Fatal(rerr)
	}
	tot := report.Run.Totals
	if tot.Failed != 1 || tot.SkippedHalt != 0 || tot.Halted != 0 {
		t.Fatalf("forged record moved the totals: %+v", tot)
	}
}
