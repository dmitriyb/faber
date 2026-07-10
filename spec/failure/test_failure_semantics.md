# Test section: Failure semantics tests

Integration/acceptance scenarios spanning ResultContract, FailurePolicy,
Journal, and RecoveryModes — failure as data, cleanup, retry, durability, and
the three re-entries. (Unit tests live beside the code; these are the
module-level behaviors that must hold.) Scenarios run against a fake
StepRunner and a recording HookRunner unless a real container is stated;
"Mid-run kill and resume" doubles as the project-level acceptance scenario.

## Fixtures

- A scripted StepRunner whose per-attempt outcomes are declared up front
  (`fail, fail, ok`), each producing a well-formed Result.
- A recording `on_failure` script that dumps its env and stdin to a file, with
  a switchable exit code.
- A small three-step diamond graph (a → b, a → c, b+c → d) and a generate
  fixture whose data source emits three items with one dependency.
- Golden journal files for replay tests.

## Scenarios

1. **Failure is a record.** A step whose agent phase dies yields exactly one
   result record: `status: failed`, error with reason `agent`, detail
   non-empty, and a `handoff` path that resolves under the run dir to the
   preserved diagnostic state (context bundle + reason file present).
2. **Unfavorable is not failure.** A review-shaped step emitting
   `{verdict: changes}` settles `ok`; FailurePolicy runs no cleanup, consumes
   no retry, and the downstream `when:` condition — not failure propagation —
   decides what runs next.
3. **Fail-stop with independent branches.** In the diamond, b fails: d is
   marked skipped (dependency failed) and never launches; c completes; the run
   report shows ok/failed/ok/skipped-dependency.
4. **Cleanup contract.** On b's failure the recording hook received every
   resolved input as `FABER_INPUT_*` env and the exact error record on stdin.
   With the hook exiting 1: a cleanup record (ok=false) is journaled, b's
   original failure record is byte-identical to the no-hook run — reported,
   never masked.
5. **Retry with between-attempt cleanup.** `retry: 2` over `fail, fail, ok`:
   three fresh attempts, cleanup ran exactly twice (between 1→2 and 2→3), the
   single journaled record is `ok, attempt: 3` with two-entry attempt history.
   Over `fail, fail, fail`: cleanup ran three times (twice between, once
   terminal), final record `failed, attempt: 3`, dependents skipped once.
6. **Journal round trip.** Run the diamond, `Load` the journal: header carries
   run id, config hash, workflow, params, IR hash; the replayed map
   reconstructs every node's terminal state; cost records exist for each
   executed (not skipped) step.
7. **Input-hash sensitivity.** Same inputs/template/image hash identically
   across process restarts; changing one slot value, the template identity, or
   only the image tag each produces a different hash — and therefore a re-run
   on resume where an unchanged step is skipped.
8. **Mid-run kill and resume.** Kill faber after a settles and while b is
   in-flight: the journal has a's record only. Resume with the identical
   config: a is skipped by (step-id, input-hash) and its payload reused
   (asserted via d's inputs), b and everything after run; b's earlier
   half-attempt left no record and needed none.
9. **Resume refuses drift.** Edit the config (add a step) or change a param
   between runs: resume fails before launching anything, naming the IR-hash or
   param mismatch and suggesting `--no-cache`.
10. **Fresh ignores the journal.** `--no-cache` after a completed run re-runs
    every step under a new run id; the old journal is untouched.
11. **Interactive reconstruction.** After scenario 1, interactive re-entry on
    the failed step launches a container from the same image tag with the same
    network env, a single-key agent for the same identity, inputs in the env,
    and the handoff dir mounted read-only; exiting the shell tears down the
    agent and appends nothing to the journal.
12. **Failed generate item is ordinary.** One of three items fails: its record
    and its dependents' skips are normal journal entries; siblings complete.
    Resume re-invokes the data source, skips the completed instances by hash,
    and re-runs only the failed item's subgraph.
13. **Loop exhaustion record.** A selector settling failed after max
    iterations journals a normal failure record with reason `loop-exhausted`;
    resume treats it like any failed step (re-enters the loop chain, does not
    special-case it).

## Edge cases

- Agent succeeded but wrote no `result.json`: the engine's fallback record is
  `ok`, validates, journals, and resumes like any other.
- `retry: 0` / absent: exactly one attempt, empty attempt history.
- `retry` without `on_failure`: attempts proceed with no cleanup records.
- Torn last journal line (crash mid-append): `Load` drops it with a warning;
  the step reads as absent and re-runs.
- Interactive on a step that settled `ok`: refused, naming the step's state.
