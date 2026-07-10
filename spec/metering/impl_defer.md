# Implementation: Defer algorithm

Covers RateLimitDefer.

## Types (internal/metering/defer.go)

```go
const ReasonRateLimit = "rate-limit" // reserved error.reason value, box contract

type ResetInfo struct {
    Reset int64 `json:"reset"` // unix epoch seconds; 0 => unknown
}

type DeferPolicy struct {
    Base    time.Duration // fallback backoff base (default 60s)
    Cap     time.Duration // fallback backoff ceiling (default 15m)
    Horizon time.Duration // sanity bound on trusted resets (default 8 days)
    Max     int           // consecutive defers before real failure (default 8)
}

type deferState struct { // per node id, in-memory + reconstructable from journal
    consecutive int
}

func (d *RateLimitDefer) Classify(rec failure.Record, now time.Time) (Decision, bool)
```

`Classify` returns `(defer decision, true)` when the failure converts, and
`(_, false)` when it must flow to the failure module untouched. It holds no
lock shared with the Admitter — it reads only the record and its own per-node
counters.

## Algorithm

```
Classify(rec, now):
  if rec.Status != failed || rec.Error.Reason != ReasonRateLimit:
      return _, false                       # not ours
  st := state[rec.NodeID]
  if st.consecutive+1 > policy.Max:
      return _, false                       # outage, not a limit: real failure,
                                            # record carries the defer history
  until := resetFrom(rec.Error.Detail)      # decode ResetInfo from detail JSON
  if until.IsZero() || until <= now || until > now+policy.Horizon:
      until = now + min(policy.Base << st.consecutive, policy.Cap)
  st.consecutive++
  return Decision{Kind: Defer, Until: until,
                  Detail: fmt.Sprintf("rate-limited, reset %s", until)}, true
```

Reset epochs inside the horizon are trusted verbatim — subscription windows
legitimately reset days out, so the backoff cap applies only to the
signal-without-reset fallback. A later successful attempt of the node zeroes
`consecutive`.

## Executor integration (order is the contract)

For a failed attempt, the pipeline runs exactly this sequence:

1. `admitter.Settle(result)` — the reservation is released and any partial
   actuals are accounted whether or not the failure converts; a deferred step
   must not hold budget headroom while it waits.
2. `rld.Classify(record, now)` — on `true`: append a `defer` event to the
   journal `{node, reason: rate-limit, until, consecutive}`, run the step's
   `on_failure` hook (the identical between-attempt cleanup call the retry
   path makes — one cleanup contract, two callers), and hand the scheduler
   the decision. The scheduler re-queues the node with an earliest-start of
   `until` and does **not** decrement `retry: N`.
3. On `false`: normal failure-policy flow (retry accounting, fail-stop).

The re-attempt is a fresh full attempt of the whole step — same input-hash,
new attempt number — so journal keying is unaffected; the defer events sit
between attempt records and let `faber resume` and the run report reconstruct
why the step's timeline has a gap. On resume, `consecutive` is rebuilt by
counting trailing defer events for the node, so the Max bound survives a
process restart.

## Why detail-JSON, not exit codes, carries the reset

The box's exit code selects the error *class* (its classification table maps
the agent CLI's rate-limit failure onto `ReasonRateLimit`); the epoch itself
travels in `error.detail` because exit codes cannot carry a timestamp and
stdout scraping is banned engine-wide. The box owns extraction (whatever
header or body field its endpoint exposes); this component owns only decoding
`ResetInfo` — a one-field contract that any endpoint's notion of "try later"
compresses into.
