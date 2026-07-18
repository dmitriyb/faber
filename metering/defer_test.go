package metering

import (
	"strings"
	"testing"
	"time"
)

var deferNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// Verifies 06e9d770841f: a failure record with the reserved rate-limit reason
// and a valid future reset epoch converts into defer(until reset) — the reset
// is trusted verbatim inside the sanity horizon.
func TestClassifyConvertsRateLimitWithReset(t *testing.T) {
	d := NewRateLimitDefer(DeferPolicy{}, nil)
	reset := deferNow.Add(2 * 24 * time.Hour) // subscription windows legitimately reset days out

	dec, ok := d.Classify(rateLimitRecord("task/a", reset.Unix()), deferNow)
	if !ok {
		t.Fatal("rate-limit record did not convert")
	}
	if dec.Kind != Defer || !dec.Until.Equal(reset) {
		t.Fatalf("got %+v, want defer until %s", dec, reset)
	}
	if d.Consecutive("task/a") != 1 {
		t.Fatalf("consecutive = %d, want 1", d.Consecutive("task/a"))
	}
}

// Verifies 06e9d770841f: any failure record without the reserved reason — or
// a non-failed record — is not this component's business and flows to the
// failure module unchanged.
func TestClassifyIgnoresOtherFailures(t *testing.T) {
	d := NewRateLimitDefer(DeferPolicy{}, nil)
	cases := map[string]FailureRecord{
		"other reason": {NodeID: "task/a", Status: StatusFailed, Reason: "assertion"},
		"no reason":    {NodeID: "task/a", Status: StatusFailed},
		"ok status":    {NodeID: "task/a", Status: StatusOK, Reason: ReasonRateLimit},
	}
	for name, rec := range cases {
		if _, ok := d.Classify(rec, deferNow); ok {
			t.Fatalf("%s: converted, want pass-through", name)
		}
	}
	if d.Consecutive("task/a") != 0 {
		t.Fatalf("pass-through records must not touch the counter, got %d", d.Consecutive("task/a"))
	}
}

// Verifies 06e9d770841f: scenario 8 — rate-limit records without a reset back
// off from Base, doubling per consecutive occurrence, clamped at Cap.
func TestClassifyFallbackBackoffDoublesAndCaps(t *testing.T) {
	policy := DeferPolicy{Base: time.Minute, Cap: 5 * time.Minute, Max: 8}
	d := NewRateLimitDefer(policy, nil)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute,
		5 * time.Minute, 5 * time.Minute, // clamped at Cap
	}
	for i, w := range want {
		dec, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow)
		if !ok {
			t.Fatalf("occurrence %d did not convert", i+1)
		}
		if got := dec.Until.Sub(deferNow); got != w {
			t.Fatalf("occurrence %d: backoff %s, want %s", i+1, got, w)
		}
	}
}

// Verifies 06e9d770841f: the Max+1th consecutive occurrence converts to a
// real failure (pass-through) with the defer history available — at some
// point a rate limit is an outage — and a successful attempt resets the
// counter.
func TestClassifyMaxConsecutiveBecomesRealFailure(t *testing.T) {
	d := NewRateLimitDefer(DeferPolicy{Max: 3}, nil)
	for i := 0; i < 3; i++ {
		if _, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow); !ok {
			t.Fatalf("occurrence %d did not convert", i+1)
		}
	}
	if _, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow); ok {
		t.Fatal("occurrence Max+1 converted, want a real failure")
	}
	if d.Consecutive("task/a") != 3 {
		t.Fatalf("defer history = %d, want 3 for the failure record", d.Consecutive("task/a"))
	}

	d.Reset("task/a") // the node's next attempt succeeded
	if d.Consecutive("task/a") != 0 {
		t.Fatalf("counter after success = %d, want 0", d.Consecutive("task/a"))
	}
	if _, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow); !ok {
		t.Fatal("post-success rate limit did not convert; success must reset the bound")
	}
}

// Verifies 06e9d770841f: a reset epoch in the past, beyond the sanity
// horizon, or malformed falls back to the backoff, and the bogus epoch is
// logged for diagnosis.
func TestClassifyBogusResetFallsBack(t *testing.T) {
	logger, buf := testLogger()
	policy := DeferPolicy{Base: time.Minute, Cap: time.Hour, Horizon: 24 * time.Hour, Max: 8}
	d := NewRateLimitDefer(policy, logger)

	cases := map[string]FailureRecord{
		"past epoch":     rateLimitRecord("task/a", deferNow.Add(-time.Hour).Unix()),
		"beyond horizon": rateLimitRecord("task/b", deferNow.Add(48*time.Hour).Unix()),
		"malformed":      {NodeID: "task/c", Status: StatusFailed, Reason: ReasonRateLimit, Detail: []byte(`??`)},
		"empty detail":   {NodeID: "task/d", Status: StatusFailed, Reason: ReasonRateLimit},
	}
	for name, rec := range cases {
		dec, ok := d.Classify(rec, deferNow)
		if !ok {
			t.Fatalf("%s: did not convert", name)
		}
		if got := dec.Until.Sub(deferNow); got != time.Minute {
			t.Fatalf("%s: backoff %s, want the Base fallback %s", name, got, time.Minute)
		}
	}
	log := buf.String()
	if !strings.Contains(log, "outside the trusted window") {
		t.Fatalf("log %q lacks the bogus-epoch diagnosis", log)
	}
	if !strings.Contains(log, "malformed") {
		t.Fatalf("log %q lacks the malformed-detail diagnosis", log)
	}
}

// Verifies 06e9d770841f: on resume the counter is rebuilt from trailing
// journal defer events, so the Max bound survives a process restart and
// cannot be evaded by restarting.
func TestRestoreRebuildsCounterAcrossRestart(t *testing.T) {
	d := NewRateLimitDefer(DeferPolicy{Max: 3}, nil)
	d.Restore("task/a", 3) // three trailing defer events counted from the journal

	if _, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow); ok {
		t.Fatal("restored-at-Max node converted, want a real failure")
	}

	d.Restore("task/a", 0)
	if _, ok := d.Classify(rateLimitRecord("task/a", 0), deferNow); !ok {
		t.Fatal("cleared node did not convert")
	}
}
