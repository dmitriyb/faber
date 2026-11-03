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

// trow is one timeline row at the current level: a leaf step, or a group
// (an iteration, a generate instance, a sub-workflow) the cursor can enter.
type trow struct {
	name   string // the segment under the level's prefix: "cycles@8", "review"
	id     string // a group's prefix ("<wf>/cycles@8/") or the step id
	deeper bool   // a group: something executed under it
	iter   bool   // the group is a loop iteration
	status string // a leaf's status
	spans  []span // what the bar draws
}

// timelineRows is one level of the timeline: every executed attempt under
// prefix grouped by its next id segment, in first-start order. A group is
// a row the cursor enters; a step is a leaf.
func timelineRows(v *RunView, prefix string, now time.Time) []trow {
	var order []string
	byName := map[string]*trow{}
	for _, s := range spansOf(v, now) {
		if !strings.HasPrefix(s.id, prefix) {
			continue
		}
		seg, _, deeper := strings.Cut(strings.TrimPrefix(s.id, prefix), "/")
		r := byName[seg]
		if r == nil {
			r = &trow{name: seg, id: prefix + seg}
			if deeper {
				r.id += "/"
				_, _, r.iter = config.IterationOf(seg)
			}
			byName[seg] = r
			order = append(order, seg)
		}
		r.deeper = r.deeper || deeper
		if !deeper {
			r.status = s.status
		}
		r.spans = append(r.spans, s)
	}
	out := make([]trow, 0, len(order))
	for _, seg := range order {
		out = append(out, *byName[seg])
	}
	return out
}

// levelSpan is the wall interval every attempt under prefix covers.
func levelSpan(v *RunView, prefix string, now time.Time) (t0, t1 time.Time, ok bool) {
	for _, s := range spansOf(v, now) {
		if !strings.HasPrefix(s.id, prefix) {
			continue
		}
		if !ok || s.start.Before(t0) {
			t0 = s.start
		}
		if !ok || s.finish.After(t1) {
			t1 = s.finish
		}
		ok = true
	}
	return t0, t1, ok
}

// timelinePane draws the current level: the breadcrumb of the path drilled
// into, the parent's bar with this level's window marked, an axis scaled to
// this level's span and the zoom, then one row per group or step — a
// group's bar as the initials of the steps under it coloured by status, a
// step's as a solid bar — the running step extended to now. A letter
// already written in a column is never overwritten by a later step, so a
// short step keeps its mark at any scale.
func timelinePane(st style, m *model, w, h int) []string {
	v := m.run
	now := m.now()
	wf := v.Replay.Header.Workflow
	prefix := m.tprefix()
	path := st.dim("whole run")
	if len(m.tpath) > 0 {
		path = st.ident(crumb(prefix, wf, v.Bounds)) + st.dim("   esc up")
	}
	out := []string{st.chrome("TIMELINE") + "   " + path + st.dim("   +/- zoom"), st.rule(w)}
	t0, t1, ok := levelSpan(v, prefix, now)
	if !ok {
		return append(out, "  "+st.dim("nothing executed yet"))
	}
	label := 22
	bw := w - label - 2
	if bw < 10 {
		bw = 10
	}
	// the parent's bar, this level's window marked, so the place in the
	// whole run stays visible
	if len(m.tpath) > 0 {
		parent := wf + "/"
		if len(m.tpath) > 1 {
			parent = m.tpath[len(m.tpath)-2]
		}
		p0, p1, _ := levelSpan(v, parent, now)
		bar := make([]byte, bw)
		for i := range bar {
			bar[i] = '-'
		}
		if d := p1.Sub(p0).Seconds(); d > 0 {
			a, b := int(t0.Sub(p0).Seconds()/d*float64(bw)), int(t1.Sub(p0).Seconds()/d*float64(bw))
			for i := a; i <= b && i < bw; i++ {
				if i >= 0 {
					bar[i] = '='
				}
			}
		}
		out = append(out, pad("  "+st.dim("in "+trunc(leaf(strings.TrimSuffix(parent, "/")), label-6)), label)+st.dim(string(bar)))
	}
	zoom := 1 << m.tzoom
	total := bw * zoom // columns of this level at this zoom
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
	rows := m.trows
	// the visible window: all of it at zoom 1, else centred on the
	// selected row's first span
	off := 0
	if zoom > 1 && m.tsel < len(rows) && len(rows[m.tsel].spans) > 0 {
		off = c(rows[m.tsel].spans[0].start) - bw/4
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
		lab := t0.Add(time.Duration(float64(off+k)*col) * time.Second).Local().Format("15:04:05")
		if col >= 60 {
			lab = lab[:5]
		}
		for i := 0; i < len(lab) && k+i < bw; i++ {
			axis[k+i] = lab[i]
		}
	}
	out = append(out, strings.Repeat(" ", label)+st.dim(string(axis)))
	avail := h - len(out) - 2
	if avail < 3 {
		avail = 3
	}
	start := 0
	if m.tsel >= avail {
		start = m.tsel - avail + 1
	}
	for ri := start; ri < len(rows) && ri-start < avail; ri++ {
		r := rows[ri]
		cells := make([]string, bw)
		plain := make([]byte, bw)
		for i := range cells {
			cells[i] = " "
			plain[i] = ' '
		}
		for _, s := range r.spans {
			ch := "#"
			if r.deeper {
				ch = "?"
				if l := leaf(s.id); l != "" {
					ch = l[:1]
				}
				if s.status == "FAIL" || s.status == "HALT" {
					ch = "X"
				}
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
		mark := " "
		if r.deeper {
			mark = ">" // enterable
		}
		name := "  " + st.ident(trunc(r.name, label-5)) + " " + st.dim(mark)
		line := pad(name, label) + strings.Join(cells, "")
		if ri == m.tsel {
			line = st.rev(pad("  > "+trunc(r.name, label-7)+" "+mark, label) + string(plain))
		}
		out = append(out, line)
	}
	unit := fmt.Sprintf("%ds", int(col))
	if col >= 60 {
		unit = durText(int(col))
	}
	hint := ""
	if m.tsel < len(rows) && rows[m.tsel].deeper {
		hint = "   enter drill in"
	}
	out = append(out, "", st.dim(fmt.Sprintf("  %s per column   letters: step initials, X failed, # one step   zoom x%d%s", unit, zoom, hint)))
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
