package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dmitriyb/faber/pipeline"
)

// row is one line of the Run screen's STEPS pane: the reading order of the
// report, folded exactly as the text report folds it, plus the live states
// the journal's starts add.
type row struct {
	indent int
	status string // the human vocabulary word, or "running"
	name   string
	dur    string
	note   string
	id     string // node id (step rows), loop prefix (loop rows), iteration prefix (iteration rows)
	kind   string // "step" | "loop" | "iter" | "range" | "gen" | "item"
	since  time.Time
}

// statusWord maps a report state onto the human vocabulary.
func statusWord(state string) string {
	switch state {
	case pipeline.StateOK:
		return "ok"
	case pipeline.StateFailed:
		return "FAIL"
	case pipeline.StateHalted:
		return "HALT"
	case pipeline.StateSkippedCondition:
		return "not needed"
	case pipeline.StateSkippedDependency, pipeline.StateSkippedHalt, "not-reached":
		return "not reached"
	case pipeline.StateAbsent:
		return "absent"
	}
	return state
}

// treeRows builds the STEPS pane. Finished iterations fold to one line
// with their composition; an iteration that failed, halted, holds a running
// node, or is explicitly opened is expanded. Running nodes come from the
// journal's starts (absent lines with a start are running or dead).
func treeRows(v *RunView, open map[string]bool, now time.Time) []row {
	running := map[string]Running{}
	for _, r := range v.Running {
		running[r.Step] = r
	}
	var rows []row
	var walk func(items []pipeline.ReadItem, scope string, indent int)
	stepRow := func(l pipeline.StepLine, scope string, indent int) row {
		r := row{indent: indent, id: l.ID, kind: "step", name: strings.TrimPrefix(l.ID, scope), status: statusWord(l.Status), dur: fmtDur(l.Duration)}
		if run, ok := running[l.ID]; ok && v.Live {
			r.status, r.since = "running", run.Since
			r.dur = durText(int(now.Sub(run.Since).Seconds()))
			if run.Attempt > 1 {
				r.note = fmt.Sprintf("attempt %d", run.Attempt)
			}
		} else if _, ok := running[l.ID]; ok && l.Status == pipeline.StateAbsent {
			r.status, r.note = "absent", "started, no result (the run died mid-box?)"
		}
		if l.Cached {
			r.note = strings.TrimSpace(r.note + " (cached)")
		}
		if l.Error != nil && r.note == "" {
			r.note = l.Error.Reason
		}
		if l.Halt != nil && r.note == "" {
			r.note = l.Halt.Reason
		}
		if l.Ancestor != "" {
			anc := l.Ancestor
			if loop, ok := v.selectorLoop(anc); ok {
				anc = loop
			}
			r.note = "after " + strings.TrimPrefix(anc, scope)
		}
		if l.Chose != "" {
			r.note = "-> " + strings.TrimPrefix(l.Chose, scope)
		}
		return r
	}
	var loopRows func(l pipeline.LoopRollup, scope string, indent int)
	loopRows = func(l pipeline.LoopRollup, scope string, indent int) {
		name := strings.TrimPrefix(l.Loop, scope)
		if l.Ran == 0 && !l.Exhausted {
			r := row{indent: indent, id: l.Loop, kind: "loop", name: name, status: "not reached"}
			if l.Absent[0] > 0 {
				r.status = "absent"
			}
			if l.Ancestor != "" {
				r.note = "after " + strings.TrimPrefix(l.Ancestor, scope)
			}
			rows = append(rows, r)
			return
		}
		hdr := row{indent: indent, id: l.Loop, kind: "loop", name: name, status: statusWord(l.Status),
			note: fmt.Sprintf("%d of up to %d", l.Ran, l.Bound)}
		if l.Exhausted {
			hdr.note += "  exhausted"
		}
		if l.Chose != "" {
			hdr.note += "  -> " + relName(l.Chose, l.Loop)
		}
		rows = append(rows, hdr)
		for _, it := range l.Iterations {
			prefix := l.Loop + "@" + strconv.Itoa(it.Index) + "/"
			live := false
			for id := range running {
				if strings.HasPrefix(id, prefix) {
					live = true
				}
			}
			expand := open[prefix] || it.Status != pipeline.StateOK || live
			ir := row{indent: indent + 1, id: prefix, kind: "iter", name: "@" + strconv.Itoa(it.Index), status: statusWord(it.Status)}
			if live {
				ir.status = "running"
			}
			ir.dur = iterationWall(it)
			if !expand {
				ir.note = composition(it)
				rows = append(rows, ir)
				continue
			}
			rows = append(rows, ir)
			for _, s := range it.Steps {
				rows = append(rows, stepRow(s, prefix, indent+2))
			}
			for _, nl := range it.Loops {
				loopRows(nl, prefix, indent+2)
			}
		}
		if l.NotReached[0] > 0 {
			rows = append(rows, row{indent: indent + 1, id: l.Loop, kind: "range", name: iterRange(l.NotReached), status: "not reached",
				note: "after " + strings.TrimPrefix(l.Ancestor, scope)})
		}
		if l.Absent[0] > 0 {
			rows = append(rows, row{indent: indent + 1, id: l.Loop, kind: "range", name: iterRange(l.Absent), status: "absent"})
		}
	}
	walk = func(items []pipeline.ReadItem, scope string, indent int) {
		for _, it := range items {
			switch {
			case it.Line != nil:
				rows = append(rows, stepRow(*it.Line, scope, indent))
			case it.Loop != nil:
				loopRows(*it.Loop, scope, indent)
			}
		}
	}
	walk(v.Report.ReadingOrder(), v.Report.Scope(), 0)
	for _, g := range v.Report.Generate {
		rows = append(rows, row{indent: 0, id: g.Node, kind: "gen", name: strings.TrimPrefix(g.Node, v.Report.Scope()), status: "", note: g.Summary})
		for i := range g.Items {
			it := &g.Items[i]
			scope := g.Node + "[" + it.ID + "]/"
			rows = append(rows, row{indent: 1, id: scope, kind: "item", name: "[" + it.ID + "]", status: statusWord(it.Status), dur: fmtDur(it.Duration)})
			walk(it.ReadingOrder(), scope, 2)
		}
	}
	return rows
}

// composition is a finished iteration's one-line summary: step kinds in
// order with multiplicity, e.g. "implement · review ×2 · fix · merge".
func composition(it pipeline.LoopIteration) string {
	type kv struct {
		k string
		n int
	}
	var parts []kv
	add := func(k string) {
		for i := range parts {
			if parts[i].k == k {
				parts[i].n++
				return
			}
		}
		parts = append(parts, kv{k, 1})
	}
	for _, s := range it.Steps {
		if s.Status == pipeline.StateOK || s.Status == pipeline.StateFailed || s.Status == pipeline.StateHalted {
			add(s.ID[strings.LastIndex(s.ID, "/")+1:])
		}
	}
	for _, l := range it.Loops {
		for _, nit := range l.Iterations {
			for _, s := range nit.Steps {
				if s.Status == pipeline.StateOK || s.Status == pipeline.StateFailed || s.Status == pipeline.StateHalted {
					add(s.ID[strings.LastIndex(s.ID, "/")+1:])
				}
			}
		}
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.n > 1 {
			out = append(out, fmt.Sprintf("%s x%d", p.k, p.n))
		} else {
			out = append(out, p.k)
		}
	}
	return strings.Join(out, " . ")
}

// iterationWall sums the iteration's executed durations as a rough wall
// (durations are strings in the report; parse what parses).
func iterationWall(it pipeline.LoopIteration) string {
	var total time.Duration
	var add func(lines []pipeline.StepLine, loops []pipeline.LoopRollup)
	add = func(lines []pipeline.StepLine, loops []pipeline.LoopRollup) {
		for _, s := range lines {
			if d, err := time.ParseDuration(s.Duration); err == nil {
				total += d
			}
		}
		for _, l := range loops {
			for _, nit := range l.Iterations {
				add(nit.Steps, nit.Loops)
			}
		}
	}
	add(it.Steps, it.Loops)
	if total == 0 {
		return ""
	}
	return durText(int(total.Seconds()))
}

// fmtDur re-renders a report duration string in the view's fixed form.
func fmtDur(s string) string {
	if s == "" {
		return ""
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return durText(int(d.Seconds()))
}

func iterRange(r [2]int) string {
	if r[0] == r[1] {
		return "@" + strconv.Itoa(r[0])
	}
	return fmt.Sprintf("@%d..@%d", r[0], r[1])
}

// relName renders a candidate id relative to its loop ("review@2").
func relName(id, loop string) string {
	rest := strings.TrimPrefix(id, loop)
	if rest == id {
		return id
	}
	rest = strings.TrimPrefix(rest, "@")
	i, body, ok := strings.Cut(rest, "/")
	if !ok {
		return rest
	}
	return body + "@" + i
}

// selectorLoop maps a selector id to its loop when the report knows it.
func (v *RunView) selectorLoop(id string) (string, bool) {
	for _, l := range v.Report.Loops {
		if l.Chose != "" && strings.HasPrefix(l.Chose, l.Loop+"@") {
			// the selector sits beside the loop: "<parent>/<step>"
			step := l.Chose[strings.LastIndex(l.Chose, "/")+1:]
			parent := l.Loop[:strings.LastIndex(l.Loop, "/")+1]
			if id == parent+step {
				return l.Loop, true
			}
		}
	}
	return "", false
}
