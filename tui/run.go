package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/pipeline"
)

// renderRun draws the Run screen: header, the now/next strip, the chosen
// left view (tree, graph, timeline) beside STEP and OUTPUT, keys below.
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
	f.put(0, pad(hdr, w-vis(right))+right)
	f.put(1, m.nowStrip(st))
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
	stepH := bodyH * 55 / 100
	rightCol := append(stepPane(st, m, rightW, stepH), outputPane(st, m, rightW, bodyH-stepH)...)
	for i, row := range panes(st, [][]string{left, rightCol}, []int{leftW, rightW}, bodyH) {
		f.put(3+i, row)
	}
	f.put(h-1, footer(st, w, m.runKeys(), "= faber runs + report"))
}

// renderWorkflow draws the preview: the compute graph (or the folded
// structure) on the left, the params and the selected node on the right.
func renderWorkflow(f *frame, st style, m *model) {
	w, h := f.w, f.h
	v := m.run
	wf := m.workflowInfo(v.ID)
	hdr := st.chrome("workflow") + "   " + st.ident(v.ID) + "   " + st.dim("preview: the flow before any run")
	f.put(0, pad(hdr, w-vis(m.project.Path))+st.dim(m.project.Path))
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
	right := []string{st.chrome("PARAMS"), st.rule(rightW), ""}
	if wf != nil {
		for _, p := range wf.Params {
			val := p.Type
			switch {
			case p.Default != "":
				val += "   default " + p.Default
			case p.Required:
				val += "   required"
			}
			right = append(right, cols([]string{"  " + st.ident(p.Name), val}, []int{14, rightW - 14}))
		}
		if len(wf.Params) == 0 {
			right = append(right, "  "+st.dim("none"))
		}
	}
	right = append(right, "", st.chrome("STRUCTURE"), st.rule(rightW), "")
	nameW := 28
	for _, sh := range loopShapes(v.IR) {
		right = append(right, cols([]string{"  " + st.ident(trunc(sh.name, nameW-3)), fmt.Sprintf("loop, up to %d iterations", sh.cap)}, []int{nameW, rightW - nameW}))
	}
	for i := range v.IR.Nodes {
		n := &v.IR.Nodes[i]
		if n.Kind == config.KindGenerate && n.Gen != nil {
			right = append(right, cols([]string{"  " + st.ident(trunc(strings.TrimPrefix(n.ID, v.ID+"/"), nameW-3)), "generate over " + n.Gen.Source + " -> " + n.Gen.Workflow}, []int{nameW, rightW - nameW}))
		}
	}
	if r := m.selectedRow(); r != nil && r.kind == "step" {
		right = append(right, "", st.chrome("STEP"), st.rule(rightW), "", st.ident(crumb(r.id, v.ID, v.Bounds)))
		for i := range v.IR.Nodes {
			n := &v.IR.Nodes[i]
			if n.ID != r.id {
				continue
			}
			if n.Template != nil {
				right = append(right, cols([]string{"  template", n.Template.Name}, []int{12, rightW - 12}))
				if n.Template.Lane != "" {
					right = append(right, cols([]string{"  lane", n.Template.Lane}, []int{12, rightW - 12}))
				}
			}
			if n.When != nil {
				right = append(right, cols([]string{"  when", trunc(n.When.CEL, rightW-12)}, []int{12, rightW - 12}))
			}
			if n.Retry > 0 {
				right = append(right, cols([]string{"  retry", fmt.Sprintf("%d", n.Retry)}, []int{12, rightW - 12}))
			}
		}
		if s, ok := v.Stats[leaf(r.id)]; ok {
			right = append(right, cols([]string{"  history", statsText(s) + st.dim("   over "+plural(s.N, "attempt"))}, []int{12, rightW - 12}))
		}
	}
	for i, row := range panes(st, [][]string{left, right}, []int{leftW, rightW}, bodyH) {
		f.put(2+i, row)
	}
	f.put(h-1, footer(st, w, m.workflowKeys(), "= faber validate --emit-ir, rendered"))
}

// plural renders "1 attempt" / "2 attempts".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// statsText renders a distribution as "p50 4m10s  p90 9m01s  p99 20m05s".
func statsText(s Stats) string {
	return fmt.Sprintf("p50 %s  p90 %s  p99 %s", durText(int(s.P50.Seconds())), durText(int(s.P90.Seconds())), durText(int(s.P99.Seconds())))
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
		return "  now   " + st.dim("nothing running") + "   failed at " + st.ident(crumb(fails[0], wf, v.Bounds)) + "   next: resume"
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
	out := []string{st.chrome("STEP"), st.rule(w), ""}
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
	kv := func(k, val string) { out = append(out, cols([]string{"  " + k, val}, []int{12, w - 12})) }
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
		kv("duration", fmtDur(line.Duration))
	}
	if s, ok := v.Stats[leaf(r.id)]; ok {
		kv("usually", statsText(s)+st.dim("   over "+plural(s.N, "attempt")+" in the store"))
	}
	if line != nil && len(line.Outputs) > 0 {
		var parts []string
		for _, k := range sortedKeys(line.Outputs) {
			parts = append(parts, fmt.Sprintf("%s=%v", k, line.Outputs[k]))
		}
		kv("outputs", trunc(strings.Join(parts, "  "), w-12))
	}
	if line != nil && line.Error != nil {
		out = append(out, "", st.chrome("error"), "  "+trunc(line.Error.Detail, w-2))
		if line.Error.Handoff != "" {
			out = append(out, "", st.chrome("handoff"), "  "+trunc(line.Error.Handoff, w-2))
			out = append(out, "", st.chrome("re-enter")+st.dim("   printed on q"), "  faber resume "+v.ID+" \\", "    --interactive "+r.id)
		}
	}
	if line != nil && line.Halt != nil {
		out = append(out, "", st.chrome("halt"), "  "+trunc(line.Halt.Detail, w-2))
	}
	return padRows(out, h)
}

// outputPane tails the selected step's newest attempt output.
func outputPane(st style, m *model, w, h int) []string {
	r := m.selectedRow()
	title := st.chrome("OUTPUT")
	out := []string{}
	if r == nil || r.kind != "step" {
		out = append(out, title, st.rule(w))
		return padRows(out, h)
	}
	attempt := lastAttempt(m.run.Replay, r.id)
	out = append(out, title+fmt.Sprintf("   attempt %d   ", attempt)+st.dim("output.log"), st.rule(w))
	lines := outputTail(m.run.RunDir, r.id, attempt, h-3)
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

// sanitize strips terminal controls from box-derived text, like the
// report's Text path.
func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			if r == '\t' {
				sb.WriteString("    ")
				continue
			}
			sb.WriteRune('�')
			continue
		}
		sb.WriteRune(r)
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
