# Implementation: Views and actions

## Entry (tui.go, app.go)

```go
type Options struct { Store *failure.Store; Images ImageProbe; LogDir, Executable string; Now func() time.Time }
type ImageProbe interface { Tag(*config.ResolvedTemplate) (string, error); Exists(ctx, tag string) (bool, error) }
func New(Options) *UI                     // config.UIRunner
func (u *UI) Run(ctx, config.UIOptions) error   // signal.NotifyContext(SIGINT, SIGTERM, SIGHUP) around the loop

type model struct {            // re-derived from the store at every tick/key
    screen string              // "home" | "run"
    project *Project; runs []RunRow; runSel, wfSel int; focus string
    cache *replayCache; irs *irCache; stats map[string]Stats; statsVer int; loadErr string
    run *RunView; preview bool
    rows []row; sel int                   // tree
    frows []frow; fsel int                // flow
    trows []trow; tsel, tzoom int         // timeline: iterations, and the attempts of opened ones
    view string; follow bool; filter string; open map[string]bool; selLines []string
    prompt *prompt; quitCmd, status string; bellNext bool
}
func (m *model) loop(ctx) error            // openTerminal → readKeys goroutine → refresh → draw; tick 1s | key → handle → draw
func (m *model) refresh(ctx)               // project on config mtime (images every 30 ticks); runs by stat; stats when the cache version moved; the open run
func (m *model) handle(ctx, key) bool      // false = exit; 0x03/0x04 (Ctrl-C/D in raw mode) exit anywhere; prompt first, then per screen
```

The cmd wiring fills `Options` with the store, an image probe over the
image tagger and the docker client, and `<state>/logs` as the log dir.

## Model (model.go, tree.go, flow.go, timeline.go)

```go
loadProject(ctx, path, probe) *Project            // Load → Validate → Desugar each workflow → probe per template; mtime kept
configMTime(path) time.Time
irCache.get(path) (map[string]*config.IR, error)  // a run's IR by its header's config path, re-derived when the file moved
replayCache.get(id) (*failure.Replay, error)      // Store.Load keyed on the journal's size+mtime; version bumps per replay
loadRuns(cache, now) ([]RunRow, error)            // AuditRuns + each replay: config.RunState, wall, live step, failed
runState(audit, rp) string                        // the faber runs vocabulary, FAILED/ok by the failed count when settled
runningOf(rp) []Running                           // failure.Replay.Running: last start per step with no result finished after it
stats(cache) map[string]Stats                     // per leaf step name: p50/p90/p99 and N over every ok executed attempt
loadRun(cache, irs, id, stats, now) (*RunView, error) // replay, IR via the header's config (Err when it no longer loads), RunReporter{Targets}, bounds
previewWorkflow(project, name) (*RunView, error)  // an empty replay over the IR: the preview
outputTail(runDir, nodeID, attempt, n) []string   // pipeline.AttemptDir + OutputLogName; n <= 0 reads nothing
crumb(id, workflow, bounds) string                // the stream's breadcrumb
treeRows(v, open, now) []row                      // ReadingOrder folded; row{indent,status,name,dur,note,id,kind,since}
flowRows(v, preview, now) []frow                  // structure from iteration 1, state from the current iteration
timelineRows(v, open, now) []trow                 // one row per outermost iteration or top-level step; an opened iteration's attempts under it
```

`row.kind` is `step | loop | iter | range | gen | item`; only step rows
have a STEP pane and an output tail. An iteration expands when
`open[prefix]`, its status is not ok, or a running node sits under it.

`frow.kind` is `open | close | step | gen | sub | items`: a loop's frame
edges, a step with `if …` (the author's guard with leaf names, shown
whole) and `after …` (a `depends_on` in the same scope; edges out of a
selector are the loop's plumbing and are never shown), a generate with its
instances, and a sub-workflow as a group row with its body one level in.
The flow cursor never rests on a frame edge.

## Screens (home.go, run.go, flow.go, timeline.go)

```go
renderHome(f, st, m); renderRun(f, st, m); renderWorkflow(f, st, m)
projectPane / runsPane / footer                 // Home panes; runs fold to id/state/now under 100 columns
nowStrip / treePane / stepPane / outputPane     // Run; stepH = 55% of the body; a header wider than the row is cut
flowPane / timelinePane                         // the other two views; each owns its cursor
```

Box-derived text (output tails) passes `sanitize`: printable ASCII only,
tab as four spaces, anything else `?` — the renderer counts one cell per
byte.

## Actions (app.go)

`spawn(sub, args…)`: `os.Executable()` (or `Options.Executable`), args plus
`--config` for run and `--progress json`, `Setsid`, stdout+stderr to
`<LogDir>/<sub>-<timestamp.ns>-<pid>.log`, `Start` then a reaping
goroutine; the status line names the command and the log. `p`:
`Store.RequestPause` only when the row is LIVE. `R`: refused with a
message on a LIVE row (the run holds its own lock). `q` on a run:
`quitCmd` = the selected failed step's `faber resume <id> --interactive
<step>` or, for a paused/failed run, `faber resume <id>`, printed after
the terminal bracket closes; opening another run clears it.

## Renderer (render.go, term.go)

```go
type frame struct{ w, h int; rows []string }; put(y, s); bytes(); plain()
fitRow(s, w)                                    // cut by visible width, codes balanced
type style struct{ color bool }                 // chrome/ident/dim/rev/status/rule
cols(cells, widths); panes(st, columns, widths, h)
openTerminal(in, out, fallback) *terminal       // raw + alt screen + hidden cursor; size(); close()
readKeys(r, out chan<- key)                     // runes, CSI arrows/page keys, enter; a lone esc followed by a key yields both
```
