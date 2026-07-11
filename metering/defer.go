package metering

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ReasonRateLimit is the reserved error.reason value of the box contract: a
// failure record carrying it converts into a defer decision instead of
// flowing to failure policy. The box owns extracting the signal from its
// endpoint (exit-code classification plus headers or body); this component
// owns only the record shape.
const ReasonRateLimit = "rate-limit"

// ResetInfo is the machine-readable payload the box writes into error.detail
// for a rate-limit failure: the endpoint's reset time as a unix epoch. It is
// a one-field contract that any endpoint's notion of "try later" compresses
// into — exit codes cannot carry a timestamp and stdout scraping is banned
// engine-wide.
type ResetInfo struct {
	Reset int64 `json:"reset"` // unix epoch seconds; 0 => unknown
}

// FailureRecord is this component's view over a failed attempt's structured
// result record. The executor adapts its own record type (the failure
// module's) to this shape; metering depends only on config.
type FailureRecord struct {
	NodeID string
	Status string          // StatusOK | StatusFailed
	Reason string          // error.reason
	Detail json.RawMessage // error.detail payload; ResetInfo for rate limits
}

// DeferPolicy bounds the reactive defer floor. Zero fields take defaults.
type DeferPolicy struct {
	Base    time.Duration // fallback backoff base (default 60s)
	Cap     time.Duration // fallback backoff ceiling (default 15m)
	Horizon time.Duration // sanity bound on trusted resets (default 8 days)
	Max     int           // consecutive defers before a real failure (default 8)
}

// withDefaults fills unset policy fields.
func (p DeferPolicy) withDefaults() DeferPolicy {
	if p.Base <= 0 {
		p.Base = 60 * time.Second
	}
	if p.Cap <= 0 {
		p.Cap = 15 * time.Minute
	}
	if p.Horizon <= 0 {
		p.Horizon = 8 * 24 * time.Hour
	}
	if p.Max <= 0 {
		p.Max = 8
	}
	return p
}

// RateLimitDefer is the reactive floor under every tier: a step that fails
// because the endpoint rate-limited it does not fail the run — the failure
// converts into the same defer decision admission uses, so the scheduler has
// one re-queue mechanism. It needs no meter, no budget, no probe, and no
// estimation; runs with the default no-op meter get it in full. It holds no
// lock shared with the Admitter — it reads only the record and its own
// per-node counters.
type RateLimitDefer struct {
	mu     sync.Mutex
	policy DeferPolicy
	state  map[string]int // node id -> consecutive rate-limit defers
	logger *slog.Logger
}

// NewRateLimitDefer builds the floor with the given policy (zero fields take
// defaults). A nil logger discards.
func NewRateLimitDefer(policy DeferPolicy, logger *slog.Logger) *RateLimitDefer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RateLimitDefer{
		policy: policy.withDefaults(),
		state:  map[string]int{},
		logger: logger.With("component", "metering.defer"),
	}
}

// Classify converts a rate-limit failure record into a defer decision. It
// returns (decision, true) when the failure converts and (zero, false) when
// the record must flow to failure policy untouched — either because it is not
// a rate-limit failure or because the consecutive-defer bound is exhausted
// (at some point a "rate limit" is an outage, and fail-stop with a
// diagnosable record beats spinning forever; Consecutive supplies the defer
// history for that record). A reset epoch in the future and inside the
// sanity horizon is trusted verbatim — subscription windows legitimately
// reset days out; missing, unparseable, past, or beyond-horizon resets fall
// back to a backoff that doubles per consecutive occurrence, capped.
func (d *RateLimitDefer) Classify(rec FailureRecord, now time.Time) (Decision, bool) {
	if rec.Status != StatusFailed || rec.Reason != ReasonRateLimit {
		return Decision{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	consecutive := d.state[rec.NodeID]
	if consecutive+1 > d.policy.Max {
		d.logger.Warn("consecutive rate-limit defers exhausted; converting to a real failure",
			"node", rec.NodeID, "consecutive", consecutive, "max", d.policy.Max)
		return Decision{}, false
	}

	until, epoch := d.resetFrom(rec.Detail)
	if until.IsZero() || !until.After(now) || until.After(now.Add(d.policy.Horizon)) {
		if !until.IsZero() {
			d.logger.Warn("rate-limit reset epoch outside the trusted window; using fallback backoff",
				"node", rec.NodeID, "reset", epoch, "horizon", d.policy.Horizon)
		}
		until = now.Add(d.backoff(consecutive))
	}
	d.state[rec.NodeID] = consecutive + 1
	return Decision{
		Kind:   Defer,
		Until:  until,
		Detail: fmt.Sprintf("rate-limited, reset %s", until.Format(time.RFC3339)),
	}, true
}

// resetFrom decodes the box's ResetInfo from the failure detail. A missing or
// malformed payload yields a zero time (fallback backoff applies).
func (d *RateLimitDefer) resetFrom(detail json.RawMessage) (time.Time, int64) {
	if len(detail) == 0 {
		return time.Time{}, 0
	}
	var info ResetInfo
	if err := json.Unmarshal(detail, &info); err != nil {
		d.logger.Warn("rate-limit detail payload malformed; using fallback backoff", "error", err)
		return time.Time{}, 0
	}
	if info.Reset <= 0 {
		return time.Time{}, 0
	}
	return time.Unix(info.Reset, 0), info.Reset
}

// backoff is the signal-without-reset fallback: Base doubled per consecutive
// occurrence, clamped at Cap.
func (d *RateLimitDefer) backoff(consecutive int) time.Duration {
	if consecutive >= 63 {
		return d.policy.Cap
	}
	b := d.policy.Base << consecutive
	if b <= 0 || b > d.policy.Cap {
		return d.policy.Cap
	}
	return b
}

// Reset clears a node's consecutive counter; the executor calls it on the
// node's next successful attempt.
func (d *RateLimitDefer) Reset(nodeID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.state, nodeID)
}

// Restore seeds a node's consecutive counter; on resume the executor rebuilds
// it by counting the node's trailing journal defer events, so the Max bound
// survives a process restart.
func (d *RateLimitDefer) Restore(nodeID string, consecutive int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if consecutive <= 0 {
		delete(d.state, nodeID)
		return
	}
	d.state[nodeID] = consecutive
}

// Consecutive reports a node's current consecutive rate-limit defer count —
// the defer history the executor attaches to the real failure record once
// the Max bound converts an occurrence into an ordinary failure.
func (d *RateLimitDefer) Consecutive(nodeID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state[nodeID]
}
