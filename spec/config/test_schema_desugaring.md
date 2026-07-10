# Test section: Schema and desugaring tests

Integration/acceptance scenarios spanning SchemaTypes, Loader, Desugarer, and
IRModel — the path from YAML text to canonical IR. (Unit tests live beside the
code; these are the module-level behaviors that must hold.)

## Fixtures

- `testdata/reference.yaml` — the full reference orchestrator.yaml from
  `spec/test_reference_workflows.md` (task + epic).
- `testdata/reference_task.ir.json`, `testdata/reference_epic.ir.json` — golden
  IR files, reviewed by hand once, regenerated only deliberately.
- A library of minimal invalid configs, one per Loader check.

## Scenarios

1. **Round trip.** Loading `reference.yaml` yields a Config whose every field
   survives a marshal/unmarshal cycle unchanged; template and workflow maps carry
   all four templates and both workflows.
2. **Union enforcement.** A step with both `use:` and `loop:` (and one with
   neither) each yield exactly one violation naming the step path and the union
   rule.
3. **Loader check catalog.** Each invalid fixture produces its expected
   field-path error and no others: duplicate step id inside a loop body, unknown
   identity in `run.identity`, `${item.x}` outside a generate binding,
   `${sources.x}` used as a `with:` value, remote with both `host_key_file` and
   `tofu`, unknown `mode` in credentials.
4. **Golden desugar (task).** Desugaring `task` matches the golden file
   byte-for-byte: 3 unrolled iterations with gate conditions
   `!(steps.review@1.verdict == "approved")` etc., ordering chain between
   iterations, selectors for `review` (referenced post-loop) with candidates
   `[review@3, review@2, review@1]`, merge wired to `implement.pr` via the data
   edge and to the selector via its `when` deps.
5. **Golden desugar (epic).** The generate node carries the source ref, workflow
   name `task`, and the item binding template; the `task` sub-IR is NOT inlined
   (run-time expansion) but was independently desugared and checked.
6. **Determinism.** Desugar the reference 100 times shuffling map iteration
   (`GODEBUG=randseed` variations or an explicit map-order fuzz harness): byte
   output identical. Two configs differing only in YAML key order and comments
   produce identical IR.
7. **Workflow-reference cycle.** `a` uses `b` uses `a`: desugar-time error naming
   the cycle path; no partial IR emitted.
8. **Unroll bound.** A loop with `max: 10000` over a 2-step body trips the
   sanity ceiling with an error naming the loop's step id.

## Edge cases

- Empty `steps:` — Loader error, not a desugar panic.
- Loop with `max: 1` — body emitted once, no gate conditions, selector still
  emitted for post-loop refs.
- `when:` on a loop-step itself applies to every iteration's nodes (conjoined
  with gates).
- A template and workflow sharing a name — Loader rejects; the Desugarer never
  sees the ambiguity.
