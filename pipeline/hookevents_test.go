package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// Verifies 8879dc1597d6: the run preflight proves every declared hook
// script on this machine before any run state exists — a missing one is
// one refusal naming the template, the slot and the path — and a script
// that is there lets the run proceed.
func TestPreflightChecksHookFiles(t *testing.T) {
	h := newHarness(t)
	n := agentNode("task/a", "out")
	n.Template.Hooks.Prelude = filepath.Join(t.TempDir(), "absent")
	n.Template.Hooks.OnFailure = filepath.Join(t.TempDir(), "cleanup")
	if err := os.WriteFile(n.Template.Hooks.OnFailure, []byte("exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ir := testIR("main", []config.Node{n}, nil)
	err := h.run(t, ir, config.RunOptions{})
	for _, want := range []string{"template tpl-task-a: hooks.prelude", "not found", "hooks.on_failure", "no #! interpreter line"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want refusal containing %q, got %v", want, err)
		}
	}
	if entries, _ := os.ReadDir(h.store.RunDir("run-test")); len(entries) > 0 {
		t.Fatal("preflight refusal must not mint run state")
	}

	h2 := newHarness(t)
	good := filepath.Join(t.TempDir(), "prelude")
	if err := os.WriteFile(good, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n2 := agentNode("task/a", "out")
	n2.Template.Hooks.Prelude = good
	if err := h2.run(t, testIR("main", []config.Node{n2}, nil), config.RunOptions{}); err != nil {
		t.Fatalf("a present hook must pass preflight: %v", err)
	}
	wantStates(t, h2.states(t, "run-test"), map[string]string{"task/a": StateOK})
}

type recordingHookRunner struct {
	mu   sync.Mutex
	invs []failure.HookInvocation
}

func (r *recordingHookRunner) RunOnFailure(_ context.Context, inv failure.HookInvocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invs = append(r.invs, inv)
	return nil
}

// Verifies 3fe064781899: the host-side on_failure hook is told the failed
// attempt's result directory — the box's own $FABER_RESULT_DIR — under the
// same name the box contract uses, so all four slots share one events file.
func TestOnFailureHookGetsTheAttemptResultDir(t *testing.T) {
	if failure.EnvResultDir != contract.EnvResultDir {
		t.Fatalf("failure.EnvResultDir %q must equal contract.EnvResultDir %q", failure.EnvResultDir, contract.EnvResultDir)
	}
	h := newHarness(t)
	hooks := &recordingHookRunner{}
	h.exec.Hooks = hooks
	n := agentNode("w/a", "out")
	n.OnFailure = "/bin/true"
	h.boxes.script("w/a", failedResult("agent-failed", "agent exited 1"))
	_ = h.run(t, testIR("w", []config.Node{n}, nil), config.RunOptions{})
	if len(hooks.invs) != 1 {
		t.Fatalf("want one on_failure invocation, got %d", len(hooks.invs))
	}
	want := ResultDir(h.store.RunDir("run-test"), "w/a", 1)
	if hooks.invs[0].ResultDir != want {
		t.Errorf("ResultDir %q, want %q", hooks.invs[0].ResultDir, want)
	}
}

// Verifies 3fe064781899: AttemptEvents finds the attempt's events file by
// the same path derivation the box run uses.
func TestAttemptEventsReadsTheResultDir(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(ResultDir(root, "w/a", 2), contract.HooksEventsFile)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"ts":"2026-11-04T10:00:00Z","hook":"h","event":"progress","detail":"x"}`+"\n"), 0o644)
	ev, err := AttemptEvents(root, "w/a", 2)
	if err != nil || len(ev.Events) != 1 || ev.Events[0].Name != "progress" {
		t.Fatalf("got %+v, %v", ev, err)
	}
}
