# Screens — Home and Run over the model

## What it is

The two screens of the live view and the model they read. Three rules keep
the view honest and bound its scope: it shows nothing the CLI cannot show
(every screen is a command rendered live — `validate`, the preflight,
`runs`, the report), it does nothing the CLI cannot do (every action is a
`faber` command, spawned or named), and it holds no state the journal does
not (every tick and keystroke rebuilds the model from the store, so a crash
loses a cursor position).

## The model

- **Project**: `config.Load` → `Validate` → `Desugar` for every workflow
  (the same pipeline `faber validate` runs); on failure only the error is
  shown, since nothing else can run. Workflows with their typed params;
  templates with their image state from the injected probe (tag resolution
  plus a daemon existence check — the run preflight, shown before anyone
  presses run; `not checked` when no probe is wired).
- **Runs**: the store's tolerant audit scan plus each header and replay:
  the `faber runs` state vocabulary (LIVE from the run lock, then
  incomplete, then the run-end status, with a settled run reading `ok` or
  `FAILED` by its failed count), wall time (start to the newest record, or
  to now while live), and for a live run its newest running step.
- **Running since**: the journal's `start` records — a step whose last
  start has no result finished after it is running (or died mid-box when
  the run is not live). Derived, never tracked.
- **Run view**: the replay, the IR re-derived from the header's config path
  the way resume does (a config that no longer loads leaves the IR nil and
  the report journal-only), the reporter with every workflow as a generate
  target so instances read in dependency order, loop caps from the IR and
  the journal, the attempt directory for `output.log`, and the
  **medians**: per step name (an id's last segment, which repeats across
  loops, instances and runs) the median duration of every ok executed
  attempt in the store — the `usually 4m10s` beside a running step, `slow`
  past twice.

## Home

Header (the config path, the clock), a rule, two panes, a rule, the
selected run's block, the footer. The project pane lists workflows with
`name=<required>` / `name=default` params, the validate verdict, and the
image table (`ok built`, `MISSING faber build`, `not checked`). The runs
pane is the `faber runs` table with wall and a `now` column (the live run's
breadcrumb), folding to id / state / now under a hundred columns; the
selection is a full reverse-video row. Below: `now` (the running step with
elapsed and median), `last` (the newest settlement), `next` (what the run
waits on). The image table names the tag hash a missing image was looked
up under, so "MISSING" is checkable against `docker image ls`. Keys: `tab`
(or left/right) moves the focus between the runs and the workflows pane;
`j`/`k` move within it; enter opens the selected run, or — with the
workflows pane focused — the selected workflow's preview; `r` prompts `run
<workflow> [k=v …]` and spawns the child; `p` pauses a live run (and refuses, with a message, a
run that is not live — no marker is written); `R` spawns the resume; `q`
quits. Prune, upgrade and key management stay CLI-only.

## Run

Header (id, state badge, workflow, name, params; started and wall on the
right), the **now/next strip** — the newest running step with elapsed and
median, `+n more running`; or `nothing running · failed at <crumb> · next:
resume`; or the last settlement — a rule, then the left view beside STEP
over OUTPUT, and the footer.

- **tree**: the report's reading order (`pipeline.RunReport.ReadingOrder`)
  folded exactly as the text report folds it, plus live states: plain steps;
  loops as `<status> <name> n of up to cap [exhausted] [-> body@i]`; each
  executed iteration as `@i <status> <wall> <composition>` where the
  composition is the executed step kinds in order with multiplicity
  (`implement . review x2 . fix . merge`), expanded to its own rows when it
  failed, halted, holds a running node, or was opened with enter; not
  reached and absent ranges as one row; generate nodes with their items and
  the items' own reading order. The status column is the vocabulary of the
  report and the stream (ok, FAIL, HALT, not needed, not reached, absent,
  running); the selection is a full reverse-video row; the window scrolls
  to keep it visible.
- **graph**: the collapsed DAG — top-level steps, each loop as one node,
  generate nodes with an instance node each, selectors folded into their
  loops — with the IR's edges and condition reads mapped through the fold.
  Columns are longest-path depth from the sources; rows one barycenter pass
  over the previous column, id order breaking ties; boxes `[name status
  dur]` in cells, orthogonal edges drawn beneath the boxes (`---+`, `|`,
  `+--->`). Above sixty nodes the columns fold to `depth n: k nodes, …`.
- **timeline**: one row per outermost iteration (`review-cycle@2`) or
  top-level step, a bar of the executed attempts' step initials (`X` for a
  failure) coloured by status on an axis scaled to the width, the running
  step extended to now; below, the selection's iteration one row per
  attempt with its duration.
- **STEP**: breadcrumb, status, reason or since, attempt, duration, usually,
  outputs, then the error, handoff, re-enter command (`printed on q`), or
  the halt. **OUTPUT**: the newest attempt's `output.log` tail,
  terminal-sanitized.

Keys: `j`/`k` and arrows move (and turn follow off), page keys jump ten,
`n` the next FAIL/HALT/WAIT, `/` a filter over id, name and status, `f`
toggles follow (selection on the newest running step, else the first
problem), `enter` opens or closes an iteration, `t`/`g`/`l` switch views,
`q` returns to Home — and, with a failed step selected, remembers its
re-enter command (or the run's resume command) to print on stdout when the
view exits.

## Workflow preview

Enter on a workflow opens the Run screen's views over the IR alone: an
empty replay reported through the same reporter, so the tree and the graph
show the compute graph before any run exists — loops as `up to N
iterations` with their body listed once, generate nodes, sub-workflows,
and the graph's dependency columns — with no status words, since nothing
has state. The right pane lists the workflow's params (type, required or
default), the structure (loops with their caps, generates with their
source and target), and the selected step's template, lane, guard and
retry. `t`/`g` switch tree and graph, enter opens a folded body, `r`
starts this workflow through the same prompt as Home, `q` returns.

## Liveness

A one-hertz tick reloads the store and redraws; so does every keystroke. A
run started by another process is watched the same way, the run lock being
what makes it LIVE. The bell rings once per newly seen FAIL or HALT step and
once when the watched run pauses — one byte the terminal turns into a
notification, so the operator can leave the window.

## Actions are detached children

`r` and `R` spawn `faber run <wf> --param k=v … --config <path> --progress
json` / `faber resume <id> --progress json` in their own session with
stdout and stderr appended to `<state>/logs/<sub>-<timestamp>.log`; the
view reports the exact command and the log path and sees the run through
the journal within a tick. Closing the view never stops a run.

Requirements implemented: Journal-fed live view, Four screens, Honest
actions.
