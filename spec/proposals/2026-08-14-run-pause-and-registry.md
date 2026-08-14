# Change Proposal: Pause, run names, and the `runs` command group

## Context

faber has no way to stop a run without losing work, and no way to see what
runs exist. Interrupting a run (SIGINT/SIGTERM) aborts immediately: the
scheduler stops dispatching, cancels context, and the in-flight box dies
mid-step — its tokens are spent, its result is never journaled, and resume
re-runs the step from scratch. For long agentic runs against metered
endpoints this is the wrong tool: the operator usually wants "finish what
you started, then stop", so the run can be resumed later without repeating
the step that was already paying for itself. Managing rate and budget
windows is the driving case.

Separately, the run store has no operator surface. Run directories
accumulate under `<state>/runs/` with no command to list them, no way to
tell live from finished from crashed, and no cleanup short of `rm -rf`.
`Store.AuditRuns` already enumerates run dirs with liveness and
completeness probes, but its only consumer is the pre-upgrade guard.

Runs are addressable only by the minted run id
(`20060102-150405-<8 hex>`), which is unfriendly to type and impossible to
recognize in a listing of many runs of the same workflow.

## Proposed change

### Pause

A cooperative, durable pause:

- **Requesting.** `faber runs pause <ref>` writes a pause marker file into
  the run's directory. The command requires the run to be live (run-lock
  flock probe); pausing a non-live run is an error. The marker is the
  mechanism — no signals, no pid trust, no IPC; the request survives races
  and process boundaries because the scheduler re-derives it from the
  durable store.
- **Honoring.** The scheduler checks the marker at its scheduling points.
  When present it stops granting box slots and stops dispatching ready
  steps, and lets every outstanding worker settle normally — the same
  drain-and-settle discipline the fatal path already uses. Settled steps
  are journaled as usual, so their `ok` records are ordinary resume reuse
  hits. When nothing is outstanding, the run ends with a run-end record of
  a new status `paused`, beside `settled` and `aborted`.
- **Whole-run semantics.** Pause drains the entire run: with parallel
  steps in flight, each finishes and settles, and nothing new dispatches.
  Loops need no special handling — iterations are unrolled at desugar
  time, so "pause after the current step of the current cycle" is exactly
  "stop dispatching new steps".
- **Exit code.** A paused run exits 4 (0 = success, 1 = failure,
  2 = usage, 3 = halted). A supervising script branches on the code, never
  on report text.
- **Resume.** Unchanged `faber resume <ref>`: journal reuse already
  re-enters at the first step without an `ok` record, which after a clean
  pause is precisely the next step. Resume clears a stale pause marker on
  start, so a marker written while the run was down cannot re-pause the
  resumed run.
- **Not an abort, not a halt.** Pause is an operator request recorded
  outside the workflow; it involves no step result, no failure policy, no
  `halt.json`. A step that fails or halts while the run is draining keeps
  its own status; the run-end status is `paused` only when the drain
  itself is what ended the run.

### Run names

`faber run --name <name>` stores an optional human name in the journal
header. Commands that take a run reference (`resume`, `runs pause`) accept
a run id or a name; resolution enumerates run directories and reads
headers — no cross-run index is introduced. An ambiguous name is an error
naming the matching run ids; an id always wins over an equal name.

### The `runs` group

One new top-level command group, so the surface grows by one word:

- `faber runs` — list runs: id, name, workflow, state (`live`, `paused`,
  `settled`, `aborted`, `incomplete`), started timestamp. `--json` emits
  the same rows machine-readably. Built on `AuditRuns` plus header reads.
- `faber runs pause <ref>` — as above.
- `faber runs prune` — delete run directories that are finished (run-end
  record present) and not live. Paused runs are kept by default — they are
  resumable state by design. `--all` additionally removes paused and
  incomplete non-live runs (crashed or abandoned); live runs are never
  touched.

`resume` stays top-level: it is a lifecycle verb of the same rank as
`run`, not run-store administration.

## Impact expectation

- **failure**: `paused` run-end status; pause-marker write/probe/clear on
  `Store`; optional name in the journal header; name-or-id resolution over
  the run root; prune deletion with the liveness guard.
- **pipeline**: scheduler pause gate at the scheduling points (reusing the
  drain-and-settle path), run-end emission with `paused`, report block and
  footer for a paused run, exit-code mapping via a typed error carrying
  exit 4 (like halt's).
- **config**: `runs` command group (list/pause/prune, `--json`, `--all`),
  `--name` on `run`, exit-code contract documentation.
- **spec/docs**: scheduler, recovery-modes, journal, and CLI leaves;
  test sections for the three modules; `docs/commands.md`.
