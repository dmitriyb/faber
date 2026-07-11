package metering

// Module-level scenarios from the metering test section: the admission
// ledger, the tier implementations, and the reactive floor exercised together
// against a fake scheduler loop, a fake ProbeRunner, and a fake journal.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// journalEvent is the fake journal's per-attempt cost record — the deferred
// aggregation seam's raw material.
type journalEvent struct {
	kind  string // "attempt"
	node  string
	costs []Cost
}

// Verifies 06e9d770841f: scenarios 1 and 7 — with the empty meter set and no
// budgets, a rate-limit failure carrying a reset epoch still converts to
// defer(until reset) instead of flowing to failure policy, and the module
// supplies everything the journal defer event needs. The executor-contract
// halves of scenario 7 (Settle always before Classify, journal defer event
// then on_failure cleanup then re-queue, retry budget untouched) are the
// pipeline module's obligations, asserted by its integration tests —
// asserting them here would only re-read values this test assigned itself.
func TestScenarioNoopDefaultRateLimitFloor(t *testing.T) {
	ctx := context.Background()
	admitter := NewAdmitter(nil, nil, nil) // no metering file, no --budget
	rld := NewRateLimitDefer(DeferPolicy{}, nil)
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	node := "task/a"

	// Ready: admission is admit and allocates nothing.
	dec, err := admitter.Admit(ctx, Step{NodeID: node})
	if err != nil || dec.Kind != Admit {
		t.Fatalf("Admit = %+v, %v; want admit under the no-op default", dec, err)
	}

	// The attempt fails rate-limited. Settlement records nothing under the
	// no-op default.
	costs, err := admitter.Settle(ctx, ResultView{NodeID: node, Status: StatusFailed})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if costs != nil {
		t.Fatalf("no-op settlement recorded %v, want nothing", costs)
	}

	// Classification converts with zero configuration and yields the journal
	// defer event's content: the same Defer vocabulary admission uses, the
	// trusted reset epoch as earliest-start, and the consecutive count.
	dec, converted := rld.Classify(rateLimitRecord(node, reset.Unix()), time.Now())
	if !converted {
		t.Fatal("rate-limit failure did not convert with zero configuration")
	}
	if dec.Kind != Defer || !dec.Until.Equal(reset) {
		t.Fatalf("got %+v, want defer with earliest-start = the reset epoch %s", dec, reset)
	}
	if rld.Consecutive(node) != 1 {
		t.Fatalf("consecutive = %d, want 1 for the journal defer event", rld.Consecutive(node))
	}

	// The re-attempt succeeds: fresh full attempt, counter resets.
	if dec, err := admitter.Admit(ctx, Step{NodeID: node}); err != nil || dec.Kind != Admit {
		t.Fatalf("re-attempt Admit = %+v, %v", dec, err)
	}
	if _, err := admitter.Settle(ctx, ResultView{NodeID: node, Status: StatusOK}); err != nil {
		t.Fatalf("re-attempt Settle: %v", err)
	}
	rld.Reset(node)
	if rld.Consecutive(node) != 0 {
		t.Fatalf("consecutive after success = %d, want 0", rld.Consecutive(node))
	}
}

// Verifies e3dae1b52167: scenario 2 — under an exact-tier class, a tokenizer
// bound above the remaining headroom rejects the step, naming the unit.
// Also verifies 58879b841ed4: the reject decision is the admission hook's
// structured outcome. Scenario 2's downstream halves — the reject settling as
// a structured step failure and dependents ending skipped-by-dependency —
// are fail-stop propagation, owned and asserted by the pipeline/failure
// modules' integration tests.
func TestScenarioExactBoundReject(t *testing.T) {
	cfg := load(t, "meters_exact.yaml", testTemplates)
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 60000}`), nil // bound 60000+8192 > 50000
	}}
	meters := BuildMeters(cfg, runner, nil)
	admitter := NewAdmitter(meters, map[Unit]int64{"tokens": 50000}, nil)

	dec, err := admitter.Admit(context.Background(), Step{
		NodeID: "task/a", Template: "step-a", Endpoint: "endpoint-a", Prompt: TextPrompt("prompt"),
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if dec.Kind != Reject || dec.Budget != "tokens" {
		t.Fatalf("got %+v, want reject(tokens)", dec)
	}
}

// Verifies e3dae1b52167: scenario 4 — the reported tier is retrospective:
// every step admits on its zero claim, settlement folds the usage sidecar
// into spent, and the step after the budget is crossed rejects — the
// circuit-breaker behavior, exhaustion discovered from actuals.
func TestScenarioReportedTierRetrospective(t *testing.T) {
	cfg := load(t, "meters_reported.yaml", testTemplates)
	meters := BuildMeters(cfg, nil, nil)
	admitter := NewAdmitter(meters, map[Unit]int64{"tokens": 100}, nil)
	ctx := context.Background()

	for i, node := range []string{"task/a", "task/b"} {
		dec, err := admitter.Admit(ctx, Step{NodeID: node, Endpoint: "endpoint-a"})
		if err != nil || dec.Kind != Admit {
			t.Fatalf("step %d Admit = %+v, %v; want admit on the zero claim", i+1, dec, err)
		}
		costs, err := admitter.Settle(ctx, ResultView{
			NodeID: node,
			Status: StatusOK,
			Usage:  map[string]int64{"input_tokens": 40, "output_tokens": 20},
		})
		if err != nil {
			t.Fatalf("step %d Settle: %v", i+1, err)
		}
		wantCosts(t, costs, Cost{Unit: "tokens", Amount: 60})
	}

	// spent = 120 > 100: the third step's zero claim now rejects.
	dec, err := admitter.Admit(ctx, Step{NodeID: "task/c", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("third Admit: %v", err)
	}
	if dec.Kind != Reject || dec.Budget != "tokens" {
		t.Fatalf("third step got %+v, want reject(tokens) once spent crossed the limit", dec)
	}
}

// Verifies e3dae1b52167: scenario 6 — a saturated probe defers admission
// until the probe's reset; once the reading drops below the threshold the
// step admits; a failing probe yields admit plus a warning, best-effort by
// contract.
func TestScenarioProbeSaturationThenAdmit(t *testing.T) {
	cfg := load(t, "meters_probe.yaml", testTemplates)
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	replies := []string{
		`{"utilization": 1.0, "reset": ` + strconv.FormatInt(reset.Unix(), 10) + `}`,
		`{"utilization": 0.2}`,
	}
	runner := &fakeRunner{fn: func(call int, _ []string, _ string) ([]byte, error) {
		if call < len(replies) {
			return []byte(replies[call]), nil
		}
		return nil, &exitError{}
	}}
	logger, buf := testLogger()
	admitter := NewAdmitter(BuildMeters(cfg, runner, logger), nil, logger)
	ctx := context.Background()
	step := Step{NodeID: "task/a", Endpoint: "endpoint-a"}

	dec, err := admitter.Admit(ctx, step)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if dec.Kind != Defer || !dec.Until.Equal(reset) {
		t.Fatalf("saturated: got %+v, want defer until %s", dec, reset)
	}

	dec, err = admitter.Admit(ctx, step)
	if err != nil || dec.Kind != Admit {
		t.Fatalf("after saturation dropped: got %+v, %v; want admit", dec, err)
	}
	if _, err := admitter.Settle(ctx, ResultView{NodeID: "task/a", Status: StatusOK}); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	// A probe that exits nonzero yields admit plus a warning.
	dec, err = admitter.Admit(ctx, Step{NodeID: "task/b", Endpoint: "endpoint-a"})
	if err != nil || dec.Kind != Admit {
		t.Fatalf("broken probe: got %+v, %v; want admit", dec, err)
	}
	if !strings.Contains(buf.String(), "admitting") {
		t.Fatalf("log %q lacks the best-effort probe warning", buf.String())
	}
}

type exitError struct{}

func (*exitError) Error() string { return "exit status 3" }

// Verifies 137f66b2ef3c: scenario 9 — the first pass of the deferred
// run-cost-aggregation requirement: after a multi-step run every completed
// step's journal record carries its unit-tagged actuals, and no run-level
// total exists anywhere; aggregation later needs no new data, only a fold.
func TestScenarioJournaledActualsWithoutAggregation(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("testdata", "meters_multi.yaml"), testTemplates)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 10}`), nil
	}}
	admitter := NewAdmitter(BuildMeters(cfg, runner, nil), nil, nil)
	ctx := context.Background()

	var journal []journalEvent
	steps := []struct {
		node, template string
		usage          map[string]int64
		want           []Cost
	}{
		{"task/a", "step-a", map[string]int64{"total": 25}, []Cost{{Unit: "tokens", Amount: 25}}},
		{"task/b", "step-b", map[string]int64{"billed_cents": 7}, []Cost{{Unit: "usd-cents", Amount: 7}}},
		{"task/c", "step-c", map[string]int64{"billed_cents": 9}, []Cost{{Unit: "usd-cents", Amount: 9}}},
	}
	for _, s := range steps {
		endpoint := cfg.ClassForTemplate(s.template)
		dec, err := admitter.Admit(ctx, Step{NodeID: s.node, Template: s.template, Endpoint: endpoint, Prompt: TextPrompt("p")})
		if err != nil || dec.Kind != Admit {
			t.Fatalf("%s Admit = %+v, %v", s.node, dec, err)
		}
		costs, err := admitter.Settle(ctx, ResultView{NodeID: s.node, Status: StatusOK, Usage: s.usage})
		if err != nil {
			t.Fatalf("%s Settle: %v", s.node, err)
		}
		journal = append(journal, journalEvent{kind: "attempt", node: s.node, costs: costs})
	}

	if len(journal) != len(steps) {
		t.Fatalf("journal has %d records, want exactly one per step and nothing else (no rollup record)", len(journal))
	}
	for i, s := range steps {
		if journal[i].node != s.node {
			t.Fatalf("journal[%d] is for %q, want %q", i, journal[i].node, s.node)
		}
		wantCosts(t, journal[i].costs, s.want...)
	}
}
