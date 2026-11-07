package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// Verifies c563d2055eb9: a check run's boxes receive FABER_CHECK=1 and a
// real run's receive nothing — the mode is one variable the wiring sets.
func TestBoxRunCheckEnv(t *testing.T) {
	run := func(check bool) map[string]string {
		containers := &fakeContainers{record: &contract.Result{Status: contract.StatusOK, Payload: map[string]any{"out": "done"}, Attempt: 1}}
		boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, Check: check}
		if _, err := boxes.RunAttempt(context.Background(), boxAttempt(t)); err != nil {
			t.Fatalf("run attempt: %v", err)
		}
		return containers.specs[0].Env
	}
	if env := run(false); env[contract.EnvCheck] != "" {
		t.Fatalf("real run emitted %s=%q", contract.EnvCheck, env[contract.EnvCheck])
	}
	if env := run(true); env[contract.EnvCheck] != "1" {
		t.Fatalf("check run env: %v", env)
	}
}

// Verifies 990c3d8a7888: a check run's report carries one line per
// executed step — the template's declared hook slots, the events its hooks
// wrote, and for a failure the handoff's phase and the reason — in the
// text under `check:` and in the JSON under `check`.
func TestCheckReportLines(t *testing.T) {
	h := newHarness(t)
	a := agentNode("w/a", "out")
	a.Template.Hooks.Context = filepath.Join(t.TempDir(), "context")
	a.Template.Hooks.Prelude = filepath.Join(t.TempDir(), "prelude")
	for _, p := range []string{a.Template.Hooks.Context, a.Template.Hooks.Prelude} {
		os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	b := agentNode("w/b", "out")
	// what the boxes leave on disk: a's events, b's handoff with its phase
	h.boxes.deflt = func(box BoxAttempt) failure.Result {
		rd := ResultDir(box.RunDir, box.NodeID, box.Attempt)
		os.MkdirAll(filepath.Join(rd, "hooks"), 0o755)
		switch box.NodeID {
		case "w/b":
			raw, _ := json.Marshal(contract.Handoff{Phase: "prelude", Reason: "hook-failed"})
			os.WriteFile(filepath.Join(rd, contract.HandoffFile), raw, 0o644)
			res := failedResult("hook-failed", "hook prelude exited 1")
			rel, _ := filepath.Rel(box.RunDir, filepath.Join(rd, contract.HandoffFile))
			res.Error.Handoff = rel
			return res
		default:
			os.WriteFile(filepath.Join(rd, contract.HooksEventsFile), []byte(`{"ts":"2026-11-05T10:00:00Z","hook":"prelude","event":"claim","result":"ok"}`+"\n"+`{"ts":"2026-11-05T10:00:01Z","hook":"prelude","event":"push_verify","result":"ok"}`+"\n"), 0o644)
			return okPayload(map[string]any{"out": box.NodeID})
		}
	}
	var text, js bytes.Buffer
	h.exec.Out, h.exec.JSONOut = &text, &js
	_ = h.run(t, testIR("w", []config.Node{a, b}, []config.Edge{orderEdge("w/a", "w/b")}), config.RunOptions{Check: true})
	for _, want := range []string{"check:", "ok           w/a  hooks context prelude  events 2", "FAIL         w/b  events 0  phase prelude  hook-failed: hook prelude exited 1"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	var rep RunReport
	if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Check) != 2 || rep.Check[0].Events != 2 || rep.Check[1].Phase != "prelude" || rep.Check[1].Reason != "hook-failed" {
		t.Errorf("json check lines: %+v", rep.Check)
	}

	// a real run carries no check account
	h2 := newHarness(t)
	var text2 bytes.Buffer
	h2.exec.Out = &text2
	_ = h2.run(t, testIR("w", []config.Node{agentNode("w/a", "out")}, nil), config.RunOptions{})
	if strings.Contains(text2.String(), "check:") {
		t.Errorf("real run printed a check account:\n%s", text2.String())
	}
}
