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

## Schema version: format 1

The journal owns an on-disk schema version, stamped as `format` in the
header — **independent of the application version**, bumped only when the
record shapes actually change. Replay fails closed on any other stamp (a
higher stamp names the newer writer; a lower or absent stamp names the
no-auto-migration rule): there is no migration path — across a schema bump an
in-flight run is finished on the faber that wrote it or restarted `--fresh`.
Header-only reads (the CLI's early guards, the upgrade audit) stay
format-tolerant; only interpretive replay refuses.

## Record kinds

1. **Header** — first line, written at run start: `{format, run id, config
   hash, workflow name, supplied params, IR hash, IR version, resolved
   per-template image tags}`. Resume compatibility is defined against this
   line: the config module's pipeline is deterministic, so "same IR hash"
   means "same graph", and a mismatch is detected before any skip decision is
   trusted. The IR version distinguishes an engine-side IR schema change from
   operator config drift, so refusals blame the right party. The image tags
   record the engine-compiled inputs (default nixpkgs pin, image schema) the
   IR hash cannot see: resume recomputes and compares them, so a pin or
   engine upgrade fails closed instead of silently invalidating every journal
   key.
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
5. **Defer** — appended the moment a step defers (`{step id, until,
   detail}`; zero until = the re-check-on-next-settlement shape). A waiting
   state is durable state: a crash mid-wait leaves the timeline on disk
   instead of losing it with the never-settled node.
6. **Run-end** — appended once per execution when the scheduler returns:
   `{status: settled|aborted, failed count, finished}`. Its absence is the
   durable signature of an interrupted run — exactly what `faber
   upgrade-check` looks for. A resumed run appends a fresh marker when it
   finishes; replay is last-wins. Unknown kinds are still skipped on replay
   (additive evolution within one format), but never silently — replay logs
   what it ignored.

Records are appended in settlement order; within one step, a later result
record for the same key supersedes an earlier one (retry sequences journal
only the final record, but a resumed run appends a fresh record after a
re-run rather than editing history — append-only means no record is ever
rewritten).

## Same-run exclusivity: one appender per run

A run accepts exactly one appending process at a time. The run directory
carries an advisory `flock(2)` lock (`run.lock`) that `Begin` (fresh) and
`Resume` acquire non-blocking before any journal read, torn-tail repair, or
append, and hold for the process lifetime; the kernel releases it when the
process exits, however it exits, so there is no stale-lock recovery and a
leftover lock file grants nothing. A second process meeting a held lock is
refused loudly, naming the recorded holder pid. This is what makes torn-tail
repair safe — repair runs only under the lock, so it can never truncate a
line another live process just committed — and what keeps two processes from
adopting each other's attempt directories. Read-only consumers (reporting,
interactive observation) take no lock: replay already tolerates a torn tail.

## Consumers

Resume (RecoveryModes) replays the journal into a lookup map the Scheduler
consults at readiness, and folds the replayed cost records into the metering
admitter's spent ledger so a declared budget covers the whole logical run, not
just the segment since the last interruption. The RunReporter folds the
journal into the human summary and JSON report. The interactive recovery mode
reads a failed record's handoff pointer. All three read the same bytes; there
is no second store.

## Deferred seam: store location and cross-run concurrency

Reserved (design edge case 10): a canonical journal-store location and
coordination between concurrent runs — two runs claiming the same generate
item need dedup or locking to avoid doing the work twice. The first pass sides
with simplicity: one journal directory per run (created under a run-scoped
path, named by run id), no cross-run index, and no cross-run locking — the
`run.lock` above guards one run against a second process, not two runs against
each other. Concurrent runs over the same items are the operator's
responsibility until this seam is designed.

Requirements implemented: Run journal; Deferred: journal store concurrency.
