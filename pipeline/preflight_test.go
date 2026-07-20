package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// fakeImageCheck is a scripted ImageChecker.
type fakeImageCheck struct {
	mu     sync.Mutex
	exists map[string]bool
	err    error
	calls  []string
}

func (f *fakeImageCheck) ImageExists(_ context.Context, tag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tag)
	if f.err != nil {
		return false, f.err
	}
	return f.exists[tag], nil
}

// Verifies 8879dc1597d6: the run preflight fails fast — a missing image is
// one aggregated refusal before any run state exists (no journal, no run
// dir), naming the tag and pointing at faber build, instead of a per-step
// launch-failure cascade burning retry budgets.
func TestPreflightRefusesMissingImage(t *testing.T) {
	h := newHarness(t)
	check := &fakeImageCheck{exists: map[string]bool{}}
	h.exec.ImageCheck = check
	ir := testIR("main", []config.Node{agentNode("task/a", "out")}, nil)

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "faber build") ||
		!strings.Contains(err.Error(), "img/tpl-task-a:test") {
		t.Fatalf("want missing-image refusal naming the tag, got %v", err)
	}
	entries, _ := os.ReadDir(h.store.RunDir("run-test"))
	if len(entries) > 0 {
		t.Fatal("preflight refusal must not mint run state")
	}
}

// Verifies 8879dc1597d6: an unreachable daemon is one error covering every
// image, and a present image set passes preflight and runs.
func TestPreflightDaemonAndSuccessPaths(t *testing.T) {
	h := newHarness(t)
	h.exec.ImageCheck = &fakeImageCheck{err: errors.New("daemon unreachable")}
	ir := testIR("main", []config.Node{agentNode("task/a", "out"), agentNode("task/b", "out")}, nil)
	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("want one daemon error, got %v", err)
	}

	h2 := newHarness(t)
	h2.exec.ImageCheck = &fakeImageCheck{exists: map[string]bool{
		"img/tpl-task-a:test": true, "img/tpl-task-b:test": true,
	}}
	if err := h2.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("preflight with all images present must run: %v", err)
	}
	wantStates(t, h2.states(t, "run-test"), map[string]string{"task/a": StateOK, "task/b": StateOK})
}

// mutableTags is an ImageTagger whose derivation can move mid-test (a pin or
// engine upgrade between two executions of one run).
type mutableTags struct{ suffix string }

func (m *mutableTags) Tag(t *config.ResolvedTemplate) (string, error) {
	return "img/" + t.Name + ":" + m.suffix, nil
}

// Verifies 87f006277d2c (§1, FE-F1): the journal header records each
// template's resolved image tag, and resume compares them against the
// current derivation — an engine-side change the IR hash cannot see fails
// closed instead of silently invalidating every journal key.
func TestResumeRefusesImageDrift(t *testing.T) {
	h := newHarness(t)
	tags := &mutableTags{suffix: "v1"}
	h.exec.Images = tags
	ir := testIR("main", []config.Node{agentNode("task/a", "out")}, nil)
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Same engine: resume passes the image guard.
	if err := h.run(t, ir, config.RunOptions{Mode: "resume", RunID: "run-test"}); err != nil {
		t.Fatalf("resume without drift: %v", err)
	}

	// The derivation moves (pin/engine upgrade): resume fails closed.
	tags.suffix = "v2"
	err := h.run(t, ir, config.RunOptions{Mode: "resume", RunID: "run-test"})
	if err == nil || !strings.Contains(err.Error(), "image inputs changed") ||
		!strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("want image-drift refusal pointing at --fresh, got %v", err)
	}
	// The refusal released the run lock (no wedged run).
	if h.store.RunLive("run-test") {
		t.Fatal("a refused resume must not leave the run lock held")
	}
}

// Verifies 8879dc1597d6 (§1 upgrade guard, L-P3d): every execution appends a
// run-end marker — settled with the scheduler's own failed count — and the
// run's exit signal comes from that count, not from re-reading the journal.
func TestRunEndMarkerAndExitAccounting(t *testing.T) {
	h := newHarness(t)
	h.boxes.script("task/bad", failedResult("agent-failed", "died"))
	ir := testIR("main", []config.Node{agentNode("task/a", "out"), agentNode("task/bad", "out")}, nil)

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "1 step(s) failed") {
		t.Fatalf("want failed-step exit signal from the scheduler count, got %v", err)
	}
	rp, lerr := h.store.Load("run-test")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if rp.End == nil || rp.End.Status != failure.RunEndSettled || rp.End.Failed != 1 {
		t.Fatalf("run-end marker missing or wrong: %+v", rp.End)
	}
}
