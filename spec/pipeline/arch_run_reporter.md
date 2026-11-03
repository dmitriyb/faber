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
emits lines in a stable order: the JSON's per-node list in the IR's
canonical (sorted id) order, the human text in **dependency order** — a
topological walk over the IR's edges and condition reads with id
tie-breaks, so `implement`, then the review loop, then `merge`, the way the
workflow reads — with generate instances ordered by their target IR the same
way. Two runs of the same config order their reports identically regardless
of completion interleaving. Nodes present in the IR but absent from the journal (a run that
died mid-flight) are reported as `absent` rather than omitted — the report
never silently shrinks.

## Per-step lines

One line per node, in the human vocabulary: `ok`, `FAIL`, `HALT`, `not
needed` (a guard that was false inside an iteration that ran), `not reached`
(past a failure or halt, naming the ancestor — a selector ancestor reads as
its loop), `absent`; lower-case for the routine, upper-case for what needs
the eye, plain ASCII throughout. Names read relative to their scope (the
workflow prefix at top level, the iteration inside a loop, the instance
inside a generate). Each line carries terminal status, wall-clock duration, attempt count, and
the payload's key output fields (`branch=…, pr=17`, `verdict=approved`).
Journal-hit steps are marked cached; steps whose agent phase was skipped by
the prelude are marked agent-skipped (cost honesty at a glance);
deferred-then-resolved steps carry their
defer count and total deferred wait. Selector lines name the candidate they
resolved to.

## Loop rollups

The IR unrolls every loop, which is right for execution and wrong for a
reader: an 8-cycle epic under `max: 45` is 856 nodes, ~790 of them skips,
and a footer of "891 skipped" says nothing about where the run got to. The
reporter folds each loop back — detected from the `<loop>@<i>/` segment of
node ids, the cap taken as the highest iteration present in the IR (or, for
generate instances, in the target IR or the journal), independent of any
selector — into a rollup: iterations ran of up to cap, the executed
iterations with their lines nested (loops nest recursively), and one range
line for the rest. The grouping rule is journal plus IR alone, so a crashed,
a killed, and a settled run roll up identically: an iteration with no
executed node is unused cap (listed only as the "of up to"); a
condition-skipped node inside an executed iteration is `not needed`; a
dependency- or halt-skipped node is `not reached`; an iteration with no
record is `absent`. The selector that coalesces the loop is consumed into
the rollup line (`-> review@2`, or `exhausted` when it settled
loop-exhausted) and never listed as a step. A loop nothing ran in is one
line: `not reached  review-cycle  after implement`.

The footer counts what the text lists — `ran N ok, M failed[, H halted];
not run X not needed, Y not reached[, Z absent]` — never the unused cap and
never a selector; the JSON totals keep every node's state.

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

## Halt diagnostics

A `halted` step gets the same first-class treatment: a dedicated block naming
the halting step, its halt reason, and detail — the operator reads why the
run stopped without parsing JSON — and each `skipped-halt` line names the
halting ancestor. Halted and halt-skipped steps have their own totals in the
footer.

## Output modes

- **Human text** to stdout: the ordered lines above with loops folded, a
  run footer (ran / not run groups, wall-clock, journal path).
- **`--json`**: the same content as one machine-readable document —
  `{run: {workflow, config_hash, ir_hash, params, totals}, steps: [...],
  loops: [...], generate: [...]}` — stably ordered and suitable for diffing
  or piping into the user's own tooling. `steps` stays complete (every IR
  node, scheduler encodings intact) and `loops` is additive, so the report
  never shrinks for machines while the text folds for humans.

## Progress renderers

The run's live surface is the scheduler's event stream
(`arch_scheduler.md`, "Progress events") rendered by one of two sinks the
cmd wiring selects from `--progress`:

- **Text** (a terminal): one ASCII line per event that matters —
  `HH:MM:SS  <status>  <breadcrumb>  <duration>  <reason>`. The breadcrumb
  folds the node id into iteration and cap (`cycles 8/45 > review-cycle 1/5
  > review`), with caps learned from the `run-started` event (every loop the
  IR unrolls) and from each `generate-expanded` (the instance loops). A
  plain terminal cannot rewrite a printed line, so the sink prints at start
  and at settle only; elapsed appears at settle. The scheduler's skip
  settlements collapse into what they mean: `ready  <crumb>  waiting for
  lane landing 1 of 1` for a held node, `not needed` for a guard inside
  a live iteration, one `settled after n of cap` line when a loop converges
  (nothing for the unused cap), and one `not reached <loop> @a..@b after
  <ancestor>` range per loop at run end. Colour, when the CLI's probe allows
  it, touches the status word only and uses the 16 palette slots (green ok,
  bold red FAIL/HALT, cyan start, yellow WAIT/PAUSE, bright black not
  needed/not reached) — never 256-colour, never a background — so the
  terminal theme owns every colour and a colour-stripped line loses nothing.
- **JSON lines** (not a terminal, or `--progress json`): one object per
  event, every field, for tooling.

A run whose last run-end record says `paused` is flagged in both outputs: the
JSON report carries `paused: true` and the human footer names the pause and
the resume command — a paused run must read as deliberately incomplete, not
as a crash (its undispatched steps report `absent` either way).

The process exit code follows the run's terminal states: exit 1 when any step
settled `failed`; exit 3 when no step failed but at least one settled `halted`
(the run stopped for an operator — resumable, not broken); exit 4 when the
run ended in a cooperative pause with nothing failed or halted; exit 0
otherwise.
A run whose only non-ok states are condition-skips is a success — a skipped
`fix` step is the workflow working as declared. The mapping comes from the
scheduler's own counters, never from re-reading the journal at report time.

## Deferred seam

Live observability is reserved: step log streaming and traces, a status view
that tails the journal of an in-flight run, and inspection of a running
workflow. The journal-only derivation is exactly what keeps that seam cheap —
a live view is this same reporter over a journal that hasn't stopped growing.
The live view exists: the tui module is this reporter plus the replay's
starts over a journal that hasn't stopped growing, with `ReadingOrder()`
the seam it walks. What remains reserved is tracing beyond `output.log` and
the session transcripts.

Requirements implemented: Run reporting, Human run vocabulary, Deferred:
observability.
