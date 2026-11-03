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
	out := []string{st.chrome("GRAPH") + st.dim("   columns are dependency depth"), st.rule(w)}
	nodes, edges := collapsedGraph(m.run, m.rows)
	if len(nodes) == 0 {
		return append(out, "  "+st.dim("nothing to draw"))
	}
	layout(nodes, edges)
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
	boxW := 24
	gap := 6
	cellW := boxW + gap
	maxRows := 0
	for _, r := range rowsPerCol {
		if r > maxRows {
			maxRows = r
		}
	}
	rowH := 2
	canvas := make([][]byte, maxRows*rowH+1)
	width := (maxCol+1)*cellW + 2
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
	// Edges first, nodes on top: a box is never cut by a line passing
	// through its column.
	for _, e := range edges {
		a, b := nodes[e[0]], nodes[e[1]]
		if b.col <= a.col {
			continue
		}
		x0 := 2 + a.col*cellW + boxW
		x1 := 2 + b.col*cellW
		y0, y1 := a.row*rowH, b.row*rowH
		mid := x0 + (x1-x0)/2
		for x := x0; x < mid; x++ {
			put(x, y0, "-")
		}
		if y0 != y1 {
			lo, hi := y0, y1
			if lo > hi {
				lo, hi = hi, lo
			}
			for y := lo; y <= hi; y++ {
				put(mid, y, "|")
			}
			put(mid, y0, "+")
			put(mid, y1, "+")
		}
		for x := mid + 1; x < x1-1; x++ {
			put(x, y1, "-")
		}
		put(x1-1, y1, ">")
	}
	colored := map[[2]int]string{} // (x,y) of a status word start -> status
	for _, n := range nodes {
		x, y := 2+n.col*cellW, n.row*rowH
		label := trunc(n.label, 12)
		status := n.status
		text := fmt.Sprintf("[%-12s %s", label, status)
		if n.dur != "" && len(text)+1+len(n.dur) < boxW-1 {
			text += " " + n.dur
		}
		text = pad(trunc(text, boxW-1), boxW-1) + "]"
		put(x, y, text)
		colored[[2]int{x + 14, y}] = status
	}
	for y, line := range canvas {
		s := string(line)
		// colour the status words in place
		for pos, status := range colored {
			if pos[1] == y && st.color {
				word := status
				if idx := strings.Index(s[pos[0]:], word); idx == 0 && word != "" {
					s = s[:pos[0]] + st.status(word) + s[pos[0]+len(word):]
				}
			}
		}
		out = append(out, fitRow(s, w))
	}
	return out
}

// keep the reporter import used for status constants in tests
var _ = pipeline.StateOK
