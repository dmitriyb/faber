# Journal — the run's durable, append-only state

## What it is

The single durable artifact of a run: an append-only JSONL file in a per-run
directory. Every completed step attempt lands here as its result record;
replaying the file reconstructs scheduler state exactly (which nodes settled,
with what status, with which outputs), which is the entire basis of resume.
Nothing else the engine holds in memory is durable — the journal *is* the run.

## Keying: (step-id, input-hash)

Result records are keyed by the pair:

- **step-id** — the IR node's path-like id (`task/review-cycle@2/fix`, or the
  generate-instance-scoped form for fan-out items). Stable across runs because
  desugaring is deterministic.
- **input-hash** — a hash over the canonical encoding of the step's resolved
  input values, its template identity, and its image tag.

The hash answers "is a prior result reusable?" precisely: same slot values fed
to the same template running the same image ⇒ the lambda would compute the
same thing, so the journaled record may stand in for a re-run. Any of the
three changing — an upstream output differs, the template was edited, the
image was rebuilt — changes the key, and resume re-runs the step. Because a
step's resolved inputs include upstream payloads, an input-hash is computable
only when the step becomes ready; journal lookup is therefore a readiness-time
decision, not an upfront partition of the graph.

## Record kinds

1. **Header** — first line, written at run start: `{run id, config hash,
   workflow name, supplied params, IR hash}`. Resume compatibility is defined
   against this line: the config module's pipeline is deterministic, so "same
   IR hash" means "same graph", and a mismatch is detected before any skip
   decision is trusted.
2. **Result** — one per settled step attempt sequence: the final ResultContract
   record (with attempt history) under its `(step-id, input-hash)` key. Loop
   exhaustion arrives here as a perfectly normal failure record — the selector
   settles `failed` with reason `loop-exhausted` after max iterations; the
   journal has no special loop state.
3. **Cost** — the metering module's `actual(result)` output per step, keyed the
   same way, so run-level cost aggregation is a journal fold and a resumed run
   does not double-count skipped steps.
4. **Cleanup** — an `on_failure` hook's outcome (ran clean / itself failed),
   attached to the step it cleaned up after; reported alongside, never
   replacing, the failure it followed.

Records are appended in settlement order; within one step, a later result
record for the same key supersedes an earlier one (retry sequences journal
only the final record, but a resumed run appends a fresh record after a
re-run rather than editing history — append-only means no record is ever
rewritten).

## Consumers

Resume (RecoveryModes) replays the journal into a lookup map the Scheduler
consults at readiness. The RunReporter folds the journal into the human
summary and JSON report. The interactive recovery mode reads a failed record's
handoff pointer. All three read the same bytes; there is no second store.

## Deferred seam: store location and cross-run concurrency

Reserved (design edge case 10): a canonical journal-store location and
coordination between concurrent runs — two runs claiming the same generate
item need dedup or locking to avoid doing the work twice. The first pass sides
with simplicity: one journal directory per run (created under a run-scoped
path, named by run id), no cross-run index, no locking. Concurrent runs over
the same items are the operator's responsibility until this seam is designed.

Requirements implemented: Run journal; Deferred: journal store concurrency.
