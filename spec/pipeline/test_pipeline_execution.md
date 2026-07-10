# Test section: Pipeline execution tests

Integration scenarios spanning Scheduler, ConditionEvaluator, GenerateExpander,
and RunReporter — the module-level behaviors of executing an IR to a report.
(Unit tests live beside the code.) Everything below the scheduler is faked at
its typed seam: a scripted StepRunner returning canned result records per node
ID (with optional latency), an in-memory journal, a scripted meter, a fake
CommandRunner for data sources, and an injected clock for defer wakes. No
docker anywhere in this section.

## Fixtures

- `testdata/reference_task.ir.json`, `testdata/reference_epic.ir.json` — the
  config module's golden IR files, consumed as-is (the executor's input
  contract is those bytes).
- Small hand-written IRs: a diamond (a -> b,c -> d), two independent chains,
  a wide fan-out (12 parallel nodes).
- Scripted data-source outputs: three items with `I-3.deps = [I-1, I-2]`,
  empty set, malformed JSON, duplicate ids, in-set dep cycle.

## Scenarios

1. **Diamond order and parallelism.** On the diamond with latency-scripted
   steps: b and c run concurrently (overlapping intervals observed), d starts
   only after both; with `--max-parallel 1` they serialize in node-ID order.
2. **Deterministic tie-break.** The 12-way fan-out dispatched with
   `--max-parallel 1` yields the identical dispatch sequence across 50 runs.
3. **Task loop settles on iteration 2.** Reference task IR; scripted verdicts
   `changes`, then `approved`: fix@1 runs, gate `!(until@1)` is true for
   iteration 2, review@2 approves, iteration 3 settles `skipped-condition`
   via the skipped-dep short-circuit, the review selector adopts review@2's
   payload, merge runs. Terminal map matches the expected states exactly.
4. **Loop exhaustion.** Three `changes` verdicts (each status ok — unfavorable
   is not failure): the review selector settles failed (loop-exhausted), merge
   settles `skipped-dependency` naming the selector, and the report's failure
   block says which bound was exhausted.
5. **Failure propagation spares independent branches.** Two independent
   chains, first node of chain A scripted to fail after `retry: 2`: all of
   chain A settles `skipped-dependency` with the root ancestor recorded on
   every record; chain B settles fully ok; run exit is nonzero.
6. **Defer and re-admission.** Meter scripted to `defer(until)` a node twice,
   then admit; separately a step failure carrying a rate-limit reset:
   both re-enter at the injected clock's wake, consume no retry attempt, and
   the report line shows the defer count and total wait.
7. **Epic fan-out respects item deps.** Reference epic IR + the three-item
   source: instances `epic/tasks[I-1]` and `[I-2]` run concurrently,
   `[I-3]`'s sources become ready only after both instances' sinks settle;
   each instance's merge runs under its own subgraph; the generate node's
   payload records `{count: 3}`.
8. **Fan-out cascade.** Same, with `[I-1]`'s implement scripted to fail:
   all of `[I-1]` and all of `[I-3]` (dep edge) settle `skipped-dependency`,
   `[I-2]` completes ok, and the rollup reads `3 items: 1 ok, 2 failed/skipped`
   with `[I-1]` as the named root cause.
9. **Resume from journal.** Run scenario 3 killing the scheduler (context
   cancel) after implement settles; re-run with the same journal: implement is
   a cached hit (StepRunner never invoked for it), execution restarts at
   review@1, and the final report marks implement `cached`. With `--no-cache`
   every node re-runs.
10. **Report golden files.** The settled runs of scenarios 3, 4, and 8 render
    text and `--json` reports matching golden files byte-for-byte; the JSON
    report of a run reconstructed from the journal alone (fresh reporter, no
    scheduler state) is identical to the one produced at settle time.

## Edge cases

- Empty item set: generate node settles ok, zero instances, dependents
  proceed, report shows the fan-out summary `0 items`; exit 0.
- Malformed source output, duplicate ids, missing `${item.field}`, and an
  in-set dep cycle: each fails the generate node with the contract error
  naming the violation; no instance node was ever created.
- Deps pointing outside the item set are ignored: no edge, no error.
- Meter `reject` settles the node failed with the budget error; propagation
  is indistinguishable from any other failure.
- Condition evaluation error against a well-typed activation (scripted
  hostile payload) fails the node — never silently false.
- A journal that refuses appends aborts the run process-level; the partial
  journal still renders a report with `absent` lines for undispatched nodes.
