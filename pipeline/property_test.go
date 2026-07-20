package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// randomDAG builds a random layered agent DAG: n nodes in ascending id order,
// each with edges only from lower ids (acyclic by construction). Deterministic
// in seed.
func randomDAG(rng *rand.Rand, n int) *config.IR {
	nodes := make([]config.Node, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("w/s%02d", i)
		nodes[i] = agentNode(ids[i], "out")
	}
	var edges []config.Edge
	for i := 1; i < n; i++ {
		// Each node draws 0..2 predecessors from the strictly-lower ids.
		for k := 0; k < rng.Intn(3); k++ {
			from := rng.Intn(i)
			edges = append(edges, orderEdge(ids[from], ids[i]))
		}
	}
	return testIR("w", nodes, edges)
}

// countAgents counts agent nodes reachable in one IR level (the property's
// settlement target).
func countAgents(ir *config.IR) int {
	n := 0
	for i := range ir.Nodes {
		if ir.Nodes[i].Kind == config.KindAgent {
			n++
		}
	}
	return n
}

// refusingBoxes fails the test if any box is launched — the resume oracle.
type refusingBoxes struct{ t *testing.T }

func (r refusingBoxes) RunAttempt(context.Context, BoxAttempt) (BoxResult, error) {
	r.t.Fatal("resume launched a box for a fully-journaled run")
	return BoxResult{}, errors.New("unreachable")
}

// Verifies 8879dc1597d6 / CC-F1 / CC-F2 (review §5.7): random DAGs run to
// settlement under a watchdog with the race detector — every node reaches a
// terminal state and the journal records exactly one result per node — then a
// resume of an all-ok run journals zero box launches (every node is a hit).
// Exercises the evDeferred/wakeParked window and the run-level lock's
// single-appender guarantee. Run with -race for the concurrency guarantees.
func TestScheduling_RandomDAGSettlesAndResumes(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed))
			n := 3 + rng.Intn(12)
			ir := randomDAG(rng, n)

			// A random subset fails: the failure-cascade settlement path. Every
			// node — failed, skipped, or ok — must still reach a terminal state.
			failing := map[string]bool{}
			hf := newHarness(t)
			hf.exec.Clock = realClock{}
			for i := range ir.Nodes {
				if rng.Intn(4) == 0 {
					failing[ir.Nodes[i].ID] = true
					hf.boxes.script(ir.Nodes[i].ID, failedResult("agent", "scripted"))
				}
			}
			runWithWatchdog(t, hf, ir, "cascade")
			rp, err := hf.store.Load("cascade")
			if err != nil {
				t.Fatal(err)
			}
			if got := len(rp.LastByStep); got != countAgents(ir) {
				t.Fatalf("cascade: journal has %d settled steps, want %d", got, countAgents(ir))
			}
			if rp.End == nil {
				t.Fatal("cascade: no run-end marker after settlement")
			}

			// An all-ok run of the same shape, then resume: every node is a
			// journal hit, so a box runner that fatals on launch is never
			// called, and the resume settles clean.
			ho := newHarness(t)
			ho.exec.Clock = realClock{}
			runWithWatchdog(t, ho, ir, "allok")
			ho.exec.Boxes = refusingBoxes{t}
			if err := ho.run(t, ir, config.RunOptions{Mode: "resume", RunID: "allok"}); err != nil {
				t.Fatalf("resume of an all-ok run must settle clean: %v", err)
			}
			rp2, err := ho.store.Load("allok")
			if err != nil {
				t.Fatal(err)
			}
			if len(rp2.LastByStep) != countAgents(ir) {
				t.Fatalf("resume changed the settled-step count: %d vs %d", len(rp2.LastByStep), countAgents(ir))
			}
		})
	}
}

// runWithWatchdog runs an IR to settlement or fails the test if it stalls.
func runWithWatchdog(t *testing.T, h *harness, ir *config.IR, runID string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.run(t, ir, config.RunOptions{RunID: runID}) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("run %q did not settle within the watchdog window (possible stall)", runID)
	}
}

// Verifies CC-F1 (review §5.7): the run-level lock is the single-appender
// guarantee — a resume attempted while the run is live is refused, so two
// journal appenders never coexist.
func TestScheduling_LiveRunRefusesConcurrentResume(t *testing.T) {
	h := newHarness(t)
	ir := testIR("w", []config.Node{agentNode("w/a", "out")}, nil)
	// Hold the run open by keeping its journal (via a slow box) in flight.
	release := make(chan struct{})
	h.boxes.deflt = func(box BoxAttempt) failure.Result {
		<-release
		return okPayload(map[string]any{"out": box.NodeID})
	}
	done := make(chan error, 1)
	go func() { done <- h.run(t, ir, config.RunOptions{RunID: "live"}) }()

	// Wait until the run has minted its journal (the lock is held).
	deadline := time.After(5 * time.Second)
	for h.store == nil || !h.store.RunLive("live") {
		select {
		case <-deadline:
			t.Fatal("run never became live")
		case <-time.After(2 * time.Millisecond):
		}
	}
	// A second executor resuming the live run must be refused.
	h2 := newHarness(t)
	h2.store = h.store
	h2.exec.Store = h.store
	err := h2.run(t, ir, config.RunOptions{Mode: "resume", RunID: "live"})
	if err == nil {
		t.Fatal("resume of a live run must be refused")
	}
	close(release)
	<-done
}
