package tui

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/pipeline"
)

// frow is one line of the flow view: a step, a loop's opening or closing
// frame edge, a generate node, or one of its instances. depth is the
// frame nesting.
type frow struct {
	kind   string // "step" | "open" | "close" | "gen" | "items"
	depth  int
	id     string // node id (step), loop prefix (open/close), generate id
	name   string
	guard  string // "if …" (steps), "until …" (loops)
	after  string // "after x" ordering dependency
	cap    string // loops: "x5" / "2 of up to 5"; generate: "x4 at once"
	status string
	dur    string
	since  time.Time
	cells  []string // items: "[id status]" cells
}

// loopInfo is what the IR says about one loop: its cap and its exit.
type loopInfo struct {
	cap   int
	until string
}

// flowRows builds the flow: the workflow's structure from the IR (every
// loop body once, from iteration 1), with each loop's current iteration
// carrying the journal's state on a run. Nothing here is a layout
// algorithm: the IR's order is the reading order.
func flowRows(v *RunView, preview bool, now time.Time) []frow {
	if v.IR == nil {
		return nil
	}
	ir := v.IR
	wf := ir.Workflow
	loops := map[string]loopInfo{}
	for loop, cap := range config.LoopBounds(ir) {
		loops[loop] = loopInfo{cap: cap}
	}
	// until: the selector's exhaustion condition at the final iteration,
	// rewritten to leaf names
	var walkSel func(*config.IR)
	walkSel = func(g *config.IR) {
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Sub != nil {
				walkSel(n.Sub)
			}
			if n.Kind != config.KindSelector || n.Sel == nil || len(n.Sel.Candidates) == 0 || n.Sel.Exhausted == nil {
				continue
			}
			segs := strings.Split(n.Sel.Candidates[0], "/")
			for k := len(segs) - 1; k >= 0; k-- {
				if name, _, ok := config.IterationOf(segs[k]); ok {
					loop := strings.Join(append(append([]string{}, segs[:k]...), name), "/")
					li := loops[loop]
					if li.until == "" {
						u := strings.TrimSpace(n.Sel.Exhausted.CEL)
						// the exhaustion rule is !(until) at the final iteration
						if strings.HasPrefix(u, "!(") && strings.HasSuffix(u, ")") {
							u = u[2 : len(u)-1]
						}
						li.until = leafCEL(u)
					}
					loops[loop] = li
					break
				}
			}
		}
	}
	walkSel(ir)
	// ordering edges within one scope: depends_on
	after := map[string]string{}
	var walkEdges func(*config.IR)
	walkEdges = func(g *config.IR) {
		for _, e := range g.Edges {
			if !e.IsOrdering() {
				continue
			}
			if scopeOf(e.From) == scopeOf(e.To) {
				after[e.To] = leaf(e.From)
			}
		}
		for i := range g.Nodes {
			if g.Nodes[i].Sub != nil {
				walkEdges(g.Nodes[i].Sub)
			}
		}
	}
	walkEdges(ir)
	nodes := map[string]*config.Node{}
	var walkNodes func(*config.IR)
	walkNodes = func(g *config.IR) {
		for i := range g.Nodes {
			nodes[g.Nodes[i].ID] = &g.Nodes[i]
			if g.Nodes[i].Sub != nil {
				walkNodes(g.Nodes[i].Sub)
			}
		}
	}
	walkNodes(ir)
	running := map[string]Running{}
	for _, r := range v.Running {
		running[r.Step] = r
	}
	// current iteration per loop on a run: the highest iteration with any
	// journaled or started node (1 when none)
	current := map[string]int{}
	if !preview {
		for key := range v.Replay.Results {
			if key.InputHash != "" {
				config.NoteLoopBounds(current, key.StepID)
			}
		}
		for _, s := range v.Replay.Starts {
			config.NoteLoopBounds(current, s.StepID)
		}
	}
	var out []frow
	var body func(scope, stateScope string, depth int)
	stateOf := func(id string) (status, dur string, since time.Time) {
		if preview {
			return "", "", time.Time{}
		}
		if r, ok := running[id]; ok && v.Live {
			return "running", durText(int(now.Sub(r.Since).Seconds())), r.Since
		}
		if line, ok := lineByID(v.Report, id); ok {
			if line.Status == pipeline.StateAbsent {
				return "", "", time.Time{}
			}
			return statusWord(line.Status), fmtDur(line.Duration), time.Time{}
		}
		return "", "", time.Time{}
	}
	body = func(scope, stateScope string, depth int) {
		// items at this scope: plain nodes, and nested loops folded once
		type item struct {
			id   string
			loop bool
		}
		var items []item
		seen := map[string]bool{}
		for id := range nodes {
			if !strings.HasPrefix(id, scope) {
				continue
			}
			rest := strings.TrimPrefix(id, scope)
			first, _, nested := strings.Cut(rest, "/")
			if !nested {
				if nodes[id].Kind == config.KindSelector || seen[id] {
					continue
				}
				seen[id] = true
				items = append(items, item{id: id})
				continue
			}
			if name, _, ok := config.IterationOf(first); ok {
				loop := scope + name
				if !seen[loop] {
					seen[loop] = true
					items = append(items, item{id: loop, loop: true})
				}
			}
		}
		rank := flowRank(ir, scope)
		sort.SliceStable(items, func(a, b int) bool {
			ra, rb := rank[items[a].id], rank[items[b].id]
			if ra != rb {
				return ra < rb
			}
			return items[a].id < items[b].id
		})
		for _, it := range items {
			if it.loop {
				li := loops[it.id]
				iter := 1
				capText := "x" + strconv.Itoa(li.cap)
				if !preview {
					if n := current[it.id]; n > 0 {
						iter = n
					}
					capText = fmt.Sprintf("%d of up to %d", iter, li.cap)
				}
				until := ""
				if li.until != "" {
					until = "until " + li.until
				}
				stateLoop := stateScope + strings.TrimPrefix(it.id, scope)
				out = append(out, frow{kind: "open", depth: depth, id: stateLoop, name: leaf(it.id), cap: capText, guard: until})
				body(it.id+"@1/", stateLoop+"@"+strconv.Itoa(iter)+"/", depth+1)
				out = append(out, frow{kind: "close", depth: depth, id: stateLoop})
				continue
			}
			n := nodes[it.id]
			stateID := stateScope + strings.TrimPrefix(n.ID, scope)
			if n.Kind == config.KindGenerate && n.Gen != nil {
				r := frow{kind: "gen", depth: depth, id: stateID, name: leaf(n.ID), guard: "generate over " + n.Gen.Source + " -> " + n.Gen.Workflow}
				if n.Gen.MaxParallel > 0 {
					r.cap = fmt.Sprintf("x%d at once", n.Gen.MaxParallel)
				}
				r.status, r.dur, _ = stateOf(stateID)
				out = append(out, r)
				if !preview {
					out = append(out, itemsRow(v, stateID, depth+1)...)
				}
				continue
			}
			r := frow{kind: "step", depth: depth, id: stateID, name: leaf(n.ID)}
			if n.When != nil {
				if g := guardCEL(n.When.CEL, scope); g != "" {
					r.guard = "if " + g
				}
			}
			if a := after[n.ID]; a != "" {
				r.after = "after " + a
			}
			r.status, r.dur, r.since = stateOf(stateID)
			if preview {
				if s, ok := v.Stats[leaf(n.ID)]; ok {
					r.dur = fmt.Sprintf("p50 %s  p90 %s", durText(int(s.P50.Seconds())), durText(int(s.P90.Seconds())))
				}
			}
			out = append(out, r)
		}
	}
	body(wf+"/", wf+"/", 0)
	return out
}

// flowRank orders a scope's items by longest dependency path, folded onto
// the items (a nested loop's nodes count for the loop).
func flowRank(ir *config.IR, scope string) map[string]int {
	fold := func(id string) string {
		if !strings.HasPrefix(id, scope) {
			return ""
		}
		rest := strings.TrimPrefix(id, scope)
		first, _, nested := strings.Cut(rest, "/")
		if nested {
			if name, _, ok := config.IterationOf(first); ok {
				return scope + name
			}
			return ""
		}
		return id
	}
	pred := map[string][]string{}
	var collect func(*config.IR)
	collect = func(g *config.IR) {
		for _, e := range g.Edges {
			a, b := fold(e.From), fold(e.To)
			if a != "" && b != "" && a != b {
				pred[b] = append(pred[b], a)
			}
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Sub != nil {
				collect(n.Sub)
			}
			if n.When == nil {
				continue
			}
			b := fold(n.ID)
			for _, d := range n.When.Deps {
				if a := fold(d); a != "" && b != "" && a != b {
					pred[b] = append(pred[b], a)
				}
			}
		}
	}
	collect(ir)
	rank := map[string]int{}
	var rankOf func(string, int) int
	rankOf = func(id string, depth int) int {
		if r, ok := rank[id]; ok {
			return r
		}
		if depth > 64 {
			return 0
		}
		best := 0
		for _, p := range pred[id] {
			if r := rankOf(p, depth+1) + 1; r > best {
				best = r
			}
		}
		rank[id] = best
		return best
	}
	for id := range pred {
		rankOf(id, 0)
	}
	return rank
}

// itemsRow lists a generate's instances side by side, as cells.
func itemsRow(v *RunView, gen string, depth int) []frow {
	for _, g := range v.Report.Generate {
		if g.Node != gen {
			continue
		}
		var cells []string
		for _, it := range g.Items {
			cells = append(cells, "["+it.ID+" "+statusWord(it.Status)+"]")
		}
		if len(cells) == 0 {
			return nil
		}
		return []frow{{kind: "items", depth: depth, id: gen, cells: cells}}
	}
	return nil
}

var stepRef = regexp.MustCompile(`steps\["([^"]+)"\]`)

// guardCEL renders a node's guard for humans: step references become
// leaf names (steps["bead/review-cycle@1/review"].verdict -> review.verdict),
// and the loop gate an unrolled iteration carries in front of the author's
// own condition (!(until@i-1) && (…)) is dropped.
func guardCEL(cel, scope string) string {
	s := strings.TrimSpace(cel)
	if strings.HasPrefix(s, "!(") {
		// "!(gate) && (own)" -> own; a bare "!(gate)" is the gate alone
		depth, end := 0, -1
		for i, r := range s {
			if r == '(' {
				depth++
			}
			if r == ')' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end > 0 {
			rest := strings.TrimSpace(s[end+1:])
			if strings.HasPrefix(rest, "&&") {
				s = strings.TrimSpace(strings.TrimPrefix(rest, "&&"))
				s = strings.TrimSuffix(strings.TrimPrefix(s, "("), ")")
			} else {
				return ""
			}
		}
	}
	return leafCEL(s)
}

// leafCEL rewrites steps["<id>"] references to the step's leaf name.
func leafCEL(cel string) string {
	return stepRef.ReplaceAllStringFunc(cel, func(m string) string {
		sub := stepRef.FindStringSubmatch(m)
		return leaf(sub[1])
	})
}

func scopeOf(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i+1]
	}
	return ""
}

func lineByID(r *pipeline.RunReport, id string) (*pipeline.StepLine, bool) {
	for i := range r.Steps {
		if r.Steps[i].ID == id {
			return &r.Steps[i], true
		}
	}
	for _, g := range r.Generate {
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

// flowPane renders the flow rows: frames in the tertiary colour, names in
// the identifier colour, facts in fixed columns, the status column by
// meaning, the selected row as the selection bar.
func flowPane(st style, m *model, w, h int) []string {
	out := []string{st.chrome("FLOW"), st.rule(w)}
	rows := m.frows
	if len(rows) == 0 {
		return append(out, "  "+st.dim("nothing to show"))
	}
	avail := h - 2
	start := 0
	if m.fsel >= avail {
		start = m.fsel - avail + 1
	}
	nameW := 16
	statusX := w - 22
	guardW := statusX - 2 - 3 - nameW - 16 // facts fit before the status column, with room for "after x"
	if guardW < 20 {
		guardW = 20
	}
	if guardW > 48 {
		guardW = 48
	}
	for i := start; i < len(rows) && i-start < avail; i++ {
		r := rows[i]
		right := w - 1 - r.depth*3
		left := "  "
		for d := 0; d < r.depth; d++ {
			left += st.dim("|") + "  "
		}
		var line string
		switch r.kind {
		case "open":
			head := st.dim("+- ") + st.ident(r.name) + "  " + r.cap
			if r.guard != "" {
				head += "  " + st.dim(r.guard)
			}
			fill := right - vis(left) - vis(head) - 1
			if fill < 1 {
				fill = 1
			}
			line = left + head + " " + st.dim(strings.Repeat("-", fill)+"+")
		case "close":
			fill := right - vis(left) - 1
			if fill < 1 {
				fill = 1
			}
			line = left + st.dim("+"+strings.Repeat("-", fill)+"^")
		case "items":
			cells := strings.Join(r.cells, "  ")
			line = left + "   " + st.dim(trunc(cells, right-vis(left)-4))
		default:
			name := st.ident(pad(trunc(r.name, nameW-2), nameW))
			if r.kind == "gen" {
				name = st.ident(pad(trunc(r.name, nameW-2), nameW))
			}
			facts := ""
			if r.guard != "" {
				facts += st.dim(pad(trunc(r.guard, guardW-2), guardW))
			} else {
				facts += strings.Repeat(" ", guardW)
			}
			if r.after != "" {
				facts += st.dim(r.after)
			}
			if r.cap != "" {
				facts += st.dim(r.cap)
			}
			// facts never run into the status column: cut to the room left
			factsW := statusX - r.depth*3 - vis(left) - 3 - nameW - 1
			if factsW > 0 && vis(facts) > factsW {
				facts = st.dim(trunc(stripSGR(facts), factsW))
			}
			line = left + "   " + name + facts
			if r.status != "" {
				line = pad(line, statusX-r.depth*3) + st.status(pad(r.status, 9)) + r.dur
			} else if r.dur != "" {
				line = pad(line, statusX-r.depth*3) + st.dim(r.dur) // a preview's history
			}
			if i == m.fsel {
				bar := pad(stripSGR(left)+" > "+stripSGR(name)+stripSGR(facts), right-1)
				if r.status != "" {
					bar = pad(stripSGR(left)+" > "+stripSGR(name)+stripSGR(facts), statusX-r.depth*3) + pad(r.status, 9) + r.dur
					bar = pad(bar, right-1)
				} else if r.dur != "" {
					bar = pad(pad(stripSGR(left)+" > "+stripSGR(name)+stripSGR(facts), statusX-r.depth*3)+r.dur, right-1)
				}
				line = st.rev(bar)
			}
		}
		// right frame edges for the enclosing frames
		if r.kind != "open" && r.kind != "close" {
			for d := r.depth; d > 0; d-- {
				edge := w - 1 - (d-1)*3
				if vis(line) < edge {
					line = pad(line, edge) + st.dim("|")
				}
			}
		} else {
			for d := r.depth; d > 0; d-- {
				edge := w - 1 - (d-1)*3
				if vis(line) < edge {
					line = pad(line, edge) + st.dim("|")
				}
			}
		}
		out = append(out, line)
	}
	return out
}
