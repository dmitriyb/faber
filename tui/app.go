package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmitriyb/faber/config"
)

// model is the whole view state: what is loaded and what is selected. It is
// rebuilt from the store at every tick and keystroke; nothing in it is
// durable, so a crash loses only a cursor position.
type model struct {
	opts   Options
	uo     config.UIOptions
	st     style
	now    func() time.Time
	screen string // "home" | "run"

	project    *Project
	runs       []RunRow
	shown      []RunRow // the runs Home lists: this config's, through the Home filter
	rfilter    string   // the Home filter over id, name, workflow and state
	runSel     int      // the cursor over shown
	wfSel      int
	focus      string // Home: "runs" | "workflows"
	stats      History
	hscope     string          // the history scope in force: ScopeAllRuns or ScopeThisRun
	preview    bool            // the Run screen is a static workflow preview (no journal)
	pfocus     bool            // the preview's right pane has the cursor
	psel       int             // the preview pane cursor, over block heads
	popen      map[string]bool // which preview blocks are open
	plines     []pline         // the preview pane as last drawn, for the cursor
	pauto      map[string]bool // blocks a problem has opened once; the user's fold wins after
	footerRows int             // how many rows the last footer took (1, or 2 when narrow)
	cache      *replayCache
	irs        *irCache
	statsVer   int      // the cache version the stats were computed at (-1 = never)
	selLines   []string // Home's now/last lines for the selected run, computed in refresh
	ticks      int
	loadErr    string // the last store error, shown instead of an empty table

	run     *RunView
	rows    []row
	sel     int
	frows   []frow   // the flow view's rows
	fsel    int      // the flow cursor
	trows   []trow   // the timeline's rows at the current level
	tsel    int      // the timeline cursor
	tpath   []string // the timeline's drill-down: the prefix of each level entered
	tback   []int    // the cursor to restore when leaving each level
	tzoom   int      // the timeline zoom exponent (columns x 2^tzoom)
	view    string   // "tree" | "flow" | "timeline"
	follow  bool
	filter  string
	open    map[string]bool
	alerted map[string]bool // ids already belled

	prompt   *prompt
	quitCmd  string // printed on stdout after the terminal is restored
	status   string // one-line transient message in the footer line above
	bellNext bool
}

// prompt is the one-line input Home uses to start a workflow.
type prompt struct {
	label string
	text  string
	done  func(text string) string // returns a status message
}

func newApp(o Options, uo config.UIOptions) *model {
	return &model{opts: o, uo: uo, st: style{color: uo.Color}, now: o.Now, screen: "home", focus: "runs", view: "tree", follow: true,
		hscope: ScopeAllRuns, popen: map[string]bool{"step": true}, pauto: map[string]bool{}, open: map[string]bool{}, alerted: map[string]bool{}, cache: newReplayCache(o.Store), irs: newIRCache(), statsVer: -1}
}

func (m *model) selectedRun() *RunRow {
	if m.runSel < 0 || m.runSel >= len(m.shown) {
		return nil
	}
	return &m.shown[m.runSel]
}

// rebuildShown is the Home table: this config's runs, through the filter,
// in the model's order (live first). The cursor is clamped onto it.
func (m *model) rebuildShown() {
	m.shown = m.shown[:0]
	for _, r := range m.runs {
		if r.ConfigPath != "" && m.project.cfg != nil && !sameConfig(r.ConfigPath, m.project.Path) {
			continue
		}
		if f := strings.ToLower(m.rfilter); f != "" {
			hay := strings.ToLower(r.ID + " " + r.Name + " " + r.Workflow + " " + r.State)
			if !strings.Contains(hay, f) {
				continue
			}
		}
		m.shown = append(m.shown, r)
	}
	m.runSel = clamp(m.runSel, 0, len(m.shown)-1)
}

func (m *model) selectedRow() *row {
	if m.view == "timeline" {
		if m.tsel < 0 || m.tsel >= len(m.trows) {
			return nil
		}
		t := m.trows[m.tsel]
		if t.iter {
			return &row{id: t.id, kind: "iter", name: t.name}
		}
		if t.deeper {
			return &row{id: t.id, kind: "item", name: t.name}
		}
		for i := range m.rows {
			if m.rows[i].id == t.id && m.rows[i].kind == "step" {
				return &m.rows[i]
			}
		}
		return &row{id: t.id, kind: "step", name: leaf(t.id), status: t.status}
	}
	if m.view == "flow" {
		if m.fsel < 0 || m.fsel >= len(m.frows) {
			return nil
		}
		f := m.frows[m.fsel]
		for i := range m.rows {
			if m.rows[i].id == f.id && m.rows[i].kind == "step" {
				return &m.rows[i]
			}
		}
		kind := "step"
		if f.kind == "open" || f.kind == "close" {
			kind = "loop"
		}
		return &row{id: f.id, kind: kind, name: f.name, status: f.status, since: f.since}
	}
	if m.sel < 0 || m.sel >= len(m.rows) {
		return nil
	}
	return &m.rows[m.sel]
}

// openRunKeepingPlace reloads the open run under the current history
// scope, cursors and levels untouched.
func (m *model) openRunKeepingPlace(id string) {
	v, err := loadRun(m.cache, m.irs, id, m.stats, m.hscope, m.now())
	if err != nil {
		m.status = err.Error()
		return
	}
	m.run = v
	m.rebuildRows()
}

// tprefix is the id prefix of the timeline's current level: the run's
// workflow at the top, else the group last entered.
func (m *model) tprefix() string {
	if len(m.tpath) > 0 {
		return m.tpath[len(m.tpath)-1]
	}
	return m.run.Replay.Header.Workflow + "/"
}

// timelineUp leaves the current level, the cursor back on the row entered.
func (m *model) timelineUp() {
	if len(m.tpath) == 0 {
		return
	}
	m.tpath = m.tpath[:len(m.tpath)-1]
	m.tsel = m.tback[len(m.tback)-1]
	m.tback = m.tback[:len(m.tback)-1]
	m.rebuildRows()
}

// nextProblem moves the current view's cursor to the next FAIL, HALT or
// WAIT after it, wrapping.
func (m *model) nextProblem() {
	problem := func(status string) bool { return status == "FAIL" || status == "HALT" || status == "WAIT" }
	switch m.view {
	case "flow":
		for i := 1; i <= len(m.frows); i++ {
			j := (m.fsel + i) % len(m.frows)
			if f := m.frows[j]; f.kind == "step" && problem(f.status) {
				m.fsel = j
				return
			}
		}
	case "timeline":
		for i := 1; i <= len(m.trows); i++ {
			j := (m.tsel + i) % len(m.trows)
			for _, s := range m.trows[j].spans {
				if problem(s.status) {
					m.tsel = j
					return
				}
			}
		}
	default:
		for i := 1; i <= len(m.rows); i++ {
			j := (m.sel + i) % len(m.rows)
			if r := m.rows[j]; r.kind == "step" && problem(r.status) {
				m.sel = j
				return
			}
		}
	}
}

// moveDown / moveUp move whichever cursor the view shows; the flow cursor
// skips frame edges, which are not things.
func (m *model) moveDown() {
	if m.view == "timeline" {
		if m.tsel < len(m.trows)-1 {
			m.tsel++
		}
		return
	}
	if m.view == "flow" {
		for i := m.fsel + 1; i < len(m.frows); i++ {
			if m.frows[i].kind == "step" || m.frows[i].kind == "gen" {
				m.fsel = i
				return
			}
		}
		return
	}
	if m.sel < len(m.rows)-1 {
		m.sel++
	}
}

func (m *model) moveUp() {
	if m.view == "timeline" {
		if m.tsel > 0 {
			m.tsel--
		}
		return
	}
	if m.view == "flow" {
		for i := m.fsel - 1; i >= 0; i-- {
			if m.frows[i].kind == "step" || m.frows[i].kind == "gen" {
				m.fsel = i
				return
			}
		}
		return
	}
	if m.sel > 0 {
		m.sel--
	}
}

func (m *model) otherRuns() int {
	n := 0
	for _, r := range m.runs {
		if r.ConfigPath != "" && m.project.cfg != nil && !sameConfig(r.ConfigPath, m.project.Path) {
			n++
		}
	}
	return n
}

// selectedRunLines are Home's now / last lines for the selection, as the
// last refresh computed them. Rendering never loads.
func (m *model) selectedRunLines() []string { return m.selLines }

// computeSelectedLines derives the selection's now and last lines from a
// cached replay. Nothing is written about what a quiet live run waits on:
// the journal cannot say, so the view does not guess.
func (m *model) computeSelectedLines() {
	m.selLines = nil
	sel := m.selectedRun()
	if sel == nil {
		return
	}
	v, err := loadRun(m.cache, m.irs, sel.ID, m.stats, m.hscope, m.now())
	if err != nil {
		m.selLines = []string{"  " + m.st.dim(err.Error())}
		return
	}
	saved, savedRows := m.run, m.rows
	m.run = v
	m.rows = treeRows(v, m.open, m.now())
	lines := []string{m.nowStrip(m.st)}
	if last := m.lastSettled(); last != "" {
		lines = append(lines, "  last  "+m.st.ident(crumb(last, v.Replay.Header.Workflow, v.Bounds))+"   "+m.st.status(m.stateOf(last)))
	}
	m.run, m.rows = saved, savedRows
	m.selLines = lines
}

// probeEvery is how many ticks pass between image re-probes: a docker
// call per template is not a per-second cost.
const probeEvery = 30

// refresh reloads what changed: the project when its file moved, the
// images on a slow cadence, every journal by stat (replayed only when it
// grew), the history when a journal grew, and the open run.
func (m *model) refresh(ctx context.Context) {
	m.ticks++
	if m.project == nil || !m.project.mtime.Equal(configMTime(m.uo.ConfigPath)) || m.ticks%probeEvery == 0 {
		m.project = loadProject(ctx, m.uo.ConfigPath, m.opts.Images, m.opts.Keys)
	}
	runs, err := loadRuns(m.cache, m.now())
	if err != nil {
		m.loadErr = err.Error()
	} else {
		m.loadErr = ""
		m.runs = runs
	}
	m.rebuildShown()
	if m.cache.version != m.statsVer {
		m.stats = stats(m.cache)
		m.statsVer = m.cache.version
	}
	m.project.stats = m.stats
	if m.screen == "home" {
		m.computeSelectedLines()
	}
	if m.screen == "run" && m.run != nil && !m.preview {
		v, err := loadRun(m.cache, m.irs, m.run.ID, m.stats, m.hscope, m.now())
		if err != nil {
			m.status = "run " + m.run.ID + ": " + err.Error()
		} else {
			m.run = v
			m.rebuildRows()
		}
	}
}

func (m *model) rebuildRows() {
	m.frows = flowRows(m.run, m.preview, m.now())
	m.trows = timelineRows(m.run, m.tprefix(), m.now())
	if m.tsel >= len(m.trows) {
		m.tsel = max(0, len(m.trows)-1)
	}
	if m.fsel >= len(m.frows) {
		m.fsel = 0
	}
	if m.fsel < len(m.frows) && m.frows[m.fsel].kind != "step" && m.frows[m.fsel].kind != "gen" {
		// settle the flow cursor on a step — the flow's own cursor only,
		// never the view-dispatching move, which a tick must not touch
		for i := m.fsel + 1; i < len(m.frows); i++ {
			if m.frows[i].kind == "step" || m.frows[i].kind == "gen" {
				m.fsel = i
				break
			}
		}
	}
	all := treeRowsMode(m.run, m.open, m.now(), m.preview)
	if m.filter != "" {
		var kept []row
		for _, r := range all {
			if strings.Contains(r.id, m.filter) || strings.Contains(r.status, m.filter) || strings.Contains(r.name, m.filter) {
				kept = append(kept, r)
			}
		}
		all = kept
	}
	m.rows = all
	if m.follow && !m.preview {
		if idx := m.newestActive(); idx >= 0 {
			m.sel = idx
		}
		for i, f := range m.frows {
			if f.kind == "step" && f.status == "running" {
				m.fsel = i
				break
			}
		}
		if len(m.run.Running) > 0 {
			running := m.run.Running[0].Step
			for i, tr := range m.trows {
				if tr.id == running || (tr.deeper && strings.HasPrefix(running, tr.id)) {
					m.tsel = i
				}
			}
		}
	}
	if m.sel >= len(m.rows) {
		m.sel = len(m.rows) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	m.bells()
}

// newestActive is the row of the newest running step, or the first problem.
func (m *model) newestActive() int {
	best, idx := time.Time{}, -1
	for i, r := range m.rows {
		if r.status == "running" && r.since.After(best) {
			best, idx = r.since, i
		}
	}
	if idx >= 0 {
		return idx
	}
	for i, r := range m.rows {
		if r.kind == "step" && (r.status == "FAIL" || r.status == "HALT") {
			return i
		}
	}
	return -1
}

// bells rings once per newly seen problem: a FAIL or HALT step, a paused
// run. The bell is one byte the terminal turns into a notification.
func (m *model) bells() {
	for _, r := range m.rows {
		if r.kind == "step" && (r.status == "FAIL" || r.status == "HALT") && !m.alerted[r.id] {
			m.alerted[r.id] = true
			m.bellNext = true
		}
	}
	if m.run != nil && !m.run.Live && m.run.Replay.End != nil && m.run.Replay.End.Status == "paused" && !m.alerted["run:"+m.run.ID] {
		m.alerted["run:"+m.run.ID] = true
		m.bellNext = true
	}
}

// openRun switches to the Run screen on a run id.
func (m *model) openRun(id string) {
	v, err := loadRun(m.cache, m.irs, id, m.stats, m.hscope, m.now())
	if err != nil {
		m.status = err.Error()
		return
	}
	m.run = v
	m.quitCmd = ""
	m.tpath, m.tback, m.tsel = nil, nil, 0
	m.preview = false
	m.screen = "run"
	m.sel = 0
	m.rebuildRows()
}

// openWorkflow switches to the Workflow screen: the Run screen's views
// over the IR alone, nothing journaled — the flow before a run.
func (m *model) openWorkflow(name string) {
	v, err := previewWorkflow(m.project, name)
	if err != nil {
		m.status = err.Error()
		return
	}
	m.run = v
	m.preview = true
	m.screen = "run"
	m.view = "flow"
	m.sel = 0
	m.follow = false
	m.pfocus, m.psel = false, 0
	m.rebuildRows()
}

// autoOpen opens a block the first time it shows a problem; after that the
// user's own fold is respected.
func (m *model) autoOpen(key string) {
	if !m.pauto[key] {
		m.pauto[key] = true
		m.popen[key] = true
	}
}

// pheads are the preview pane's block heads, where its cursor can rest.
func (m *model) pheads() []int {
	var idx []int
	for i, l := range m.plines {
		if l.key != "" {
			idx = append(idx, i)
		}
	}
	return idx
}

// render draws the current screen into a frame.
func (m *model) render(w, h int) *frame {
	f := newFrame(w, h)
	switch {
	case m.screen == "run" && m.preview:
		renderWorkflow(f, m.st, m)
	case m.screen == "run":
		renderRun(f, m.st, m)
	default:
		renderHome(f, m.st, m)
	}
	if m.prompt != nil {
		f.put(h-1, m.st.chrome(m.prompt.label)+" "+m.prompt.text+"_")
	} else if m.status != "" {
		f.put(h-1-m.footerRows, m.st.dim(trunc(m.status, w)))
	} else if m.loadErr != "" {
		f.put(h-1-m.footerRows, m.st.status("FAIL")+" "+m.st.dim(trunc("store: "+m.loadErr, w-5)))
	}
	return f
}

func (m *model) homeKeys() [][2]string {
	if m.focus == "workflows" {
		return [][2]string{{"j/k", "move"}, {"enter", "preview"}, {"tab/->", "runs"}, {"/", "filter"}, {"r", "run workflow"}, {"q", "quit"}}
	}
	return [][2]string{{"j/k", "move"}, {"enter", "open"}, {"tab/<-", "workflows"}, {"/", "filter"}, {"r", "run workflow"}, {"p", "pause"}, {"R", "resume"}, {"q", "quit"}}
}

func (m *model) workflowKeys() [][2]string {
	if m.pfocus {
		return [][2]string{{"j/k", "move"}, {"enter", "open/close"}, {"tab/<-", "flow"}, {"r", "run this workflow"}, {"q", "back"}}
	}
	return [][2]string{{"j/k", "move"}, {"enter", "open/close"}, {"tab/->", "details"}, {"t/w", "views"}, {"r", "run this workflow"}, {"q", "back"}}
}

// runKeys is the footer per view: a key the footer does not list does not
// exist.
func (m *model) runKeys() [][2]string {
	switch m.view {
	case "timeline":
		return [][2]string{{"j/k", "move"}, {"->", "in"}, {"<-", "up"}, {"+/-", "zoom"}, {"n", "problem"}, {"h", "history"}, {"p", "pause"}, {"R", "resume"}, {"t/w/l", "views"}, {"f", "follow"}, {"q", "back"}}
	case "flow":
		return [][2]string{{"j/k", "move"}, {"n", "problem"}, {"h", "history"}, {"p", "pause"}, {"R", "resume"}, {"t/w/l", "views"}, {"f", "follow"}, {"q", "back"}}
	}
	return [][2]string{{"j/k", "move"}, {"->", "open"}, {"<-", "close"}, {"n", "problem"}, {"/", "filter"}, {"h", "history"}, {"p", "pause"}, {"R", "resume"}, {"t/w/l", "views"}, {"f", "follow"}, {"q", "back"}}
}

// handle applies one key; it returns false when the app should exit.
func (m *model) handle(ctx context.Context, k key) bool {
	m.status = ""
	if k.r == 0x03 || k.r == 0x04 { // raw mode delivers Ctrl-C and Ctrl-D as bytes
		return false
	}
	if m.prompt != nil {
		switch {
		case k.name == "esc":
			m.prompt = nil
		case k.name == "enter":
			p := m.prompt
			m.prompt = nil
			m.status = p.done(p.text)
		case k.r == 0x7f || k.r == 0x08:
			if len(p(m).text) > 0 {
				m.prompt.text = m.prompt.text[:len(m.prompt.text)-1]
			}
		case k.r >= 0x20 && k.r < 0x7f:
			m.prompt.text += string(k.r)
		}
		return true
	}
	switch m.screen {
	case "home":
		return m.handleHome(ctx, k)
	case "run":
		return m.handleRun(ctx, k)
	}
	return true
}

func p(m *model) *prompt { return m.prompt }

// The key vocabulary is the same on every screen:
//
//	j/k, arrows, page keys   move the list that has the cursor
//	tab, <- ->               the pane, where a screen has two (Home: project | runs; the
//	                         preview: flow | details) — right goes to the right pane
//	-> / enter               go into the thing under the cursor: open an iteration, drill
//	                         into a timeline group, open a block
//	<-                       come out of it
//	esc                      back out one step if there is one, else leave the screen
//	q                        leave the screen
//	r, p, R                  run a workflow; pause or resume the run under the cursor or on screen
//
// A key the footer does not list does not exist, and the footer folds to a
// second row rather than dropping one.
func (m *model) handleHome(ctx context.Context, k key) bool {
	switch {
	case k.name == "esc" && m.rfilter != "":
		m.rfilter = "" // back out one step: the filter goes before the screen
		m.rebuildShown()
	case k.r == 'q' || k.name == "esc":
		return false
	case k.r == '/':
		m.prompt = &prompt{label: "filter runs:", done: func(text string) string {
			m.rfilter = text
			m.rebuildShown()
			if text == "" {
				return ""
			}
			return "filter: " + text + " (empty or esc to clear)"
		}}
	case k.r == '\t':
		if m.focus == "runs" {
			m.focus = "workflows"
		} else {
			m.focus = "runs"
		}
	case k.name == "right":
		m.focus = "runs"
	case k.name == "left":
		m.focus = "workflows"
	case k.r == 'j' || k.name == "down":
		m.homeMove(1)
	case k.r == 'k' || k.name == "up":
		m.homeMove(-1)
	case k.name == "pgdn":
		m.homeMove(10)
	case k.name == "pgup":
		m.homeMove(-10)
	case k.name == "enter":
		if m.focus == "workflows" {
			if m.wfSel < len(m.project.Workflows) {
				m.openWorkflow(m.project.Workflows[m.wfSel].Name)
			}
			break
		}
		if sel := m.selectedRun(); sel != nil {
			m.openRun(sel.ID)
		}
	case k.r == 'p':
		if sel := m.selectedRun(); sel != nil {
			m.pauseRun(sel.ID, sel.State == "LIVE")
		}
	case k.r == 'R':
		if sel := m.selectedRun(); sel != nil {
			m.resumeRun(sel.ID, sel.State == "LIVE")
		}
	case k.r == 'r':
		if m.project.cfg == nil {
			m.status = "run: the config does not validate"
			break
		}
		m.prompt = &prompt{label: "run <workflow> [k=v ...]:", done: func(text string) string {
			fields := strings.Fields(text)
			if len(fields) == 0 {
				return "run: no workflow given"
			}
			args := []string{fields[0]}
			for _, kv := range fields[1:] {
				args = append(args, "--param", kv)
			}
			return m.spawn("run", args...)
		}}
	}
	return true
}

// homeMove moves the focused Home list by d rows, clamped.
func (m *model) homeMove(d int) {
	if m.focus == "workflows" {
		m.wfSel = clamp(m.wfSel+d, 0, len(m.project.Workflows)-1)
		return
	}
	m.runSel = clamp(m.runSel+d, 0, len(m.shown)-1)
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

// pauseRun writes the pause marker for a live run; a settled run is refused
// with a message and no marker.
func (m *model) pauseRun(id string, live bool) {
	if !live {
		m.status = "pause: run " + id + " is not live"
		return
	}
	if err := m.opts.Store.RequestPause(id); err != nil {
		m.status = err.Error()
		return
	}
	m.status = "pause requested: faber runs pause " + id
}

// resumeRun spawns the resume for a settled run; a live run holds its own
// lock and is refused.
func (m *model) resumeRun(id string, live bool) {
	if live {
		m.status = "resume: run " + id + " is live; it holds its own lock"
		return
	}
	m.status = m.spawn("resume", id)
}

// handleRun is the Run screen's key set over the view that has the cursor.
func (m *model) handleRun(ctx context.Context, k key) bool {
	if m.preview {
		return m.handleWorkflow(ctx, k)
	}
	back := k.name == "left" || k.r == 0x7f || k.r == 0x08
	switch {
	case (k.name == "esc" || back) && m.view == "timeline" && len(m.tpath) > 0:
		m.timelineUp() // a level up comes before leaving the run
	case k.name == "esc" && m.filter != "":
		m.filter = "" // so does a filter
		m.rebuildRows()
	case back && m.view == "tree":
		m.closeIteration()
	case k.r == 'q' || k.name == "esc":
		if r := m.selectedRow(); r != nil {
			if line, ok := m.lineOf(r.id); ok && line.Error != nil && line.Error.Handoff != "" {
				m.quitCmd = "faber resume " + m.run.ID + " --interactive " + r.id
			} else if m.run.Replay.End != nil && (m.run.Replay.End.Status == "paused" || m.run.Replay.End.Failed > 0) {
				m.quitCmd = "faber resume " + m.run.ID
			}
		}
		m.screen = "home"
		m.filter = ""
	case k.r == 'j' || k.name == "down":
		m.follow = false
		m.moveDown()
	case k.r == 'k' || k.name == "up":
		m.follow = false
		m.moveUp()
	case k.name == "pgdn":
		m.follow = false
		for i := 0; i < 10; i++ {
			m.moveDown()
		}
	case k.name == "pgup":
		m.follow = false
		for i := 0; i < 10; i++ {
			m.moveUp()
		}
	case k.r == 'n':
		m.follow = false
		m.nextProblem()
	case k.r == 'f':
		m.follow = !m.follow
		m.rebuildRows()
	case k.r == 'h':
		if m.hscope == ScopeAllRuns {
			m.hscope = ScopeThisRun
		} else {
			m.hscope = ScopeAllRuns
		}
		m.openRunKeepingPlace(m.run.ID)
	case k.r == 'p':
		m.pauseRun(m.run.ID, m.run.Live)
	case k.r == 'R':
		m.resumeRun(m.run.ID, m.run.Live)
	case k.r == 't':
		m.view = "tree"
	case k.r == 'w':
		m.view = "flow"
	case k.r == 'l':
		m.view = "timeline"
	case (k.r == '+' || k.r == '=') && m.view == "timeline":
		if m.tzoom < 6 {
			m.tzoom++
		}
	case k.r == '-' && m.view == "timeline":
		if m.tzoom > 0 {
			m.tzoom--
		}
	case k.name == "enter" || k.name == "right":
		switch m.view {
		case "timeline":
			if m.tsel < len(m.trows) && m.trows[m.tsel].deeper {
				m.follow = false
				m.tpath = append(m.tpath, m.trows[m.tsel].id)
				m.tback = append(m.tback, m.tsel)
				m.tsel = 0
				m.rebuildRows()
			}
		case "tree":
			if r := m.selectedRow(); r != nil && r.kind == "iter" {
				if k.name == "right" {
					m.open[r.id] = true // right goes in; enter toggles
				} else {
					m.open[r.id] = !m.open[r.id]
				}
				m.rebuildRows()
			}
		}
	case k.r == '/':
		m.prompt = &prompt{label: "filter:", done: func(text string) string {
			m.filter = text
			m.rebuildRows()
			if text == "" {
				return ""
			}
			return "filter: " + text + " (empty or esc to clear)"
		}}
	}
	return true
}

// closeIteration is "come out" on the tree: the iteration under the cursor
// folds, or the one the cursor is inside folds with the cursor on its row.
func (m *model) closeIteration() {
	if m.sel >= len(m.rows) {
		return
	}
	r := m.rows[m.sel]
	if r.kind == "iter" {
		m.open[r.id] = false
		m.rebuildRows()
		return
	}
	for i := m.sel - 1; i >= 0; i-- {
		if it := m.rows[i]; it.kind == "iter" && strings.HasPrefix(r.id, it.id) {
			m.open[it.id] = false
			m.follow = false
			m.sel = i
			m.rebuildRows()
			return
		}
	}
}

// handleWorkflow is the preview's key set: the flow on the left, the
// details pane on the right, the same vocabulary as Home for the pane and
// as the tree for opening things.
func (m *model) handleWorkflow(ctx context.Context, k key) bool {
	switch {
	case k.name == "esc" && m.pfocus:
		m.pfocus = false // back out one step: the pane returns the cursor to the flow
	case k.r == 'q' || k.name == "esc":
		m.screen = "home"
		m.preview = false
	case k.r == '\t':
		m.pfocus = !m.pfocus
	case k.name == "right" && !m.pfocus:
		m.pfocus = true
	case k.name == "left" && m.pfocus:
		m.pfocus = false
	case m.pfocus && (k.r == 'j' || k.name == "down"):
		m.paneMove(1)
	case m.pfocus && (k.r == 'k' || k.name == "up"):
		m.paneMove(-1)
	case m.pfocus && k.name == "pgdn":
		m.paneMove(10)
	case m.pfocus && k.name == "pgup":
		m.paneMove(-10)
	case m.pfocus && (k.name == "enter" || k.name == "right"):
		if heads := m.pheads(); m.psel < len(heads) {
			key := m.plines[heads[m.psel]].key
			m.popen[key] = !m.popen[key]
		}
	case k.r == 'j' || k.name == "down":
		m.moveDown()
	case k.r == 'k' || k.name == "up":
		m.moveUp()
	case k.name == "pgdn":
		for i := 0; i < 10; i++ {
			m.moveDown()
		}
	case k.name == "pgup":
		for i := 0; i < 10; i++ {
			m.moveUp()
		}
	case k.r == 't':
		m.view = "tree"
	case k.r == 'w':
		m.view = "flow"
	case k.name == "enter":
		if r := m.selectedRow(); r != nil && r.kind == "iter" {
			m.open[r.id] = !m.open[r.id]
			m.rebuildRows()
		}
	case k.r == 'r':
		wf := m.run.Replay.Header.Workflow
		m.prompt = &prompt{label: "run " + wf + " [k=v ...]:", done: func(text string) string {
			args := []string{wf}
			for _, kv := range strings.Fields(text) {
				args = append(args, "--param", kv)
			}
			return m.spawn("run", args...)
		}}
	}
	return true
}

// paneMove moves the preview pane's cursor over its block heads.
func (m *model) paneMove(d int) {
	m.psel = clamp(m.psel+d, 0, len(m.pheads())-1)
}

// spawn starts `faber <sub> args…` as a detached child whose output goes to
// the log directory, and names the command it ran. The TUI then sees the
// run through the store like any other.
func (m *model) spawn(sub string, args ...string) string {
	exe := m.opts.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "spawn: " + err.Error()
		}
	}
	full := append([]string{sub}, args...)
	if sub == "run" {
		full = append(full, "--config", m.uo.ConfigPath)
	}
	full = append(full, "--progress", "json")
	if err := os.MkdirAll(m.opts.LogDir, 0o755); err != nil {
		return "spawn: " + err.Error()
	}
	logPath := filepath.Join(m.opts.LogDir, fmt.Sprintf("%s-%s-%d.log", sub, m.now().Format("20060102-150405.000000000"), os.Getpid()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "spawn: " + err.Error()
	}
	defer logFile.Close()
	cmd := exec.Command(exe, full...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "spawn: " + err.Error()
	}
	go cmd.Wait() // reap; the child outlives this view
	return "started: faber " + strings.Join(full, " ") + "   (log " + logPath + ")"
}

// loop is the event loop: keys, a one-hertz tick, redraw.
func (m *model) loop(ctx context.Context) error {
	t := openTerminal(m.uo.Stdin, m.uo.Stdout, [2]int{80, 24})
	defer func() {
		t.close()
		if m.quitCmd != "" {
			fmt.Fprintln(m.uo.Stdout, m.quitCmd)
		}
	}()
	keys := make(chan key, 16)
	go readKeys(m.uo.Stdin, keys)
	m.refresh(ctx)
	if m.uo.Run != "" {
		m.openRun(m.uo.Run)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	draw := func() {
		w, h := t.size()
		f := m.render(w, h)
		out := f.bytes()
		if m.bellNext {
			out = append(out, '\a')
			m.bellNext = false
		}
		io.WriteString(m.uo.Stdout, string(out))
	}
	draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			m.refresh(ctx)
			draw()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			if !m.handle(ctx, k) {
				return nil
			}
			if m.screen == "run" {
				m.rebuildRows()
			}
			draw()
		}
	}
}
