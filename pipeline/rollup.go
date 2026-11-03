package pipeline

import (
	"sort"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// LoopRollup is one unrolled loop folded back into what a human means by a
// loop: how many iterations ran of how many the cap allowed, what each
// executed iteration did, and what happened to the rest. It is derived from
// journal plus IR alone — the executor never sees loops — so a crashed, a
// killed, and a settled run roll up through the same code.
type LoopRollup struct {
	Loop   string `json:"loop"`            // the loop's node-id prefix, e.g. task/review-cycle
	Bound  int    `json:"bound"`           // iterations the cap allowed (highest known)
	Ran    int    `json:"ran"`             // iterations with at least one executed node
	Status string `json:"status"`          // ok|failed|halted|not-reached|absent
	Chose  string `json:"chose,omitempty"` // the selector's adopted candidate, when a selector exists
	// Exhausted marks the loop-exhaustion failure: every iteration ran and
	// the until condition never held (the selector settled loop-exhausted).
	Exhausted  bool            `json:"exhausted,omitempty"`
	Iterations []LoopIteration `json:"iterations"`
	// NotReached is the range of iterations with no executed node that were
	// skipped past a failure or halt (zero when none); Ancestor names the
	// root cause. Absent is the range with no journal record at all.
	NotReached [2]int `json:"not_reached,omitempty"`
	Ancestor   string `json:"ancestor,omitempty"`
	Absent     [2]int `json:"absent,omitempty"`
}

// LoopIteration is one iteration that executed at least one node.
type LoopIteration struct {
	Index  int          `json:"index"`
	Status string       `json:"status"` // ok|failed|halted
	Steps  []StepLine   `json:"steps"`  // the iteration's own lines, nested loops excluded
	Loops  []LoopRollup `json:"loops,omitempty"`
}

// parseIter recognizes the desugarer's iteration segment "<loop>@<i>"
// (config owns the grammar).
func parseIter(seg string) (loop string, i int, ok bool) { return config.IterationOf(seg) }

// loopOf locates the outermost iteration segment of id after scope: the
// loop's prefix id, the iteration index, and whether one exists.
func loopOf(id, scope string) (loop string, iter int, ok bool) {
	if !strings.HasPrefix(id, scope) {
		return "", 0, false
	}
	rel := id[len(scope):]
	segs := strings.Split(rel, "/")
	for k, seg := range segs {
		if name, i, ok := parseIter(seg); ok {
			return scope + strings.Join(append(append([]string{}, segs[:k]...), name), "/"), i, true
		}
	}
	return "", 0, false
}

// loopBounds reads every loop's cap off the IR: the highest iteration index
// present under each loop prefix (the desugarer unrolls every iteration, so
// the highest present is the cap). Sub-workflow graphs are walked too.
func loopBounds(ir *config.IR) map[string]int { return config.LoopBounds(ir) }

// noteBounds folds one node id's iteration segments into the bounds map.
func noteBounds(out map[string]int, id string) { config.NoteLoopBounds(out, id) }

// selectorLoops maps each selector node id to the loop it coalesces, read
// off its candidates' iteration prefix.
func selectorLoops(irNodes map[string]irNodeInfo) map[string]string {
	out := map[string]string{}
	for id, info := range irNodes {
		if info.kind != config.KindSelector || info.sel == nil || len(info.sel.Candidates) == 0 {
			continue
		}
		c := info.sel.Candidates[0]
		// The loop prefix is everything before the candidate's deepest "@i"
		// segment that precedes the body step — scope-agnostic: scan from the
		// right so nested loops resolve to the innermost one the selector sits
		// beside.
		segs := strings.Split(c, "/")
		for k := len(segs) - 1; k >= 0; k-- {
			if name, _, ok := parseIter(segs[k]); ok {
				out[id] = strings.Join(append(append([]string{}, segs[:k]...), name), "/")
				break
			}
		}
	}
	return out
}

// executed reports whether a line's state means a box (or cheap settlement)
// actually produced a result — the opposite of every skip and of absent.
func executed(status string) bool {
	return status == StateOK || status == StateFailed || status == StateHalted
}

// rollupLoops folds lines under one scope into plain lines (not inside any
// loop) and loop rollups, preserving the input order for first appearance.
// sels maps selector ids to their loop; selector lines are consumed into
// their loop (chose / exhausted) and never returned as plain lines.
// bounds supplies caps known from the IR; the lines themselves extend it.
func rollupLoops(lines []StepLine, scope string, bounds map[string]int, sels map[string]string) (plain []StepLine, loops []LoopRollup) {
	type group struct {
		iters map[int][]StepLine
		sel   *StepLine
	}
	groups := map[string]*group{}
	var order []string
	get := func(loop string) *group {
		g := groups[loop]
		if g == nil {
			g = &group{iters: map[int][]StepLine{}}
			groups[loop] = g
			order = append(order, loop)
		}
		return g
	}
	for _, line := range lines {
		if loop, ok := sels[line.ID]; ok {
			// A selector belongs to the scope its loop lives in: a nested
			// loop's selector (epic/cycles@1/review for the loop
			// epic/cycles@1/review-cycle) rides into cycle 1 first and is
			// consumed one level down, never as a top-level loop of its own.
			if outer, i, nested := loopOf(line.ID, scope); nested && outer != loop {
				g := get(outer)
				g.iters[i] = append(g.iters[i], line)
				continue
			}
			l := line
			get(loop).sel = &l
			continue
		}
		loop, i, ok := loopOf(line.ID, scope)
		if !ok {
			plain = append(plain, line)
			continue
		}
		g := get(loop)
		g.iters[i] = append(g.iters[i], line)
	}
	for _, loop := range order {
		g := groups[loop]
		r := LoopRollup{Loop: loop, Bound: bounds[loop]}
		idxs := make([]int, 0, len(g.iters))
		for i := range g.iters {
			idxs = append(idxs, i)
			if i > r.Bound {
				r.Bound = i
			}
		}
		sort.Ints(idxs)
		anyFailed, anyHalted := false, false
		for _, i := range idxs {
			inner := loop + "@" + strconv.Itoa(i) + "/"
			ip, il := rollupLoops(g.iters[i], inner, bounds, sels)
			it := LoopIteration{Index: i, Steps: ip, Loops: il}
			ran := false
			for _, l := range ip {
				if executed(l.Status) {
					ran = true
				}
			}
			for _, l := range il {
				if l.Ran > 0 {
					ran = true
				}
			}
			if ran {
				it.Status = iterationStatus(ip, il)
				switch it.Status {
				case StateFailed:
					anyFailed = true
				case StateHalted:
					anyHalted = true
				}
				r.Ran++
				r.Iterations = append(r.Iterations, it)
				continue
			}
			// Nothing executed: unused cap, not reached, or absent.
			kind, ancestor := idleIterationKind(ip, il)
			switch kind {
			case StateSkippedDependency:
				noteRange(&r.NotReached, i)
				if r.Ancestor == "" {
					r.Ancestor = ancestor
				}
			case StateAbsent:
				noteRange(&r.Absent, i)
			}
		}
		if g.sel != nil {
			if g.sel.Status == StateFailed && g.sel.Error != nil && g.sel.Error.Reason == failure.ReasonLoopExhausted {
				r.Exhausted = true
			}
			r.Chose = g.sel.Chose
		}
		switch {
		case r.Exhausted || anyFailed:
			r.Status = StateFailed
		case anyHalted:
			r.Status = StateHalted
		case r.Ran == 0 && r.NotReached[0] > 0:
			r.Status = "not-reached"
		case r.Ran == 0 && r.Absent[0] > 0:
			r.Status = StateAbsent
		default:
			r.Status = StateOK
		}
		loops = append(loops, r)
	}
	return plain, loops
}

// iterationStatus aggregates an executed iteration: failure outranks halt
// outranks ok, exactly as the run exit does.
func iterationStatus(lines []StepLine, loops []LoopRollup) string {
	status := StateOK
	for _, l := range lines {
		if l.Status == StateFailed {
			return StateFailed
		}
		if l.Status == StateHalted {
			status = StateHalted
		}
	}
	for _, l := range loops {
		if l.Status == StateFailed {
			return StateFailed
		}
		if l.Status == StateHalted {
			status = StateHalted
		}
	}
	return status
}

// idleIterationKind classifies an iteration nothing executed in: a
// dependency or halt skip anywhere makes it not reached (with the first
// ancestor seen), an absent line makes it absent, otherwise it is unused
// cap (every node condition-skipped).
func idleIterationKind(lines []StepLine, loops []LoopRollup) (kind, ancestor string) {
	kind = StateSkippedCondition
	for _, l := range lines {
		switch l.Status {
		case StateSkippedDependency, StateSkippedHalt:
			return StateSkippedDependency, l.Ancestor
		case StateAbsent:
			kind = StateAbsent
		}
	}
	for _, l := range loops {
		if l.NotReached[0] > 0 {
			return StateSkippedDependency, l.Ancestor
		}
		if l.Absent[0] > 0 {
			kind = StateAbsent
		}
	}
	return kind, ""
}

func noteRange(r *[2]int, i int) {
	if r[0] == 0 || i < r[0] {
		r[0] = i
	}
	if i > r[1] {
		r[1] = i
	}
}

// topoOrder returns the IR's node ids in dependency order with id
// tie-breaks — the order a human reads a workflow in, where the canonical
// (sorted) order would put "merge" before "review-cycle@1/review".
// Sub-workflow graphs are flattened; ids outside the IR keep their input
// position at the end, sorted.
func topoOrder(ir *config.IR, extra []string) []string {
	indeg := map[string]int{}
	succ := map[string][]string{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		if g == nil {
			return
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if _, ok := indeg[n.ID]; !ok {
				indeg[n.ID] = 0
			}
			// A condition's reads are readiness dependencies too (the
			// scheduler evaluates when: only once its deps settled), so they
			// order the reading as they order the run.
			if n.When != nil {
				for _, d := range n.When.Deps {
					if _, ok := indeg[d]; !ok {
						indeg[d] = 0
					}
					indeg[n.ID]++
					succ[d] = append(succ[d], n.ID)
				}
			}
			if n.Sub != nil {
				walk(n.Sub)
			}
		}
		for _, e := range g.Edges {
			if _, ok := indeg[e.To]; !ok {
				indeg[e.To] = 0
			}
			if _, ok := indeg[e.From]; !ok {
				indeg[e.From] = 0
			}
			indeg[e.To]++
			succ[e.From] = append(succ[e.From], e.To)
		}
	}
	walk(ir)
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		for _, s := range succ[id] {
			indeg[s]--
			if indeg[s] == 0 {
				ready = append(ready, s)
				sort.Strings(ready)
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range out {
		seen[id] = true
	}
	var rest []string
	for _, id := range extra {
		if !seen[id] {
			rest = append(rest, id)
			seen[id] = true
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// instanceOrder maps a generate target IR's dependency order onto one
// instance's ids: the target's workflow prefix becomes the instance scope.
// Ids the target does not know keep their place at the end, sorted.
func instanceOrder(tgt *config.IR, scope string, ids []string) []string {
	prefix := tgt.Workflow + "/"
	var out []string
	for _, id := range topoOrder(tgt, nil) {
		out = append(out, scope+strings.TrimPrefix(id, prefix))
	}
	return topoOrderExtras(out, ids)
}

// topoOrderExtras appends, sorted, every id in extra not already in order.
func topoOrderExtras(order, extra []string) []string {
	seen := map[string]bool{}
	for _, id := range order {
		seen[id] = true
	}
	var rest []string
	for _, id := range extra {
		if !seen[id] {
			rest = append(rest, id)
			seen[id] = true
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// instanceBounds rebases a target IR's loop caps under the instance scope,
// on top of the entry IR's.
func instanceBounds(tgt *config.IR, scope string, base map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range base {
		out[k] = v
	}
	prefix := tgt.Workflow + "/"
	for loop, n := range loopBounds(tgt) {
		out[scope+strings.TrimPrefix(loop, prefix)] = n
	}
	return out
}
