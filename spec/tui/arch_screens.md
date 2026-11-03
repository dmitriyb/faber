# Screens — Home and Run over the model

## What it is

The two screens of the live view and the model they read. Three rules keep
the view honest and bound its scope: it shows nothing the store, the
reporter and the IR cannot answer (every screen is a reading of what
`validate`, the preflight, `runs` and the report read — never a guess
about what the engine is doing), it does nothing the CLI cannot do (every
action is a `faber` command, spawned or named), and it holds no state the
journal does not (every tick and keystroke re-derives the model from the
store — journals by stat, replayed only when they grew — so a crash loses
a cursor position).

## The model

- **Project**: `config.Load` → `Validate` → `Desugar` for every workflow
  (the same pipeline `faber validate` runs); on failure only the error is
  shown, since nothing else can run. Workflows with their typed params,
  and the **preflight**: what the run would find out when a box starts,
  read before anyone presses run and never by running anything. Images:
  tag resolution plus a daemon existence check through the injected
  probe. Roles: every declared identity — `unused` when no template names
  it (a template without an identity needs no key, so a declared role is
  a problem only when it is used), `ok` when a key resolves (a path-form
  key by stat; a registry role through the injected key probe, the same
  registry-plus-locator hop the run makes), `MISSING` with its fix
  (`faber add-key --role …`) when nothing does, `not checked` when no
  probe is wired. Templates: a template owns four hook slots (context,
  prelude, postlude, on_failure), each empty or holding one script; the
  only fact worth stating is a slot whose script is not there, by stat,
  named by template and slot. The
  credential resolver by path; its services only by name and mode, `not
  checked`, since only the resolver can say whether one resolves, and
  running it would be an action. A root config's paths are CWD-relative
  and are checked as such, which is also how the run opens them.
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
  **history**: per workflow and step name (an id's last segment, which
  repeats across loops, instances and runs; a step is the same step only
  within its workflow) the duration distribution of ok executed attempts
  — p50, p90, p99 and the sample count — in one of two scopes: **all
  runs** of this workflow in the store (the default: the step, not the
  run, sets its duration, and the sample is large enough for p90 to mean
  something) or **this run** (is this cycle slower than the others under
  the same conditions). `h` toggles the scope for the open run, and the
  screen always says which is in force: STEP's history line reads `89
  attempts   all runs of task   h to switch` or `12 attempts   this run
  …`, and `usually`/`slow` follow the same scope. p50 is the `usually
  4m10s` beside a running step (`slow` past twice); STEP shows all three
  with the count; the workflow preview has no run, so it shows p50 and
  p90 from all runs on every step row and says so, and reads as "what
  this workflow costs, from your own history".

## Home

Header (the config path, the clock), a rule, two panes, a rule, the
selected run's block, the footer. The project pane lists workflows with
`name=<required>` / `name=default` params, then the preflight as
**counts, with a row per problem**: `config ok valid`, `images 4 ok`,
`identities 12   2 MISSING` with the two roles beneath it and their fix,
`templates 4   1 MISSING` with `implement   prelude   ./hooks/claim-item`
beneath it, `credentials resolver ok   3 services not checked`. Fifty
healthy roles are one line; an unused one is a dim row; a missing image
is a row with `faber build` and the tag hash it was looked up under, so
"MISSING" is checkable against `docker image ls`. The runs
pane is the `faber runs` table with wall and a `now` column (the live run's
breadcrumb), folding to id / state / now under a hundred columns; the
selection is a full reverse-video row. Live runs are pinned at the top
whatever their age, with a blank line under the block and the count in
the title (`LIVE 2`); the history beneath is newest first and scrolls so
the cursor stays visible while the live block never scrolls away. `/`
filters the table over id, name, workflow and state (`live`, `failed`,
`epic`), the title names the filter, and esc clears it before it quits. Below: `now` (the running step with
elapsed and p50), `last` (the newest settlement) — computed on refresh,
never while drawing. A quiet live run gets no `next` line: the journal
cannot say what the scheduler waits on, so the view does not guess. Keys: `tab` (or
left/right) moves the focus between the runs and the workflows pane;
`j`/`k` move within it; enter opens the selected run, or — with the
workflows pane focused — the selected workflow's preview; `r` prompts `run
<workflow> [k=v …]` and spawns the child; `p` pauses a live run (and
refuses, with a message, a run that is not live — no marker is written);
`R` spawns the resume (and refuses a live run, which holds its own lock);
`q` quits. The footer lists every key the screen answers to. Prune,
upgrade and key management stay CLI-only.

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
- **flow**: the workflow as the author wrote it, vertical. Steps are
  lines in dependency order, each with its facts in fixed columns: `if …`
  for a guard (the author's condition whole, step references rewritten
  to leaf names — the structure is read from iteration 1, which carries
  no loop gate), `after …` for a `depends_on` (an edge out of a selector
  is the loop's plumbing and is not one). A sub-workflow is a group row
  naming its workflow with the body one level in, no frame, since it
  does not repeat. A loop is a frame:
  `+- name  x5  until …  ---+` opening it, its body indented inside, and
  `+-----^` closing it — the `^` is the repeat. A generate is a line
  naming its source, target and bound, with its instances side by side
  beneath it on a run. No edges are drawn: faber workflows are sequences
  with guards and loops, and the only true fan-out is a generate. On a run
  each loop shows its current iteration (`2 of up to 5`) with the
  journal's state in the status column, `running` on the live step, and
  the cursor skips frame edges. The same rows draw the workflow preview
  with `x5` for the cap and no status column.
- **timeline**: one level at a time, drilled down by id prefix. At the
  top, one row per outermost iteration (`review-cycle@2`) or top-level
  step; a group's bar is the executed attempts' step initials (`X` for a
  failure) coloured by status, a step's a solid bar, on an axis scaled to
  the level's own span and the width, the running step extended to now. A
  column keeps the first letter written into it, so a short step is never
  hidden by the long one after it at a coarse scale. Enter on a group (an
  iteration, a generate instance, a sub-workflow — anything with attempts
  under it, marked `>`) replaces the rows with that group's own, on its
  own axis, so a twenty-minute cycle fills the width and its steps are
  wide bars; nesting goes as deep as the ids do. The breadcrumb above the
  axis carries the full path (`cycles 21/45 > review-cycle 1/3`), and one
  dim line draws the parent's bar with this level's window marked, so the
  place in the whole run stays visible. Right arrow drills in and left
  arrow goes one level up with the cursor back on the row entered (enter,
  esc and backspace are aliases of the two); esc at the top leaves the
  run. The timeline has its own cursor, the selected row is the selection
  bar, STEP and OUTPUT follow a selected step. `+`/`-` zoom the columns
  by powers of two within a level, the window centred on the selected row.
- **STEP**: breadcrumb, status, reason or since, attempt, duration, usually,
  outputs, then the error, handoff, re-enter command (`printed on q`), or
  the halt. **OUTPUT**: the newest attempt's `output.log` tail,
  terminal-sanitized.

One key vocabulary on every screen: `j`/`k`, arrows and page keys move
the list that has the cursor; `tab` and left/right move between panes
where a screen has two (Home: left is the project, right the runs; the
preview: left the flow, right the details); right or enter goes into the
thing under the cursor (opens an iteration, drills into a timeline group,
opens a block) and left comes out of it (closes the iteration, even from
inside it, with the cursor back on its row; a level up); esc backs out
one step if there is one (a timeline level, the details pane, a prompt)
and otherwise leaves the screen, which `q` always does; `r`, `p`, `R`
run, pause and resume the run under the cursor on Home or the run on
screen. The footer lists every key the screen answers to, and folds onto
a second row on a narrow terminal rather than dropping one. On the Run
screen: `j`/`k` and arrows move (and turn follow off), page keys jump ten,
`n` the next FAIL/HALT/WAIT — each on the cursor of the view shown — `/` a filter over id, name and status, `f`
toggles follow (selection on the newest running step, else the first
problem), `enter` opens or closes an iteration (on the timeline: `enter`/right drills into a group, left/`esc` comes up), `h` switches the history scope, `t`/`w`/`l` switch tree, workflow, timeline,
`q` returns to Home — and, with a failed step selected, remembers its
re-enter command (or the run's resume command) to print on stdout when the
view exits. The footer is per view and lists every key the view answers
to: a key it does not name does not exist.

## Workflow preview

Enter on a workflow opens the Run screen's views over the IR alone: an
empty replay reported through the same reporter, so the tree and the flow
show the flow before any run exists — loops as frames with their cap and
exit, their body listed once (nested loops framed the same way, in
dependency order), generate nodes, sub-workflows — with no status column,
since nothing has state. STRUCTURE lists loop shapes once (`cycles >
review-cycle, up to 5`), never the unrolled iterations. The right pane
is **blocks that start folded**, each one summary line, so the screen
holds the minimum and opens on request: `PARAMS   2 params`, `STRUCTURE
1 loop`, `NEEDS   3 roles   4 templates   1 service   ok` — the preflight
narrowed to this workflow's templates: its roles with their state, a
template only by the slot that stops it, the services being global — and
`STEP`, open by default, with the selected step's schema
facts: template, identity with its state, lane, guard (leaf names), retry,
on_failure, then sub-blocks `hooks   2 of 4 set   ok` (all four hooks
listed when open, `none` where unset, each path's state), `inputs/outputs`
(bindings as the YAML wrote them, outputs with their type), the agent
line (skill, model, effort, tools), and `history` (p50 in the summary,
the distribution when open, always all runs of this workflow). Nothing
opaque — env, volumes, packages — is shown. A block with a MISSING opens
itself once; after that the user's fold wins. `tab` moves the cursor to
the pane, where `j`/`k` walk the block heads (marked `>` closed, `v`
open), `enter` opens or closes, and `tab` returns to the flow; the footer
names the keys of whichever side has the cursor. `t`/`w` switch tree and
workflow, enter on the flow opens a folded body, `r` starts this workflow
through the same prompt as Home, `q` returns.

## Liveness

A one-hertz tick reloads the store and redraws; so does every keystroke. A
journal is replayed only when its size or mtime moved; the history is
recomputed only when some journal was; the project reloads when the
config's mtime moved, and the images are re-probed on a slow cadence. A
run started by another process is watched the same way, the run lock being
what makes it LIVE. A store that cannot be listed shows its error on the
status row over the last good table. Ctrl-C and Ctrl-D arrive as bytes in
raw mode and quit like `q`; SIGTERM and SIGHUP cancel the loop's context,
so the terminal is restored on every exit path. A run whose config no
longer loads still renders from its journal, with the reason on the strip
and an empty flow. The bell rings once per newly seen FAIL or HALT step and
once when the watched run pauses — one byte the terminal turns into a
notification, so the operator can leave the window.

## Actions are detached children

`r` and `R` spawn `faber run <wf> --param k=v … --config <path> --progress
json` / `faber resume <id> --progress json` in their own session with
stdout and stderr appended to `<state>/logs/<sub>-<timestamp>-<pid>.log`
(nanosecond timestamp, so two spawns never share a file); the
view reports the exact command and the log path and sees the run through
the journal within a tick. Closing the view never stops a run.

Requirements implemented: Journal-fed live view, Four screens, Honest
actions.
