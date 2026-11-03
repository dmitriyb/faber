package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
)

// span is one executed attempt's wall-clock interval.
type span struct {
	id     string
	start  time.Time
	finish time.Time
	status string
}

// spansOf lists every executed record's span, running steps extended to
// now, in start order.
func spansOf(v *RunView, now time.Time) []span {
	var out []span
	for key, rec := range v.Replay.Results {
		if key.InputHash == "" {
			continue
		}
		t := rec.Result.Timing
		if t.Started.IsZero() || !t.Finished.After(t.Started) {
			continue
		}
		out = append(out, span{id: key.StepID, start: t.Started, finish: t.Finished, status: statusWord(string(rec.Result.Status))})
	}
	for _, r := range v.Running {
		if v.Live {
			out = append(out, span{id: r.Step, start: r.Since, finish: now, status: "running"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// timelineGroups is the timeline's row order: one row per outermost
// iteration ("cycles@8") or top-level step, in first-start order.
func timelineGroups(v *RunView, now time.Time) []string {
	wf := v.Replay.Header.Workflow
	var order []string
	seen := map[string]bool{}
	for _, s := range spansOf(v, now) {
		g := groupKey(s.id, wf)
		if !seen[g] {
			seen[g] = true
			order = append(order, g)
		}
	}
	return order
}

// groupPrefix maps a timeline row back to the ids it covers: an iteration
// prefix ("<wf>/cycles@8/") or a step id.
func groupPrefix(v *RunView, group string) (prefix string, iter bool) {
	wf := v.Replay.Header.Workflow
	if _, _, ok := config.IterationOf(group); ok {
		return wf + "/" + group + "/", true
	}
	return wf + "/" + group, false
}

// timelinePane draws one row per outermost iteration or step: a bar of
// the executed attempts' step initials coloured by status on an axis
// scaled to the width and the zoom, the running step extended to now,
// the selected row as the selection bar; below, the selected row's
// attempts one row each. A letter already written in a column is never
// overwritten by a later step, so a short step keeps its mark at any
// scale.
func timelinePane(st style, m *model, w, h int) []string {
	v := m.run
	now := m.now()
	spans := spansOf(v, now)
	out := []string{st.chrome("TIMELINE") + st.dim("   +/- zoom"), st.rule(w)}
	if len(spans) == 0 {
		return append(out, "  "+st.dim("nothing executed yet"))
	}
	t0, t1 := spans[0].start, spans[0].finish
	for _, s := range spans {
		if s.start.Before(t0) {
			t0 = s.start
		}
		if s.finish.After(t1) {
			t1 = s.finish
		}
	}
	label := 20
	bw := w - label - 2
	if bw < 10 {
		bw = 10
	}
	zoom := 1 << m.tzoom
	total := bw * zoom // columns of the whole run at this zoom
	col := t1.Sub(t0).Seconds() / float64(total)
	if col <= 0 {
		col = 1
	}
	c := func(t time.Time) int {
		i := int(t.Sub(t0).Seconds() / col)
		if i >= total {
			i = total - 1
		}
		if i < 0 {
			i = 0
		}
		return i
	}
	groups := m.tgroups
	byGroup := map[string][]span{}
	wf := v.Replay.Header.Workflow
	for _, s := range spans {
		g := groupKey(s.id, wf)
		byGroup[g] = append(byGroup[g], s)
	}
	// the visible window: all of it at zoom 1, else centred on the
	// selected row's first span
	off := 0
	if zoom > 1 && m.tsel < len(groups) {
		if gs := byGroup[groups[m.tsel]]; len(gs) > 0 {
			off = c(gs[0].start) - bw/4
		}
		if off > total-bw {
			off = total - bw
		}
		if off < 0 {
			off = 0
		}
	}
	axis := make([]byte, bw)
	for i := range axis {
		axis[i] = ' '
	}
	for k := 0; k < bw; k += 16 {
		lab := t0.Add(time.Duration(float64(off+k)*col) * time.Second).Local().Format("15:04")
		for i := 0; i < len(lab) && k+i < bw; i++ {
			axis[k+i] = lab[i]
		}
	}
	out = append(out, strings.Repeat(" ", label)+st.dim(string(axis)))
	avail := h - len(out) - 4
	if avail < 3 {
		avail = 3
	}
	start := 0
	if m.tsel >= avail {
		start = m.tsel - avail + 1
	}
	for gi := start; gi < len(groups) && gi-start < avail; gi++ {
		g := groups[gi]
		cells := make([]string, bw)
		plain := make([]byte, bw)
		for i := range cells {
			cells[i] = " "
			plain[i] = ' '
		}
		for _, s := range byGroup[g] {
			ch := string(leaf(s.id)[0])
			if s.status == "FAIL" || s.status == "HALT" {
				ch = "X"
			}
			for i := c(s.start); i <= c(s.finish); i++ {
				x := i - off
				if x < 0 || x >= bw {
					continue
				}
				if plain[x] != ' ' {
					continue // first come, first drawn: a short step keeps its mark
				}
				plain[x] = ch[0]
				cells[x] = colorCell(st, ch, s.status)
			}
		}
		name := "  " + st.ident(trunc(g, label-3))
		line := pad(name, label) + strings.Join(cells, "")
		if gi == m.tsel {
			line = st.rev(pad("  > "+trunc(g, label-5), label) + string(plain))
		}
		out = append(out, line)
	}
	out = append(out, "", st.dim(fmt.Sprintf("  %ds per column   letters: step initial   X failed   zoom x%d", int(col), zoom)))
	// expanded: the selected row's attempts, one row each
	if m.tsel < len(groups) && len(out) < h-2 {
		g := groups[m.tsel]
		subs := byGroup[g]
		if len(subs) > 0 {
			prefix, _ := groupPrefix(v, g)
			title := crumb(strings.TrimSuffix(prefix, "/"), wf, v.Bounds)
			out = append(out, "", st.chrome(title)+st.dim("  by attempt"))
			s0, s1 := subs[0].start, subs[0].finish
			for _, s := range subs {
				if s.finish.After(s1) {
					s1 = s.finish
				}
			}
			sw := w - 30
			if sw < 10 {
				sw = 10
			}
			col2 := s1.Sub(s0).Seconds() / float64(sw)
			if col2 <= 0 {
				col2 = 1
			}
			for _, s := range subs {
				if len(out) >= h {
					break
				}
				row := make([]string, sw)
				for i := range row {
					row[i] = " "
				}
				a, b := int(s.start.Sub(s0).Seconds()/col2), int(s.finish.Sub(s0).Seconds()/col2)
				if b >= sw {
					b = sw - 1
				}
				for i := a; i <= b && i < sw; i++ {
					row[i] = colorCell(st, "#", s.status)
				}
				d := durText(int(s.finish.Sub(s.start).Seconds()))
				name := strings.TrimPrefix(s.id, prefix)
				out = append(out, pad("  "+st.ident(trunc(name, 18)), 22)+strings.Join(row, "")+" "+d)
			}
		}
	}
	return out
}

// colorCell colours one bar cell by status.
func colorCell(st style, ch, status string) string {
	switch status {
	case "ok":
		return st.wrap(sgrGreen, ch)
	case "FAIL", "HALT":
		return st.wrap(sgrRed, ch)
	case "running":
		return st.wrap(sgrCyan, ch)
	}
	return ch
}

// groupKey is the timeline row an id belongs to: its outermost iteration
// ("cycles@8") when inside a loop, else the step itself.
func groupKey(id, wf string) string {
	segs := strings.Split(id, "/")
	if len(segs) > 1 && segs[0] == wf {
		segs = segs[1:]
	}
	for _, seg := range segs {
		if _, _, ok := config.IterationOf(seg); ok {
			return seg
		}
	}
	return strings.Join(segs, "/")
}
