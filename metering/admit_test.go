package metering

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Verifies 58879b841ed4: with no meters and no budgets (the default no-op
// meter set) every admission is admit and every settlement records nothing —
// scenario 1's first half.
func TestNoopDefaultAdmitsEverything(t *testing.T) {
	a := NewAdmitter(nil, nil, nil)
	for _, node := range []string{"task/a", "task/b", "task/c"} {
		d, err := a.Admit(context.Background(), Step{NodeID: node, Endpoint: ""})
		if err != nil {
			t.Fatalf("Admit(%s): %v", node, err)
		}
		if d.Kind != Admit {
			t.Fatalf("Admit(%s) = %s, want admit", node, d.Kind)
		}
		costs, err := a.Settle(context.Background(), ResultView{NodeID: node, Status: StatusOK})
		if err != nil {
			t.Fatalf("Settle(%s): %v", node, err)
		}
		if costs != nil {
			t.Fatalf("Settle(%s) recorded %v, want nothing", node, costs)
		}
	}
}

// Verifies 58879b841ed4: an estimate that can never fit (spent + estimate >
// limit; budgets do not replenish within a run) rejects with a structured
// decision naming the exhausted unit.
func TestAdmitRejectsWhenEstimateCanNeverFit(t *testing.T) {
	m := fixedMeter([]Unit{"tokens"}, []Cost{{Unit: "tokens", Amount: 60}}, nil)
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"tokens": 50}, nil)

	d, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if d.Kind != Reject || d.Budget != "tokens" {
		t.Fatalf("got %+v, want reject(tokens)", d)
	}
}

// Verifies 58879b841ed4: scenario 3 — a step that fits alone but not
// alongside in-flight reservations gets a zero-until defer, and the actual
// (well under the bound) frees headroom so the deferred step re-admits on the
// settlement re-check. Reserved never exceeds the limit; spent reflects
// actuals, not estimates.
func TestReservationContentionDefersAndSettlementFrees(t *testing.T) {
	m := &stubMeter{
		units:      []Unit{"tokens"},
		estimateFn: func(Step) (Estimate, error) { return Estimate{Costs: []Cost{{Unit: "tokens", Amount: 60}}}, nil },
		actualFn:   func(ResultView) ([]Cost, error) { return []Cost{{Unit: "tokens", Amount: 30}}, nil },
	}
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"tokens": 100}, nil)
	ctx := context.Background()

	first, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil || first.Kind != Admit {
		t.Fatalf("first Admit = %+v, %v; want admit", first, err)
	}
	if a.reserved["tokens"] != 60 || a.reserved["tokens"] > a.limit["tokens"] {
		t.Fatalf("reserved = %d, want 60 and never above limit %d", a.reserved["tokens"], a.limit["tokens"])
	}

	second, err := a.Admit(ctx, Step{NodeID: "task/b", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("second Admit: %v", err)
	}
	if second.Kind != Defer || !second.Until.IsZero() {
		t.Fatalf("second Admit = %+v, want zero-until defer (re-check on next settlement)", second)
	}

	costs, err := a.Settle(ctx, ResultView{NodeID: "task/a", Status: StatusOK})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	wantCosts(t, costs, Cost{Unit: "tokens", Amount: 30})
	if a.spent["tokens"] != 30 {
		t.Fatalf("spent = %d, want the actual 30, not the estimate 60", a.spent["tokens"])
	}
	if a.reserved["tokens"] != 0 {
		t.Fatalf("reserved = %d after settlement, want 0", a.reserved["tokens"])
	}

	retry, err := a.Admit(ctx, Step{NodeID: "task/b", Endpoint: "endpoint-a"})
	if err != nil || retry.Kind != Admit {
		t.Fatalf("re-admission after settlement = %+v, %v; want admit", retry, err)
	}
}

// Verifies a9a5faefadd6: scenario 5 — a budget in a unit no configured meter
// reports is announced as a startup warning and never blocks a step.
func TestBudgetWithoutMeterWarnsAndNeverBlocks(t *testing.T) {
	logger, buf := testLogger()
	m := fixedMeter([]Unit{"tokens"}, []Cost{{Unit: "tokens", Amount: 5}}, nil)
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"usd-cents": 0, "tokens": 100}, logger)

	if !strings.Contains(buf.String(), "never be enforced") || !strings.Contains(buf.String(), "usd-cents") {
		t.Fatalf("startup log %q lacks the never-enforced warning for usd-cents", buf.String())
	}
	if strings.Contains(buf.String(), "unit=tokens") {
		t.Fatalf("startup log %q warns about tokens, which a meter does report", buf.String())
	}

	for _, node := range []string{"task/a", "task/b"} {
		d, err := a.Admit(context.Background(), Step{NodeID: node, Endpoint: "endpoint-a"})
		if err != nil || d.Kind != Admit {
			t.Fatalf("Admit(%s) = %+v, %v; a zero usd-cents budget nothing measures must never block", node, d, err)
		}
		if _, err := a.Settle(context.Background(), ResultView{NodeID: node, Status: StatusOK}); err != nil {
			t.Fatalf("Settle(%s): %v", node, err)
		}
	}
}

// Verifies a9a5faefadd6: two budgets in two units, one step estimated in
// both — rejection on either unit rejects the step and the decision names the
// exhausted unit only.
func TestRejectNamesExhaustedUnitOnly(t *testing.T) {
	m := fixedMeter([]Unit{"tokens", "gpu-seconds"},
		[]Cost{{Unit: "tokens", Amount: 10}, {Unit: "gpu-seconds", Amount: 500}}, nil)
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}},
		map[Unit]int64{"tokens": 1000, "gpu-seconds": 100}, nil)

	d, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if d.Kind != Reject || d.Budget != "gpu-seconds" {
		t.Fatalf("got %+v, want reject(gpu-seconds)", d)
	}
	if strings.Contains(d.Detail, "tokens") {
		t.Fatalf("detail %q names a unit other than the exhausted one", d.Detail)
	}
}

// Verifies 58879b841ed4: an Estimate error from a budget-participating meter
// is a structured admission failure — a hard bound that cannot be computed
// must not silently admit.
func TestEstimateErrorFailsAdmission(t *testing.T) {
	m := &stubMeter{
		units:      []Unit{"tokens"},
		estimateFn: func(Step) (Estimate, error) { return Estimate{}, errors.New("boom") },
	}
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"tokens": 100}, nil)

	if _, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"}); err == nil {
		t.Fatal("Admit succeeded past a failing budget-participating meter, want error")
	}
}

// Verifies 58879b841ed4: an Estimate error from a meter outside every
// declared budget is a warning, not an admission failure.
func TestEstimateErrorOutsideBudgetsWarnsAndAdmits(t *testing.T) {
	logger, buf := testLogger()
	m := &stubMeter{
		units:      []Unit{"tokens"},
		estimateFn: func(Step) (Estimate, error) { return Estimate{}, errors.New("boom") },
	}
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, nil, logger)

	d, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil || d.Kind != Admit {
		t.Fatalf("Admit = %+v, %v; want admit with a warning", d, err)
	}
	if !strings.Contains(buf.String(), "estimate failed") {
		t.Fatalf("log %q lacks the estimate-failure warning", buf.String())
	}
}

// Verifies 58879b841ed4: any saturation hint defers, and the latest hint
// wins.
func TestSaturationHintDefersLatestWins(t *testing.T) {
	early := time.Now().Add(1 * time.Minute).Truncate(time.Second)
	late := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	hint := func(at time.Time) *stubMeter {
		return &stubMeter{estimateFn: func(Step) (Estimate, error) { return Estimate{DeferUntil: &at}, nil }}
	}
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {hint(late), hint(early)}}, nil, nil)

	d, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if d.Kind != Defer || !d.Until.Equal(late) {
		t.Fatalf("got %+v, want defer until %s (latest hint)", d, late)
	}
}

// Verifies a9a5faefadd6: a budget declared in a reported meter's secondary
// fields-unit participates in admission via the meter's zero claims — it is
// enforced once actuals exhaust it, and it is not announced as never
// enforced.
func TestSecondaryFieldsUnitBudgetEnforced(t *testing.T) {
	logger, buf := testLogger()
	m := newReportedMeter("tokens",
		map[string]Unit{"input_tokens": "tokens", "billed_cents": "usd-cents"}, logger)
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"usd-cents": 10}, logger)
	ctx := context.Background()

	if strings.Contains(buf.String(), "never be enforced") {
		t.Fatalf("startup log %q warns about a unit the meter's fields do report", buf.String())
	}

	d, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a"})
	if err != nil || d.Kind != Admit {
		t.Fatalf("first Admit = %+v, %v; want admit on the zero claims", d, err)
	}
	costs, err := a.Settle(ctx, ResultView{NodeID: "task/a", Status: StatusOK,
		Usage: map[string]int64{"input_tokens": 3, "billed_cents": 20}})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	wantCosts(t, costs, Cost{Unit: "tokens", Amount: 3}, Cost{Unit: "usd-cents", Amount: 20})

	d, err = a.Admit(ctx, Step{NodeID: "task/b", Endpoint: "endpoint-a"})
	if err != nil {
		t.Fatalf("second Admit: %v", err)
	}
	if d.Kind != Reject || d.Budget != "usd-cents" {
		t.Fatalf("got %+v, want reject(usd-cents): the secondary-unit budget must be enforced", d)
	}
}

// Verifies 58879b841ed4: estimates are gathered outside the ledger lock — a
// slow opaque user command in one endpoint's Estimate must not serialize
// concurrent admission elsewhere. Two cross-waiting estimates deadlock if
// Admit holds the ledger lock while estimating; with the lock scoped to the
// ledger checks they both complete.
func TestAdmitGathersEstimatesOutsideLedgerLock(t *testing.T) {
	aStarted := make(chan struct{})
	bStarted := make(chan struct{})
	meterA := &stubMeter{estimateFn: func(Step) (Estimate, error) {
		close(aStarted)
		<-bStarted
		return Estimate{}, nil
	}}
	meterB := &stubMeter{estimateFn: func(Step) (Estimate, error) {
		close(bStarted)
		<-aStarted
		return Estimate{}, nil
	}}
	a := NewAdmitter(map[string][]Meter{"endpoint-a": {meterA}, "endpoint-b": {meterB}}, nil, nil)

	done := make(chan error, 2)
	go func() {
		_, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "endpoint-a"})
		done <- err
	}()
	go func() {
		_, err := a.Admit(context.Background(), Step{NodeID: "task/b", Endpoint: "endpoint-b"})
		done <- err
	}()
	timeout := time.After(10 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
		case <-timeout:
			t.Fatal("concurrent Admit calls deadlocked: estimates are gathered under the ledger lock")
		}
	}
}

// boundsLen reads an exact meter's retained per-step bound count.
func boundsLen(m *exactMeter) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bounds)
}

// Verifies 58879b841ed4: every non-admit outcome (reject, contention defer,
// saturation defer, admission failure) releases the per-step estimate state
// meters hold, so the exact tier's bound neither leaks for steps that never
// settle nor goes stale to be charged to a later attempt.
func TestNonAdmitDecisionsReleaseEstimateState(t *testing.T) {
	tokenizer := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 100}`), nil
	}}
	newExact := func() *exactMeter {
		return newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, nil, tokenizer, discard())
	}
	ctx := context.Background()

	t.Run("reject releases", func(t *testing.T) {
		m := newExact()
		a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"tokens": 100}, nil)
		d, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a", Prompt: TextPrompt("p")})
		if err != nil || d.Kind != Reject {
			t.Fatalf("Admit = %+v, %v; want reject", d, err)
		}
		if n := boundsLen(m); n != 0 {
			t.Fatalf("rejected step left %d retained bound(s)", n)
		}
	})

	t.Run("contention defer releases only the deferred step", func(t *testing.T) {
		m := newExact()
		a := NewAdmitter(map[string][]Meter{"endpoint-a": {m}}, map[Unit]int64{"tokens": 200}, nil)
		if d, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a", Prompt: TextPrompt("p")}); err != nil || d.Kind != Admit {
			t.Fatalf("first Admit = %+v, %v", d, err)
		}
		d, err := a.Admit(ctx, Step{NodeID: "task/b", Endpoint: "endpoint-a", Prompt: TextPrompt("p")})
		if err != nil || d.Kind != Defer {
			t.Fatalf("second Admit = %+v, %v; want contention defer", d, err)
		}
		if n := boundsLen(m); n != 1 {
			t.Fatalf("retained bounds = %d, want exactly the admitted step's", n)
		}
	})

	t.Run("saturation defer releases", func(t *testing.T) {
		m := newExact()
		probe := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
			return []byte(`{"utilization": 1.0}`), nil
		}}
		p := newProbeMeter([]string{"./hooks/probe"}, 0.9, probe, discard())
		a := NewAdmitter(map[string][]Meter{"endpoint-a": {m, p}}, nil, nil)
		d, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a", Prompt: TextPrompt("p")})
		if err != nil || d.Kind != Defer {
			t.Fatalf("Admit = %+v, %v; want saturation defer", d, err)
		}
		if n := boundsLen(m); n != 0 {
			t.Fatalf("saturation-deferred step left %d retained bound(s)", n)
		}
	})

	t.Run("admission failure releases", func(t *testing.T) {
		m := newExact()
		broken := &stubMeter{
			units:      []Unit{"tokens"},
			estimateFn: func(Step) (Estimate, error) { return Estimate{}, errors.New("boom") },
		}
		a := NewAdmitter(map[string][]Meter{"endpoint-a": {m, broken}}, map[Unit]int64{"tokens": 100000}, nil)
		if _, err := a.Admit(ctx, Step{NodeID: "task/a", Endpoint: "endpoint-a", Prompt: TextPrompt("p")}); err == nil {
			t.Fatal("Admit succeeded past a failing participating meter")
		}
		if n := boundsLen(m); n != 0 {
			t.Fatalf("failed admission left %d retained bound(s)", n)
		}
	})
}

// Verifies a9a5faefadd6: budgets arrive from the CLI as float flags and must
// be non-negative integers in the unit's atomic granularity.
func TestParseBudgets(t *testing.T) {
	got, err := ParseBudgets(map[string]float64{"tokens": 50000, "usd-cents": 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["tokens"] != 50000 || got["usd-cents"] != 0 {
		t.Fatalf("got %v, want tokens=50000 usd-cents=0", got)
	}
	if got, _ := ParseBudgets(nil); got != nil {
		t.Fatalf("ParseBudgets(nil) = %v, want nil", got)
	}
	for name, flags := range map[string]map[string]float64{
		"fractional": {"tokens": 1.5},
		"negative":   {"tokens": -1},
		"empty unit": {"": 5},
	} {
		if _, err := ParseBudgets(flags); err == nil {
			t.Fatalf("%s: ParseBudgets(%v) succeeded, want error", name, flags)
		}
	}
}

// Verifies 58879b841ed4 / bff0f92afc29's fold: FoldSpent seeds the spent
// ledger from a resumed run's journaled actuals, so a budget bounds the whole
// logical run — an estimate that fit before the fold rejects after it.
// Negative amounts are clamped away and additions saturate: disk-resident
// records never widen a budget or wrap the ledger.
func TestFoldSpentSeedsLedgerClampedAndSaturating(t *testing.T) {
	m := fixedMeter([]Unit{"tokens"}, []Cost{{Unit: "tokens", Amount: 30}}, nil)
	a := NewAdmitter(map[string][]Meter{"ep": {m}}, map[Unit]int64{"tokens": 50}, nil)

	a.FoldSpent([]Cost{
		{Unit: "tokens", Amount: 25},
		{Unit: "tokens", Amount: -100}, // clamped: never widens the budget
	})
	d, err := a.Admit(context.Background(), Step{NodeID: "task/a", Endpoint: "ep"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if d.Kind != Reject || d.Budget != "tokens" {
		t.Fatalf("got %+v, want reject(tokens): 25 folded + 30 estimated > 50", d)
	}

	// Saturation: two near-max folds must not wrap negative (which would
	// reopen the budget); the ledger pins at the maximum instead.
	b := NewAdmitter(map[string][]Meter{"ep": {m}}, map[Unit]int64{"tokens": 50}, nil)
	huge := int64(1) << 62
	b.FoldSpent([]Cost{{Unit: "tokens", Amount: huge}, {Unit: "tokens", Amount: huge}, {Unit: "tokens", Amount: huge}})
	d, err = b.Admit(context.Background(), Step{NodeID: "task/b", Endpoint: "ep"})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if d.Kind != Reject {
		t.Fatalf("saturated ledger must still reject, got %+v", d)
	}
}
