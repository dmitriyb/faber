package pipeline

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/metering"
	"github.com/google/cel-go/cel"
)

// Terminal step states. Exactly four; deferral is a waiting state, never one
// of these, and settles into one of the four with its record annotated.
const (
	StateOK                = "ok"
	StateFailed            = "failed"
	StateSkippedCondition  = "skipped-condition"
	StateSkippedDependency = "skipped-dependency"
	// StateAbsent is a report-only status for IR nodes with no journal record
	// (a run that died mid-flight). It is never a scheduler state.
	StateAbsent = "absent"
)

// Pipeline-produced failure reasons (the failure module's per-producer
// vocabulary). The two skip reasons are the journal encoding of the skip
// terminal states: the failure module's record union has only ok|failed, so a
// skip journals as a failed-status record carrying one of these reasons and a
// null input hash — never a resume hit, decoded back to its skip state by the
// reporter.
const (
	reasonSkippedCondition  = StateSkippedCondition
	reasonSkippedDependency = StateSkippedDependency
	reasonBudget            = "budget"    // metering admission reject
	reasonAdmission         = "admission" // admission machinery error
	reasonCondition         = "condition" // condition evaluation error
	reasonExpansion         = "expansion" // generate splice machinery error
	// reasonDeferred and reasonCached tag annotation entries appended to a
	// record's attempt history: defer occurrences (count + wait window in the
	// entry's timing) and journal-hit adoption. They are annotations, not
	// attempts — the record's Attempt count never includes them.
	reasonDeferred = "deferred"
	reasonCached   = "cached"
)

// node lifecycle states (scheduler-internal, distinct from terminal status).
type nodeLife int

const (
	nsPending  nodeLife = iota // waiting on in-edges
	nsQueued                   // in the ready heap
	nsSlotWait                 // dispatched, waiting for a box slot
	nsRunning                  // worker in flight
	nsDeferred                 // deferred(until); wake re-queues it
	nsDone                     // terminal
)

// scopeEnv is one parameter scope: the run's root params, one inlined
// sub-workflow's params (entry is the sub node, params filled when it
// settles), or one generate instance's params (no entry node, params bound at
// expansion).
type scopeEnv struct {
	entry  string
	params map[string]any
}

// execNode is the scheduler's per-node state over an IR node. The config.Node
// is read-only shared structure; everything mutable lives here and is touched
// only on the scheduler's loop goroutine.
type execNode struct {
	n   *config.Node
	env *scopeEnv

	whenProg cel.Program // compiled When, nil if none
	exhProg  cel.Program // compiled selector exhaustion, nil if none

	life    nodeLife
	status  string          // one of State* once life == nsDone
	payload map[string]any  // decoded ok payload for threading and conditions
	raw     json.RawMessage // the ok payload's raw bytes (selector adoption)

	inputs map[string]any // resolved at dispatch (agent nodes)
	hash   string         // input hash (agent nodes)
	image  string         // resolved image tag (agent nodes)

	cached      bool
	used        int                   // real failed attempts consumed across defer re-queues
	history     []failure.AttemptInfo // real attempt history accumulated across defer re-queues
	annotations []failure.AttemptInfo // defer/cached annotations (journal-bound)
	costs       []metering.Cost       // settled costs stashed across defer re-queues
}

func (n *execNode) terminal() bool { return n.life == nsDone }

// graph is the flattened execution DAG: the entry IR with every sub-workflow
// inlined, plus any generate splices applied mid-run. All mutation happens on
// the scheduler loop goroutine.
type graph struct {
	nodes   map[string]*execNode
	succ    map[string][]string        // dedup adjacency: edges + condition deps
	pair    map[string]map[string]bool // dedup guard for succ/indeg
	indeg   map[string]int
	dataIn  map[string][]config.Edge // inbound data edges per node
	members map[string][]string      // sub entry id -> transitive member ids
	done    int                      // terminal node count
}

func newGraph() *graph {
	return &graph{
		nodes:   map[string]*execNode{},
		succ:    map[string][]string{},
		pair:    map[string]map[string]bool{},
		indeg:   map[string]int{},
		dataIn:  map[string][]config.Edge{},
		members: map[string][]string{},
	}
}

// edge records one readiness edge (data, ordering, or condition-dep),
// deduplicated so a pair contributes one in-degree no matter how many forms
// connect it.
func (g *graph) edge(from, to string) {
	if g.pair[from] == nil {
		g.pair[from] = map[string]bool{}
	}
	if g.pair[from][to] {
		return
	}
	g.pair[from][to] = true
	g.succ[from] = append(g.succ[from], to)
	g.indeg[to]++
}

// addLevel splices one IR level into the graph under env: nodes registered,
// conditions compiled, sub-workflow nodes recursively inlined (the desugarer
// already scoped their inner ids), edges and condition deps indexed, and the
// structural sub-workflow wiring added (entry gates its inner sources; outer
// dependents of the entry also wait on its deep sinks). It returns the ids it
// added (this level and below). On error the caller rolls back via remove.
func (g *graph) addLevel(ir *config.IR, env *scopeEnv, ce *condEval) ([]string, error) {
	var added []string
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if _, dup := g.nodes[n.ID]; dup {
			return added, fmt.Errorf("pipeline: duplicate node id %q", n.ID)
		}
		gn := &execNode{n: n, env: env}
		if n.When != nil {
			p, err := ce.compile(n.When)
			if err != nil {
				return added, fmt.Errorf("pipeline: node %s: %w", n.ID, err)
			}
			gn.whenProg = p
		}
		if n.Sel != nil && n.Sel.Exhausted != nil {
			p, err := ce.compile(n.Sel.Exhausted)
			if err != nil {
				return added, fmt.Errorf("pipeline: node %s: %w", n.ID, err)
			}
			gn.exhProg = p
		}
		g.nodes[n.ID] = gn
		added = append(added, n.ID)

		if n.Kind == config.KindSubWorkflow {
			if n.Sub == nil {
				return added, fmt.Errorf("pipeline: sub-workflow node %s has no inlined graph", n.ID)
			}
			subEnv := &scopeEnv{entry: n.ID, params: map[string]any{}}
			inner, err := g.addLevel(n.Sub, subEnv, ce)
			added = append(added, inner...)
			if err != nil {
				return added, err
			}
			g.members[n.ID] = inner
			// The entry gates its inner sources; nothing inside runs before
			// the entry has settled ok and bound the scope params.
			for _, src := range levelSources(n.Sub) {
				g.edge(n.ID, src)
			}
		}
	}

	for _, e := range ir.Edges {
		if _, ok := g.nodes[e.From]; !ok {
			return added, fmt.Errorf("pipeline: edge from unknown node %q", e.From)
		}
		if _, ok := g.nodes[e.To]; !ok {
			return added, fmt.Errorf("pipeline: edge to unknown node %q", e.To)
		}
		g.edge(e.From, e.To)
		if !e.IsOrdering() {
			g.dataIn[e.To] = append(g.dataIn[e.To], e)
		}
	}
	// Condition deps order a node exactly like edges do: the expression reads
	// a completed result.
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		for _, spec := range nodeConds(n) {
			for _, dep := range spec.Deps {
				if _, ok := g.nodes[dep]; !ok {
					return added, fmt.Errorf("pipeline: node %s condition reads unknown node %q", n.ID, dep)
				}
				g.edge(dep, n.ID)
			}
		}
	}
	// "After the sub-workflow" means after every inner node: an outer
	// dependent of the entry also waits on the sub graph's deep sinks.
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if n.Kind != config.KindSubWorkflow {
			continue
		}
		sinks := deepSinks(n.Sub)
		for _, e := range ir.Edges {
			if e.From != n.ID {
				continue
			}
			for _, s := range sinks {
				g.edge(s, e.To)
			}
		}
	}
	return added, nil
}

// remove deletes nodes (a failed splice's rollback). Only intra-splice edges
// exist at rollback time, so deleting the node set restores the prior graph.
func (g *graph) remove(ids []string) {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	for _, id := range ids {
		delete(g.nodes, id)
		delete(g.dataIn, id)
		delete(g.members, id)
		delete(g.succ, id)
		delete(g.pair, id)
		delete(g.indeg, id)
	}
	for from, tos := range g.succ {
		kept := tos[:0]
		for _, to := range tos {
			if drop[to] {
				delete(g.pair[from], to)
				continue
			}
			kept = append(kept, to)
		}
		g.succ[from] = kept
	}
}

// nodeConds lists a node's condition specs (when: plus selector exhaustion).
func nodeConds(n *config.Node) []*config.CondSpec {
	var out []*config.CondSpec
	if n.When != nil {
		out = append(out, n.When)
	}
	if n.Sel != nil && n.Sel.Exhausted != nil {
		out = append(out, n.Sel.Exhausted)
	}
	return out
}

// levelEdges is one IR level's readiness edge set: declared edges plus
// condition deps, as (from, to) pairs.
func levelEdges(ir *config.IR) [][2]string {
	var out [][2]string
	for _, e := range ir.Edges {
		out = append(out, [2]string{e.From, e.To})
	}
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		for _, spec := range nodeConds(n) {
			for _, dep := range spec.Deps {
				out = append(out, [2]string{dep, n.ID})
			}
		}
	}
	return out
}

// levelSources returns the level's in-degree-0 node ids, sorted.
func levelSources(ir *config.IR) []string {
	hasIn := map[string]bool{}
	for _, e := range levelEdges(ir) {
		hasIn[e[1]] = true
	}
	var out []string
	for i := range ir.Nodes {
		if !hasIn[ir.Nodes[i].ID] {
			out = append(out, ir.Nodes[i].ID)
		}
	}
	sort.Strings(out)
	return out
}

// deepSinks returns the level's sink nodes, with sub-workflow sinks expanded
// to their own deep sinks — the nodes whose settlement means "this graph is
// done", sorted.
func deepSinks(ir *config.IR) []string {
	hasOut := map[string]bool{}
	for _, e := range levelEdges(ir) {
		hasOut[e[0]] = true
	}
	var out []string
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if hasOut[n.ID] {
			continue
		}
		if n.Kind == config.KindSubWorkflow && n.Sub != nil {
			out = append(out, deepSinks(n.Sub)...)
			continue
		}
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return out
}

// sortedIDs returns every node id in the graph, sorted (the deterministic
// iteration order for seeding and reporting).
func (g *graph) sortedIDs() []string {
	out := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
