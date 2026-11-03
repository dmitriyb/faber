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

	project *Project
	runs    []RunRow
	runSel  int
	wfSel   int
	focus   string // Home: "runs" | "workflows"
	meds    map[string]time.Duration
	preview bool // the Run screen is a static workflow preview (no journal)

	run     *RunView
	rows    []row
	sel     int
	frows   []frow // the flow view's rows
	fsel    int    // the flow cursor
	view    string // "tree" | "flow" | "timeline"
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
	return &model{opts: o, uo: uo, st: style{color: uo.Color}, now: o.Now, screen: "home", focus: "runs", view: "tree", follow: true, open: map[string]bool{}, alerted: map[string]bool{}}
}

func (m *model) selectedRun() *RunRow {
	if m.runSel < 0 || m.runSel >= len(m.runs) {
		return nil
	}
	return &m.runs[m.runSel]
}

func (m *model) selectedRow() *row {
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

// moveDown / moveUp move whichever cursor the view shows; the flow cursor
// skips frame edges, which are not things.
func (m *model) moveDown() {
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

// selectedRunLines are Home's now / last / next lines for the selection.
func (m *model) selectedRunLines() []string {
	sel := m.selectedRun()
	if sel == nil {
		return nil
	}
	v, err := loadRun(m.opts.Store, sel.ID, m.meds, m.now())
	if err != nil {
		return []string{"  " + m.st.dim(err.Error())}
	}
	saved := m.run
	m.run = v
	m.rows = treeRows(v, m.open, m.now())
	lines := []string{m.nowStrip(m.st)}
	if last := m.lastSettled(); last != "" {
		lines = append(lines, "  last  "+m.st.ident(crumb(last, v.Replay.Header.Workflow, v.Bounds))+"   "+m.st.status(m.stateOf(last)))
	}
	if v.Live && len(v.Running) == 0 {
		lines = append(lines, "  next  "+m.st.dim("waiting for a slot, a lane, or a defer window"))
	}
	m.run = saved
	return lines
}

// refresh reloads everything the current screen shows.
func (m *model) refresh(ctx context.Context) {
	if m.project == nil {
		m.project = loadProject(ctx, m.uo.ConfigPath, m.opts.Images)
	}
	if m.meds == nil {
		m.meds = medians(m.opts.Store)
	}
	m.runs = loadRuns(m.opts.Store, m.now())
	if m.runSel >= len(m.runs) {
		m.runSel = len(m.runs) - 1
	}
	if m.runSel < 0 {
		m.runSel = 0
	}
	if m.screen == "run" && m.run != nil && !m.preview {
		if v, err := loadRun(m.opts.Store, m.run.ID, m.meds, m.now()); err == nil {
			m.run = v
			m.rebuildRows()
		}
	}
}

func (m *model) rebuildRows() {
	m.frows = flowRows(m.run, m.preview, m.now())
	if m.fsel >= len(m.frows) {
		m.fsel = 0
	}
	if m.fsel < len(m.frows) && m.frows[m.fsel].kind != "step" && m.frows[m.fsel].kind != "gen" {
		m.moveDown()
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
	v, err := loadRun(m.opts.Store, id, m.meds, m.now())
	if err != nil {
		m.status = err.Error()
		return
	}
	m.run = v
	m.preview = false
	m.screen = "run"
	m.sel = 0
	m.rebuildRows()
}

// openWorkflow switches to the Workflow screen: the Run screen's views
// over the IR alone, nothing journaled — the compute graph before a run.
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
	m.rebuildRows()
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
		f.put(h-2, m.st.dim(trunc(m.status, w)))
	}
	return f
}

func (m *model) homeKeys() [][2]string {
	if m.focus == "workflows" {
		return [][2]string{{"enter", "preview workflow"}, {"tab", "runs"}, {"r", "run workflow"}, {"q", "quit"}}
	}
	return [][2]string{{"enter", "open run"}, {"tab", "workflows"}, {"r", "run workflow"}, {"p", "pause"}, {"R", "resume"}, {"q", "quit"}}
}

func (m *model) workflowKeys() [][2]string {
	return [][2]string{{"j/k", "move"}, {"t/g", "tree flow"}, {"enter", "expand"}, {"r", "run this workflow"}, {"q", "back"}}
}

func (m *model) runKeys() [][2]string {
	return [][2]string{{"j/k", "move"}, {"n", "next problem"}, {"/", "filter"}, {"t/g/l", "tree flow timeline"}, {"f", "follow"}, {"q", "back"}}
}

// handle applies one key; it returns false when the app should exit.
func (m *model) handle(ctx context.Context, k key) bool {
	m.status = ""
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

func (m *model) handleHome(ctx context.Context, k key) bool {
	switch {
	case k.r == 'q' || k.name == "esc":
		return false
	case k.r == '\t' || k.name == "right" || k.name == "left":
		if m.focus == "runs" {
			m.focus = "workflows"
		} else {
			m.focus = "runs"
		}
	case k.r == 'j' || k.name == "down":
		if m.focus == "workflows" {
			if m.wfSel < len(m.project.Workflows)-1 {
				m.wfSel++
			}
		} else if m.runSel < len(m.runs)-1 {
			m.runSel++
		}
	case k.r == 'k' || k.name == "up":
		if m.focus == "workflows" {
			if m.wfSel > 0 {
				m.wfSel--
			}
		} else if m.runSel > 0 {
			m.runSel--
		}
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
			if sel.State != "LIVE" {
				m.status = "pause: run " + sel.ID + " is not live"
				break
			}
			if err := m.opts.Store.RequestPause(sel.ID); err != nil {
				m.status = err.Error()
			} else {
				m.status = "pause requested: faber runs pause " + sel.ID
			}
		}
	case k.r == 'R':
		if sel := m.selectedRun(); sel != nil {
			m.status = m.spawn("resume", sel.ID)
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

func (m *model) handleRun(ctx context.Context, k key) bool {
	if m.preview {
		return m.handleWorkflow(ctx, k)
	}
	switch {
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
		m.sel = min(m.sel+10, len(m.rows)-1)
	case k.name == "pgup":
		m.follow = false
		m.sel = max(m.sel-10, 0)
	case k.r == 'n':
		m.follow = false
		for i := 1; i <= len(m.rows); i++ {
			j := (m.sel + i) % len(m.rows)
			if r := m.rows[j]; r.kind == "step" && (r.status == "FAIL" || r.status == "HALT" || r.status == "WAIT") {
				m.sel = j
				break
			}
		}
	case k.r == 'f':
		m.follow = !m.follow
		m.rebuildRows()
	case k.r == 't':
		m.view = "tree"
	case k.r == 'g':
		m.view = "flow"
	case k.r == 'l':
		m.view = "timeline"
	case k.name == "enter":
		if r := m.selectedRow(); r != nil && r.kind == "iter" {
			m.open[r.id] = !m.open[r.id]
			m.rebuildRows()
		}
	case k.r == '/':
		m.prompt = &prompt{label: "filter:", done: func(text string) string {
			m.filter = text
			m.rebuildRows()
			if text == "" {
				return ""
			}
			return "filter: " + text + " (empty to clear)"
		}}
	}
	return true
}

// handleWorkflow is the preview's key set: move, open iterations, switch
// tree/graph, start the workflow, back.
func (m *model) handleWorkflow(ctx context.Context, k key) bool {
	switch {
	case k.r == 'q' || k.name == "esc":
		m.screen = "home"
		m.preview = false
	case k.r == 'j' || k.name == "down":
		m.moveDown()
	case k.r == 'k' || k.name == "up":
		m.moveUp()
	case k.r == 't':
		m.view = "tree"
	case k.r == 'g':
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
	logPath := filepath.Join(m.opts.LogDir, fmt.Sprintf("%s-%s.log", sub, m.now().Format("20060102-150405")))
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
