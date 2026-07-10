package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
)

// WiringError is one wiring violation, addressed by IR node id and field path
// within the node, e.g. "task/review-cycle@2/fix.with.pr: ...". Violations are
// sorted by (Node, Path) before joining so output order is stable for tests
// and users.
type WiringError struct {
	Node string // IR node id (or "params" for run-entry errors)
	Path string // e.g. "with.pr" / "when" / "depends_on"
	Msg  string
}

func (e *WiringError) Error() string {
	if e.Path == "" {
		return e.Node + ": " + e.Msg
	}
	return e.Node + "." + e.Path + ": " + e.Msg
}

// CheckWiring validates the typed dataflow over the IR — the gate between
// "parses" and "will run": reference resolution, slot discipline, type
// compatibility, acyclicity, condition sanity, and the tool-subset rule.
// All violations are collected and joined; sub-workflow graphs are checked
// recursively. Generate nodes are checked as a boundary, not expanded: their
// target workflow's params are checked against the generate's binding
// template, which is what makes run-time expansion safe.
func CheckWiring(ir *IR, cfg *Config) error {
	env, err := newCondEnv()
	if err != nil {
		return err
	}
	c := &wiringChecker{cfg: cfg, env: env}
	c.checkGraph(ir)
	sort.Slice(c.errs, func(i, j int) bool {
		a, b := c.errs[i], c.errs[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Msg < b.Msg
	})
	joined := make([]error, len(c.errs))
	for i := range c.errs {
		joined[i] = &c.errs[i]
	}
	return errors.Join(joined...)
}

// CheckRunParams validates supplied --param bindings against a workflow's
// typed params interface at run entry: every required param present, every
// value type-correct, no unknown names. Missing required params are a hard
// error — there is no implicit fallback.
func CheckRunParams(wf WorkflowDef, supplied map[string]string) (Params, error) {
	return CheckParams(wf.Params, supplied)
}

// CompileCondition compile-checks one CEL condition in the environment every
// faber condition is typed in (steps and params as maps, item as dyn). The
// pipeline module's ConditionEvaluator is expected to reuse this entry point
// so validate-time compilation and run-time evaluation cannot drift.
func CompileCondition(expr string) error {
	env, err := newCondEnv()
	if err != nil {
		return err
	}
	return compileIn(env, expr)
}

func newCondEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("item", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("config: build CEL environment: %w", err)
	}
	return env, nil
}

func compileIn(env *cel.Env, expr string) error {
	_, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return iss.Err()
	}
	return nil
}

type wiringChecker struct {
	cfg  *Config
	env  *cel.Env
	errs []WiringError
}

func (c *wiringChecker) add(node, path, format string, args ...any) {
	c.errs = append(c.errs, WiringError{Node: node, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *wiringChecker) checkGraph(g *IR) {
	byID := make(map[string]*Node, len(g.Nodes))
	for i := range g.Nodes {
		byID[g.Nodes[i].ID] = &g.Nodes[i]
	}

	// Index: declared output fields and input slots per node. Selector nodes
	// proxy their newest candidate's output schema.
	outputs := map[string]map[string]FieldDef{}
	inputs := map[string]map[string]ParamDef{}
	var selectors []*Node
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch n.Kind {
		case KindAgent:
			if n.Template != nil {
				outputs[n.ID] = n.Template.Output
				inputs[n.ID] = n.Template.Inputs
			}
		case KindSubWorkflow:
			if n.Sub != nil {
				inputs[n.ID] = c.cfg.Workflows[n.Sub.Workflow].Params
			}
			outputs[n.ID] = map[string]FieldDef{}
		case KindGenerate:
			if n.Gen != nil {
				inputs[n.ID] = c.cfg.Workflows[n.Gen.Workflow].Params
			}
			outputs[n.ID] = map[string]FieldDef{}
		case KindSelector:
			selectors = append(selectors, n)
		default:
			// The kind discriminator is an open extension point (deferred
			// non-agent step kinds), but no such kind ships in this pass.
			c.add(n.ID, "", "unsupported node kind %q (no non-agent kinds ship in this IR version)", n.Kind)
		}
	}
	for _, sel := range selectors {
		if sel.Sel != nil && len(sel.Sel.Candidates) > 0 {
			outputs[sel.ID] = outputs[sel.Sel.Candidates[0]]
		}
	}

	c.passResolution(g, byID, outputs, inputs)
	inbound := inboundBySlot(g)
	c.passSlots(g, inputs, inbound)
	c.passTypes(g, outputs, inputs)
	c.passAcyclic(g, byID)
	c.passConditions(g, byID, outputs)
	c.passTools(g)

	for i := range g.Nodes {
		if g.Nodes[i].Sub != nil {
			c.checkGraph(g.Nodes[i].Sub)
		}
	}
}

// passResolution: every data edge's from names an existing node and a field
// declared in its output schema; every to names an existing node and slot.
func (c *wiringChecker) passResolution(g *IR, byID map[string]*Node, outputs map[string]map[string]FieldDef, inputs map[string]map[string]ParamDef) {
	for _, e := range g.Edges {
		if _, ok := byID[e.From]; !ok {
			c.add(e.To, edgePath(e), "references unknown node %q", e.From)
			continue
		}
		if _, ok := byID[e.To]; !ok {
			c.add(e.From, edgePath(e), "targets unknown node %q", e.To)
			continue
		}
		if e.IsOrdering() {
			continue
		}
		if fields, ok := outputs[e.From]; ok {
			if _, ok := fields[e.FromPort]; !ok {
				c.add(e.To, edgePath(e), "references %s.%s — output field does not exist%s",
					e.From, e.FromPort, didYouMean(e.FromPort, sortedKeys(fields)))
			}
		}
		if slots, ok := inputs[e.To]; ok {
			if _, ok := slots[e.ToPort]; !ok {
				c.add(e.To, edgePath(e), "binds undeclared input slot %q%s",
					e.ToPort, didYouMean(e.ToPort, sortedKeys(slots)))
			}
		}
	}
}

func edgePath(e Edge) string {
	if e.IsOrdering() {
		return "depends_on"
	}
	return "with." + e.ToPort
}

// passSlots: every required input slot bound exactly once from an inbound edge,
// a binding descriptor, or a declared default; no unknown-slot bindings; no
// double bindings.
func (c *wiringChecker) passSlots(g *IR, inputs map[string]map[string]ParamDef, inbound map[string]map[string]int) {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		slots, ok := inputs[n.ID]
		if !ok {
			continue
		}
		binds := effectiveBindings(n)
		for _, slot := range sortedKeys(binds) {
			if _, declared := slots[slot]; !declared {
				c.add(n.ID, "with."+slot, "binds undeclared input slot %q%s", slot, didYouMean(slot, sortedKeys(slots)))
			}
		}
		for _, slot := range sortedKeys(slots) {
			count := inbound[n.ID][slot]
			if _, ok := binds[slot]; ok {
				count++
			}
			decl := slots[slot]
			switch {
			case count > 1:
				c.add(n.ID, "with."+slot, "input slot bound %d times — a slot is bound exactly once", count)
			case count == 0 && decl.Required && decl.Default == nil:
				c.add(n.ID, "with."+slot, "required input slot %q is unbound", slot)
			}
		}
	}
}

func effectiveBindings(n *Node) map[string]BindingDesc {
	if n.Kind == KindGenerate && n.Gen != nil {
		return n.Gen.Bindings
	}
	return n.Bindings
}

// inboundBySlot counts data edges per (node, slot).
func inboundBySlot(g *IR) map[string]map[string]int {
	m := map[string]map[string]int{}
	for _, e := range g.Edges {
		if e.IsOrdering() {
			continue
		}
		if m[e.To] == nil {
			m[e.To] = map[string]int{}
		}
		m[e.To][e.ToPort]++
	}
	return m
}

// passTypes: the bound source type must equal the slot type, with
// enum-narrowing (an enum field satisfies a string slot; a plain string source
// does not satisfy an enum slot). No implicit conversions.
func (c *wiringChecker) passTypes(g *IR, outputs map[string]map[string]FieldDef, inputs map[string]map[string]ParamDef) {
	wfParams := c.cfg.Workflows[g.Workflow].Params
	for _, e := range g.Edges {
		if e.IsOrdering() {
			continue
		}
		src, okSrc := outputs[e.From][e.FromPort]
		dst, okDst := inputs[e.To][e.ToPort]
		if !okSrc || !okDst {
			continue // resolution pass reported it
		}
		if reason := typeSatisfies(src, dst); reason != "" {
			c.add(e.To, edgePath(e), "type mismatch: %s.%s %s", e.From, e.FromPort, reason)
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		slots, ok := inputs[n.ID]
		if !ok {
			continue
		}
		binds := effectiveBindings(n)
		for _, slot := range sortedKeys(binds) {
			dst, declared := slots[slot]
			if !declared {
				continue // slot pass reported it
			}
			b := binds[slot]
			path := "with." + slot
			switch b.Kind {
			case BindParam:
				decl, ok := wfParams[b.Name]
				if !ok {
					c.add(n.ID, path, "references params.%s — param not declared%s", b.Name, didYouMean(b.Name, sortedKeys(wfParams)))
					continue
				}
				if reason := typeSatisfies(decl, dst); reason != "" {
					c.add(n.ID, path, "type mismatch: params.%s %s", b.Name, reason)
				}
			case BindItem:
				// The data-source item contract guarantees id and deps; other
				// fields pass through typed as any.
				if t := itemFieldType(b.Field); t != "" && t != dst.Type {
					c.add(n.ID, path, "type mismatch: item.%s is %s, slot wants %s", b.Field, t, dst.Type)
				}
			case BindLiteral:
				if b.Type != dst.Type {
					c.add(n.ID, path, "type mismatch: literal is %s, slot wants %s", b.Type, dst.Type)
					continue
				}
				if len(dst.Enum) > 0 {
					if s, ok := b.Value.(string); !ok || !contains(dst.Enum, s) {
						c.add(n.ID, path, "literal %v not in slot enum [%s]", b.Value, strings.Join(dst.Enum, ", "))
					}
				}
			}
		}
	}
}

// itemFieldType returns the guaranteed type of a data-source item field, or ""
// for pass-through fields (typed as any).
func itemFieldType(field string) string {
	switch field {
	case "id":
		return "string"
	case "deps":
		return "object"
	default:
		return ""
	}
}

func typeSatisfies(src, dst ParamDef) string {
	if src.Type != dst.Type {
		return fmt.Sprintf("is %s, slot wants %s", src.Type, dst.Type)
	}
	if len(dst.Enum) == 0 {
		return "" // enum source satisfies a plain slot of the same base type
	}
	if len(src.Enum) == 0 {
		return fmt.Sprintf("is a plain %s, slot wants enum [%s]", src.Type, strings.Join(dst.Enum, ", "))
	}
	var missing []string
	for _, v := range src.Enum {
		if !contains(dst.Enum, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("enum values [%s] are not accepted by slot enum [%s]",
			strings.Join(missing, ", "), strings.Join(dst.Enum, ", "))
	}
	return ""
}

// condDepEdges returns virtual dep -> node edges for every condition
// dependency (when: and the selector exhaustion rule): a condition reads a
// completed result, so its deps order the node exactly like data edges do, and
// a mutual condition cycle must fail validation rather than deadlock the
// scheduler.
func condDepEdges(g *IR, byID map[string]*Node) []Edge {
	var out []Edge
	for i := range g.Nodes {
		n := &g.Nodes[i]
		conds := []*CondSpec{n.When}
		if n.Sel != nil {
			conds = append(conds, n.Sel.Exhausted)
		}
		for _, cond := range conds {
			if cond == nil {
				continue
			}
			for _, dep := range cond.Deps {
				if _, ok := byID[dep]; ok {
					out = append(out, Edge{From: dep, To: n.ID})
				}
			}
		}
	}
	return out
}

// passAcyclic: Kahn's algorithm over data + ordering edges plus virtual
// condition-dependency edges; on failure one concrete cycle is extracted and
// reported node-by-node. The IR must already be acyclic if the Desugarer is
// correct — this is defense in depth for hand-authored IR and for condition
// reference cycles the frontend can legally express.
func (c *wiringChecker) passAcyclic(g *IR, byID map[string]*Node) {
	indeg := map[string]int{}
	succ := map[string][]string{}
	for i := range g.Nodes {
		indeg[g.Nodes[i].ID] = 0
	}
	edges := append(append([]Edge(nil), g.Edges...), condDepEdges(g, byID)...)
	for _, e := range edges {
		if _, ok := byID[e.From]; !ok {
			continue
		}
		if _, ok := byID[e.To]; !ok {
			continue
		}
		indeg[e.To]++
		succ[e.From] = append(succ[e.From], e.To)
	}
	queue := []string{}
	for _, id := range sortedKeys(indeg) {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	done := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		done++
		for _, next := range succ[id] {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if done == len(g.Nodes) {
		return
	}
	// Kahn's leftovers are the cycle nodes plus their descendants; prune
	// leftover nodes with no outgoing edge into the leftover set until only
	// cycle participants remain, then walk one concrete cycle.
	leftover := map[string]bool{}
	for id, deg := range indeg {
		if deg > 0 {
			leftover[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, id := range sortedKeys(leftover) {
			hasOut := false
			for _, next := range succ[id] {
				if leftover[next] {
					hasOut = true
					break
				}
			}
			if !hasOut {
				delete(leftover, id)
				changed = true
			}
		}
	}
	if len(leftover) == 0 {
		return // unreachable: leftovers always contain a cycle
	}
	cycle := findCycle(sortedKeys(leftover)[0], succ, leftover)
	c.add(cycle[0], "", "reference cycle: %s", strings.Join(cycle, " -> "))
}

// findCycle walks successor edges within the cycle-participant set (every
// member has at least one such edge) until a node repeats.
func findCycle(start string, succ map[string][]string, members map[string]bool) []string {
	seen := map[string]int{start: 0}
	path := []string{start}
	cur := start
	for {
		var next string
		nbrs := append([]string(nil), succ[cur]...)
		sort.Strings(nbrs)
		for _, n := range nbrs {
			if members[n] {
				next = n
				break
			}
		}
		if at, ok := seen[next]; ok {
			return append(path[at:], next)
		}
		seen[next] = len(path)
		path = append(path, next)
		cur = next
	}
}

// passConditions: every condition's deps exist and are not descendants of the
// condition's own node (a condition may only read completed predecessors),
// every field the condition reads is declared in the dep's output schema, and
// the CEL source compiles in the standard condition environment.
func (c *wiringChecker) passConditions(g *IR, byID map[string]*Node, outputs map[string]map[string]FieldDef) {
	succ := map[string][]string{}
	for _, e := range g.Edges {
		succ[e.From] = append(succ[e.From], e.To)
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		type namedCond struct {
			path string
			cond *CondSpec
		}
		conds := []namedCond{{"when", n.When}}
		if n.Sel != nil {
			conds = append(conds, namedCond{"until", n.Sel.Exhausted})
		}
		for _, nc := range conds {
			if nc.cond == nil {
				continue
			}
			desc := descendants(n.ID, succ)
			for _, dep := range nc.cond.Deps {
				if _, ok := byID[dep]; !ok {
					c.add(n.ID, nc.path, "condition references unknown node %q", dep)
					continue
				}
				if desc[dep] {
					c.add(n.ID, nc.path, "condition reads %q, which does not precede this node (it is a descendant)", dep)
				}
			}
			for _, ref := range condFieldRefs(nc.cond.CEL) {
				fields, ok := outputs[ref.node]
				if !ok {
					continue // unknown node already reported above
				}
				if _, ok := fields[ref.field]; !ok {
					c.add(n.ID, nc.path, "references %s.%s — output field does not exist%s",
						ref.node, ref.field, didYouMean(ref.field, sortedKeys(fields)))
				}
			}
			if err := compileIn(c.env, nc.cond.CEL); err != nil {
				c.add(n.ID, nc.path, "condition does not compile: %v", err)
			}
		}
	}
}

type condFieldRef struct {
	node  string
	field string
}

// condFieldRefPattern matches the desugarer's canonical rewritten form
// steps["<node-id>"].<field>. The desugarer is the only author of this form
// (user source is rewritten before it reaches the IR), so a pattern match over
// the CEL source recovers exactly the (node, field) pairs the condition reads.
var condFieldRefPattern = regexp.MustCompile(`steps\["([^"]+)"\]\.([A-Za-z_][A-Za-z0-9_]*)`)

func condFieldRefs(cel string) []condFieldRef {
	var out []condFieldRef
	for _, m := range condFieldRefPattern.FindAllStringSubmatch(cel, -1) {
		out = append(out, condFieldRef{node: m[1], field: m[2]})
	}
	return out
}

func descendants(id string, succ map[string][]string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string(nil), succ[id]...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		stack = append(stack, succ[cur]...)
	}
	return out
}

// passTools: a step's declared tool needs must be a subset of its template's
// package list. One-directional by design: the image is built from the list,
// so the list is ground truth.
func (c *wiringChecker) passTools(g *IR) {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Kind != KindAgent || len(n.Tools) == 0 || n.Template == nil {
			continue
		}
		have := map[string]bool{}
		for _, p := range n.Template.Packages {
			have[p] = true
		}
		var missing []string
		for _, t := range n.Tools {
			if !have[t] {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			c.add(n.ID, "tools", "tools not provided by template %q packages: [%s]",
				n.Template.Name, strings.Join(missing, ", "))
		}
	}
}

// didYouMean returns a near-miss suggestion when a candidate is within edit
// distance 2 of the given name.
func didYouMean(name string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := levenshtein(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %s?)", best)
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
