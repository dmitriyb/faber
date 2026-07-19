package metering

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"
)

// Kind is an admission decision kind. Exactly three decisions cross to the
// scheduler; RateLimitDefer reuses Defer on the post-failure path so the
// scheduler has one re-queue mechanism, not two.
type Kind int

const (
	// Admit launches the step; its estimate is held as a reservation until
	// settlement.
	Admit Kind = iota
	// Defer re-queues the step (a waiting state, never terminal). A zero
	// Until means re-check on the next in-flight settlement rather than at a
	// wall-clock time.
	Defer
	// Reject is a structured step failure naming the exhausted budget unit;
	// fail-stop propagation marks dependents skipped as with any failure.
	Reject
)

// String implements fmt.Stringer.
func (k Kind) String() string {
	switch k {
	case Admit:
		return "admit"
	case Defer:
		return "defer"
	case Reject:
		return "reject"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// Decision is the admission verdict handed to the scheduler.
type Decision struct {
	Kind   Kind
	Until  time.Time // Defer only; zero => re-check on next settlement
	Budget Unit      // Reject only: the exhausted unit
	Detail string
}

// reservation is the in-flight state held between Admit and Settle.
type reservation struct {
	endpoint string
	held     map[Unit]int64
}

// Admitter keeps the run's budget ledger and wraps the meter hooks around
// every step: Admit at readiness, Settle at every attempt's completion. With
// no meters and no budgets it is the no-op path and allocates nothing per
// step. Admit and Settle serialize on one mutex so admission reads a
// consistent ledger and two concurrent ready steps cannot both reserve the
// last headroom.
type Admitter struct {
	mu       sync.Mutex
	meters   map[string][]Meter     // endpoint class -> meters
	limit    map[Unit]int64         // declared budgets, from --budget unit=n
	spent    map[Unit]int64         // settled actuals
	reserved map[Unit]int64         // in-flight estimates
	inflight map[string]reservation // node id -> reservation to release
	logger   *slog.Logger
}

// NewAdmitter builds the run's admitter from the per-class meter sets and the
// declared budgets. A budget whose unit no configured meter reports is
// announced as a startup warning ("budget declared, nothing measures it") and
// never blocks — a silently never-enforced budget is the failure mode that
// warning exists to surface. A nil logger discards.
func NewAdmitter(meters map[string][]Meter, budgets map[Unit]int64, logger *slog.Logger) *Admitter {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With("component", "metering.admitter")
	a := &Admitter{
		meters:   meters,
		limit:    make(map[Unit]int64, len(budgets)),
		spent:    map[Unit]int64{},
		reserved: map[Unit]int64{},
		inflight: map[string]reservation{},
		logger:   logger,
	}
	for u, n := range budgets {
		a.limit[u] = n
	}
	a.warnUnenforced()
	return a
}

// warnUnenforced logs, per declared budget unit no meter reports, the
// never-enforced warning. If any meter does not declare its units the
// coverage is unknown and no warning is emitted.
func (a *Admitter) warnUnenforced() {
	if len(a.limit) == 0 {
		return
	}
	reported := map[Unit]bool{}
	for _, ms := range a.meters {
		for _, m := range ms {
			ur, ok := m.(UnitReporter)
			if !ok {
				return // unknown coverage: cannot prove any budget unmeasured
			}
			for _, u := range ur.Units() {
				reported[u] = true
			}
		}
	}
	units := make([]string, 0, len(a.limit))
	for u := range a.limit {
		if !reported[u] {
			units = append(units, string(u))
		}
	}
	sort.Strings(units)
	for _, u := range units {
		a.logger.Warn("budget declared but no configured meter reports its unit; it will never be enforced", "unit", u)
	}
}

// ParseBudgets converts the CLI's --budget flag map (config.RunOptions.Budgets)
// into unit-tagged atomic amounts. Amounts must be non-negative integers in
// the unit's atomic granularity — faber never converts, so fractional budgets
// have no meaning here.
func ParseBudgets(flags map[string]float64) (map[Unit]int64, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[Unit]int64, len(flags))
	for k, v := range flags {
		if k == "" {
			return nil, fmt.Errorf("metering: budget unit must not be empty")
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v != math.Trunc(v) || v > math.MaxInt64 {
			return nil, fmt.Errorf("metering: budget %s=%v: amount must be a non-negative integer in the unit's atomic granularity", k, v)
		}
		out[Unit(k)] = int64(v)
	}
	return out, nil
}

// Admit gathers estimates from the step's endpoint-class meters and decides
// admission. The latest saturation hint wins as defer(until). Then per
// declared budget unit, with est summed over costs tagged that unit — a meter
// silent on a unit neither blocks nor consumes it: spent+est > limit means
// the step can never fit (budgets do not replenish within a run) => Reject;
// spent+reserved+est > limit => zero-Until Defer (re-check on next
// settlement); otherwise the estimate is held as a reservation and the step
// admits. An Estimate error from a budget-participating meter is returned as
// a structured admission failure — a hard bound that cannot be computed must
// not silently admit.
func (a *Admitter) Admit(ctx context.Context, s Step) (Decision, error) {
	ms := a.meters[s.Endpoint] // the meter map is immutable after construction
	if len(ms) == 0 {
		return Decision{Kind: Admit}, nil
	}

	// Gather estimates outside the ledger lock: meters shell out to opaque
	// user commands (tokenizers, probes), and one hung command must not block
	// every concurrent Admit and Settle. The ledger is read and mutated only
	// under the lock below, so admission still reads a consistent ledger.
	var deferUntil time.Time
	est := map[Unit]int64{}
	for _, m := range ms {
		e, err := m.Estimate(ctx, s)
		if err != nil {
			if a.participates(m) {
				a.releaseEstimates(ms, s.NodeID)
				return Decision{}, fmt.Errorf("metering: estimate %s (endpoint class %s): %w", s.NodeID, s.Endpoint, err)
			}
			a.logger.Warn("estimate failed for a meter outside every declared budget; continuing",
				"node", s.NodeID, "endpoint", s.Endpoint, "error", err)
			continue
		}
		if e.DeferUntil != nil && e.DeferUntil.After(deferUntil) {
			deferUntil = *e.DeferUntil
		}
		// Estimates are clamped non-negative and summed saturating: meters run
		// opaque user commands, and a negative or overflowing estimate must
		// never widen a budget or wrap the ledger comparisons below. Zero
		// claims stay — a zero-amount cost enrolls its unit in admission (the
		// reported tier's retrospective enforcement rides on that).
		for _, c := range e.Costs {
			if c.Amount < 0 {
				continue
			}
			est[c.Unit] = satAdd(est[c.Unit], c.Amount)
		}
	}

	if !deferUntil.IsZero() {
		a.releaseEstimates(ms, s.NodeID)
		return Decision{Kind: Defer, Until: deferUntil, Detail: "endpoint saturated"}, nil
	}

	units := sortedUnits(est)
	a.mu.Lock()
	for _, u := range units {
		limit, budgeted := a.limit[u]
		if !budgeted {
			continue
		}
		if satAdd(a.spent[u], est[u]) > limit {
			d := Decision{
				Kind:   Reject,
				Budget: u,
				Detail: fmt.Sprintf("budget %s exhausted: spent %d + estimate %d exceeds limit %d", u, a.spent[u], est[u], limit),
			}
			a.mu.Unlock()
			a.releaseEstimates(ms, s.NodeID)
			return d, nil
		}
	}
	for _, u := range units {
		limit, budgeted := a.limit[u]
		if !budgeted {
			continue
		}
		if satAdd(satAdd(a.spent[u], a.reserved[u]), est[u]) > limit {
			d := Decision{
				Kind:   Defer,
				Detail: fmt.Sprintf("budget %s contended: estimate %d does not fit alongside in-flight reservations %d", u, est[u], a.reserved[u]),
			}
			a.mu.Unlock()
			a.releaseEstimates(ms, s.NodeID)
			return d, nil
		}
	}

	if prev, ok := a.inflight[s.NodeID]; ok { // defensive: never double-reserve a node
		a.release(prev)
	}
	for u, n := range est {
		a.reserved[u] = satAdd(a.reserved[u], n)
	}
	a.inflight[s.NodeID] = reservation{endpoint: s.Endpoint, held: est}
	a.mu.Unlock()
	return Decision{Kind: Admit}, nil
}

// releaseEstimates tells every meter holding per-step estimate state that the
// step was not admitted, so the state is discarded instead of leaking or
// being charged later.
func (a *Admitter) releaseEstimates(ms []Meter, nodeID string) {
	for _, m := range ms {
		if r, ok := m.(EstimateReleaser); ok {
			r.ReleaseEstimate(nodeID)
		}
	}
}

// Settle releases the step's reservation, collects each meter's actuals, and
// folds them into spent. It runs for every attempt, any status, and always
// precedes rate-limit classification so a waiting step never holds budget
// headroom. The returned costs are the step's journal record material — the
// deferred aggregation seam's raw input; no rollup happens here. Accounting
// is best-effort: a meter whose Actual fails is logged and records nothing.
func (a *Admitter) Settle(ctx context.Context, r ResultView) ([]Cost, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	res, ok := a.inflight[r.NodeID]
	if !ok {
		return nil, nil
	}
	delete(a.inflight, r.NodeID)
	a.release(res)

	var actuals []Cost
	for _, m := range a.meters[res.endpoint] {
		costs, err := m.Actual(ctx, r)
		if err != nil {
			a.logger.Warn("actual accounting failed; recording nothing for this meter",
				"node", r.NodeID, "endpoint", res.endpoint, "error", err)
			continue
		}
		// Actuals derive from the box's usage sidecar — untrusted bytes. The
		// ledger clamps them non-negative and adds saturating so a hostile or
		// garbled sidecar can neither refund a budget nor wrap the ledger; the
		// journaled record keeps the clamped values the ledger actually used.
		for _, c := range costs {
			if c.Amount < 0 {
				a.logger.Warn("negative actual cost clamped to zero", "node", r.NodeID, "unit", c.Unit, "amount", c.Amount)
				c.Amount = 0
			}
			a.spent[c.Unit] = satAdd(a.spent[c.Unit], c.Amount)
			actuals = append(actuals, c)
		}
	}
	return actuals, nil
}

// release returns a held reservation to the ledger, clamping at zero — the
// floor pairs with the saturating add so the in-flight sum can never read
// negative and quietly widen admission.
func (a *Admitter) release(res reservation) {
	for u, n := range res.held {
		if a.reserved[u] < n {
			a.reserved[u] = 0
			continue
		}
		a.reserved[u] -= n
	}
}

// FoldSpent seeds the spent ledger from a resumed run's journaled actuals,
// so a declared budget bounds the whole logical run across interruptions
// rather than resetting at every resume. Amounts are clamped non-negative
// and added saturating: the journal is host-written but disk-resident, and
// the ledger never lets a stray record widen a budget.
func (a *Admitter) FoldSpent(costs []Cost) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range costs {
		if c.Amount <= 0 {
			continue
		}
		a.spent[c.Unit] = satAdd(a.spent[c.Unit], c.Amount)
	}
}

// satAdd adds non-negative b to non-negative a, saturating at MaxInt64.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// participates reports whether m protects any declared budget: an estimate
// failure from such a meter must fail admission, while one from a meter no
// budget depends on is only a warning. A meter that does not declare its
// units is treated as participating whenever any budget exists (fail safe).
func (a *Admitter) participates(m Meter) bool {
	if len(a.limit) == 0 {
		return false
	}
	ur, ok := m.(UnitReporter)
	if !ok {
		return true
	}
	for _, u := range ur.Units() {
		if _, budgeted := a.limit[u]; budgeted {
			return true
		}
	}
	return false
}

// sortedUnits returns the map's units in deterministic order so rejection
// among multiple exhausted budgets is stable.
func sortedUnits(m map[Unit]int64) []Unit {
	units := make([]Unit, 0, len(m))
	for u := range m {
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
	return units
}
