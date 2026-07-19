# Data flow: Failure and resume flow

What a step's result record sets in motion — at failure time within a run, and
at re-entry time across runs.

## Failure path (within a run)

```
box result dir ──ResultExtractor──► Result (schema-validated)
        │
        ▼
FailurePolicy.RunStep
        │  status == failed?
        │      │
        │      ├─► on_failure script (host-side)
        │      │      env: FABER_INPUT_* (resolved inputs)
        │      │      stdin: error record JSON
        │      │      outcome ──► cleanup record (never masks the failure)
        │      │
        │      └─► attempts left? ──yes──► re-run whole step (fresh container)
        │                          no
        ▼
final Result {status, attempt, attempts[]}
        │
        ├──► Journal.Append(result record @ (step-id, input-hash))
        ├──► Meter.Actual(result) ──► Journal.Append(cost record)
        └──► Scheduler:
                ok      → payload threads to dependents
                failed  → transitive dependents marked skipped (dependency
                          failed); independent branches keep running
```

## Re-entry path (across runs)

```
faber resume <run>
        │  Load(journal): header + last-wins {(step-id, input-hash) → result}
        │  re-desugar config ──► IR; hash & params must match header, else refuse
        ▼
Scheduler (normal execution) ── step becomes ready ──► compute input-hash
        │                                                      │
        │                            ┌─────────────────────────┤
        │                            ▼                         ▼
        │                    hit, status ok            miss / status failed
        │                    skip; reuse payload       run the step normally
        ▼
faber resume <run> --fresh   → new run dir + journal, empty prior map, no lookups
faber resume --interactive <run> <step>
                       → rebuild failed step's box (image, bindings, inputs,
                         handoff ro-mounted) → operator shell → no journal write
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| ResultExtractor → FailurePolicy | `Result` | validated union: ok+payload xor failed+error; payload already schema-checked |
| FailurePolicy → on_failure script | env + stdin JSON | resolved inputs as `FABER_INPUT_*`; `ErrorRecord` on stdin; exit code is the only return |
| FailurePolicy → Journal | `ResultRecord`, `CleanupRecord` | one result record per step per run (attempt history inside); cleanup records additive |
| Metering → Journal | `CostRecord` | keyed like the result record; skipped steps on resume emit none |
| Journal → RecoveryModes | header + `map[Key]ResultRecord` | last-wins replay; torn tail line dropped with a warning |
| RecoveryModes → Scheduler | `Lookup(stepID, inputs, tmpl, image)` | readiness-time; hit(ok) ⇒ skip with reused payload, else run |
| RecoveryModes → security/infra | BindingSet + shell entry | interactive only; teardown guaranteed, journal untouched |

## Invariants the flow preserves

- Every settled step leaves exactly one result record; failure is never an
  absence, and an absence (crash before append) reads as "never ran".
- The record that threads data is byte-identical to the one journaled and
  metered — no consumer sees a private variant.
- Cleanup outcomes travel beside failures, never in place of them.
- Loop exhaustion and failed generate items enter this flow as ordinary
  failure records; neither has a special path.
- Re-entry decisions derive only from (journal bytes, deterministic IR); no
  in-memory state of the dead run is needed.
