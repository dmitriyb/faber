package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/pipeline"
)

// renderRun draws the Run screen: header, the now/next strip, the chosen
// left view (tree, flow, timeline) beside STEP and OUTPUT, keys below.
func renderRun(f *frame, st style, m *model) {
	w, h := f.w, f.h
	v := m.run
	now := m.now()
	state := runStateOf(v)
	hdr := st.chrome("run") + "   " + st.ident(v.ID) + "   " + st.status(state) + "   " + v.Replay.Header.Workflow
	if n := v.Replay.Header.Name; n != "" {
		hdr += "   (" + n + ")"
	}
	for _, k := range sortedKeys(v.Replay.Header.Params) {
		hdr += "   " + k + "=" + v.Replay.Header.Params[k]
	}
	right := fmt.Sprintf("started %s   wall %s", v.Replay.Header.Started.Local().Format("15:04"), durText(int(wallOf(v.Replay, v.Live, now).Seconds())))
	if room := w - vis(right) - 1; vis(hdr) > room { // params can outgrow the width; colour is dropped with the overflow
		hdr = trunc(stripSGR(hdr), room)
	}
	f.put(0, pad(hdr, w-vis(right))+right)
	strip := m.nowStrip(st)
	if v.IR == nil && v.Err != nil {
		strip = pad(strip, w/2) + st.status("FAIL") + " " + st.dim(trunc("config: "+v.Err.Error(), w/2-6))
	}
	f.put(1, strip)
	f.put(2, st.rule(w))
	leftW := w * 42 / 100
	if leftW < 40 {
		leftW = 40
	}
	if leftW > w-13 {
		leftW = max(10, w-13) // a tiny terminal still gets both panes, however cramped
	}
	rightW := max(10, w-leftW-3)
	bodyH := max(3, h-4)
	var left []string
	switch m.view {
	case "flow":
		left = flowPane(st, m, leftW, bodyH)
	case "timeline":
		left = timelinePane(st, m, leftW, bodyH)
	default:
		left = treePane(st, m, leftW, bodyH)
	}
	// The scroll offsets belong to one selected step; a new selection
	// starts both panes at their natural place.
	id := ""
	if r := m.selectedRow(); r != nil {
		id = r.id
	}
	if m.scrolled != "" && id != m.scrolled {
		m.soff, m.ooff = 0, 0
	}
	m.scrolled = id
	// STEP takes the larger share when the hooks wrote about the attempt;
	// OUTPUT keeps a quarter
	stepH := bodyH * 55 / 100
	if r := m.selectedRow(); bodyH >= 24 && r != nil && r.kind == "step" && m.hasEvents(r.id) {
		stepH = bodyH * 75 / 100
	}
	rightCol := append(stepPane(st, m, rightW, stepH), outputPane(st, m, rightW, bodyH-stepH)...)
	for i, row := range panes(st, [][]string{left, rightCol}, []int{leftW, rightW}, bodyH) {
		f.put(3+i, row)
	}
	putFooter(f, st, m, m.runKeys(), "= faber runs + report")
}

// renderWorkflow draws the preview: the flow (or the folded
// structure) on the left, the params and the selected node on the right.
func renderWorkflow(f *frame, st style, m *model) {
	w, h := f.w, f.h
	v := m.run
	hdr := st.chrome("workflow") + "   " + st.ident(v.ID) + "   " + st.dim("preview: the flow before any run")
	path := displayPath(m.project.Path)
	f.put(0, pad(hdr, w-vis(path))+st.dim(path))
	f.put(1, st.rule(w))
	leftW := w * 60 / 100
	if leftW < 40 {
		leftW = 40
	}
	if leftW > w-13 {
		leftW = max(10, w-13)
	}
	rightW := max(10, w-leftW-3)
	bodyH := max(3, h-3)
	var left []string
	if m.view == "tree" {
		left = treePane(st, m, leftW, bodyH)
	} else {
		left = flowPane(st, m, leftW, bodyH)
	}
	right := m.paneWithCursor(st, previewPane(st, m, rightW), rightW)
	for i, row := range panes(st, [][]string{left, right}, []int{leftW, rightW}, bodyH) {
		f.put(2+i, row)
	}
	putFooter(f, st, m, m.workflowKeys(), "= faber validate --emit-ir, rendered")
}

// plural renders "1 attempt" / "2 attempts".
func scopeText(v *RunView) string {
	if v.Scope == ScopeAllRuns {
		return "all runs of " + v.Replay.Header.Workflow
	}
	return v.Scope
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// historyBlock renders a step's history as its own block: an empty line,
// then the sample count and one percentile per line, labels dim and
// values plain, so the eye reads a column of numbers rather than a
// sentence.
func historyBlock(st style, s Stats, where, hint string, w int) []string {
	row := func(k, val string) string { return cols([]string{"  " + st.dim(k), val}, []int{12, w - 12}) }
	return []string{
		"",
		row("history", plural(s.N, "attempt")+st.dim("   "+where+hint)),
		row("  p50", durText(int(s.P50.Seconds()))),
		row("  p90", durText(int(s.P90.Seconds()))),
		row("  p99", durText(int(s.P99.Seconds()))),
	}
}

// loopShape is one loop of the workflow as the author wrote it: its path
// with the iteration marks dropped ("cycles > review-cycle") and its cap.
type loopShape struct {
	name string
	cap  int
}

// loopShapes folds the unrolled loop bounds back to shapes, each once, in
// path order.
func loopShapes(ir *config.IR) []loopShape {
	seen := map[string]int{}
	var order []string
	for loop, cap := range config.LoopBounds(ir) {
		segs := strings.Split(loop, "/")
		var parts []string
		for k, seg := range segs {
			if k == 0 && seg == ir.Workflow {
				continue
			}
			if name, _, ok := config.IterationOf(seg); ok {
				parts = append(parts, name)
				continue
			}
			parts = append(parts, seg)
		}
		name := strings.Join(parts, " > ")
		if _, ok := seen[name]; !ok {
			order = append(order, name)
		}
		seen[name] = cap
	}
	sort.Strings(order)
	out := make([]loopShape, 0, len(order))
	for _, n := range order {
		out = append(out, loopShape{name: n, cap: seen[n]})
	}
	return out
}

func (m *model) workflowInfo(name string) *WorkflowInfo {
	for i := range m.project.Workflows {
		if m.project.Workflows[i].Name == name {
			return &m.project.Workflows[i]
		}
	}
	return nil
}

// runStateOf is the run's badge: LIVE, paused, FAILED, HALT, ok, incomplete.
func runStateOf(v *RunView) string {
	switch {
	case v.Live:
		return "LIVE"
	case v.Replay.End == nil:
		return "incomplete"
	case v.Replay.End.Status == failure.RunEndPaused:
		return "paused"
	case v.Replay.End.Status == failure.RunEndAborted:
		return "aborted"
	case v.Replay.End.Failed > 0:
		return "FAILED"
	case v.Replay.End.Halted > 0:
		return "HALT"
	}
	return "ok"
}

// nowStrip is the one-line answer to "where are we": the newest running
// step (or the failure), what settled last, and what comes next.
func (m *model) nowStrip(st style) string {
	v := m.run
	wf := v.Replay.Header.Workflow
	now := m.now()
	if len(v.Running) > 0 && v.Live {
		r := v.Running[0]
		line := "  now   " + st.ident(crumb(r.Step, wf, v.Bounds)) + "   " + st.status("running") + "  " + elapsedText(r.Since, now)
		if s, ok := v.Stats[leaf(r.Step)]; ok {
			if now.Sub(r.Since) > 2*s.P50 {
				line += "   " + st.status("slow") + st.dim("  usually "+durText(int(s.P50.Seconds())))
			} else {
				line += st.dim("   usually " + durText(int(s.P50.Seconds())))
			}
		}
		if len(v.Running) > 1 {
			line += st.dim(fmt.Sprintf("   +%d more running", len(v.Running)-1))
		}
		return line
	}
	if fails := m.problemIDs(); len(fails) > 0 && !v.Live {
		next := "resume"
		if n := m.haltNext(fails[0]); n != "" {
			next = trunc(sanitize(n), 60)
		}
		return "  now   " + st.dim("nothing running") + "   failed at " + st.ident(crumb(fails[0], wf, v.Bounds)) + "   next: " + next
	}
	if last := m.lastSettled(); last != "" {
		return "  last  " + st.ident(crumb(last, wf, v.Bounds)) + "   " + st.status(m.stateOf(last))
	}
	return "  now   " + st.dim("nothing running")
}

func leaf(id string) string { return id[strings.LastIndex(id, "/")+1:] }

// treePane renders the STEPS rows with the selection highlighted and the
// window scrolled to keep it visible.
func treePane(st style, m *model, w, h int) []string {
	out := []string{st.chrome("STEPS"), st.rule(w), st.dim(cols([]string{"", "step", "status", "duration", ""}, []int{2, 28, 13, 10, 1}))}
	rows := m.rows
	avail := h - len(out)
	start := 0
	if m.sel >= avail {
		start = m.sel - avail + 1
	}
	for i := start; i < len(rows) && i-start < avail; i++ {
		r := rows[i]
		name := strings.Repeat("  ", r.indent) + r.name
		nameW := 28
		statusCell := st.status(r.status)
		if m.preview {
			statusCell = ""
		}
		var line string
		switch r.kind {
		case "loop", "iter", "item", "gen":
			line = cols([]string{"", st.ident(trunc(name, nameW-1)), statusCell, r.dur, st.dim(r.note)}, []int{2, nameW, 13, 10, w - 53})
		default:
			line = cols([]string{"", st.ident(trunc(name, nameW-1)), statusCell, r.dur, r.note}, []int{2, nameW, 13, 10, w - 53})
		}
		if i == m.sel {
			line = st.rev(pad(stripSGR(line), w))
		}
		out = append(out, line)
	}
	if len(rows) == 0 {
		out = append(out, "  "+st.dim("nothing journaled yet"))
	}
	return out
}

// stepPane renders the selected step's facts.
func stepPane(st style, m *model, w, h int) []string {
	title := st.chrome("STEP")
	if m.rfocus == "step" {
		title = st.rev(" STEP ")
	}
	out := []string{title, st.rule(w), ""}
	r := m.selectedRow()
	v := m.run
	if r == nil || (r.kind != "step") {
		if r != nil {
			out = append(out, st.ident(crumb(r.id, v.Replay.Header.Workflow, v.Bounds)), "", st.dim("  select a step for its details"))
		}
		return padRows(out, h)
	}
	wf := v.Replay.Header.Workflow
	out = append(out, st.ident(crumb(r.id, wf, v.Bounds)))
	kv := func(k, val string) { out = append(out, cols([]string{"  " + st.dim(k), val}, []int{12, w - 12})) }
	line, _ := m.lineOf(r.id)
	kv("status", st.status(r.status))
	rec, hasRec := v.Replay.LastByStep[r.id]
	switch {
	case r.status == "running":
		kv("since", r.since.Local().Format("15:04:05")+"   "+elapsedText(r.since, m.now()))
	case line != nil && line.Error != nil:
		kv("reason", line.Error.Reason)
	case line != nil && line.Halt != nil:
		kv("reason", line.Halt.Reason)
	}
	if hasRec && rec.Result.Attempt > 0 {
		kv("attempt", fmt.Sprintf("%d", rec.Result.Attempt))
	}
	if line != nil && line.Duration != "" {
		dur := fmtDur(line.Duration)
		if s, ok := v.Stats[leaf(r.id)]; ok {
			if d, err := time.ParseDuration(line.Duration); err == nil && s.P90 > 0 && d > s.P90 {
				dur += "   " + st.status("slow") + st.dim("  past this step's p90")
			}
		}
		kv("duration", dur)
	}
	if line != nil && len(line.Outputs) > 0 {
		var parts []string
		for _, k := range sortedKeys(line.Outputs) {
			parts = append(parts, fmt.Sprintf("%s=%v", k, line.Outputs[k]))
		}
		kv("outputs", trunc(strings.Join(parts, "  "), w-12))
	}
	if s, ok := v.Stats[leaf(r.id)]; ok {
		out = append(out, historyBlock(st, s, scopeText(v), "   h to switch", w)...)
	} else {
		out = append(out, "", cols([]string{"  " + st.dim("history"), st.dim("no ok attempt   " + scopeText(v) + "   h to switch")}, []int{12, w - 12}))
	}
	// the error, handoff and halt keep their room; the hooks' account of
	// the attempt takes what is left, newest lines first to go
	var tail []string
	if line != nil && line.Error != nil {
		tail = append(tail, "", st.chrome("error"), "  "+trunc(line.Error.Detail, w-2))
		if line.Error.Handoff != "" {
			tail = append(tail, "", st.chrome("handoff"), "  "+trunc(line.Error.Handoff, w-2))
			tail = append(tail, "", st.chrome("re-enter")+st.dim("   printed on q"), "  faber resume "+v.ID+" \\", "    --interactive "+r.id)
		}
	}
	if line != nil && line.Halt != nil {
		tail = append(tail, "", st.chrome("halt"), "  "+trunc(line.Halt.Detail, w-2))
	}
	avail := h - len(out) - len(tail)
	if m.rfocus == "step" || m.soff > 0 {
		avail = 1 << 20 // every event row exists to scroll through
	}
	out = append(out, hooksBlock(st, m, r.id, avail, w)...)
	rows := append(out, tail...)
	// scroll: the title and rule stay, the body drops m.soff rows
	body := rows[2:]
	m.soff = clamp(m.soff, 0, max(0, len(body)-1))
	if m.soff > 0 {
		rows[0] += st.dim(fmt.Sprintf("   %d above", m.soff))
	}
	return padRows(append(rows[:2:2], body[m.soff:]...), h)
}

// hooksBlock is the attempt's hooks events file as the hooks wrote it:
// one row per event — writer, event, then the fields as key=value — with
// a halt event and a failed result in the FAIL colour, an ok result in the
// ok colour, and every line the reader refused shown as malformed with its
// line number. Nothing when no hook wrote anything.
func hooksBlock(st style, m *model, id string, avail, w int) []string {
	attempt := lastAttempt(m.run.Replay, id)
	ev, err := pipeline.AttemptEvents(m.run.RunDir, id, attempt)
	if err != nil {
		return []string{"", st.chrome("hooks"), "  " + st.dim(trunc(sanitize(err.Error()), w-2))}
	}
	if len(ev.Events) == 0 && len(ev.Malformed) == 0 {
		return nil
	}
	type row struct {
		line int
		text string
	}
	var rows []row
	for _, e := range ev.Events {
		name := sanitize(e.Name)
		if e.Name == contract.EventHalt {
			name = st.wrap(sgrRed, name)
		}
		var parts []string
		for _, k := range e.SortedFieldKeys() {
			kv := sanitize(k) + "=" + sanitize(e.Fields[k])
			if k == contract.EventResult {
				switch e.Fields[k] {
				case contract.ResultOK:
					kv = st.wrap(sgrGreen, kv)
				case contract.ResultFailed:
					kv = st.wrap(sgrRed, kv)
				}
			}
			parts = append(parts, kv)
		}
		rows = append(rows, row{e.Line, cols([]string{"  " + st.ident(sanitize(e.Hook)), name, strings.Join(parts, "  ")}, []int{18, 20, w - 38})})
	}
	for _, bad := range ev.Malformed {
		rows = append(rows, row{bad.Line, "  " + st.wrap(sgrRed, "malformed") + st.dim(fmt.Sprintf("  line %d: %s", bad.Line, trunc(sanitize(bad.Err), w-24)))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].line < rows[j].line })
	if avail < 3 {
		return nil
	}
	title := st.chrome("hooks") + st.dim(fmt.Sprintf("   attempt %d   %s", attempt, plural(len(ev.Events), "event")))
	if len(ev.Malformed) > 0 {
		title += "   " + st.wrap(sgrRed, plural(len(ev.Malformed), "malformed line"))
	}
	if ev.Truncated {
		title += st.dim("   oldest not read")
	}
	if n := avail - 2; len(rows) > n {
		title += st.dim(fmt.Sprintf("   %d earlier not shown", len(rows)-n))
		rows = rows[len(rows)-n:]
	}
	out := []string{"", title}
	for _, r := range rows {
		out = append(out, r.text)
	}
	return out
}

// hasEvents reports whether a hook wrote anything about the step's newest
// attempt.
func (m *model) hasEvents(id string) bool {
	ev, err := pipeline.AttemptEvents(m.run.RunDir, id, lastAttempt(m.run.Replay, id))
	return err == nil && (len(ev.Events) > 0 || len(ev.Malformed) > 0)
}

// haltNext is the operator's next step as the failed step's own halt event
// states it, or empty.
func (m *model) haltNext(id string) string {
	ev, err := pipeline.AttemptEvents(m.run.RunDir, id, lastAttempt(m.run.Replay, id))
	if err != nil {
		return ""
	}
	next := ""
	for _, e := range ev.Events {
		if e.Name == contract.EventHalt && e.Fields[contract.EventNext] != "" {
			next = e.Fields[contract.EventNext]
		}
	}
	return next
}

// outputPane tails the selected step's newest attempt output.
func outputPane(st style, m *model, w, h int) []string {
	r := m.selectedRow()
	title := st.chrome("OUTPUT")
	if m.rfocus == "output" {
		title = st.rev(" OUTPUT ")
	}
	out := []string{}
	if r == nil || r.kind != "step" {
		out = append(out, title, st.rule(w))
		return padRows(out, h)
	}
	attempt := lastAttempt(m.run.Replay, r.id)
	lines, above := outputWindow(m.run.RunDir, r.id, attempt, max(0, h-3), &m.ooff)
	head := title + fmt.Sprintf("   attempt %d   ", attempt) + st.dim("output.log")
	if above > 0 {
		head += st.dim(fmt.Sprintf("   %d above", above))
	}
	out = append(out, head, st.rule(w))
	if len(lines) == 0 {
		out = append(out, "  "+st.dim("no output captured"))
	}
	for _, l := range lines {
		out = append(out, trunc(sanitize(l), w))
	}
	return padRows(out, h)
}

func padRows(rows []string, h int) []string {
	for len(rows) < h {
		rows = append(rows, "")
	}
	return rows[:h]
}

// sanitize keeps box-derived text inside the renderer's contract: plain
// printable ASCII, one byte per cell. Controls and anything wider become
// '?', since a double-width rune would break every column after it.
func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			sb.WriteString("    ")
		case r < 0x20 || r >= 0x7f:
			sb.WriteByte('?')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// lineOf finds a report line by id across top level and instances.
func (m *model) lineOf(id string) (*pipeline.StepLine, bool) {
	for i := range m.run.Report.Steps {
		if m.run.Report.Steps[i].ID == id {
			return &m.run.Report.Steps[i], true
		}
	}
	for _, g := range m.run.Report.Generate {
		for _, it := range g.Items {
			for i := range it.Steps {
				if it.Steps[i].ID == id {
					return &it.Steps[i], true
				}
			}
		}
	}
	return nil, false
}

func (m *model) stateOf(id string) string {
	if l, ok := m.lineOf(id); ok {
		return statusWord(l.Status)
	}
	return ""
}

// lastSettled is the newest result record's step.
func (m *model) lastSettled() string {
	var best string
	var at time.Time
	for id, rec := range m.run.Replay.LastByStep {
		if rec.InputHash == "" {
			continue
		}
		if f := rec.Result.Timing.Finished; f.After(at) {
			at, best = f, id
		}
	}
	return best
}

// problemIDs lists failed and halted executed steps in reading order.
func (m *model) problemIDs() []string {
	var out []string
	for _, r := range m.rows {
		if r.kind == "step" && (r.status == "FAIL" || r.status == "HALT") {
			out = append(out, r.id)
		}
	}
	return out
}

// pline is one line of the preview's right pane. A line with a key is a
// block head the cursor can rest on and enter opens or closes; the rest
// is content.
type pline struct {
	text string
	key  string
}

// previewPane is the preview's right pane: PARAMS, STRUCTURE, NEEDS and
// STEP as blocks that start folded to one summary line each (STEP open),
// opened by enter when the pane has focus. A block with a problem opens
// itself. The pane's lines are kept on the model for the cursor.
func previewPane(st style, m *model, w int) []string {
	v := m.run
	wf := m.workflowInfo(v.ID)
	var lines []pline
	head := func(key, title, summary string) {
		mark := ">"
		if m.popen[key] {
			mark = "v"
		}
		lines = append(lines, pline{text: st.dim(mark) + " " + st.chrome(title) + "   " + summary, key: key})
	}
	body := func(key string, rows ...string) {
		if m.popen[key] {
			for _, r := range rows {
				lines = append(lines, pline{text: r})
			}
			lines = append(lines, pline{})
		}
	}
	kv := func(k, val string) string { return cols([]string{"    " + st.dim(k), val}, []int{16, w - 16}) }

	// PARAMS
	var prows []string
	if wf != nil {
		for _, p := range wf.Params {
			val := p.Type
			switch {
			case p.Default != "":
				val += "   default " + p.Default
			case p.Required:
				val += "   required"
			}
			prows = append(prows, cols([]string{"    " + st.ident(p.Name), val}, []int{16, w - 16}))
		}
	}
	head("params", "PARAMS", plural(len(prows), "param"))
	if len(prows) == 0 {
		prows = []string{"    " + st.dim("none")}
	}
	body("params", prows...)

	// STRUCTURE
	var srows []string
	shapes := loopShapes(v.IR)
	for _, sh := range shapes {
		srows = append(srows, cols([]string{"    " + st.ident(trunc(sh.name, 26)), fmt.Sprintf("loop, up to %d iterations", sh.cap)}, []int{30, w - 30}))
	}
	gens := 0
	for i := range v.IR.Nodes {
		n := &v.IR.Nodes[i]
		if n.Kind == config.KindGenerate && n.Gen != nil {
			gens++
			srows = append(srows, cols([]string{"    " + st.ident(trunc(strings.TrimPrefix(n.ID, v.ID+"/"), 26)), "generate over " + n.Gen.Source + " -> " + n.Gen.Workflow}, []int{30, w - 30}))
		}
	}
	sum := plural(len(shapes), "loop")
	if gens > 0 {
		sum += "   " + plural(gens, "generate")
	}
	head("structure", "STRUCTURE", sum)
	if len(srows) == 0 {
		srows = []string{"    " + st.dim("a straight sequence")}
	}
	body("structure", srows...)

	// NEEDS: what this workflow's steps require, and whether it is there
	needs := needsOf(m.project, v.IR)
	missing := len(needs.Slots)
	for _, c := range append(append([]Check{}, needs.Images...), needs.Roles...) {
		if c.State == "MISSING" || c.State == "FAIL" {
			missing++
		}
	}
	if needs.Resolver.State == "MISSING" {
		missing++
	}
	state := st.status("ok")
	if missing > 0 {
		state = st.wrap(sgrRed, fmt.Sprintf("%d MISSING", missing))
		m.autoOpen("needs")
	}
	head("needs", "NEEDS", fmt.Sprintf("%s   %s   %s   %s   ", plural(len(needs.Images), "image"), plural(len(needs.Roles), "role"), plural(needs.Templates, "template"), plural(len(needs.Services), "service"))+state)
	body("needs", checklistLines(st, needsClasses(m.project, v.IR), w, 2, -1)...)

	// STEP: the selected step's schema facts, with sub-blocks
	r := m.selectedRow()
	if r == nil || r.kind != "step" {
		m.plines = lines
		return plineTexts(lines)
	}
	var n *config.Node
	for _, cand := range nodesOf(v.IR) {
		if cand.ID == r.id {
			n = cand
		}
	}
	if n == nil || n.Template == nil {
		m.plines = lines
		return plineTexts(lines)
	}
	t := n.Template
	roleState := ""
	for _, c := range m.project.Preflight.Roles {
		if c.Name == t.Identity {
			roleState = c.State
		}
	}
	summary := st.ident(leaf(r.id)) + "   template " + t.Name
	if t.Identity != "" {
		summary += "   identity " + t.Identity
	}
	head("step", "STEP", summary)
	if m.popen["step"] {
		rows := []string{kv("template", t.Name)}
		if t.Identity != "" {
			rows = append(rows, kv("identity", t.Identity+"   "+st.status(roleState)))
		}
		if t.Lane != "" {
			rows = append(rows, kv("lane", t.Lane))
		}
		if n.When != nil {
			rows = append(rows, kv("when", trunc(leafCEL(n.When.CEL), w-16)))
		}
		if n.Retry > 0 {
			rows = append(rows, kv("retry", fmt.Sprintf("%d", n.Retry)))
		}
		if n.OnFailure != "" {
			rows = append(rows, kv("on_failure", n.OnFailure))
		}
		for _, row := range rows {
			lines = append(lines, pline{text: row})
		}
		// hooks: all four, always
		set, hmissing := 0, 0
		var hrows []string
		for _, h := range hookPaths(t) {
			if h[1] == "" {
				hrows = append(hrows, cols([]string{"      " + st.ident(h[0]), st.dim("none")}, []int{18, w - 18}))
				continue
			}
			set++
			hs := st.status("ok")
			if err := config.CheckHookFile(h[1]); err != nil {
				hs = st.wrap(sgrRed, "MISSING")
				if !errors.Is(err, config.ErrHookNotFound) {
					hs += st.dim("  " + err.Error())
				}
				hmissing++
			}
			hrows = append(hrows, cols([]string{"      " + st.ident(h[0]), relPath(m.project.Dir, h[1]) + "   " + hs}, []int{18, w - 18}))
		}
		hsum := fmt.Sprintf("%d of 4 set   ", set) + st.status("ok")
		if hmissing > 0 {
			hsum = fmt.Sprintf("%d of 4 set   ", set) + st.wrap(sgrRed, fmt.Sprintf("%d MISSING", hmissing))
			m.autoOpen("step.hooks")
		}
		sub := func(key, title, summary string, rows []string) {
			mark := ">"
			if m.popen[key] {
				mark = "v"
			}
			lines = append(lines, pline{text: "  " + st.dim(mark) + " " + st.dim(title) + "   " + summary, key: key})
			if m.popen[key] {
				for _, row := range rows {
					lines = append(lines, pline{text: row})
				}
			}
		}
		sub("step.hooks", "hooks", hsum, hrows)
		// inputs and outputs
		var iorows []string
		for _, name := range sortedKeys(n.Bindings) {
			iorows = append(iorows, cols([]string{"      " + st.ident(name), st.dim("<- ") + bindingText(n.Bindings[name])}, []int{18, w - 18}))
		}
		for _, name := range sortedKeys(t.Output) {
			iorows = append(iorows, cols([]string{"      " + st.ident(name), st.dim("-> ") + t.Output[name].Type}, []int{18, w - 18}))
		}
		sub("step.io", "inputs/outputs", fmt.Sprintf("%s   %s", plural(len(n.Bindings), "input"), plural(len(t.Output), "output")), iorows)
		agent := "skill " + t.Skill + "   model " + t.Model + "   effort " + t.Effort
		if len(n.Tools) > 0 {
			agent += "   tools " + strings.Join(n.Tools, " ")
		}
		lines = append(lines, pline{text: kv("agent", trunc(agent, w-16))})
		if s, ok := v.Stats[leaf(r.id)]; ok {
			sub("step.history", "history", plural(s.N, "attempt")+"   p50 "+durText(int(s.P50.Seconds()))+"   "+st.dim("all runs of "+v.ID), []string{
				kv("  p50", durText(int(s.P50.Seconds()))), kv("  p90", durText(int(s.P90.Seconds()))), kv("  p99", durText(int(s.P99.Seconds()))),
			})
		} else {
			lines = append(lines, pline{text: "  " + st.dim("  history   no ok attempt in the store")})
		}
	}
	m.plines = lines
	return plineTexts(lines)
}

func plineTexts(lines []pline) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

// paneWithCursor marks the focused pane's selected head as the selection bar.
func (m *model) paneWithCursor(st style, lines []string, w int) []string {
	if !m.pfocus {
		return lines
	}
	heads := m.pheads()
	if m.psel >= len(heads) {
		m.psel = max(0, len(heads)-1)
	}
	if m.psel < len(heads) {
		i := heads[m.psel]
		lines[i] = st.rev(pad(stripSGR(lines[i]), w))
	}
	return lines
}

// checkLine is one NEEDS row: the kind, the name, where it was looked for,
// the state, and the fix when MISSING.
func checkLine(st style, kind string, c Check, w int) string {
	val := st.ident(c.Name)
	if c.Detail != "" {
		val += "   " + c.Detail
	}
	val += "   " + st.status(c.State)
	if c.State == "MISSING" && c.Fix != "" {
		val += "   " + c.Fix
	}
	return cols([]string{"    " + st.dim(kind), trunc(val, w-16)}, []int{16, w - 16})
}

// bindingText reads a binding the way the YAML wrote it.
func bindingText(b config.BindingDesc) string {
	switch b.Kind {
	case config.BindLiteral:
		return fmt.Sprintf("%v", b.Value)
	case "param":
		return "params." + b.Name
	case "item":
		return "item." + b.Field
	}
	return b.Kind
}

// nodesOf lists every node of an IR, sub-workflows included.
func nodesOf(ir *config.IR) []*config.Node {
	var out []*config.Node
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		for i := range g.Nodes {
			out = append(out, &g.Nodes[i])
			if g.Nodes[i].Sub != nil {
				walk(g.Nodes[i].Sub)
			}
		}
	}
	walk(ir)
	return out
}
