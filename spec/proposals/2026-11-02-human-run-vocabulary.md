# Change Proposal: Human run vocabulary — loop-aware report and progress rendering

## Context

The IR unrolls every loop at desugar time: a `cycles` loop with `max: 45`
over a body of a dozen steps is several hundred nodes before anything runs,
and `review-cycle` with `max: 5` inside each cycle multiplies it. That is
correct for execution — the scheduler never has to understand a loop — and
wrong for a human. The post-run report prints one line per node in sorted id
order and counts every unrolled node in its totals. On a real 8-cycle epic
run the report is 856 lines, around 790 of them `skipped-condition` or
`skipped-dependency`, and the footer reads `66 ok, 1 failed, 891 skipped
(condition), 721 skipped (dependency)`. The 891 are mostly iterations past
the point where the loop's `until` held: not steps that were skipped, the
unused part of a cap. The 721 are everything past a failure, re-journaled at
every resume. Neither number says where the run got to.

The progress stream from the previous proposal has the same problem one
level down: its minimal line sink prints raw node ids
(`epic/cycles@8/review-cycle@1/review`), one line per node, skip lines
included, with no notion of iteration or bound. And it prints the same bytes
to a terminal and to a file.

"Skipped" is the scheduler's word. It covers three different things that an
operator needs told apart:

1. iterations past convergence — the cap's unused part, not steps at all;
2. a guard that was false inside an iteration that ran (`fix` when the
   review approved) — a declared branch not taken, worth one line;
3. steps past a failure or halt — not reached, and exactly the count resume
   will have to do.

All three are `skipped-condition` or `skipped-dependency` in the journal, and
that encoding must stay: resume and the exit code depend on it. Only the
human renderings change.

## Proposed change

- **Vocabulary.** Human text — the report's text form and the progress
  lines — stops using the scheduler's skip encodings. A loop renders as
  `n of up to cap`; iterations past convergence are not listed (the "of up
  to" carries the count); a guard-false step inside an executed iteration is
  `not needed`; steps past a failure or halt are `not reached`, with the root
  ancestor and the range. Status words are lower-case for the routine
  (`ok`, `start`) and upper-case for what needs the eye (`FAIL`, `HALT`,
  `WAIT`). Plain ASCII only: no glyphs, every character one cell.
- **Loop rollups in the report.** `RunReport` gains a loop rollup beside the
  generate rollup: for each loop (detected from the `<id>@<i>/` segment of
  node ids; the bound is the highest iteration present in the IR), the
  iterations that executed with their body lines, then one line for the rest.
  The grouping rule is derived from journal plus IR alone, so it holds for a
  crashed, a killed, and a settled run alike: an iteration with no executed
  node is unused cap; a condition-skipped node inside an executed iteration
  is `not needed`; a dependency- or halt-skipped node is `not reached`.
  Nested loops roll up inside their parent iteration; generate instances
  keep their rollup and get loop rollups inside it. The text footer becomes
  two groups, `ran: N ok, M failed[, H halted]` and `not run: X not needed,
  Y not reached`, plus the loop line per top-level loop. The JSON report
  keeps every `StepLine` exactly as today — machine consumers and the
  never-shrinks rule depend on it — and adds `loops`.
- **The progress renderer.** A real human sink replaces the minimal line
  sink: a breadcrumb from the node id with iteration and bound
  (`cycles 8/45 > review-cycle 1/5 > review`), an aligned status column, the
  duration at settle, `not needed` lines for guard-false steps inside a live
  iteration, and no line at all for unused-cap or not-reached settlements —
  those collapse into one line when the loop settles
  (`review-cycle  settled after 1 of 5`) or when a failure propagates
  (`cycles  @9..@45 not reached after cycles@8/review-cycle@1/review`).
  Colour is additive and never carries meaning alone: the status word only,
  from the 16 palette slots (green ok, bold red FAIL and HALT, cyan start,
  yellow WAIT, bright black not needed and not reached), so the terminal
  theme owns every colour. A JSON-lines sink serves machines.
- **Terminal probe and flags.** Colour is on only when stderr is a TTY,
  `TERM` is not `dumb`, and `NO_COLOR` is unset; `--color auto|always|never`
  overrides. `--progress text|json|none` selects the sink (`auto`: text on a
  TTY, json otherwise). Both on `run` and `resume`.
- **Out of scope.** A live view, a TUI, ticking elapsed counters: a plain
  terminal cannot rewrite a printed line, so the renderer prints at start
  and at settle only. The screen is a later proposal.

## Impact expectation

- **pipeline**: loop rollup derivation and the new text rendering in the
  reporter (JSON additive); the progress renderer (breadcrumb, collapse,
  colour) and the JSON-lines sink replacing the minimal line sink; golden
  reports regenerated.
- **config**: `--color` and `--progress` on run and resume, carried in
  `RunOptions`; the terminal probe beside `InitLogging`; cmd wiring selects
  the sink; command docs.
