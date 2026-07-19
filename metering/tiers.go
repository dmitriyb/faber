package metering

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProbeRunner invokes an opaque user command host-side with structured I/O:
// stdin in, decoded stdout out — never stdout-scraping of free-form text. It
// is the single subprocess seam for every tier (tokenizers and probes alike)
// and is fake-able in tests, matching the engine-wide os/exec discipline.
type ProbeRunner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)
}

// defaultProbeDefer is the saturation-hint horizon used when a saturated
// probe reading carries no reset epoch (or a bogus one).
const defaultProbeDefer = 60 * time.Second

// probeResetHorizon is the sanity bound on probe reset epochs, mirroring the
// rate-limit floor: a future reset inside the horizon is trusted verbatim,
// anything else is treated as absent.
const probeResetHorizon = 8 * 24 * time.Hour

// EstimateReleaser is an optional Meter extension for meters that hold
// per-step state between Estimate and Actual (the exact tier's bound). The
// Admitter calls it whenever an estimate does not lead to an admitted launch
// (reject, defer, or admission failure) so that state can neither leak nor go
// stale and be charged later.
type EstimateReleaser interface {
	ReleaseEstimate(nodeID string)
}

// exactMeter is the exact tier: a tokenizer-exact hard upper bound for
// endpoints the user controls. Estimate tokenizes the full assembled prompt
// with the endpoint's own tokenizer (an opaque user command emitting
// {"tokens": n}), assumes no cache reuse, fresh input, and maximum configured
// output — a deliberately pessimistic admission guarantee, not a prediction.
// faber ships no tokenizer and learns no model family.
type exactMeter struct {
	unit      Unit
	tokenizer []string
	maxOutput int64
	fields    []string // usage sidecar fields allowlisted into the tier's unit; empty => usage ignored
	runner    ProbeRunner
	logger    *slog.Logger

	mu     sync.Mutex
	bounds map[string]int64 // node id -> last estimated bound, for Actual fallback
}

func newExactMeter(unit Unit, tokenizer []string, maxOutput int64, fields []string, runner ProbeRunner, logger *slog.Logger) *exactMeter {
	return &exactMeter{
		unit:      unit,
		tokenizer: tokenizer,
		maxOutput: maxOutput,
		fields:    fields,
		runner:    runner,
		logger:    logger.With("component", "metering.exact"),
		bounds:    map[string]int64{},
	}
}

// Units implements UnitReporter.
func (m *exactMeter) Units() []Unit { return []Unit{m.unit} }

// Estimate implements Meter. A tokenizer failure is an error — a hard bound
// that cannot be computed must not silently admit.
func (m *exactMeter) Estimate(ctx context.Context, s Step) (Estimate, error) {
	var prompt io.Reader = strings.NewReader("")
	if s.Prompt != nil {
		r, err := s.Prompt.Prompt(ctx)
		if err != nil {
			return Estimate{}, fmt.Errorf("assemble prompt for %s: %w", s.NodeID, err)
		}
		prompt = r
	}
	out, err := m.runner.Run(ctx, m.tokenizer, prompt)
	if err != nil {
		return Estimate{}, fmt.Errorf("tokenizer: %w", err)
	}
	var reply struct {
		Tokens *int64 `json:"tokens"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return Estimate{}, fmt.Errorf("tokenizer: decode reply: %w", err)
	}
	if reply.Tokens == nil || *reply.Tokens < 0 {
		return Estimate{}, fmt.Errorf("tokenizer: reply carries no valid %q field", "tokens")
	}
	bound := *reply.Tokens + m.maxOutput

	m.mu.Lock()
	m.bounds[s.NodeID] = bound
	m.mu.Unlock()

	return Estimate{Costs: []Cost{{Unit: m.unit, Amount: bound}}}, nil
}

// Actual implements Meter: it prefers the endpoint's own usage when fields
// are allowlisted into the tier's unit and the sidecar is present — never a
// blind sum of every sidecar field, which would coerce foreign units. With no
// usable usage it falls back to the estimated bound, but only for successful
// attempts: a failed attempt (a rate-limited one especially) must not charge
// the full pessimistic bound into non-replenishing spent per attempt.
func (m *exactMeter) Actual(_ context.Context, r ResultView) ([]Cost, error) {
	m.mu.Lock()
	bound, haveBound := m.bounds[r.NodeID]
	delete(m.bounds, r.NodeID)
	m.mu.Unlock()

	if len(m.fields) > 0 && r.Usage != nil {
		var total int64
		for _, f := range m.fields {
			v, ok := r.Usage[f]
			if !ok {
				// A renamed vendor field would otherwise silently read as
				// zero and under-charge every step from here on.
				m.logger.Warn("configured usage field absent from the sidecar; it contributes zero",
					"node", r.NodeID, "field", f)
				continue
			}
			// Saturating, matching the ledger: usage is box-authored bytes
			// (already clamped >= 0 at read), and a multi-field sum must not
			// wrap negative before it reaches Settle.
			total = satAdd(total, v)
		}
		return []Cost{{Unit: m.unit, Amount: total}}, nil
	}
	if r.Status != StatusOK {
		m.logger.Debug("failed attempt without mapped usage; recording zero", "node", r.NodeID)
		return nil, nil
	}
	if !haveBound {
		m.logger.Warn("no mapped usage and no recorded bound; recording nothing", "node", r.NodeID)
		return nil, nil
	}
	return []Cost{{Unit: m.unit, Amount: bound}}, nil
}

// ReleaseEstimate implements EstimateReleaser: it discards the bound recorded
// by an estimate whose step was never admitted.
func (m *exactMeter) ReleaseEstimate(nodeID string) {
	m.mu.Lock()
	delete(m.bounds, nodeID)
	m.mu.Unlock()
}

// reportedMeter is the reported tier: vendor usage blocks read from responses.
// Estimate returns a zero cost tagged with the tier's unit — it cannot bound
// ahead of time, but the zero claim keeps the meter participating in its
// budget's admission, so an already-exhausted budget still rejects the step.
// All real enforcement is retrospective: budgets act as circuit breakers.
type reportedMeter struct {
	unit   Unit
	fields map[string]Unit // usage sidecar field -> unit
	logger *slog.Logger
}

func newReportedMeter(unit Unit, fields map[string]Unit, logger *slog.Logger) *reportedMeter {
	return &reportedMeter{unit: unit, fields: fields, logger: logger.With("component", "metering.reported")}
}

// Units implements UnitReporter.
func (m *reportedMeter) Units() []Unit {
	seen := map[Unit]bool{m.unit: true}
	units := []Unit{m.unit}
	for _, u := range m.fields {
		if !seen[u] {
			seen[u] = true
			units = append(units, u)
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
	return units
}

// Estimate implements Meter: it returns one zero-amount cost per distinct
// unit this meter can report (the tier's unit plus every fields unit — the
// same set Units advertises), so a budget in any of those units keeps
// participating in admission and an already-exhausted one still rejects.
func (m *reportedMeter) Estimate(context.Context, Step) (Estimate, error) {
	units := m.Units()
	costs := make([]Cost, 0, len(units))
	for _, u := range units {
		costs = append(costs, Cost{Unit: u, Amount: 0})
	}
	return Estimate{Costs: costs}, nil
}

// Actual implements Meter: it maps the configured sidecar fields to
// unit-tagged costs. A missing sidecar is a logged warning and zero cost —
// accounting is best-effort, never a step failure.
func (m *reportedMeter) Actual(_ context.Context, r ResultView) ([]Cost, error) {
	if r.Usage == nil {
		m.logger.Warn("no usage sidecar deposited; recording zero cost", "node", r.NodeID)
		return []Cost{{Unit: m.unit, Amount: 0}}, nil
	}
	byUnit := map[Unit]int64{}
	for field, unit := range m.fields {
		v, ok := r.Usage[field]
		if !ok {
			m.logger.Warn("configured usage field absent from the sidecar; it contributes zero",
				"node", r.NodeID, "field", field)
			if _, seen := byUnit[unit]; !seen {
				byUnit[unit] = 0 // enroll the unit so a zero cost is still reported
			}
			continue
		}
		byUnit[unit] = satAdd(byUnit[unit], v)
	}
	costs := make([]Cost, 0, len(byUnit))
	for _, u := range sortedUnits(byUnit) {
		costs = append(costs, Cost{Unit: u, Amount: byUnit[u]})
	}
	return costs, nil
}

// probeMeter is the probe tier: subscription endpoints with no usage API. The
// user supplies an opaque command run host-side before admission, emitting a
// saturation reading {"utilization": 0..1, "reset": epoch?}. That command is
// user policy shipped in the user's config layer — faber ships no probe and
// pins no vendor headers — and every reading is best-effort: a failing or
// malformed probe degrades to admit-with-warning, because a broken probe must
// not wedge a run the rate-limit floor can still protect.
type probeMeter struct {
	command   []string
	threshold float64
	runner    ProbeRunner
	logger    *slog.Logger
	now       func() time.Time
}

func newProbeMeter(command []string, threshold float64, runner ProbeRunner, logger *slog.Logger) *probeMeter {
	return &probeMeter{
		command:   command,
		threshold: threshold,
		runner:    runner,
		logger:    logger.With("component", "metering.probe"),
		now:       time.Now,
	}
}

// Units implements UnitReporter: the probe tier makes no cost claims.
func (m *probeMeter) Units() []Unit { return nil }

// Estimate implements Meter: utilization at or above the threshold yields a
// saturation hint (defer until reset, or a default horizon when the reading
// carries none); below, the step admits with no cost claim.
func (m *probeMeter) Estimate(ctx context.Context, s Step) (Estimate, error) {
	out, err := m.runner.Run(ctx, m.command, nil)
	if err != nil {
		m.logger.Warn("probe failed; admitting (best-effort by contract)", "node", s.NodeID, "error", err)
		return Estimate{}, nil
	}
	var reading struct {
		Utilization *float64 `json:"utilization"`
		Reset       int64    `json:"reset"`
	}
	if err := json.Unmarshal(out, &reading); err != nil || reading.Utilization == nil {
		m.logger.Warn("probe reply malformed; admitting (best-effort by contract)", "node", s.NodeID, "error", err)
		return Estimate{}, nil
	}
	if *reading.Utilization < m.threshold {
		return Estimate{}, nil
	}
	// A future reset inside the sanity horizon is trusted verbatim; a past or
	// far-future epoch is bogus — a past one would spin the probe hot, a
	// far-future one would park the step for years.
	now := m.now()
	until := now.Add(defaultProbeDefer)
	if reading.Reset > 0 {
		reset := time.Unix(reading.Reset, 0)
		if reset.After(now) && !reset.After(now.Add(probeResetHorizon)) {
			until = reset
		} else {
			m.logger.Warn("probe reset epoch outside the trusted window; using the default defer",
				"node", s.NodeID, "reset", reading.Reset, "horizon", probeResetHorizon)
		}
	}
	return Estimate{DeferUntil: &until}, nil
}

// Actual implements Meter: the probe tier accounts nothing.
func (m *probeMeter) Actual(context.Context, ResultView) ([]Cost, error) {
	return nil, nil
}
