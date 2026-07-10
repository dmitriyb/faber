# RateLimitDefer — the reactive floor under every tier

## What it is

The guarantee that holds when nothing else is configured: a step that fails
because the endpoint rate-limited it does **not** fail the run. The failure is
converted into a defer(until reset) decision and the scheduler re-queues the
step for after the window replenishes. This is the floor beneath the tiers —
it needs no meter, no budget, no probe, and no estimation; it reacts to a
signal the failure already carries. Runs with the default no-op meter get it
in full.

## The signal contract with the box

Detection reads the step's structured result record, not stdout. The contract
with the box (agent module) is: when the agent invocation fails on a
rate-limit error class — recognized from the agent CLI's exit code or error
output per the box's error-classification table — the box writes a failure
record whose `error.reason` is the reserved value `rate-limit` and whose
`error.detail` carries a machine-readable reset epoch extracted from the
response (headers or body — whatever the endpoint provides; the box owns the
extraction, faber owns only the record shape). Any failure record without that
reason is not this component's business and flows to the failure module
unchanged.

## The conversion

On a failure record with `reason == "rate-limit"`:

1. Extract the reset epoch from `error.detail`. Valid and in the future ⇒
   defer until it (a subscription window legitimately resets days out; the
   reset time is trusted up to a sanity horizon). Missing, unparseable, in the
   past, or beyond the horizon ⇒ defer with a conservative default backoff
   that doubles per consecutive rate-limit failure of the same step, capped.
2. Journal a defer event for the step (the run's durable state must explain
   the gap in the timeline), release the step's budget reservation via the
   normal settlement path, and hand the scheduler `defer(until t)` — the same
   decision vocabulary admission uses, so re-queueing is one mechanism.
3. The step's `on_failure` cleanup hook runs before the re-attempt, exactly as
   between retry attempts — the box may have claimed an item or pushed a
   branch before the limit hit, and the between-attempt-cleanup guarantee is
   what makes re-running the whole step safe.
4. The re-attempt does **not** consume the step's `retry: N` budget: a
   rate-limit is the environment's failure, not the step's.

## Bounds

Deferral is not unbounded patience. A per-step cap on consecutive rate-limit
defers (with the backoff cap for signal-without-reset cases) converts the
N+1th occurrence into an ordinary failure carrying the full defer history —
at some point a "rate limit" is an outage, and fail-stop with a diagnosable
record beats spinning forever. A successful attempt resets the counter.

## Position in the executor

The check sits between result settlement and failure-policy classification:
settle accounting first (reservations must not leak while a step waits),
convert second, and only failures that are not converted reach fail-stop
propagation. `deferred` is a waiting state, never terminal — every deferred
step ultimately resolves to ok, failed, or skipped, and the run report shows
the defer events on the way.

Requirements implemented: Reactive 429 defer.
