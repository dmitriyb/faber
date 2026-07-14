# Implementation: Scheduling algorithm

Covers Scheduler.

## State (internal/pipeline/scheduler.go)

```go
type Scheduler struct {
    nodes   map[string]*nodeState // grows on generate splice
    edges   *edgeIndex            // out-edges + in-degree, both directions
    ready   *readyQueue           // min-heap ordered by node ID
    slots   chan struct{}         // cap = --max-parallel; box runs only
    events  chan event            // workers -> loop; loop owns all state
    conds   *ConditionEvaluator
    expand  *GenerateExpander
    journal failure.Journal
    meter   metering.Meter
    steps   StepRunner            // agent box wrapped in failure retry
    clock   clock                 // injectable for defer tests
}

type nodeState struct {
    node    *config.Node
    state   State                 // pending|running|deferred|terminal…
    result  *failure.Result
}

type event interface{ isEvent() }
// settled{id, result} | deferred{id, until} | expanded{id, splice, err}
```

All graph mutation happens on the loop goroutine; workers communicate only via
`events`. No mutex guards the DAG — the splice, the decrements, and the ready
queue are single-threaded by construction, and `go test -race` stays quiet.

The `steps` closure is the host-side `infra.RunSpec` assembly seam: it bridges
the security module's `Assembled` (verbatim argv fragment + `SecretsStdin`) and
the engine mounts/env into one run spec before handing it to
`infra.ContainerRunner`, so it is the single place that owns `RunSpec.Env`
assembly and therefore enforces the credentials pairing — a non-empty
`Assembled.SecretsStdin` is copied into `RunSpec.StdinSecrets` and
`RunSpec.Env[contract.EnvSecretsStdin]="1"` is set in the same step, never one
without the other (see spec/infra/impl_run_argv.md).

## The loop

```go
func (s *Scheduler) Run(ctx context.Context) error {
    s.seed() // in-degrees; indeg==0 -> ready
    for s.unsettled() > 0 {
        for s.ready.Len() > 0 {
            s.dispatch(ctx, s.ready.Pop()) // ID order = deterministic
        }
        s.step(ctx, <-s.events)
    }
    return s.runError() // nil unless some node failed
}
```

`dispatch` runs the cheap gates inline, in the fixed order:

1. **Condition**: `conds.Evaluate` false (or skipped-dep short-circuit) ⇒
   `settle(id, SkippedCondition)`.
2. **Journal**: `journal.Lookup(id, inputHash(id))` returning an `ok` record ⇒
   `settle(id, cached(rec))`. `inputHash` covers resolved input values,
   template identity, and image tag — the failure module's key contract.
3. **Selector**: resolve newest `ok` candidate; exhaustion condition true on
   the final iteration ⇒ settle `failed(loop-exhausted)`, else adopt payload.
4. Otherwise spawn a worker: acquire a slot, `meter.Estimate` ⇒ on
   `defer(until)` release the slot and emit `deferred`; on `reject` emit
   `settled(failed(budget))`; on `admit` run `steps.Run` (retry loop inside)
   and emit `settled` with the final record. A rate-limit failure carrying a
   reset time comes back from metering's defer floor as `deferred`, not
   `settled`, and consumes no retry.

`step` folds one event into the graph:

```go
case settled:
    s.record(ev)                       // journal append (skips too, hashless)
    for _, d := range s.edges.dependents(ev.id) {
        if s.edges.dec(d) == 0 { s.ready.Push(d) }
    }
    if ev.result.Failed() { s.propagate(ev.id) }
    if s.isGenerate(ev.id) && ev.result.OK() { /* settled by expanded */ }
case deferred:
    s.clock.AfterFunc(until, func() { s.ready.Push(ev.id) }) // re-admission
case expanded:
    s.splice(ev) // add nodes+edges, seed in-degrees, rewire dependents
```

Generate nodes dispatch to `expand.Expand` in a worker; the `expanded` event
carries the splice (or the contract error, settling the node failed). The node
settles `ok` only after the splice is applied, so no dependent can slip
between expansion and rewiring.

## Failure propagation

```go
func (s *Scheduler) propagate(failed string) {
    q := s.edges.dependents(failed)
    for len(q) > 0 {
        id := pop(&q)
        if s.nodes[id].settledOrRunning() { continue }
        s.settle(id, failure.SkippedDependency(failed)) // ancestor recorded
        q = append(q, s.edges.dependents(id)...)
    }
}
```

BFS, idempotent, and cheap: nodes already settled (or mid-flight — their own
result will tell) are left alone; everything else downstream terminates
immediately with the root cause attached. Independent subgraphs never appear
in `dependents` and keep executing.

## Journal records for skips

Skip settlements append journal records with a null input hash (a skipped
node's inputs may be unresolvable — its producer failed). Null-hash records
are never resume hits; they exist so the report and journal replay see every
terminal state. Ok/failed records carry the real hash and full result.
