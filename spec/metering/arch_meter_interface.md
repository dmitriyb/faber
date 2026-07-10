# MeterInterface — the budget hook and cost vocabulary

## What it is

The pluggable seam between the executor and any notion of cost. The scheduler
calls exactly two hooks around every step: `Estimate(step)` before launch —
feeding an admission decision — and `Actual(result)` after completion — feeding
accounting. Everything else in this module (tiers, probes, the 429 floor) is an
implementation or a neighbor of these two calls. The engine never learns what a
token, a dollar, or a GPU-second *is*; it learns only how to compare tagged
quantities and enforce declared limits.

## The two hooks

- **`Estimate(step) -> Estimate`** runs pre-launch with the step's resolved
  inputs, template identity, and endpoint class. It returns zero or more
  unit-tagged upper-bound costs, and optionally a saturation hint (`defer
  until t`) for endpoints that know their own replenishment window. An
  estimate is an admission bound, not a prediction — over-claiming is safe,
  under-claiming defeats the budget.
- **`Actual(result) -> []Cost`** runs post-completion (every attempt, any
  status) with a view over the step's result record and the usage sidecar the
  box deposited. Actuals replace the step's held reservation in the ledger and
  are recorded per step in the journal.

## Cost and Unit

`Cost` is an opaque unit-tagged integer quantity: `{unit, amount}`, where
`amount` is in the unit's atomic granularity (tokens, cents, GPU-seconds —
the user's choice; faber never converts). Costs are summed and compared **only
within a unit**; cross-unit arithmetic is a programming error surfaced as an
error value, never a silent coercion. There is no exchange rate anywhere in
the engine.

## Budgets and admission

A budget is declared per unit at run entry: `--budget unit=n` (repeatable, one
per unit — the CLI leaf already reserves this flag). The Admitter keeps a
ledger per unit: `spent` (settled actuals), `reserved` (in-flight estimates),
`limit`. Admission for a ready step:

1. Collect estimates from the step's configured meters.
2. Any saturation hint ⇒ **defer(until t)** (latest hint wins).
3. Per declared budget `u`: only meters that returned a cost tagged `u`
   participate — a meter silent on `u` neither blocks nor consumes it. If
   `spent + estimate > limit`, the step can never fit (budgets do not
   replenish within a run) ⇒ **reject(u)**. If it fits alone but not alongside
   current reservations (`spent + reserved + estimate > limit`) ⇒ **defer**
   until the next in-flight settlement (zero-time defer; the scheduler
   re-checks on the next completion event rather than at a wall-clock time).
4. Otherwise **admit** and hold the estimate as a reservation until settlement.

A budget whose unit no configured meter reports is announced as a startup
warning ("budget declared, nothing measures it") and never blocks — a silent
never-enforced budget is the failure mode this rule exists to surface.

## Decision vocabulary

Exactly three decisions cross to the scheduler: **admit**, **defer(until t)**
(re-queue, not a failure; `deferred` is a scheduler waiting state, never a
terminal one), and **reject(budget)** (a structured step failure naming the
exhausted unit; fail-stop propagation marks dependents skipped-by-dependency
as with any failure). RateLimitDefer reuses the same defer decision on the
post-failure path, so the scheduler has one re-queue mechanism, not two.

## Default: the no-op meter

With no metering configuration and no `--budget` flags, the meter set is
empty: every admission is admit, every settlement records nothing, and the run
is byte-identical to one on an engine without this module. Users plug meters
per endpoint class (see EndpointTiers); the 429 floor operates regardless.

## Deferred seam: run cost aggregation

Backlog (design edge case 9). The first pass records each step's actuals as
unit-tagged costs on the step's journal record — the journal is already the
single source the run report reads. Reserved on that seam: rolling per-step
actuals into run-level and generate-item-level totals with a reporting surface
(human summary line per unit, machine-readable export). The first pass does no
rollup; the per-step records are complete enough that aggregation needs no new
data, only a fold.

Requirements implemented: Estimate and actual hooks, Pluggable cost unit,
Deferred: run cost aggregation.
