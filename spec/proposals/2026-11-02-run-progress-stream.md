# Change Proposal: Run progress stream, durable step starts, per-attempt output

## Context

An operator watching a run today sees six slog info lines — step started,
settled, failed, halted, deferred, generate expanded — interleaved with every
other info/warn/debug record the engine emits, in slog's own key=value shape,
on the same stderr. The run's heartbeat and its diagnostics are one stream
with one level knob, so the terminal shows
`time=… level=INFO msg="step started" step=… run=…` for the heartbeat and the
`host config` dump at every start, while the thing the operator wants —
where the run is, and since when — has no dedicated surface.

The journal, which is the run's durable state and the reserved seam for any
live view (`arch_run_reporter.md`, "Deferred seam"), records only
settlements: results, costs, cleanups, defers, run-end. A step that has been
running for twenty minutes does not exist in the journal until it settles, so
no reader of the journal — a second terminal, a crash post-mortem, a future
status view — can say "running since".

The container's combined output is forwarded line by line at debug level and
kept only as a bounded in-memory tail for the failure record. `--sessions`
captures the harness's own transcript, which is a different artifact: the
hook output, faber-box's phase lines, and the agent's terminal output have no
durable home. A hook failure in a step that ran for an hour leaves a 256 KiB
tail inside a failure record and nothing else.

These three gaps are one design gap: progress is a side effect of logging
instead of a first-class, durable event stream. Every later surface — the
loop-aware human renderer, the report rewrite, a TUI — needs the stream, so
it comes first and alone.

## Proposed change

- **Typed progress events.** The pipeline scheduler emits a typed event at
  each point that already logs a heartbeat line: step started, step settled
  (ok / failed / halted / the skip states), step deferred, generate expanded,
  pause requested, run ended. The executor exposes an event sink seam
  (`Events`, nil = none) that the cmd wiring fills; the pipeline core never
  renders. The six heartbeat slog lines stay for diagnostics but drop to
  debug — the event stream is now the operator surface, slog is not.
- **Durable step start.** A new journal record kind, `start` (`step id`,
  `attempt`, `started`), is appended the moment a box attempt launches,
  mirroring how `defer` is appended the moment a defer happens. Additive
  within format 1: replay exposes starts beside results, so any journal
  reader derives "running since" for a step with a start and no later
  settlement. Resume semantics are unchanged — a start never keys anything
  and is never a lookup hit; it is the timeline, not the state.
- **Per-attempt output file.** The container runner's combined
  stdout+stderr, already tee'd into the bounded tail and the debug logger,
  additionally streams into `<run>/boxes/<step>/attempt-<n>/output.log`.
  Always on: it is bounded by the run directory the attempt already owns,
  costs one file, and is the only durable record of hook and phase output.
  The in-memory tail and the debug forwarding are unchanged; the pre-attempt
  scrub that clears a reused attempt dir clears it too (it is this attempt's
  output, exactly like `result/`).
- **Diagnostics recede on a TTY.** With progress on its own stream, slog's
  default level becomes `warn` when stderr is a terminal and no `--log-level`
  is given (`info` stays the default for non-TTY stderr, where the JSON
  handler feeds machines). The `host config` line moves to debug. Explicit
  `--log-level` beats both, as it does today.
- **Rendering is out of scope.** This proposal defines the stream and the
  records. The human line renderer, its vocabulary, colour, and the report
  rewrite are the next proposal; the cmd wiring in this one attaches only a
  minimal line sink so the stream is visible end to end.

## Impact expectation

- **pipeline**: the `Event` type and sink seam; emission at the scheduler's
  existing heartbeat points and the executor's run-end; heartbeat slog lines
  to debug; `RunAttempt` opens the per-attempt `output.log` and passes it to
  the container runner.
- **failure**: the `start` record kind — append, replay exposure, torn-tail
  behaviour unchanged; spec of the journal's record kinds.
- **infra**: `RunSpec` carries an optional output writer the runner tees
  into beside the tail buffer and the debug logger.
- **config**: TTY-dependent default log level; wiring-level `host config`
  line to debug; command docs.
