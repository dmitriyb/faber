package tui

import (
	"fmt"
	"strings"
	"time"
)

// renderHome draws the Home screen: the project on the left, the runs on
// the right, the selected run's now/last/next below, keys in the footer.
func renderHome(f *frame, st style, m *model) {
	w, h := f.w, f.h
	now := m.now()
	f.put(0, pad(st.chrome("faber")+"   "+displayPath(m.project.Path), w-8)+now.Format("15:04:05"))
	f.put(1, st.rule(w))
	leftW := w * 3 / 10
	if leftW < 36 {
		leftW = 36
	}
	if leftW > 66 {
		leftW = 66
	}
	rightW := w - leftW - 3
	bodyH := h - 2 - 1 - 5 - 1 // header+rule, rule, selected block, footer
	if bodyH < 4 {
		bodyH = 4
	}
	left := projectPane(st, m, leftW, bodyH)
	right := runsPane(st, m, rightW, bodyH)
	for i, row := range panes(st, [][]string{left, right}, []int{leftW, rightW}, bodyH) {
		f.put(2+i, row)
	}
	y := 2 + bodyH
	f.put(y, st.rule(w))
	y++
	if sel := m.selectedRun(); sel != nil {
		f.put(y, st.chrome("SELECTED")+"   "+st.ident(sel.ID)+"   "+sel.Workflow+"   started "+sel.Started.Local().Format("2006-01-02 15:04")+"   "+st.status(sel.State))
		lines := m.selectedRunLines()
		for i := 0; i < 3; i++ {
			if i < len(lines) {
				f.put(y+1+i, lines[i])
			}
		}
	} else {
		f.put(y, st.dim("no runs in the store yet"))
	}
	putFooter(f, st, m, m.homeKeys(), "= faber runs")
}

func projectPane(st style, m *model, w, h int) []string {
	p := m.project
	if p.Err == nil && m.pfDrill != "" {
		if c, ok := classByName(preflightClasses(p), m.pfDrill); ok {
			return drillPane(st, m, c, w, h)
		}
	}
	out := []string{st.chrome("PROJECT"), st.rule(w), ""}
	if p.Err != nil {
		out = append(out, "  config  "+st.status("FAIL"))
		for _, l := range strings.Split(p.Err.Error(), "\n") {
			out = append(out, "  "+trunc(l, w-2))
		}
		return out
	}
	widths := []int{14, w - 14}
	out = append(out, st.dim(cols([]string{"  workflow", "params"}, widths)))
	for i, wf := range p.Workflows {
		var ps []string
		for _, pr := range wf.Params {
			switch {
			case pr.Default != "":
				ps = append(ps, pr.Name+"="+pr.Default)
			case pr.Required:
				ps = append(ps, pr.Name+"=<required>")
			default:
				ps = append(ps, pr.Name+"=?")
			}
		}
		line := cols([]string{"  " + st.ident(wf.Name), strings.Join(ps, "  ")}, widths)
		if m.focus == "workflows" && i == m.wfSel {
			line = st.rev(pad(stripSGR(line), w))
		}
		out = append(out, line)
	}
	// the checklist: two problem rows per class, one on a short pane
	budget := 2
	if h < 30 {
		budget = 1
	}
	sel := -1
	if m.focus == "workflows" && m.wfSel >= len(p.Workflows) {
		sel = m.wfSel - len(p.Workflows)
	}
	out = append(out, "")
	out = append(out, checklistLines(st, preflightClasses(p), w, budget, sel)...)
	return out
}

func runsPane(st style, m *model, w, h int) []string {
	live := 0
	for _, r := range m.shown {
		if r.State == "LIVE" {
			live++
		}
	}
	title := st.chrome("RUNS") + "   this config"
	if live > 0 {
		title += "   " + st.status("LIVE") + fmt.Sprintf(" %d", live)
	}
	if m.rfilter != "" {
		title += "   " + st.dim("filter "+m.rfilter)
	}
	if w >= 70 {
		title = pad(title, w-20) + st.dim(fmt.Sprintf("other configs: %d", m.otherRuns()))
	}
	out := []string{title, st.rule(w), ""}
	wide := w >= 100
	widths := []int{28, 12, 11, 11, 18, 11, w - 91}
	header := []string{"  run id", "name", "workflow", "state", "started", "wall", "now"}
	if !wide {
		widths = []int{27, 10, w - 37}
		header = []string{"  run id", "state", "now"}
	}
	out = append(out, st.dim(cols(header, widths)))
	if len(m.shown) == 0 {
		if m.rfilter != "" {
			return append(out, "  "+st.dim("no run matches the filter"))
		}
		return append(out, "  "+st.dim("no journaled runs"))
	}
	row := func(i int) string {
		r := m.shown[i]
		name := r.Name
		if name == "" {
			name = st.dim("unnamed")
		}
		wall := ""
		if r.Wall > 0 {
			wall = durText(int(r.Wall.Seconds()))
		}
		nowCol := ""
		if r.Now != "" {
			nowCol = st.ident(crumb(r.Now, r.Workflow, r.bounds)) + "   " + st.status("running")
		}
		cells := []string{"  " + st.ident(r.ID), name, r.Workflow, st.status(r.State), r.Started.Local().Format("2006-01-02 15:04"), wall, nowCol}
		if !wide {
			cells = []string{"  " + st.ident(r.ID), st.status(r.State), nowCol}
		}
		line := cols(cells, widths)
		switch {
		case i == m.runSel && m.focus == "runs":
			line = st.rev(pad(stripSGR(line), w))
		case i == m.runSel:
			line = st.dim(">") + line[1:] // the remembered selection, unfocused
		}
		return line
	}
	// the live block is pinned at the top, a blank line under it; the
	// history scrolls beneath so the cursor stays visible
	gap := 0
	if live > 0 && live < len(m.shown) {
		gap = 1
	}
	avail := max(1, h-len(out)-gap)
	for i := 0; i < live && i < avail; i++ {
		out = append(out, row(i))
	}
	if gap == 1 {
		out = append(out, "")
	}
	room := avail - live
	start := live
	if m.runSel >= live && m.runSel-live >= room {
		start = m.runSel - room + 1
	}
	for i := start; i < len(m.shown) && i-start < room; i++ {
		out = append(out, row(i))
	}
	return out
}

// tagHash is the hash part of an image tag ("faber/review:20be39bb9bb7" ->
// "20be39bb9bb7"), what distinguishes a rebuilt image.
func tagHash(tag string) string {
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		return tag[i+1:]
	}
	return tag
}

// sameConfig compares a journaled config path with the project's: equal
// paths, or the same base name when one is relative.
func sameConfig(journaled, project string) bool {
	if journaled == project {
		return true
	}
	return strings.HasSuffix(journaled, "/"+project) || strings.HasSuffix(project, "/"+journaled)
}

// putFooter draws the key legend on the last row, with the CLI equivalent
// on the right when it fits; on a narrow terminal the legend folds onto a
// second row above rather than dropping a key.
func putFooter(f *frame, st style, m *model, keys [][2]string, equiv string) {
	rows := footerRows(st, f.w, keys, equiv)
	m.footerRows = len(rows)
	for i, r := range rows {
		f.put(f.h-len(rows)+i, r)
	}
}

func footerRows(st style, w int, keys [][2]string, equiv string) []string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, st.chrome(k[0])+" "+k[1])
	}
	line := func(ps []string) string {
		s := strings.Join(ps, "   ")
		if vis(s) > w {
			s = strings.Join(ps, " ")
		}
		return s
	}
	one := line(parts)
	if vis(one) <= w {
		if vis(one)+vis(equiv)+2 <= w {
			one = pad(one, w-vis(equiv)) + st.dim(equiv)
		}
		return []string{one}
	}
	// two rows: fill the first while it fits, the rest on the second
	cut := len(parts)
	for cut > 1 && vis(line(parts[:cut])) > w {
		cut--
	}
	return []string{line(parts[:cut]), line(parts[cut:])}
}

// elapsedText renders "since" as elapsed from now.
func elapsedText(since, now time.Time) string {
	return durText(int(now.Sub(since).Seconds()))
}
