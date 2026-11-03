package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/pipeline"
)

// gnode is one node of the collapsed graph: a top-level step, a loop (its
// iterations folded), or a generate instance.
type gnode struct {
	id     string
	label  string
	status string
	dur    string
	col    int
	row    int
}

// collapsedGraph folds the IR to what a human draws: loops become one
// node, iteration nodes vanish, generate instances become one node each
// under their generate. Edges are the IR's, mapped through the fold.
func collapsedGraph(v *RunView, rows []row) ([]gnode, [][2]int) {
	return buildGraph(v, rows, false)
}

// previewGraph inlines every loop body once: the nodes of iteration 1 at
// every nesting level, labelled by their path without the iteration
// marks (review-cycle/review), with the IR's own edges among them — the
// compute graph as the author drew it, before any run.
func previewGraph(v *RunView) ([]gnode, [][2]int) {
	return buildGraph(v, nil, true)
}

func buildGraph(v *RunView, rows []row, preview bool) ([]gnode, [][2]int) {
	if preview {
		return inlinedGraph(v)
	}
	foldIter := func(id string) string {
		segs := strings.Split(id, "/")
		for k, seg := range segs {
			if name, _, ok := config.IterationOf(seg); ok {
				return strings.Join(append(append([]string{}, segs[:k]...), name), "/")
			}
		}
		return id
	}
	// A selector stands for its loop: an edge or condition read through it
	// is an edge from the loop.
	selLoop := map[string]string{}
	if v.IR != nil {
		for i := range v.IR.Nodes {
			n := &v.IR.Nodes[i]
			if n.Kind == config.KindSelector && n.Sel != nil && len(n.Sel.Candidates) > 0 {
				selLoop[n.ID] = foldIter(n.Sel.Candidates[0])
			}
		}
	}
	fold := func(id string) string {
		if loop, ok := selLoop[id]; ok {
			return loop
		}
		return foldIter(id)
	}
	var nodes []gnode
	index := map[string]int{}
	add := func(id, label, status, dur string) {
		if _, ok := index[id]; ok {
			return
		}
		index[id] = len(nodes)
		nodes = append(nodes, gnode{id: id, label: label, status: status, dur: dur})
	}
	for _, r := range rows {
		switch r.kind {
		case "step":
			if r.indent == 0 {
				add(r.id, r.name, r.status, r.dur)
			}
		case "loop":
			if r.indent == 0 {
				add(r.id, r.name, r.status, r.note)
			}
		case "item":
			add(r.id, r.name, r.status, r.dur)
		case "gen":
			add(r.id, r.name, "", r.note)
		}
	}
	var edges [][2]int
	seen := map[[2]int]bool{}
	if v.IR != nil {
		for _, e := range v.IR.Edges {
			a, aok := index[fold(e.From)]
			b, bok := index[fold(e.To)]
			if !aok || !bok || a == b || seen[[2]int{a, b}] {
				continue
			}
			seen[[2]int{a, b}] = true
			edges = append(edges, [2]int{a, b})
		}
		for i := range v.IR.Nodes {
			n := &v.IR.Nodes[i]
			if n.When == nil {
				continue
			}
			b, bok := index[fold(n.ID)]
			if !bok {
				continue
			}
			for _, d := range n.When.Deps {
				a, aok := index[fold(d)]
				if !aok || a == b || seen[[2]int{a, b}] {
					continue
				}
				seen[[2]int{a, b}] = true
				edges = append(edges, [2]int{a, b})
			}
		}
	}
	// instances hang under their generate node
	for i, n := range nodes {
		if strings.Contains(n.id, "[") && strings.HasSuffix(n.id, "]/") {
			gen := n.id[:strings.Index(n.id, "[")]
			if g, ok := index[gen]; ok && !seen[[2]int{g, i}] {
				seen[[2]int{g, i}] = true
				edges = append(edges, [2]int{g, i})
			}
		}
	}
	return nodes, edges
}

// inlinedGraph is the preview's graph: first-iteration nodes only, loop
// marks dropped from labels, IR edges and condition reads among them.
func inlinedGraph(v *RunView) ([]gnode, [][2]int) {
	if v.IR == nil {
		return nil, nil
	}
	first := func(id string) (string, bool) {
		segs := strings.Split(id, "/")
		var out []string
		for k, seg := range segs {
			if name, i, ok := config.IterationOf(seg); ok {
				if i != 1 {
					return "", false
				}
				out = append(out, name)
				continue
			}
			if k == 0 && seg == v.IR.Workflow {
				continue
			}
			out = append(out, seg)
		}
		return strings.Join(out, "/"), true
	}
	var nodes []gnode
	index := map[string]int{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Sub != nil {
				walk(n.Sub)
			}
			if n.Kind == config.KindSelector {
				continue
			}
			label, ok := first(n.ID)
			if !ok {
				continue
			}
			index[n.ID] = len(nodes)
			nodes = append(nodes, gnode{id: n.ID, label: label})
		}
	}
	walk(v.IR)
	var edges [][2]int
	seen := map[[2]int]bool{}
	selTarget := map[string]string{} // selector id -> its first (newest-first list's last) candidate in iteration 1
	var sels func(*config.IR)
	sels = func(g *config.IR) {
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Sub != nil {
				sels(n.Sub)
			}
			if n.Kind == config.KindSelector && n.Sel != nil && len(n.Sel.Candidates) > 0 {
				selTarget[n.ID] = n.Sel.Candidates[len(n.Sel.Candidates)-1]
			}
		}
	}
	sels(v.IR)
	resolve := func(id string) (int, bool) {
		if t, ok := selTarget[id]; ok {
			id = t
		}
		i, ok := index[id]
		return i, ok
	}
	addEdge := func(from, to string) {
		a, aok := resolve(from)
		b, bok := resolve(to)
		if !aok || !bok || a == b || seen[[2]int{a, b}] {
			return
		}
		seen[[2]int{a, b}] = true
		edges = append(edges, [2]int{a, b})
	}
	var collect func(*config.IR)
	collect = func(g *config.IR) {
		for _, e := range g.Edges {
			addEdge(e.From, e.To)
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Sub != nil {
				collect(n.Sub)
			}
			if n.When != nil {
				for _, d := range n.When.Deps {
					addEdge(d, n.ID)
				}
			}
		}
	}
	collect(v.IR)
	return nodes, edges
}

// commonLoopPrefix is the loop path every preview node shares, or "".
func commonLoopPrefix(nodes []gnode) string {
	if len(nodes) == 0 {
		return ""
	}
	first, _, ok := strings.Cut(nodes[0].label, "/")
	if !ok {
		return ""
	}
	for _, n := range nodes {
		if !strings.HasPrefix(n.label, first+"/") {
			return ""
		}
	}
	return first
}

// layout assigns columns by longest path from the sources and rows by one
// barycenter pass over the previous column, id order breaking ties.
func layout(nodes []gnode, edges [][2]int) {
	n := len(nodes)
	indeg := make([]int, n)
	succ := make([][]int, n)
	pred := make([][]int, n)
	for _, e := range edges {
		indeg[e[1]]++
		succ[e[0]] = append(succ[e[0]], e[1])
		pred[e[1]] = append(pred[e[1]], e[0])
	}
	var ready []int
	for i := 0; i < n; i++ {
		nodes[i].col = 0
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		for _, s := range succ[i] {
			if nodes[i].col+1 > nodes[s].col {
				nodes[s].col = nodes[i].col + 1
			}
			indeg[s]--
			if indeg[s] == 0 {
				ready = append(ready, s)
			}
		}
	}
	cols := map[int][]int{}
	maxCol := 0
	for i := range nodes {
		cols[nodes[i].col] = append(cols[nodes[i].col], i)
		if nodes[i].col > maxCol {
			maxCol = nodes[i].col
		}
	}
	for c := 0; c <= maxCol; c++ {
		ids := cols[c]
		sort.SliceStable(ids, func(a, b int) bool {
			ba, bb := bary(nodes, pred[ids[a]]), bary(nodes, pred[ids[b]])
			if ba != bb {
				return ba < bb
			}
			return nodes[ids[a]].id < nodes[ids[b]].id
		})
		for r, i := range ids {
			nodes[i].row = r
		}
	}
}

func bary(nodes []gnode, preds []int) float64 {
	if len(preds) == 0 {
		return 0
	}
	sum := 0.0
	for _, p := range preds {
		sum += float64(nodes[p].row)
	}
	return sum / float64(len(preds))
}

// graphPane draws the laid-out graph: boxes per column, orthogonal edges.
// Above a size the drawing cannot read, columns fold to summaries.
func graphPane(st style, m *model, w, h int) []string {
	title := st.chrome("GRAPH") + st.dim("   columns are dependency depth")
	nodes, edges := m.graph()
	if len(nodes) == 0 {
		return append([]string{title, st.rule(w)}, "  "+st.dim("nothing to draw"))
	}
	frame := ""
	if m.preview {
		// Every node inside one outer loop shares its path: draw that loop
		// as a labelled frame around the body, once, instead of in every
		// caption — the difference between a chain and a chain repeated.
		if prefix := commonLoopPrefix(nodes); prefix != "" {
			frame = fmt.Sprintf("%s, up to %d iterations", strings.ReplaceAll(prefix, "/", " > "), m.run.Bounds[m.run.ID+"/"+prefix])
			for i := range nodes {
				nodes[i].label = strings.TrimPrefix(nodes[i].label, prefix+"/")
			}
		}
	}
	out := []string{title, st.rule(w)}
	maxCol, rowsPerCol := 0, map[int]int{}
	for _, n := range nodes {
		if n.col > maxCol {
			maxCol = n.col
		}
		if n.row+1 > rowsPerCol[n.col] {
			rowsPerCol[n.col] = n.row + 1
		}
	}
	if len(nodes) > 60 {
		for c := 0; c <= maxCol; c++ {
			counts := map[string]int{}
			for _, n := range nodes {
				if n.col == c {
					counts[n.status]++
				}
			}
			var parts []string
			for _, k := range sortedKeys(counts) {
				parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
			}
			out = append(out, fmt.Sprintf("  depth %d: %d nodes   %s", c+1, rowsPerCol[c], strings.Join(parts, ", ")))
		}
		return out
	}
	// Cells fit the pane: the column pitch is the width over the columns,
	// boxes two lines tall (the loop path above the step name) so a label
	// like review-cycle/review needs no 30-cell box.
	cells := maxCol + 1
	cellW := (w - 4) / cells
	if cellW < 18 {
		cellW = 18
	}
	if cellW > 34 {
		cellW = 34
	}
	gap := 5
	boxW := cellW - gap
	maxRows := 0
	for _, r := range rowsPerCol {
		if r > maxRows {
			maxRows = r
		}
	}
	rowH := 3
	selectedX := map[int][2]int{}
	canvas := make([][]byte, maxRows*rowH+1)
	width := cells*cellW + 2
	for i := range canvas {
		canvas[i] = []byte(strings.Repeat(" ", width))
	}
	put := func(x, y int, s string) {
		if y < 0 || y >= len(canvas) {
			return
		}
		for i := 0; i < len(s) && x+i < width; i++ {
			if x+i >= 0 {
				canvas[y][x+i] = s[i]
			}
		}
	}
	// Edges first, nodes on top. A multi-column edge turns in the gap right
	// after its source column and runs along the target's row, so it never
	// threads a box; boxes drawn afterwards hide what it crosses. Arrowheads
	// go last, so a longer edge's line never overwrites a shorter one's head.
	var heads [][2]int
	for _, e := range edges {
		a, b := nodes[e[0]], nodes[e[1]]
		if b.col <= a.col {
			continue
		}
		x0 := 2 + a.col*cellW + boxW
		x1 := 2 + b.col*cellW
		y0, y1 := a.row*rowH+1, b.row*rowH+1
		xv := x0 + 2
		if y0 == y1 {
			for x := x0; x < x1-1; x++ {
				put(x, y0, "-")
			}
		} else {
			for x := x0; x < xv; x++ {
				put(x, y0, "-")
			}
			lo, hi := y0, y1
			if lo > hi {
				lo, hi = hi, lo
			}
			for y := lo; y <= hi; y++ {
				put(xv, y, "|")
			}
			put(xv, y0, "+")
			put(xv, y1, "+")
			for x := xv + 1; x < x1-1; x++ {
				put(x, y1, "-")
			}
		}
		heads = append(heads, [2]int{x1 - 1, y1})
	}
	for _, h := range heads {
		put(h[0], h[1], ">")
	}
	// Boxes are steps. A preview box carries a dim caption above it — the
	// loop path the step sits in — and a run's box its status and duration
	// below. Edges run on the box line.
	colored := map[[2]int]string{} // (x,y) of a status word start -> status
	captions := map[[2]int]int{}   // (x,y) -> caption length, dimmed at output
	selected := map[int]bool{}
	for i, n := range nodes {
		x, y := 2+n.col*cellW, n.row*rowH+1
		caption, name := "", n.label
		if m.preview {
			if j := strings.LastIndex(n.label, "/"); j >= 0 {
				caption, name = n.label[:j], n.label[j+1:]
			}
		}
		inner := boxW - 2
		box := "[" + pad(trunc(name, inner), inner) + "]"
		put(x, y, box)
		if caption != "" {
			c := trunc(caption, inner)
			put(x+1, y-1, c)
			captions[[2]int{x + 1, y - 1}] = len(c)
		}
		if !m.preview {
			below := " " + n.status
			if n.dur != "" {
				below += " " + n.dur
			}
			put(x, y+1, trunc(below, boxW))
			if n.status != "" {
				colored[[2]int{x + 1, y + 1}] = n.status
			}
		}
		if i == m.selectedNode() {
			selected[y] = true
			selectedX[y] = [2]int{x, x + boxW}
		}
	}
	var body []string
	for y, line := range canvas {
		s := string(line)
		for pos, n := range captions {
			if pos[1] == y && st.color {
				s = s[:pos[0]] + st.dim(s[pos[0]:pos[0]+n]) + s[pos[0]+n:]
			}
		}
		if selected[y] {
			// the selected box in reverse video, codes stripped inside it
			x0, x1 := selectedX[y][0], selectedX[y][1]
			if x1 > len(s) {
				x1 = len(s)
			}
			s = s[:x0] + st.rev(s[x0:x1]) + s[x1:]
			body = append(body, s)
			continue
		}
		// colour the status words in place
		for pos, status := range colored {
			if pos[1] == y && st.color {
				word := status
				if idx := strings.Index(s[pos[0]:], word); idx == 0 && word != "" {
					s = s[:pos[0]] + st.status(word) + s[pos[0]+len(word):]
				}
			}
		}
		body = append(body, s)
	}
	if frame != "" {
		// the shared outer loop as a labelled frame around the body
		inner := width
		top := "+-- " + frame + " " + strings.Repeat("-", max(0, inner-len(frame)-2)) + "+"
		out = append(out, fitRow(st.dim(top), w))
		for _, s := range body {
			out = append(out, fitRow(st.dim("| ")+pad(s, inner)+st.dim(" |"), w))
		}
		out = append(out, fitRow(st.dim("+"+strings.Repeat("-", inner+2)+"+"), w))
		return out
	}
	for _, s := range body {
		out = append(out, fitRow(s, w))
	}
	return out
}

// keep the reporter import used for status constants in tests
var _ = pipeline.StateOK
