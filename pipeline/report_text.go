package pipeline

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/failure"
)

// The human vocabulary. The journal and the JSON report keep the
// scheduler's encodings (skipped-condition, skipped-dependency,
// skipped-halt); only these renderings translate them, so an operator reads
// what happened rather than how the scheduler filed it.
const (
	wordOK         = "ok"
	wordFail       = "FAIL"
	wordHalt       = "HALT"
	wordNotNeeded  = "not needed"
	wordNotReached = "not reached"
	wordAbsent     = "absent"
)

// statusWord maps a terminal state to its human word.
func statusWord(status string) string {
	switch status {
	case StateOK:
		return wordOK
	case StateFailed:
		return wordFail
	case StateHalted:
		return wordHalt
	case StateSkippedCondition:
		return wordNotNeeded
	case StateSkippedDependency, StateSkippedHalt, "not-reached":
		return wordNotReached
	case StateAbsent:
		return wordAbsent
	}
	return status
}

// Text renders the human report in reading (dependency) order: plain step
// lines, loops folded to "ran of up to cap" with their executed iterations
// nested, generate rollups with the same folding per instance, failure and
// halt blocks, and a two-group footer (ran / not run).
func (r *RunReport) Text(w io.Writer) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
	if r.Run.Name != "" {
		// The name is operator-authored but journal-carried; sanitize like
		// any other journal-derived text on the Text path.
		p("run %s (%s)  workflow %s", r.Run.RunID, sanitizeTerm(r.Run.Name), r.Run.Workflow)
	} else {
		p("run %s  workflow %s", r.Run.RunID, r.Run.Workflow)
	}
	tr := textRenderer{p: p, sels: r.sels}
	tr.items(r.items, r.scope, "  ")
	for _, g := range r.Generate {
		p("  generate %s: %s", g.Node, g.Summary)
		for _, it := range g.Items {
			p("    [%s] %s%s", it.ID, statusWord(it.Status), textDuration(it.Duration))
			tr.items(it.items, g.Node+"["+it.ID+"]/", "      ")
		}
	}
	blocks := r.failureBlocks()
	if len(blocks) > 0 {
		p("failures:")
		for _, b := range blocks {
			for _, l := range b {
				p("  %s", l)
			}
		}
	}
	halts := r.haltBlocks()
	if len(halts) > 0 {
		p("halted:")
		for _, l := range halts {
			p("  %s", l)
		}
	}
	// The footer counts what a human counts: unused cap is not a step that
	// did not run, so it is neither not-needed nor not-reached here (the JSON
	// totals keep every node's state).
	t := r.humanTotals()
	ran := fmt.Sprintf("ran %d ok, %d failed", t.OK, t.Failed)
	if t.Halted > 0 {
		ran += fmt.Sprintf(", %d halted", t.Halted)
	}
	var notRun []string
	if t.SkippedCondition > 0 {
		notRun = append(notRun, fmt.Sprintf("%d not needed", t.SkippedCondition))
	}
	if n := t.SkippedDependency + t.SkippedHalt; n > 0 {
		notRun = append(notRun, fmt.Sprintf("%d not reached", n))
	}
	if t.Absent > 0 {
		notRun = append(notRun, fmt.Sprintf("%d absent", t.Absent))
	}
	footer := ran
	if len(notRun) > 0 {
		footer += "; not run " + strings.Join(notRun, ", ")
	}
	if r.Run.Wall != "" {
		footer += "; wall " + r.Run.Wall
	}
	p("%s", footer)
	if r.Run.Paused {
		p("paused: the run stopped on request; resume with: faber resume %s", r.Run.RunID)
	}
	if r.Run.Journal != "" {
		p("journal: %s", r.Run.Journal)
	}
	return nil
}

// humanTotals tallies the lines the text actually lists: plain lines and
// the steps of executed iterations, at every nesting level — never the
// unused cap, never the selectors, never a not-reached iteration's nodes
// (those are one range line, counted once each as not reached).
func (r *RunReport) humanTotals() Totals {
	var t Totals
	var items func([]textItem)
	var loops func([]LoopRollup)
	line := func(l StepLine) {
		switch l.Status {
		case StateOK:
			t.OK++
		case StateFailed:
			t.Failed++
		case StateHalted:
			t.Halted++
		case StateSkippedCondition:
			t.SkippedCondition++
		case StateSkippedDependency:
			t.SkippedDependency++
		case StateSkippedHalt:
			t.SkippedHalt++
		default:
			t.Absent++
		}
	}
	loops = func(ls []LoopRollup) {
		for _, l := range ls {
			if l.Exhausted {
				t.Failed++
			}
			for _, it := range l.Iterations {
				for _, s := range it.Steps {
					line(s)
				}
				loops(it.Loops)
			}
			// An idle loop is one listed line, so it counts once; a range
			// inside a loop that ran counts its iterations.
			idle := l.Ran == 0 && !l.Exhausted
			if l.NotReached[0] > 0 {
				if idle {
					t.SkippedDependency++
				} else {
					t.SkippedDependency += l.NotReached[1] - l.NotReached[0] + 1
				}
			}
			if l.Absent[0] > 0 {
				if idle {
					t.Absent++
				} else {
					t.Absent += l.Absent[1] - l.Absent[0] + 1
				}
			}
		}
	}
	items = func(is []textItem) {
		for _, it := range is {
			switch {
			case it.line != nil:
				line(*it.line)
			case it.loop != nil:
				loops([]LoopRollup{*it.loop})
			}
		}
	}
	items(r.items)
	for _, g := range r.Generate {
		for _, gi := range g.Items {
			items(gi.items)
		}
	}
	return t
}

type textRenderer struct {
	p    func(string, ...any)
	sels map[string]string
}

// items renders one scope's reading order at the given indent; names are
// relative to scope.
// step renders one line with a selector ancestor named as its loop.
func (tr textRenderer) step(line StepLine, scope string) string {
	if loop, ok := tr.sels[line.Ancestor]; ok {
		line.Ancestor = loop
	}
	return stepText(line, scope)
}

func (tr textRenderer) items(items []textItem, scope, indent string) {
	for _, it := range items {
		switch {
		case it.line != nil:
			tr.p("%s%s", indent, tr.step(*it.line, scope))
		case it.loop != nil:
			tr.loop(*it.loop, scope, indent)
		}
	}
}

func (tr textRenderer) loop(l LoopRollup, scope, indent string) {
	name := strings.TrimPrefix(l.Loop, scope)
	if idle := tr.idleLoop(l, scope); idle != "" {
		tr.p("%s%s", indent, idle)
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s%-12s %s  %d of up to %d", indent, statusWord(l.Status), name, l.Ran, l.Bound)
	if l.Exhausted {
		sb.WriteString("  exhausted")
	}
	if l.Chose != "" {
		sb.WriteString("  -> " + relName(l.Chose, l.Loop))
	}
	tr.p("%s", sb.String())
	for _, it := range l.Iterations {
		tag := "@" + strconv.Itoa(it.Index)
		inner := l.Loop + tag + "/"
		first := true
		sub := indent + "    "
		for _, line := range it.Steps {
			lead := strings.Repeat(" ", len(tag)+2)
			if first {
				lead = tag + "  "
				first = false
			}
			tr.p("%s%s%s", indent+"  ", lead, tr.step(line, inner))
		}
		for _, nl := range it.Loops {
			if first {
				tr.p("%s%s  %s", indent+"  ", tag, strings.TrimSpace(tr.loopLine(nl, inner)))
				first = false
			} else {
				tr.p("%s%s", sub, strings.TrimSpace(tr.loopLine(nl, inner)))
			}
			tr.iterations(nl, sub+"  ")
		}
	}
	if l.NotReached[0] > 0 {
		tr.p("%s  %s  %s  after %s", indent, iterRange(l.NotReached), wordNotReached, tr.ancestor(l.Ancestor, scope))
	}
	if l.Absent[0] > 0 {
		tr.p("%s  %s  %s", indent, iterRange(l.Absent), wordAbsent)
	}
}

// idleLoop renders a loop nothing ran in as one line — not reached after
// its ancestor, or absent — and "" for a loop with executed iterations.
func (tr textRenderer) idleLoop(l LoopRollup, scope string) string {
	if l.Ran > 0 || l.Exhausted {
		return ""
	}
	name := strings.TrimPrefix(l.Loop, scope)
	switch {
	case l.NotReached[0] > 0:
		return fmt.Sprintf("%-12s %s  after %s", wordNotReached, name, tr.ancestor(l.Ancestor, scope))
	case l.Absent[0] > 0:
		return fmt.Sprintf("%-12s %s", wordAbsent, name)
	}
	return ""
}

// loopLine is the loop header line alone (for nested loops, whose
// iterations the caller renders).
func (tr textRenderer) loopLine(l LoopRollup, scope string) string {
	if idle := tr.idleLoop(l, scope); idle != "" {
		return idle
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-12s %s  %d of up to %d", statusWord(l.Status), strings.TrimPrefix(l.Loop, scope), l.Ran, l.Bound)
	if l.Exhausted {
		sb.WriteString("  exhausted")
	}
	if l.Chose != "" {
		sb.WriteString("  -> " + relName(l.Chose, l.Loop))
	}
	return sb.String()
}

// iterations renders a nested loop's executed iterations and ranges.
func (tr textRenderer) iterations(l LoopRollup, indent string) {
	if l.Ran == 0 && !l.Exhausted {
		return // rendered as one idle line by loopLine
	}
	for _, it := range l.Iterations {
		tag := "@" + strconv.Itoa(it.Index)
		inner := l.Loop + tag + "/"
		first := true
		for _, line := range it.Steps {
			lead := strings.Repeat(" ", len(tag)+2)
			if first {
				lead = tag + "  "
				first = false
			}
			tr.p("%s%s%s", indent, lead, tr.step(line, inner))
		}
		for _, nl := range it.Loops {
			lead := strings.Repeat(" ", len(tag)+2)
			if first {
				lead = tag + "  "
				first = false
			}
			tr.p("%s%s%s", indent, lead, tr.loopLine(nl, inner))
			tr.iterations(nl, indent+strings.Repeat(" ", len(tag)+4))
		}
	}
	if l.NotReached[0] > 0 {
		tr.p("%s%s  %s  after %s", indent, iterRange(l.NotReached), wordNotReached, tr.ancestor(l.Ancestor, ""))
	}
	if l.Absent[0] > 0 {
		tr.p("%s%s  %s", indent, iterRange(l.Absent), wordAbsent)
	}
}

func (tr textRenderer) ancestor(id, scope string) string {
	if loop, ok := tr.sels[id]; ok {
		id = loop
	}
	return strings.TrimPrefix(id, scope)
}

func iterRange(r [2]int) string {
	if r[0] == r[1] {
		return "@" + strconv.Itoa(r[0])
	}
	return fmt.Sprintf("@%d..@%d", r[0], r[1])
}

// relName renders a candidate id relative to its loop: "review@2" for
// task/review-cycle@2/review under task/review-cycle.
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

// stepText renders one step line with its name relative to scope.
func stepText(line StepLine, scope string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-12s %s", statusWord(line.Status), strings.TrimPrefix(line.ID, scope))
	if line.Cached {
		sb.WriteString("  (cached)")
	}
	if line.AgentSkipped {
		sb.WriteString("  (agent-skipped)")
	}
	if line.Duration != "" {
		fmt.Fprintf(&sb, "  %s", line.Duration)
	}
	if line.Attempts > 1 {
		fmt.Fprintf(&sb, "  attempts=%d", line.Attempts)
	}
	if line.Deferred > 0 {
		fmt.Fprintf(&sb, "  deferred=%d wait=%s", line.Deferred, line.DeferredFor)
	}
	if line.Chose != "" {
		fmt.Fprintf(&sb, "  -> %s", strings.TrimPrefix(line.Chose, scope))
	}
	if line.Ancestor != "" {
		// The ancestor is a scheduler-authored node id in both skip flavors.
		if line.Status == StateSkippedHalt {
			fmt.Fprintf(&sb, "  after halt of %s", strings.TrimPrefix(line.Ancestor, scope))
		} else {
			fmt.Fprintf(&sb, "  after %s", strings.TrimPrefix(line.Ancestor, scope))
		}
	}
	if line.Halt != nil {
		// The halt reason is box-authored text; sanitize like any other.
		fmt.Fprintf(&sb, "  halt=%s", sanitizeTerm(line.Halt.Reason))
	}
	for _, k := range sortedKeys(line.Outputs) {
		fmt.Fprintf(&sb, "  %s=%v", sanitizeTerm(k), sanitizeTerm(fmt.Sprintf("%v", line.Outputs[k])))
	}
	return sb.String()
}

func textDuration(d string) string {
	if d == "" {
		return ""
	}
	return "  " + d
}

// instanceChose names the candidate an instance selector adopted: the
// newest iteration's ok record of the same body step whose payload equals
// the selector's — the scheduler's own adoption rule, recomputed.
func instanceChose(selID string, recs map[string]failure.ResultRecord, scope string) string {
	rel := strings.TrimPrefix(selID, scope)
	step := rel[strings.LastIndex(rel, "/")+1:]
	parent := scope + strings.TrimSuffix(rel, step)
	sel := recs[selID]
	var candidates []string
	for id, rec := range recs {
		if id == selID || !strings.HasPrefix(id, parent) || !strings.HasSuffix(id, "/"+step) {
			continue
		}
		if _, _, ok := loopOf(id, parent); !ok || rec.Result.Status != failure.StatusOK {
			continue
		}
		if string(rec.Result.Payload) == string(sel.Result.Payload) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(a, b int) bool {
		_, ia, _ := loopOf(candidates[a], parent)
		_, ib, _ := loopOf(candidates[b], parent)
		return ia > ib
	})
	return candidates[0]
}
