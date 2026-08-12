package pipeline

import (
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
)

// Verifies ff8e85704b0a: the agent-skipped marker crosses the record
// adaptation seam on both the ok and the failed path — the journal must
// show that no agent ran regardless of how the attempt settled.
func TestAdaptResultCarriesAgentSkipped(t *testing.T) {
	box := BoxAttempt{NodeID: "w/x", Attempt: 1, Template: testTemplate("tpl")}
	okRec := agent.Result{Status: agent.StatusOK, Payload: map[string]any{}, AgentSkipped: true}
	if got := adaptResult(okRec, box, infra.RunResult{}, nil); !got.AgentSkipped {
		t.Fatalf("ok adapt lost the marker: %+v", got)
	}
	failedRec := agent.Result{
		Status:       agent.StatusFailed,
		Error:        &agent.ResultError{Reason: "missing-output", Detail: "x"},
		AgentSkipped: true,
	}
	if got := adaptResult(failedRec, box, infra.RunResult{}, nil); !got.AgentSkipped {
		t.Fatalf("failed adapt lost the marker: %+v", got)
	}
}

// Verifies 990c3d8a7888 + 87f006277d2c: a skipped-agent step journals and
// reports its marker — the step line reads (agent-skipped) — and a resumed
// run adopts the prior ok record verbatim, marker included, without ever
// re-running the box: resume across a skipped-agent step behaves identically
// to any settled step.
func TestSkippedAgentStepReportsAndResumes(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	res := okPayload(map[string]any{"out": "landed"})
	res.AgentSkipped = true

	h := newHarness(t)
	h.boxes.script("w/x", res)

	if err := h.run(t, ir, config.RunOptions{RunID: "run-skip"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec := h.record(t, "run-skip", "w/x")
	if !rec.Result.AgentSkipped {
		t.Fatalf("journal record lost the marker: %+v", rec.Result)
	}
	if !strings.Contains(h.text.String(), "(agent-skipped)") {
		t.Fatalf("report missing the agent-skipped marker:\n%s", h.text.String())
	}

	if err := h.run(t, ir, config.RunOptions{RunID: "run-skip", Mode: "resume"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := h.boxes.attempts("w/x"); got != 1 {
		t.Fatalf("resume re-ran a settled skipped-agent step (%d attempts, want 1)", got)
	}
	rec = h.record(t, "run-skip", "w/x")
	if rec.Result.Status != failure.StatusOK || !rec.Result.AgentSkipped {
		t.Fatalf("adopted record must keep the marker: %+v", rec.Result)
	}
}
