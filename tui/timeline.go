package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
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

// timelinePane draws one row per top-level loop iteration (letters by step
// initial) or per step when there is no loop, then the selected
// iteration's attempts one row each. Columns scale to the width.
func timelinePane(st style, m *model, w, h int) []string {
	v := m.run
	now := m.now()
	spans := spansOf(v, now)
	out := []string{st.chrome("TIMELINE"), st.rule(w)}
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
	bw := w - label - 10
	if bw < 10 {
		bw = 10
	}
	col := t1.Sub(t0).Seconds() / float64(bw)
	if col <= 0 {
		col = 1
	}
	c := func(t time.Time) int {
		i := int(t.Sub(t0).Seconds() / col)
		if i >= bw {
			i = bw - 1
		}
		if i < 0 {
			i = 0
		}
		return i
	}
	axis := make([]byte, bw)
	for i := range axis {
		axis[i] = ' '
	}
	for k := 0; k < bw; k += 16 {
		lab := t0.Add(time.Duration(float64(k)*col) * time.Second).Local().Format("15:04")
		for i := 0; i < len(lab) && k+i < bw; i++ {
			axis[k+i] = lab[i]
		}
	}
	out = append(out, strings.Repeat(" ", label)+st.dim(string(axis)))
	// rows: group by the outermost iteration prefix under the workflow root
	wf := v.Replay.Header.Workflow
	groups := map[string][]span{}
	var order []string
	for _, s := range spans {
		g := groupKey(s.id, wf)
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], s)
	}
	for _, g := range order {
		bar := make([]string, bw)
		for i := range bar {
			bar[i] = " "
		}
		for _, s := range groups[g] {
			ch := string(leaf(s.id)[0])
			if s.status == "FAIL" || s.status == "HALT" {
				ch = "X"
			}
			for i := c(s.start); i <= c(s.finish); i++ {
				bar[i] = colorCell(st, ch, s.status)
			}
		}
		out = append(out, pad("  "+st.ident(trunc(g, label-3)), label)+strings.Join(bar, ""))
	}
	out = append(out, "", st.dim(fmt.Sprintf("  %ds per column   letters: step initial   X failed", int(col))))
	// expanded: the selected row's iteration, one row per attempt
	if r := m.selectedRow(); r != nil && len(out) < h-2 {
		prefix := r.id
		if r.kind == "step" {
			prefix = iterPrefixOf(r.id)
		}
		var subs []span
		for _, s := range spans {
			if prefix != "" && strings.HasPrefix(s.id, prefix) {
				subs = append(subs, s)
			}
		}
		if len(subs) > 0 {
			out = append(out, "", st.chrome(crumb(strings.TrimSuffix(prefix, "/"), wf, v.Bounds))+st.dim("  by attempt"))
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
				out = append(out, pad("  "+st.ident(trunc(strings.TrimPrefix(s.id, prefix), 18)), 22)+strings.Join(row, "")+" "+d)
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

// iterPrefixOf is the innermost iteration prefix of an id ("…/loop@i/").
func iterPrefixOf(id string) string {
	segs := strings.Split(id, "/")
	for k := len(segs) - 1; k >= 0; k-- {
		if _, _, ok := config.IterationOf(segs[k]); ok {
			return strings.Join(segs[:k+1], "/") + "/"
		}
	}
	return ""
}

// statusOfResult maps a record status for the timeline.
func statusOfResult(s failure.Status) string { return statusWord(string(s)) }
