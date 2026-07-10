# Test section: Metering tests

Integration scenarios spanning MeterInterface, EndpointTiers, and
RateLimitDefer — the admission ledger, the tier implementations, and the
reactive floor exercised together against a fake scheduler loop and a fake
ProbeRunner. (Unit tests live beside the code; these are the module-level
behaviors that must hold.)

## Fixtures

- `testdata/meters_*.yaml` — metering configs: one per tier, one multi-class,
  one invalid per load-check.
- Scripted `ProbeRunner` fakes: tokenizer replying `{"tokens": n}`, probe
  replying utilization/reset sequences, failing commands.
- Canned result records and usage sidecars, including the `rate-limit` failure
  record with and without a `reset` epoch in `error.detail`.

## Scenarios

1. **No-op default.** No metering file, no `--budget`: every Admit returns
   admit, Settle returns no costs, journal records carry no cost fields — the
   run is indistinguishable from one without the module. The rate-limit
   scenarios below still pass in this configuration.
2. **Exact-tier hard bound rejects.** Budget `tokens=50000`; the tokenizer
   fake prices a step's bound above the remaining headroom: Admit returns
   reject(tokens), the step settles as a structured failure naming the unit,
   and its dependents end skipped (dependency failed).
3. **Reservation contention defers, actuals free headroom.** Two ready steps,
   budget fits one estimate: the first admits and reserves; the second gets a
   zero-until defer; the first settles with an actual well under its bound;
   the second re-admits on the settlement re-check. Ledger asserts: reserved
   never exceeds limit, spent reflects actuals not estimates.
4. **Reported tier is retrospective.** Three sequential steps under a
   reported-tier class: each admits on a zero claim, each Settle folds the
   usage sidecar into spent, and the third rejects once spent crosses the
   limit — the circuit-breaker behavior, exhaustion discovered from actuals.
5. **Unit segregation.** Budget in `usd-cents`, only meter reports `tokens`:
   startup logs the never-enforced warning, no step ever blocks on that
   budget, and `Cost.Add` across the two units returns an error. Nothing ever
   sums tokens into cents.
6. **Probe saturation defers.** Probe fake returns utilization 1.0 with a
   reset epoch: Admit returns defer(until reset); after the fake flips to 0.2
   the step admits. A probe that exits nonzero yields admit plus a warning —
   best-effort by contract, asserted explicitly.
7. **429 floor with zero config.** Empty meter set: a step fails with the
   `rate-limit` record carrying a reset epoch. Assert order: Settle ran, a
   defer event was journaled, `on_failure` executed, the scheduler re-queued
   with earliest-start = reset, `retry: N` was not decremented, and the
   re-attempt's success reset the consecutive counter.
8. **Missing reset backs off, cap and Max hold.** Rate-limit records without
   `reset`: defer intervals double from Base and clamp at Cap; the Max+1th
   consecutive occurrence converts to a real failure whose record carries the
   defer history; fail-stop then propagates normally.
9. **Journaled actuals (aggregation seam).** After a multi-step run, every
   completed step's journal record carries its unit-tagged actuals; no
   run-level total exists anywhere — asserting the first-pass boundary of the
   deferred aggregation requirement.

## Edge cases

- Tokenizer command fails mid-run: Admit returns an error, the step fails as
  an admission failure — never a silent admit past a hard-bound tier.
- Malformed usage sidecar: warning, zero cost recorded, step status untouched.
- Reset epoch in the past or beyond the horizon: fallback backoff is used, the
  bogus epoch is logged verbatim for diagnosis.
- Two budgets, two units, one step estimated in both: rejection on either unit
  rejects the step; the decision names the exhausted unit only.
- Resume after a kill during a defer wait: the consecutive counter is rebuilt
  from trailing journal defer events and the earliest-start is re-derived, so
  Max cannot be evaded by restarting the process.
