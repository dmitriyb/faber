# Data flow: Admission flow

The metering path wrapped around every step the scheduler readies. The no-op
configuration collapses the left column to "admit" and the accounting steps to
nothing; the 429 re-entry on the right operates regardless.

```
ready step (scheduler)
        │  Step{node, template, endpoint, inputs}
        ▼
Admitter.Admit ──── meters[endpoint].Estimate
        │              exact:    tokenizer bound (hard upper)
        │              reported: zero-cost claim in its unit
        │              probe:    saturation hint (defer until reset)
        ▼
Decision
   ├─ reject(budget) ──► structured step failure ──► fail-stop propagation
   ├─ defer(until t) ──► scheduler re-queue (earliest-start t;
   │                     zero t = re-check on next settlement)
   └─ admit (reservation held)
        │  box runs
        ▼
result record + usage sidecar
        │  ResultView
        ▼
Admitter.Settle ──► reservation released, actuals folded into spent,
        │           per-step costs appended to the journal record
        ▼
   status ok ──────────────────────► done (defer counter reset)
   status failed
        │
        ▼
RateLimitDefer.Classify
   ├─ reason == rate-limit, under Max ──► journal defer event
   │                                      ──► on_failure cleanup
   │                                      ──► defer(until reset) ─┐
   └─ anything else ──► failure module (retry / fail-stop)        │
                                                                  │
scheduler re-queue (fresh attempt, retry budget untouched) ◄──────┘
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| scheduler -> Admit | `Step` | resolved inputs only; called once per launch attempt, serialized on the ledger lock |
| meter -> Admit | `Estimate{costs[], deferUntil?}` | costs are unit-tagged upper bounds; error from a budget-participating meter = admission failure, never silent admit |
| Admit -> scheduler | `Decision{admit \| defer(until) \| reject(budget)}` | the only three words; reject is a normal structured failure downstream |
| box -> Settle | result record + optional usage sidecar | sidecar is best-effort; absence is a warning, not a failure |
| Settle -> journal | `[]Cost` on the step's record | raw material for the deferred aggregation seam; no rollup in the first pass |
| Classify -> scheduler | `Decision{defer(until)}` or pass-through | same defer vocabulary as admission — one re-queue mechanism |

## Ordering invariants

- Settle always precedes Classify: a waiting step never holds a reservation.
- Cleanup precedes re-queue: the between-attempt guarantee is identical for
  retry and defer.
- Every defer (admission or reactive) is journaled before the scheduler sees
  it: resume and the run report reconstruct the timeline from the journal
  alone.

## Who runs it

The pipeline scheduler is the only caller: Admit at readiness (after the
condition check and journal skip-lookup — a skipped step is never estimated),
Settle at every attempt's completion, Classify on every failed attempt. The
CLI constructs the Admitter from `--budget` flags plus the metering config
file and passes it into `pipeline.Execute` as a dependency — the metering
module never reaches into the executor.
