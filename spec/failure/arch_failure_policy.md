# FailurePolicy — fail-stop, cleanup hooks, opt-in retry

## What it is

The decision layer between a step's result record and the rest of the run:
what a `failed` status means for the graph (fail-stop), what runs to undo the
step's external side-effects (`on_failure`), and when the step is re-run
(`retry: N`). This module owns the semantics; the pipeline module's Scheduler
executes the graph consequences.

## Fail-stop

A failed step halts its dependency chain: every transitive dependent is marked
`skipped (dependency failed)` and never launches. Independent branches
continue — a failed generate instance stops only its own downstream steps, not
its siblings. The propagation itself (in-degree bookkeeping, marking, report
states) is the Scheduler's; FailurePolicy defines the rule and the terminal
vocabulary the Scheduler applies. There is no continue-on-error mode: a step
whose contract was not met must not feed dependents, because the type system
promised them a schema-valid payload.

## on_failure cleanup hooks

A template or step may declare `on_failure`: an opaque user script faber runs
host-side when the step fails. Its job is to undo *external* side-effects the
lambda left behind — delete the orphan branch at the gateway, release the
claimed work item — things container teardown cannot reach. The contract:

- **Inputs.** The script receives the failed step's resolved input values as
  environment variables and the error record as JSON on stdin. That is
  everything a cleanup can act on; it never sees engine internals.
- **Host-side execution.** The hook runs on the host (or, if the user's script
  needs the sealed toolset, the user may have it re-enter a fresh box — faber
  just execs the script). It is not part of the box's fixed phase order.
- **Never masks.** A cleanup that itself fails is reported — a cleanup record
  in the journal and the run report — but the step's original failure record
  stands untouched. The operator sees both; the original is never rewritten.

## Opt-in retry

`retry: N` on a step allows up to N re-runs after the first failure. The unit
of retry is the whole step: a step is a lambda, atomic by design, and faber
never resumes into an agent's chain of thought. Each attempt is a fresh
container from the same image with the same resolved inputs.

Cleanup is what makes retry sound: `on_failure` runs between attempts, so each
attempt starts from a clean slate (no half-claimed item, no leftover branch to
collide with). A step declaring `retry` without `on_failure` is accepted but
the idempotency burden shifts entirely to the user's hooks. Attempt accounting
lands in the result record: every attempt is numbered, and the final record —
the one that is journaled and reported — carries the full attempt history,
whether the last attempt succeeded or exhausted the budget. Exhausted retries
yield a single final failure record; downstream fail-stop then applies once.

Each re-run passes through the same scheduler gates as any launch (metering
admission included), so retry cannot bypass budget controls.

## Deferred seam: timeouts and cancellation

Per-step wall-clock timeouts and user-initiated abort are reserved
(design edge case 6): a timeout would kill the in-flight container, tear down
its bindings, run `on_failure`, and journal the interruption as a failure
record with a distinct reason; user abort would do the same for every in-flight
step. The first pass has no step timeout (only the resource limits and any
budget bound), and abort is process-level. One cancellation behavior does
exist inside the attempt loop: the loop head checks the run context, and a
cancelled run settles the step with a `canceled` record carrying the real
attempts' history — the remaining retries are never launched against a dead
context (each would fully stage skills and bindings only to die). A killed
process still abandons in-flight containers to their `--rm`; the journal
simply lacks records for them, which resume treats as absent steps.

Requirements implemented: Fail-stop and cleanup hooks; Opt-in retry with
cleanup; Deferred: timeouts and cancellation.
