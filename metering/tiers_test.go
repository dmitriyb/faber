package metering

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Verifies e3dae1b52167: the exact tier streams the assembled prompt to the
// opaque tokenizer command and returns tokens + max output as a hard upper
// bound in the tier's unit.
func TestExactTierTokenizerBound(t *testing.T) {
	runner := &fakeRunner{fn: func(_ int, argv []string, _ string) ([]byte, error) {
		if argv[0] != "./hooks/tokenize" {
			t.Fatalf("tokenizer invoked as %v", argv)
		}
		return []byte(`{"tokens": 100}`), nil
	}}
	m := newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, nil, runner, discard())

	est, err := m.Estimate(context.Background(), Step{NodeID: "task/a", Prompt: TextPrompt("skill plus context")})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	wantCosts(t, est.Costs, Cost{Unit: "tokens", Amount: 150})
	if runner.stdins[0] != "skill plus context" {
		t.Fatalf("tokenizer stdin = %q, want the assembled prompt", runner.stdins[0])
	}
}

// Verifies 58879b841ed4 and a9a5faefadd6: the exact tier's Actual prefers the
// allowlisted usage fields when a sidecar is present — summing only fields
// explicitly mapped to the tier's unit, never coercing foreign-unit sidecar
// fields — and falls back to the estimated bound when none was deposited.
func TestExactTierActualPrefersAllowlistedUsageFallsBackToBound(t *testing.T) {
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 100}`), nil
	}}
	m := newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, []string{"input", "output"}, runner, discard())
	ctx := context.Background()

	if _, err := m.Estimate(ctx, Step{NodeID: "task/a", Prompt: TextPrompt("p")}); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	got, err := m.Actual(ctx, ResultView{NodeID: "task/a", Status: StatusOK,
		Usage: map[string]int64{"input": 10, "output": 5, "billed_cents": 999}})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 15}) // billed_cents is not allowlisted and never summed

	if _, err := m.Estimate(ctx, Step{NodeID: "task/b", Prompt: TextPrompt("p")}); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	got, err = m.Actual(ctx, ResultView{NodeID: "task/b", Status: StatusOK})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 150})
}

// Verifies a9a5faefadd6: with no fields allowlisted the exact tier ignores
// the usage sidecar entirely rather than blindly summing every numeric field
// into its unit, and settles the bound.
func TestExactTierWithoutFieldsIgnoresUsage(t *testing.T) {
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 100}`), nil
	}}
	m := newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, nil, runner, discard())
	ctx := context.Background()

	if _, err := m.Estimate(ctx, Step{NodeID: "task/a", Prompt: TextPrompt("p")}); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	got, err := m.Actual(ctx, ResultView{NodeID: "task/a", Status: StatusOK,
		Usage: map[string]int64{"billed_cents": 999}})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 150})
}

// Verifies 58879b841ed4: a failed attempt without usable usage records
// nothing — charging the full pessimistic bound per failed attempt would
// drain the non-replenishing budget the rate-limit defer floor exists to
// protect.
func TestExactTierFailedAttemptWithoutUsageRecordsNothing(t *testing.T) {
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"tokens": 100}`), nil
	}}
	m := newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, []string{"input"}, runner, discard())
	ctx := context.Background()

	if _, err := m.Estimate(ctx, Step{NodeID: "task/a", Prompt: TextPrompt("p")}); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	got, err := m.Actual(ctx, ResultView{NodeID: "task/a", Status: StatusFailed})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	if got != nil {
		t.Fatalf("failed attempt without usage settled %v, want nothing", got)
	}

	// A failed attempt WITH allowlisted usage still settles the real usage.
	if _, err := m.Estimate(ctx, Step{NodeID: "task/b", Prompt: TextPrompt("p")}); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	got, err = m.Actual(ctx, ResultView{NodeID: "task/b", Status: StatusFailed, Usage: map[string]int64{"input": 7}})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 7})
}

// Verifies e3dae1b52167: a failing or malformed tokenizer is an estimate
// error — never a silent admit past a hard-bound tier (edge case: tokenizer
// command fails mid-run).
func TestExactTierTokenizerFailureIsError(t *testing.T) {
	cases := map[string]func(int, []string, string) ([]byte, error){
		"command fails":  func(int, []string, string) ([]byte, error) { return nil, errors.New("exit status 1") },
		"malformed json": func(int, []string, string) ([]byte, error) { return []byte(`nonsense`), nil },
		"missing field":  func(int, []string, string) ([]byte, error) { return []byte(`{}`), nil },
		"negative count": func(int, []string, string) ([]byte, error) { return []byte(`{"tokens": -1}`), nil },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			m := newExactMeter("tokens", []string{"./hooks/tokenize"}, 50, nil, &fakeRunner{fn: fn}, discard())
			if _, err := m.Estimate(context.Background(), Step{NodeID: "task/a", Prompt: TextPrompt("p")}); err == nil {
				t.Fatal("Estimate succeeded, want error")
			}
		})
	}
}

// Verifies e3dae1b52167: the reported tier estimates a zero cost tagged with
// its unit — it cannot bound ahead of time, but the zero claim keeps it
// participating in its budget's admission.
func TestReportedTierZeroClaim(t *testing.T) {
	m := newReportedMeter("tokens", map[string]Unit{"input_tokens": "tokens"}, discard())
	est, err := m.Estimate(context.Background(), Step{NodeID: "task/a"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	wantCosts(t, est.Costs, Cost{Unit: "tokens", Amount: 0})
}

// Verifies a9a5faefadd6: the reported tier zero-claims every distinct unit it
// can report (the tier's unit plus all fields units — the same set Units
// advertises), so a budget in a secondary fields-unit is enforced by
// admission instead of being silently ignored.
func TestReportedTierZeroClaimCoversAllFieldUnits(t *testing.T) {
	m := newReportedMeter("tokens",
		map[string]Unit{"input_tokens": "tokens", "billed_cents": "usd-cents"}, discard())
	est, err := m.Estimate(context.Background(), Step{NodeID: "task/a"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	wantCosts(t, est.Costs, Cost{Unit: "tokens", Amount: 0}, Cost{Unit: "usd-cents", Amount: 0})
}

// Verifies e3dae1b52167: the reported tier maps the configured sidecar fields
// to unit-tagged costs.
func TestReportedTierActualMapsSidecarFields(t *testing.T) {
	m := newReportedMeter("tokens", map[string]Unit{"input_tokens": "tokens", "output_tokens": "tokens"}, discard())
	got, err := m.Actual(context.Background(), ResultView{
		NodeID: "task/a",
		Status: StatusOK,
		Usage:  map[string]int64{"input_tokens": 7, "output_tokens": 3, "unrelated": 99},
	})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 10})
}

// Verifies e3dae1b52167: a missing usage sidecar is a logged warning and zero
// cost — accounting is best-effort, never a step failure (edge case:
// malformed usage sidecar).
func TestReportedTierMissingSidecarWarnsZeroCost(t *testing.T) {
	logger, buf := testLogger()
	m := newReportedMeter("tokens", map[string]Unit{"input_tokens": "tokens"}, logger)
	got, err := m.Actual(context.Background(), ResultView{NodeID: "task/a", Status: StatusOK})
	if err != nil {
		t.Fatalf("Actual: %v", err)
	}
	wantCosts(t, got, Cost{Unit: "tokens", Amount: 0})
	if !strings.Contains(buf.String(), "no usage sidecar") {
		t.Fatalf("log %q lacks the missing-sidecar warning", buf.String())
	}
}

// Verifies e3dae1b52167: probe utilization at or above the threshold yields a
// saturation hint deferring until the probe's reset epoch; below the
// threshold the step admits with no cost claim.
func TestProbeTierSaturationHint(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	replies := []string{
		`{"utilization": 1.0, "reset": ` + strconv.FormatInt(reset, 10) + `}`,
		`{"utilization": 0.2}`,
	}
	runner := &fakeRunner{fn: func(call int, argv []string, _ string) ([]byte, error) {
		if argv[0] != "./hooks/probe" {
			t.Fatalf("probe invoked as %v", argv)
		}
		return []byte(replies[call]), nil
	}}
	m := newProbeMeter([]string{"./hooks/probe"}, 0.9, runner, discard())
	ctx := context.Background()

	est, err := m.Estimate(ctx, Step{NodeID: "task/a"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.DeferUntil == nil || !est.DeferUntil.Equal(time.Unix(reset, 0)) {
		t.Fatalf("saturated probe: DeferUntil = %v, want reset %v", est.DeferUntil, time.Unix(reset, 0))
	}

	est, err = m.Estimate(ctx, Step{NodeID: "task/a"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.DeferUntil != nil || len(est.Costs) != 0 {
		t.Fatalf("below-threshold probe: got %+v, want plain admit with no cost claim", est)
	}
}

// Verifies e3dae1b52167: a saturated probe reading with no reset epoch defers
// on a default horizon from now.
func TestProbeTierSaturationWithoutResetUsesDefault(t *testing.T) {
	runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
		return []byte(`{"utilization": 1.0}`), nil
	}}
	m := newProbeMeter([]string{"./hooks/probe"}, 0.9, runner, discard())
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	est, err := m.Estimate(context.Background(), Step{NodeID: "task/a"})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.DeferUntil == nil || !est.DeferUntil.Equal(now.Add(defaultProbeDefer)) {
		t.Fatalf("DeferUntil = %v, want now + %s", est.DeferUntil, defaultProbeDefer)
	}
}

// Verifies e3dae1b52167: a probe reset epoch in the past or beyond the sanity
// horizon is not trusted — a past reset would spin the probe hot, a
// far-future one would park the step for years — and the default defer is
// used instead, with the bogus epoch logged for diagnosis.
func TestProbeTierBogusResetUsesDefault(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]int64{
		"past reset":     now.Add(-time.Hour).Unix(),
		"reset at now":   now.Unix(),
		"beyond horizon": now.Add(probeResetHorizon + time.Hour).Unix(),
	}
	for name, reset := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{fn: func(int, []string, string) ([]byte, error) {
				return []byte(`{"utilization": 1.0, "reset": ` + strconv.FormatInt(reset, 10) + `}`), nil
			}}
			logger, buf := testLogger()
			m := newProbeMeter([]string{"./hooks/probe"}, 0.9, runner, logger)
			m.now = func() time.Time { return now }

			est, err := m.Estimate(context.Background(), Step{NodeID: "task/a"})
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if est.DeferUntil == nil || !est.DeferUntil.Equal(now.Add(defaultProbeDefer)) {
				t.Fatalf("DeferUntil = %v, want the default defer %v", est.DeferUntil, now.Add(defaultProbeDefer))
			}
			if !strings.Contains(buf.String(), "outside the trusted window") ||
				!strings.Contains(buf.String(), strconv.FormatInt(reset, 10)) {
				t.Fatalf("log %q lacks the bogus-epoch diagnosis", buf.String())
			}
		})
	}
}

// Verifies e3dae1b52167: a failing or malformed probe degrades to
// admit-with-warning — best-effort by contract, a broken probe must not
// wedge a run.
func TestProbeTierFailureAdmitsWithWarning(t *testing.T) {
	cases := map[string]func(int, []string, string) ([]byte, error){
		"nonzero exit": func(int, []string, string) ([]byte, error) { return nil, errors.New("exit status 3") },
		"malformed":    func(int, []string, string) ([]byte, error) { return []byte(`??`), nil },
		"no reading":   func(int, []string, string) ([]byte, error) { return []byte(`{}`), nil },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			logger, buf := testLogger()
			m := newProbeMeter([]string{"./hooks/probe"}, 0.9, &fakeRunner{fn: fn}, logger)
			est, err := m.Estimate(context.Background(), Step{NodeID: "task/a"})
			if err != nil {
				t.Fatalf("Estimate: %v (probe failures must not error)", err)
			}
			if est.DeferUntil != nil || len(est.Costs) != 0 {
				t.Fatalf("got %+v, want plain admit", est)
			}
			if !strings.Contains(buf.String(), "admitting") {
				t.Fatalf("log %q lacks the best-effort warning", buf.String())
			}
		})
	}
}

// Verifies e3dae1b52167: the production runner invokes the opaque command via
// os/exec with structured I/O — stdin in, captured stdout out — and surfaces
// failures as errors.
func TestExecRunnerStructuredIO(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), []string{"cat"}, strings.NewReader(`{"tokens": 7}`))
	if err != nil {
		t.Fatalf("Run(cat): %v", err)
	}
	if string(out) != `{"tokens": 7}` {
		t.Fatalf("stdout = %q, want the stdin echoed", out)
	}
	if _, err := (ExecRunner{}).Run(context.Background(), []string{"/nonexistent-command-for-test"}, nil); err == nil {
		t.Fatal("Run(nonexistent) succeeded, want error")
	}
	if _, err := (ExecRunner{}).Run(context.Background(), nil, nil); err == nil {
		t.Fatal("Run(empty argv) succeeded, want error")
	}
}

// Verifies e3dae1b52167 (F1-review): the meters sum usage saturating — a
// crafted multi-field usage cannot wrap the intermediate sum negative, so no
// negative cost is ever produced for the ledger to (also) clamp.
func TestMeterActualSaturates(t *testing.T) {
	log, _ := testLogger()
	huge := int64(1) << 62
	exact := newExactMeter("tokens", nil, 0, []string{"a", "b", "c"}, nil, log)
	costs, err := exact.Actual(context.Background(), ResultView{NodeID: "n", Status: StatusOK,
		Usage: map[string]int64{"a": huge, "b": huge, "c": huge}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range costs {
		if c.Amount < 0 {
			t.Fatalf("exact meter produced a negative saturating sum: %d", c.Amount)
		}
	}

	rep := newReportedMeter("tokens", map[string]Unit{"a": "tokens", "b": "tokens"}, log)
	costs, err = rep.Actual(context.Background(), ResultView{NodeID: "n",
		Usage: map[string]int64{"a": huge, "b": huge}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range costs {
		if c.Amount < 0 {
			t.Fatalf("reported meter produced a negative saturating sum: %d", c.Amount)
		}
	}
}
