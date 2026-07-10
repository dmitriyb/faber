# RunReporter — journal-derived run summary

## What it is

The component that turns a settled run into something a person (or a calling
program) can act on. Its defining constraint: the report is **derived from the
journal alone**, never from the scheduler's in-memory state. A run that
crashed, was killed, or is being inspected days later reports identically to
one summarized the moment it settled — the journal is the single source of
truth, and the reporter is a pure function of it plus the IR.

## Inputs and ordering

The reporter reads the journal's header (config hash, workflow, params, IR
hash) and its append-only records, joins them against the IR's node list, and
emits lines in **stable config order** — the IR's canonical node order, whose
path-like IDs reproduce the workflow's declared structure. Two runs of the
same config order their reports identically regardless of completion
interleaving. Nodes present in the IR but absent from the journal (a run that
died mid-flight) are reported as `absent` rather than omitted — the report
never silently shrinks.

## Per-step lines

One line per node: terminal status, wall-clock duration, attempt count, and
the payload's key output fields (`branch=…, pr=17`, `verdict=approved`).
Journal-hit steps are marked cached; deferred-then-resolved steps carry their
defer count and total deferred wait. Selector lines name the candidate they
resolved to.

## Generate rollups

Nodes under a generate instance prefix (`epic/tasks[I-3]/…`) are grouped per
item: a rollup line per instance (item id, aggregate status, duration, counts
of ok/failed/skipped nodes) with the instance's own step lines nested beneath,
and a fan-out summary on the generate node's line (`3 items: 2 ok, 1 failed`).
A partial fan-out is thus legible at a glance without scanning every instance.

## Failure diagnostics

Each `failed` step's block carries the structured error record — reason,
detail, and the **handoff pointer** (the path to the failed attempt's handoff
record and result directory) — plus the retry history and the re-entry
command (`faber resume … --interactive <node-id>`). Each `skipped-dependency`
line names its failed ancestor, so a cascade reads as one root cause, not
thirty mysteries.

## Output modes

- **Human text** to stdout: the ordered lines above, a run footer (totals per
  terminal state, wall-clock, journal path).
- **`--json`**: the same content as one machine-readable document —
  `{run: {workflow, config_hash, ir_hash, params, totals}, steps: [...],
  generate: [...]}` — stably ordered and suitable for diffing or piping into
  the user's own tooling.

The process exit code follows the report: nonzero iff any step settled
`failed`. A run whose only non-ok states are condition-skips is a success — a
skipped `fix` step is the workflow working as declared.

## Deferred seam

Live observability is reserved: step log streaming and traces, a status view
that tails the journal of an in-flight run, and inspection of a running
workflow. The journal-only derivation is exactly what keeps that seam cheap —
a live view is this same reporter over a journal that hasn't stopped growing.
The first pass ships structured slog lines during execution plus this post-run
report, and nothing else.

Requirements implemented: Run reporting, Deferred: observability.
