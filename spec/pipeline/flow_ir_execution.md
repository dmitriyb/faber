# Data flow: IR execution flow

The run-time flow from proven IR to run report — everything that happens after
`faber run` passes the config module's frontend pipeline.

```
IR (proven wiring) + resolved params
        │  seed in-degrees; indeg==0 -> ready       Scheduler.Run
        ▼
ready node (queue ordered by node ID)
        │
        ├─ condition check ──────── false ──► skipped-condition ──┐
        │        ConditionEvaluator                               │
        ├─ journal lookup ────────── ok hit ► cached ok ──────────┤
        │        (step-id, input-hash)                            │
        ├─ meter admission ──┬─ defer(until) ► re-queue at wake   │
        │                    └─ reject ──────► failed ────────────┤
        ▼  admit                                                  │
box run (agent module; retry loop from failure module)            │
        │  result record (429 w/ reset re-enters as defer)        │
        ▼                                                         ▼
generate node? ── GenerateExpander ── splice ──►  journal append + decrement
   │  data-source command (infra.CommandRunner)   dependents; failed -> BFS
   │  {items:[{id,deps,...}]} -> instance subgraphs  skipped-dependency
   ▼                                                              │
selector node? resolve newest ok candidate (pure wiring)          │
                                                                  ▼
                                    settled run ──► RunReporter(journal, IR)
                                                    ──► human text / --json
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| IR -> Scheduler | `IR{nodes[], edges[]}` + `map[string]any` params | canonical struct from the frontend; executor never reads Config |
| Scheduler -> ConditionEvaluator | node's `CondSpec` + settled payloads | activation `{steps: {name: {field: value}}, params: {...}}`; skipped dep ⇒ false without eval |
| Scheduler -> Journal | `(step-id, input-hash)` | hash covers resolved inputs + template identity + image tag; ok hits skip the run; skip records are null-hash, never hits |
| Scheduler -> Meter | step estimate | decision `admit \| defer(until) \| reject`; unit-tagged Cost |
| Scheduler -> box | resolved node (template, bindings, identity) | one result record per attempt: `{status, payload\|error, timing, attempt}` |
| GenerateExpander -> data source | argv via infra.CommandRunner | stdout `{"items":[{"id","deps",...}]}`; anything else = contract error |
| GenerateExpander -> Scheduler | `Splice{nodes, edges}` | applied atomically on the loop goroutine before the generate node settles |
| Journal -> RunReporter | header + append-only records | report is a pure function of journal + IR; in-memory state never consulted |

## Error paths

Per-node failure is a record, not an exception: a failed box run (after
retries), a rejected admission, a data-source contract violation, or a
loop-exhausted selector all settle their node `failed` and trigger the same
BFS — transitive dependents settle `skipped-dependency` with the ancestor
named, independent branches continue, and the run itself keeps going until
every node is terminal. The only whole-run aborts are process-level: context
cancellation and a journal that cannot be appended (durability gone means no
record of what ran — stop).

## Who runs it

- `faber run`: the frontend pipeline in-process, then this flow; the journal
  header (config hash, workflow, params, IR hash) is written before the first
  dispatch.
- `faber resume`: the same flow with the prior journal preloaded — hits at the
  lookup gate make resume a property of this flow, not a separate mode.
  `--no-cache` (fresh) empties the lookup; interactive recovery replays up to
  the failed node and hands the operator its reconstructed box instead of
  dispatching it.
- The report step runs unconditionally, even when the run errors mid-flight —
  whatever reached the journal is reportable.
