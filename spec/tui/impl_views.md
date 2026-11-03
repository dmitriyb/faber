# Implementation: Views and actions

## Entry (tui.go, app.go)

```go
type Options struct { Store *failure.Store; Images ImageProbe; LogDir, Executable string; Now func() time.Time }
type ImageProbe interface { Tag(*config.ResolvedTemplate) (string, error); Exists(ctx, tag string) (bool, error) }
func New(Options) *UI                     // config.UIRunner
func (u *UI) Run(ctx, config.UIOptions) error

type model struct {            // rebuilt from the store at every tick/key
    screen string              // "home" | "run"
    project *Project; runs []RunRow; runSel int; meds map[string]time.Duration
    run *RunView; rows []row; sel int; view string; follow bool; filter string; open map[string]bool
    prompt *prompt; quitCmd, status string; bellNext bool
}
func (m *model) loop(ctx) error            // openTerminal → readKeys goroutine → refresh → draw; tick 1s | key → handle → draw
func (m *model) handle(ctx, key) bool      // false = exit; prompt first, then per screen
```

The cmd wiring fills `Options` with the store, an image probe over the
image tagger and the docker client, and `<state>/logs` as the log dir.

## Model (model.go, tree.go)

```go
loadProject(ctx, path, probe) *Project          // Load → Validate → Desugar each workflow → probe per template
loadRuns(store, now) []RunRow                   // AuditRuns + LoadHeader + Load: state, wall, live step, failed
running(rp, now) []Running                      // last start per step with no result finished after it
medians(store) map[string]time.Duration         // per leaf step name over every ok executed attempt
loadRun(store, id, meds, now) (*RunView, error) // replay, IR via the header's config, RunReporter{Targets}, bounds
outputTail(runDir, nodeID, attempt, n) []string // pipeline.AttemptDir + OutputLogName
crumb(id, workflow, bounds) string              // the stream's breadcrumb
treeRows(v, open, now) []row                    // ReadingOrder folded; row{indent,status,name,dur,note,id,kind,since}
```

`row.kind` is `step | loop | iter | range | gen | item`; only step rows
have a STEP pane and an output tail. An iteration expands when
`open[prefix]`, its status is not ok, or a running node sits under it.

## Screens (home.go, run.go, graph.go, timeline.go)

```go
renderHome(f, st, m); renderRun(f, st, m)
projectPane / runsPane / footer                 // Home panes; runs fold to id/state/now under 100 columns
nowStrip / treePane / stepPane / outputPane     // Run; stepH = 55% of the body
collapsedGraph(v, rows) ([]gnode, [][2]int)     // steps, loops, items; selectors folded; IR edges + When deps
layout(nodes, edges)                            // longest-path columns, one barycenter pass
graphPane / timelinePane                        // drawing; >60 nodes → per-depth summaries
```

## Actions (app.go)

`spawn(sub, args…)`: `os.Executable()` (or `Options.Executable`), args plus
`--config` for run and `--progress json`, `Setsid`, stdout+stderr to
`<LogDir>/<sub>-<timestamp>.log`, `Start` then a reaping goroutine; the
status line names the command and the log. `p`: `Store.RequestPause` only
when the row is LIVE. `q` on a run: `quitCmd` = the selected failed step's
`faber resume <id> --interactive <step>` or, for a paused/failed run,
`faber resume <id>`, printed after the terminal bracket closes.

## Renderer (render.go, term.go)

```go
type frame struct{ w, h int; rows []string }; put(y, s); bytes(); plain()
fitRow(s, w)                                    // cut by visible width, codes balanced
type style struct{ color bool }                 // chrome/ident/dim/rev/status/rule
cols(cells, widths); panes(st, columns, widths, h)
openTerminal(in, out, fallback) *terminal       // raw + alt screen + hidden cursor; size(); close()
readKeys(r, out chan<- key)                     // runes, CSI arrows/page keys, enter, esc
```
