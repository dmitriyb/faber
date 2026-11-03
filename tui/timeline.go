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

// trow is one timeline row: an outermost iteration or top-level step,
// or — under an opened iteration — one executed attempt inside it.
type trow struct {
	group  string // "cycles@8" or "implement"
	id     string // the iteration prefix ("<wf>/cycles@8/") or the step id
	iter   bool   // a group row that is an iteration (openable)
	sub    bool   // an attempt row under an opened iteration
	status string // sub rows: the attempt's status
	spans  []span // what the bar draws
}

// timelineRows is the timeline's row order: one row per outermost
// iteration or top-level step in first-start order, and under an opened
// iteration its executed attempts in start order. Iterations open and
// close with the same map the tree folds by.
func timelineRows(v *RunView, open map[string]bool, now time.Time) []trow {
	wf := v.Replay.Header.Workflow
	var order []string
	byGroup := map[string][]span{}
	for _, s := range spansOf(v, now) {
		g := groupKey(s.id, wf)
		if _, seen := byGroup[g]; !seen {
			order = append(order, g)
		}
		byGroup[g] = append(byGroup[g], s)
	}
	var out []trow
	for _, g := range order {
		prefix, iter := groupPrefix(v, g)
		out = append(out, trow{group: g, id: prefix, iter: iter, spans: byGroup[g]})
		if iter && open[prefix] {
			for _, s := range byGroup[g] {
				out = append(out, trow{group: g, id: s.id, sub: true, status: s.status, spans: []span{s}})
			}
		}
	}
	return out
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

// timelinePane draws the rows: a bar of the executed attempts' step
// initials coloured by status on an axis scaled to the width and the
// zoom, the running step extended to now; under an opened iteration each
// attempt on its own row, a solid bar on the same axis, so the cursor
// walks into the iteration and STEP follows the attempt. A letter already
// written in a column is never overwritten by a later step, so a short
// step keeps its mark at any scale.
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
	label := 22
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
		lab := t0.Add(time.Duration(float64(off+k)*col) * time.Second).Local().Format("15:04")
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
			ch := "?"
			if r.sub {
				ch = "#"
			} else if l := leaf(s.id); l != "" {
				ch = l[:1]
			}
			if !r.sub && (s.status == "FAIL" || s.status == "HALT") {
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
		text, indent := r.group, "  "
		if r.sub {
			text, indent = strings.TrimPrefix(r.id, groupPrefixOf(v, r.group)), "    "
		}
		name := indent + st.ident(trunc(text, label-len(indent)-1))
		line := pad(name, label) + strings.Join(cells, "")
		if ri == m.tsel {
			line = st.rev(pad(indent+"> "+trunc(text, label-len(indent)-3), label) + string(plain))
		}
		out = append(out, line)
	}
	hint := "enter open/close iteration"
	if m.tsel < len(rows) && rows[m.tsel].sub {
		hint = "enter close iteration"
	}
	out = append(out, "", st.dim(fmt.Sprintf("  %ds per column   letters: step initial   X failed   zoom x%d   %s", int(col), zoom, hint)))
	return out
}

func groupPrefixOf(v *RunView, group string) string {
	p, _ := groupPrefix(v, group)
	return p
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
