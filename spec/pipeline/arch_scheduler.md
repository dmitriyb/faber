# Scheduler — readiness-driven parallel execution of the IR

## What it is

The run loop. It takes the validated, desugared IR plus resolved params and
drives every node to a terminal state. The scheduler only ever sees the pure
DAG — loops arrived unrolled, sugar resolved — so its whole job is Kahn-style
readiness: a node is eligible when every incoming edge (data reference or
ordering) is satisfied by a settled source.

## Readiness and dispatch

- **In-degree tracking** over the union of data and ordering edges. Settling a
  node decrements each dependent; a dependent at zero joins the ready queue.
- **Deterministic tie-break**: the ready queue is ordered by node ID (path-like
  IDs give a stable, config-shaped order), so two runs of the same IR dispatch
  in the same sequence. Parallelism may interleave completions differently, but
  dispatch order — and therefore every tie-break-sensitive decision — is fixed.
- **Goroutine per ready node**, gated by a semaphore of `--max-parallel` slots.
  The slot guards box execution only; condition checks, journal lookups, and
  selector resolution are cheap host-side work that never holds a slot.

## Per-node lifecycle

Each ready node passes through a fixed sequence of gates before any container
exists:

1. **Condition check** (ConditionEvaluator). `when:` false ⇒ terminal state
   `skipped-condition`; no journal hash, no meter call, no box.
2. **Journal lookup** by `(step-id, input-hash)`. An `ok` hit ⇒ the journaled
   result is adopted verbatim (resume semantics); dependents decrement as if
   the step had run.
3. **Meter admission** — `estimate(step)` ⇒ `admit` | `defer(until)` |
   `reject`. Defer re-queues the node with a wake time; reject settles it
   `failed` with a structured budget error.
4. **Box run** via the agent module, wrapped in the failure module's retry
   orchestration; the scheduler sees only the final result record.
5. **Result record → journal append → decrement dependents.** Every terminal
   state — including both skip flavors — appends a journal record, because the
   report is derived from the journal alone.

**Selector nodes** execute as pure wiring: no container, no meter. When all
candidates are terminal, the selector adopts the payload of the newest executed
(`ok`) candidate; if the newest candidate is the final iteration and the loop's
exhaustion condition holds, the selector settles `failed` (loop exhausted) —
the failure record the failure module defined for bounded loops.

## Deferral

`defer(until)` — from meter admission or from the metering module's 429-defer
floor converting a rate-limit failure — is not a terminal state. The node
re-enters the ready queue at its wake time and repeats admission; a deferred
attempt consumes no retry budget. Its eventual record notes it was
deferred-then-resolved, but it settles as exactly one of the four terminal
states.

Two defer shapes exist, with distinct wake protocols:

- **Timed** (`until` set): a clock wake re-queues the node at the reset time.
- **Zero-until** ("re-check on the next settlement", the budget-contention
  shape): the node parks in the `waitZero` lot and every settlement drains
  the lot. Two edges close the stall windows: a zero-until defer arriving
  with nothing left in flight re-checks immediately (its releasing
  settlement raced ahead of it), and a **timed** defer also drains the lot —
  it released a slot and settled its attempts' costs, which is exactly the
  state change a parked node waits on, and it is not itself a settlement.
  The just-parked timed node has a clock wake, not a lot entry, so no
  self-wake loop exists.

Admission is per **attempt**, not per dispatch: a retry re-admits before it
launches (the first attempt's reservation settled with its actuals), a
mid-retry `defer` re-queues without consuming the retry, and a mid-retry
`reject` settles the step failed immediately — spent only grows, so burning
the remaining attempts on refusals would be noise.

## Run preflight

Before any run state exists (no journal, no run dir, no lock), every agent
template reachable from the run — entry IR plus generate targets — must
resolve its image tag and the docker daemon must already hold the image. One
early, aggregated refusal (pointing at `faber build`) replaces a per-step
launch-failure cascade that would burn each step's retry budget against a
dead daemon or an unbuilt image. The preflight applies to every execute mode,
resume included: uniform fail-fast, at the cost of requiring images present
even for a fully-cached replay.

## Failure propagation

When a node settles `failed` (final, after retries), the scheduler walks its
dependents breadth-first and marks every not-yet-settled transitive dependent
`skipped-dependency`, recording the failed ancestor's ID in each record.
Independent branches are untouched and run to completion — fail-stop is
per-chain, never per-run. Terminal states are exactly:
`ok | failed | skipped-condition | skipped-dependency`.

## Reference workflows, concretely

**task**: `task/implement` is the only in-degree-0 node and runs first. Its
`pr` output releases `task/review-cycle@1/review`; `fix@1` runs only if the
verdict was `changes`; iteration 2's gate `!(until@1)` skips the whole tail
once a review approves. The `task/review` selector settles on the newest
executed review; `task/merge`'s `when: verdict == "approved"` then admits or
skips it. Exhaustion (three `changes`) fails the selector and merge settles
`skipped-dependency`. **epic**: `epic/tasks` (generate) runs its data-source
command, the GenerateExpander splices one task instance per item, and the
scheduler simply keeps consuming its ready queue — instances for independent
items run concurrently up to the semaphore; an item with deps waits on its
predecessors' instance sinks.

## Deferred seam

Concurrency control beyond the flat `--max-parallel` semaphore is reserved: a
scheduler that weighs host CPU, local GPU presence, and per-endpoint API rate
classes when sizing the dispatch window. The admission gate is already the
right choke point, so the first pass keeps the single semaphore and the seam is
the admission decision — richer schedulers slot in without touching readiness.

Requirements implemented: Topological parallel execution, Failure propagation
and skip semantics, Reference workflow execution, Deferred: concurrency
control.
