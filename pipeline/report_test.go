package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// renderStable derives the run's report from the journal alone with a fixed
// journal path, so goldens are byte-deterministic across machines.
func renderStable(t *testing.T, h *harness, ir *config.IR, runID string) (string, string) {
	t.Helper()
	rp, err := h.store.Load(runID)
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	report, err := (RunReporter{}).Report(rp, ir, "runs/"+runID+"/journal.jsonl")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var text, jsonOut bytes.Buffer
	if err := report.Text(&text); err != nil {
		t.Fatalf("render text: %v", err)
	}
	if err := report.JSON(&jsonOut); err != nil {
		t.Fatalf("render JSON: %v", err)
	}
	return text.String(), jsonOut.String()
}

// checkGolden compares got against testdata/<name>, rewriting the file when
// UPDATE_GOLDEN=1.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// settledTaskLoop runs the reference task loop to its iteration-2 settle.
func settledTaskLoop(t *testing.T) (*harness, *config.IR) {
	t.Helper()
	ir := loadReferenceIR(t, "reference_task.ir.json")
	h := newHarness(t)
	h.params = taskParams()
	h.boxes.script("task/implement", prPayload())
	h.boxes.script("task/review-cycle@1/review", verdict("changes"))
	h.boxes.script("task/review-cycle@2/review", verdict("approved"))
	h.boxes.script("task/review-cycle@1/fix", okPayload(map[string]any{"status": "pushed"}))
	h.boxes.script("task/merge", okPayload(map[string]any{"merged": "yes"}))
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return h, ir
}

// Verifies 990c3d8a7888: the settled task-loop run renders text and JSON
// reports matching golden files byte for byte.
func TestReport_GoldenTaskLoop(t *testing.T) {
	h, ir := settledTaskLoop(t)
	text, jsonOut := renderStable(t, h, ir, "run-test")
	checkGolden(t, "report_task_loop.txt", text)
	checkGolden(t, "report_task_loop.json", jsonOut)
}

// Verifies 990c3d8a7888: the exhausted-loop run's failure block renders the
// structured error byte for byte — and, because a loop exhaustion preserves
// no handoff state, offers no re-entry command (interactive mode would only
// refuse it).
func TestReport_GoldenLoopExhaustion(t *testing.T) {
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
	if err := h.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatalf("want a failed-run error")
	}
	text, jsonOut := renderStable(t, h, ir, "run-test")
	if strings.Contains(text, "re-enter:") {
		t.Errorf("a handoff-less failure must not suggest re-entry (interactive would refuse):\n%s", text)
	}
	checkGolden(t, "report_exhaustion.txt", text)
	checkGolden(t, "report_exhaustion.json", jsonOut)
}

// Verifies 990c3d8a7888 / L-P3c: the re-entry suggestion appears exactly when
// the failed record preserved handoff state to reconstruct from.
func TestReport_ReentrySuggestionGatedOnHandoff(t *testing.T) {
	ir := testIR("main", []config.Node{agentNode("task/a", "out")}, nil)
	h := newHarness(t)
	withHandoff := failedResult("agent-failed", "died")
	withHandoff.Error.Handoff = "boxes/a/attempt-1/result/handoff.json"
	h.boxes.script("task/a", withHandoff)
	if err := h.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatalf("want a failed-run error")
	}
	text, _ := renderStable(t, h, ir, "run-test")
	if !strings.Contains(text, "re-enter: faber resume run-test --interactive task/a") {
		t.Errorf("a failure with handoff state must suggest re-entry:\n%s", text)
	}
}

// Verifies 990c3d8a7888: the fan-out cascade run's rollups render byte for
// byte, and the partial fan-out is legible at a glance.
func TestReport_GoldenFanOutCascade(t *testing.T) {
	h := epicHarness(t)
	h.boxes.script("epic/tasks[I-1]/implement", failedResult("agent", "box died"))
	ir := loadReferenceIR(t, "reference_epic.ir.json")
	if err := h.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatalf("want a failed-run error")
	}
	text, jsonOut := renderStable(t, h, ir, "run-test")
	checkGolden(t, "report_cascade.txt", text)
	checkGolden(t, "report_cascade.json", jsonOut)
}

// Verifies 990c3d8a7888: the report is derived from the journal, never from
// in-memory state — a report reconstructed by a fresh reporter from the
// journal alone is identical to the one the executor emitted at settle time.
func TestReport_JournalOnlyReconstruction(t *testing.T) {
	h, ir := settledTaskLoop(t)
	settleTime := h.json.String()
	if settleTime == "" {
		t.Fatalf("executor emitted no JSON report")
	}
	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	journalPath := filepath.Join(h.store.RunDir("run-test"), "journal.jsonl")
	report, err := (RunReporter{}).Report(rp, ir, journalPath)
	if err != nil {
		t.Fatalf("fresh report: %v", err)
	}
	var rebuilt bytes.Buffer
	if err := report.JSON(&rebuilt); err != nil {
		t.Fatalf("render: %v", err)
	}
	if rebuilt.String() != settleTime {
		t.Errorf("journal-only reconstruction differs from the settle-time report\n--- rebuilt ---\n%s\n--- settle ---\n%s",
			rebuilt.String(), settleTime)
	}
}

// Verifies 990c3d8a7888: the selector's report line names the candidate it
// resolved to, and cost records exist for executed steps only.
func TestReport_SelectorChoseAndTotals(t *testing.T) {
	h, ir := settledTaskLoop(t)
	text, _ := renderStable(t, h, ir, "run-test")
	if !strings.Contains(text, "chose=task/review-cycle@2/review") {
		t.Errorf("selector line does not name its candidate:\n%s", text)
	}
	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	costSteps := map[string]bool{}
	for _, c := range rp.Costs {
		costSteps[c.StepID] = true
	}
	for _, executed := range []string{"task/implement", "task/review-cycle@1/review", "task/merge"} {
		if !costSteps[executed] {
			t.Errorf("executed step %s journaled no cost record", executed)
		}
	}
	for _, cheap := range []string{"task/review", "task/review-cycle@3/fix"} {
		if costSteps[cheap] {
			t.Errorf("non-executed step %s journaled a cost record", cheap)
		}
	}
}

// Verifies 990c3d8a7888 (blocker regression): a failed step whose box emits
// the reserved reason "skipped-condition" (or "skipped-dependency") stays a
// FAILURE — genuine skips are hashless scheduler records, an executed failure
// carries a real input hash. The run's totals, exit code, and dependents'
// root cause must all treat it as the failure it is.
func TestReport_BoxAuthoredSkipReasonStaysFailed(t *testing.T) {
	ir := testIR("w",
		[]config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")},
		[]config.Edge{orderEdge("w/a", "w/b")},
	)
	h := newHarness(t)
	h.boxes.script("w/a", failedResult(reasonSkippedCondition, "hostile box claims a skip"))

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("run exit: got %v, want a failed-run error (exit must be nonzero)", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/a": StateFailed,
		"w/b": StateSkippedDependency,
	})
	rec := h.record(t, "run-test", "w/a")
	if rec.InputHash == "" {
		t.Fatalf("executed failure journaled without an input hash")
	}
	text, jsonOut := renderStable(t, h, ir, "run-test")
	report := reportOf(t, h, ir)
	if report.Run.Totals.Failed != 1 {
		t.Errorf("Totals.Failed = %d, want 1", report.Run.Totals.Failed)
	}
	if report.Run.Totals.SkippedCondition != 0 {
		t.Errorf("Totals.SkippedCondition = %d, want 0", report.Run.Totals.SkippedCondition)
	}
	if !strings.Contains(text, "failed              w/a") {
		t.Errorf("text report does not show w/a failed:\n%s", text)
	}
	_ = jsonOut

	// The genuine skip encodings still decode: w/b is a hashless scheduler
	// record and reports skipped-dependency, ancestor intact.
	for _, line := range report.Steps {
		if line.ID == "w/b" {
			if line.Status != StateSkippedDependency || line.Ancestor != "w/a" {
				t.Errorf("w/b line %+v, want skipped-dependency after w/a", line)
			}
		}
	}
}

// Verifies a0f44481f57b: box-authored failure reasons colliding with the
// annotation vocabulary ("deferred"/"cached") are real attempts — the report
// counts no phantom defers, marks nothing cached, and the attempt count is
// the real one. Scheduler annotations are keyed on their Attempt == 0 marker.
func TestReport_BoxAuthoredAnnotationReasonIsRealAttempt(t *testing.T) {
	node := agentNode("w/x", "out")
	node.Retry = 1
	ir := testIR("w", []config.Node{node}, nil)
	h := newHarness(t)
	h.boxes.script("w/x",
		failedResult(reasonDeferred, "hostile box claims a defer"),
		okPayload(map[string]any{"out": "done"}),
	)

	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec := h.record(t, "run-test", "w/x")
	deferred, wait, cached, real := decodeAnnotations(rec.Result.Attempts)
	if deferred != 0 || wait != 0 {
		t.Errorf("box-authored reason counted as %d defers (wait %s), want 0", deferred, wait)
	}
	if cached {
		t.Errorf("box-authored reason marked the record cached")
	}
	if real != 1 {
		t.Errorf("real prior attempts %d, want 1", real)
	}
	report := reportOf(t, h, ir)
	for _, line := range report.Steps {
		if line.ID != "w/x" {
			continue
		}
		if line.Deferred != 0 || line.DeferredFor != "" || line.Cached {
			t.Errorf("report line carries phantom annotations: %+v", line)
		}
		if line.Attempts != 2 {
			t.Errorf("attempts %d, want 2", line.Attempts)
		}
	}
	// No rate-limit counter was consumed: the box never emitted the reserved
	// rate-limit reason, so the retry (not the defer floor) handled it.
	if got := h.boxes.attempts("w/x"); got != 2 {
		t.Errorf("box ran %d attempts, want 2 (one retry)", got)
	}
}

// reportOf derives the RunReport from the journal for assertions.
func reportOf(t *testing.T, h *harness, ir *config.IR) *RunReport {
	t.Helper()
	rp, err := h.store.Load("run-test")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	report, err := (RunReporter{}).Report(rp, ir, "runs/run-test/journal.jsonl")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return report
}

// Verifies 990c3d8a7888 (TB-F2): box-derived text is sanitized on the human
// Text path — control bytes (ANSI escapes, newlines that would spoof report
// lines) never reach the operator's terminal; the JSON path is
// encoding-escaped by construction.
func TestReport_TerminalSanitized(t *testing.T) {
	ir := testIR("main", []config.Node{agentNode("task/a", "out")}, nil)
	h := newHarness(t)
	hostile := failedResult("agent-failed", "\x1b]0;pwned\x07cleared\x1b[2J\nok fake/line injected")
	h.boxes.script("task/a", hostile)
	if err := h.run(t, ir, config.RunOptions{}); err == nil {
		t.Fatal("want a failed-run error")
	}
	text := h.text.String()
	for _, banned := range []string{"\x1b", "\x07", "\nok fake/line"} {
		if strings.Contains(text, banned) {
			t.Fatalf("report text carries unsanitized control bytes %q:\n%q", banned, text)
		}
	}
	if !strings.Contains(text, "cleared") {
		t.Fatalf("printable content must survive sanitization:\n%s", text)
	}

	// Payload values on ok lines are sanitized too.
	h2 := newHarness(t)
	h2.boxes.script("task/a", okPayload(map[string]any{"out": "x\x1b[31mred"}))
	if err := h2.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h2.text.String(), "\x1b") {
		t.Fatalf("ok-line payload values carry unsanitized escapes:\n%q", h2.text.String())
	}
}

// Verifies 990c3d8a7888 (F1-review): sanitizeTerm is self-sufficient — a raw
// invalid-UTF-8 C1 byte (0x9b, a bare CSI introducer) is neutralized even
// without an upstream JSON round-trip, so the fast path cannot pass raw
// control bytes through, while ordinary printable text is untouched.
func TestSanitizeTermRawControlBytes(t *testing.T) {
	for _, in := range []string{"\x9b2Kraw-csi", "esc\x1b[31m", "bel\x07", "line1\nline2"} {
		out := sanitizeTerm(in)
		for i := 0; i < len(out); i++ {
			b := out[i]
			if b == 0x1b || b == 0x07 || b == 0x9b || b == '\n' {
				t.Errorf("sanitizeTerm(%q) still carries control byte %#x: %q", in, b, out)
			}
		}
	}
	if got := sanitizeTerm("plain text 123"); got != "plain text 123" {
		t.Errorf("printable text mangled: %q", got)
	}
}
