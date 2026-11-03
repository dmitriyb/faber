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
	f.put(0, pad(st.chrome("faber")+"   "+m.project.Path, w-8)+now.Format("15:04:05"))
	f.put(1, st.rule(w))
	leftW := w * 3 / 10
	if leftW < 36 {
		leftW = 36
	}
	if leftW > 66 {
		leftW = 66
	}
	rightW := w - leftW - 3
	left := projectPane(st, m, leftW)
	right := runsPane(st, m, rightW)
	bodyH := h - 2 - 1 - 5 - 1 // header+rule, rule, selected block, footer
	if bodyH < 4 {
		bodyH = 4
	}
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
	f.put(h-1, footer(st, w, m.homeKeys(), "= faber runs"))
}

func projectPane(st style, m *model, w int) []string {
	p := m.project
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
	out = append(out, "", cols([]string{"  config", st.status("ok") + "  valid"}, widths), "")
	tw := []int{20, w - 20}
	out = append(out, st.dim(cols([]string{"  image", "state"}, tw)))
	for _, t := range p.Templates {
		state := st.dim("not checked")
		switch {
		case t.Err != "":
			state = st.status("FAIL") + "  " + trunc(t.Err, w-30)
		case t.Checked && t.Built:
			state = st.status("ok") + "  built"
		case t.Checked:
			state = st.wrap(sgrRed, "MISSING") + "  faber build  " + st.dim(tagHash(t.Tag))
		}
		out = append(out, cols([]string{"  " + st.ident(t.Name), state}, tw))
	}
	return out
}

func runsPane(st style, m *model, w int) []string {
	title := st.chrome("RUNS") + "   this config"
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
	for i, r := range m.runs {
		if r.ConfigPath != "" && m.project.cfg != nil && !sameConfig(r.ConfigPath, m.project.Path) {
			continue
		}
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
		out = append(out, line)
	}
	if len(m.runs) == 0 {
		out = append(out, "  "+st.dim("no journaled runs"))
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

// footer renders the key legend and the CLI equivalent on the right.
func footer(st style, w int, keys [][2]string, equiv string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, st.chrome(k[0])+" "+k[1])
	}
	left := strings.Join(parts, "   ")
	if vis(left) > w {
		left = strings.Join(parts, " ") // narrow: every key still named, tighter
	}
	if vis(left)+vis(equiv)+2 > w {
		return left
	}
	return pad(left, w-vis(equiv)) + st.dim(equiv)
}

// elapsedText renders "since" as elapsed from now.
func elapsedText(since, now time.Time) string {
	return durText(int(now.Sub(since).Seconds()))
}
