package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
)

// item is one data-source item: id and deps are the guaranteed contract
// fields; every decoded field (including id and deps) stays bindable through
// ${item.*}. The engine never learns what an item is.
type item struct {
	ID     string
	Deps   []string
	Fields map[string]any
}

// instance is one expanded sub-workflow instance, ready to splice: the
// renamed clone of the target IR, the item-bound parameter scope, and the
// in-set deps its ordering derives from.
type instance struct {
	itemID string
	ir     *config.IR
	env    *scopeEnv
	deps   []string
}

// expander is the GenerateExpander: it runs the node's opaque data-source
// command host-side, enforces the items contract, and instantiates the
// pre-validated target workflow once per item. It owns no scheduling; the
// scheduler applies the returned instances atomically on its loop goroutine.
type expander struct {
	runner infra.CommandRunner
	irs    map[string]*config.IR // generate targets by name, proven at validate
}

// contractError is a data-source contract violation: it fails the generate
// node with the failure module's source-contract reason, naming the
// violation. Nothing has been launched when it is raised.
type contractError struct {
	node string
	msg  string
	err  error
}

func (e *contractError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("pipeline: generate %s: %s: %v", e.node, e.msg, e.err)
	}
	return fmt.Sprintf("pipeline: generate %s: %s", e.node, e.msg)
}

func (e *contractError) Unwrap() error { return e.err }

// expand runs the generate node's data-source command and instantiates the
// target workflow per item, in stable input order. An empty item set returns
// zero instances (the node settles ok as a no-op); every contract violation
// returns a *contractError before any instance exists.
func (x *expander) expand(ctx context.Context, n *config.Node, env *scopeEnv) ([]instance, error) {
	gen := n.Gen
	if gen == nil {
		return nil, &contractError{node: n.ID, msg: "node carries no generate spec"}
	}
	target, ok := x.irs[gen.Workflow]
	if !ok {
		return nil, &contractError{node: n.ID, msg: fmt.Sprintf("target workflow %q is not available to the executor", gen.Workflow)}
	}
	if x.runner == nil {
		return nil, &contractError{node: n.ID, msg: "no data-source command runner is wired"}
	}
	args, err := resolveArgs(gen.Args, env)
	if err != nil {
		return nil, &contractError{node: n.ID, msg: "resolve data-source args", err: err}
	}
	out, err := x.runner.Run(ctx, infra.CmdSpec{Path: gen.Command, Args: args})
	if err != nil {
		return nil, &contractError{node: n.ID, msg: "data-source command failed", err: err}
	}
	items, err := parseItems(out.Stdout)
	if err != nil {
		return nil, &contractError{node: n.ID, msg: "malformed data-source output", err: err}
	}
	if err := validateItems(items, gen.Bindings); err != nil {
		return nil, &contractError{node: n.ID, msg: "item contract violation", err: err}
	}
	if len(items) == 0 {
		return nil, nil // no-op: the node settles ok with zero instances
	}
	// The expansion ceiling mirrors desugar's maxUnrolledNodes: a data source
	// is opaque user output, and an absurd item count must be a contract
	// error, not an unbounded graph splice.
	if total := len(items) * countIRNodes(target); total > maxExpandedNodes {
		return nil, &contractError{node: n.ID, msg: fmt.Sprintf(
			"expansion too large: %d items × %d nodes per instance = %d nodes, over the %d-node ceiling",
			len(items), countIRNodes(target), total, maxExpandedNodes)}
	}

	inSet := map[string]bool{}
	for _, it := range items {
		inSet[it.ID] = true
	}
	instances := make([]instance, 0, len(items))
	for _, it := range items {
		params, err := bindInstanceParams(gen.Bindings, it, env)
		if err != nil {
			return nil, &contractError{node: n.ID, msg: fmt.Sprintf("bind item %q", it.ID), err: err}
		}
		prefix := fmt.Sprintf("%s[%s]", n.ID, it.ID)
		clone, err := cloneRenamedIR(target, prefix)
		if err != nil {
			return nil, &contractError{node: n.ID, msg: fmt.Sprintf("instantiate item %q", it.ID), err: err}
		}
		var deps []string
		for _, d := range it.Deps {
			if d == it.ID {
				continue // a self-dep is trivially satisfied, never an edge
			}
			if inSet[d] {
				deps = append(deps, d) // deps outside the set are ignored
			}
		}
		instances = append(instances, instance{
			itemID: it.ID,
			ir:     clone,
			env:    &scopeEnv{params: params},
			deps:   deps,
		})
	}
	return instances, nil
}

// resolveArgs resolves the data-source argv: each arg is either a literal
// string or exactly one ${params.*} reference resolved from the enclosing
// scope.
func resolveArgs(args []string, env *scopeEnv) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		b, err := config.ParseBinding(a)
		if err != nil {
			return nil, err
		}
		if !b.IsRef {
			out = append(out, a)
			continue
		}
		if b.Ref.Root != config.RootParams {
			return nil, fmt.Errorf("arg %q: only ${params.*} references are legal in data-source args", a)
		}
		v, ok := env.params[b.Ref.Name]
		if !ok {
			return nil, fmt.Errorf("arg %q: param %q is not bound", a, b.Ref.Name)
		}
		s, err := scalarString(v)
		if err != nil {
			return nil, fmt.Errorf("arg %q: %w", a, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// parseItems decodes the strict {"items": [...]} data-source shape. Unknown
// top-level keys, a missing items key, or non-object items are malformed
// output. id must be a string; deps, when present, a list of strings; every
// other field passes through untyped.
func parseItems(stdout []byte) ([]item, error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.DisallowUnknownFields()
	var envelope struct {
		Items *[]map[string]any `json:"items"`
	}
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("expected {\"items\": [...]}: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the items document")
	}
	if envelope.Items == nil {
		return nil, fmt.Errorf("missing \"items\" key")
	}
	items := make([]item, 0, len(*envelope.Items))
	for i, raw := range *envelope.Items {
		if raw == nil {
			return nil, fmt.Errorf("items[%d]: not an object", i)
		}
		it := item{Fields: raw}
		switch id := raw["id"].(type) {
		case string:
			it.ID = id
		case nil:
			return nil, fmt.Errorf("items[%d]: missing \"id\"", i)
		default:
			return nil, fmt.Errorf("items[%d]: \"id\" is %T, want string", i, raw["id"])
		}
		if depsRaw, ok := raw["deps"]; ok {
			list, ok := depsRaw.([]any)
			if !ok {
				return nil, fmt.Errorf("items[%d] (%s): \"deps\" is %T, want a list of ids", i, it.ID, depsRaw)
			}
			for j, d := range list {
				s, ok := d.(string)
				if !ok {
					return nil, fmt.Errorf("items[%d] (%s): deps[%d] is %T, want string", i, it.ID, j, d)
				}
				it.Deps = append(it.Deps, s)
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// maxExpandedNodes is the run-time expansion ceiling, the symmetric twin of
// the desugarer's maxUnrolledNodes: items × per-instance nodes above it is a
// contract error before any instance exists.
const maxExpandedNodes = 10000

// countIRNodes counts one IR's nodes including inlined sub-workflow graphs.
func countIRNodes(ir *config.IR) int {
	n := len(ir.Nodes)
	for i := range ir.Nodes {
		if ir.Nodes[i].Sub != nil {
			n += countIRNodes(ir.Nodes[i].Sub)
		}
	}
	return n
}

// itemIDPattern is the item-id grammar: ids embed into instance node ids as
// "<gen>[<id>]/..." and into the textual CEL rename of the canonical
// steps["<node-id>"] form, so the reserved namespacing characters ('[', ']',
// '/', '@'), quotes, backslashes, and control bytes are all contract
// violations — the same closed grammar step ids obey.
var itemIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// validateItems enforces the item contract: non-empty unique ids in the
// closed grammar, every ${item.field} the binding template references
// present on every item, and the in-set dep edges acyclic — a cyclic item
// set can never splice into the DAG, so it is rejected naming the cycle
// before any instance exists.
func validateItems(items []item, bindings map[string]config.BindingDesc) error {
	seen := map[string]int{}
	for i, it := range items {
		if it.ID == "" {
			return fmt.Errorf("items[%d]: empty id", i)
		}
		if !itemIDPattern.MatchString(it.ID) {
			return fmt.Errorf("items[%d]: item id %q must match %s (ids embed into node ids and CEL keys)", i, it.ID, itemIDPattern)
		}
		if prev, dup := seen[it.ID]; dup {
			return fmt.Errorf("items[%d]: duplicate id %q (first at items[%d])", i, it.ID, prev)
		}
		seen[it.ID] = i
	}
	var fields []string
	for _, slot := range sortedKeys(bindings) {
		if bindings[slot].Kind == config.BindItem {
			fields = append(fields, bindings[slot].Field)
		}
	}
	for _, it := range items {
		for _, f := range fields {
			if _, ok := it.Fields[f]; !ok {
				return fmt.Errorf("item %q lacks field %q referenced by the binding template", it.ID, f)
			}
		}
	}
	return checkItemCycles(items)
}

// checkItemCycles runs Kahn over the in-set dep edges and, on failure, walks
// one concrete cycle to name it.
func checkItemCycles(items []item) error {
	inSet := map[string]bool{}
	for _, it := range items {
		inSet[it.ID] = true
	}
	indeg := map[string]int{}
	succ := map[string][]string{}
	for _, it := range items {
		indeg[it.ID] += 0
		for _, d := range it.Deps {
			// Out-of-set deps are ignored (the source may describe a wider
			// world), and a self-dep is ignored under the same leniency — an
			// item trivially follows itself, so it is never treated as a
			// one-cycle. expand() applies the same rule when deriving edges.
			if !inSet[d] || d == it.ID {
				continue
			}
			succ[d] = append(succ[d], it.ID)
			indeg[it.ID]++
		}
	}
	var queue []string
	for _, it := range items {
		if indeg[it.ID] == 0 {
			queue = append(queue, it.ID)
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
	if done == len(items) {
		return nil
	}
	var leftover []string
	for id, d := range indeg {
		if d > 0 {
			leftover = append(leftover, id)
		}
	}
	sort.Strings(leftover)
	cycle := walkItemCycle(leftover[0], succ, leftover)
	return fmt.Errorf("dependency cycle among items: %s", strings.Join(cycle, " -> "))
}

// walkItemCycle follows in-set successor edges within the leftover set until
// an id repeats, returning the cycle path.
func walkItemCycle(start string, succ map[string][]string, members []string) []string {
	inLeft := map[string]bool{}
	for _, m := range members {
		inLeft[m] = true
	}
	seen := map[string]int{start: 0}
	path := []string{start}
	cur := start
	for {
		next := ""
		nbrs := append([]string(nil), succ[cur]...)
		sort.Strings(nbrs)
		for _, n := range nbrs {
			if inLeft[n] {
				next = n
				break
			}
		}
		if next == "" {
			return path
		}
		if at, ok := seen[next]; ok {
			return append(path[at:], next)
		}
		seen[next] = len(path)
		path = append(path, next)
		cur = next
	}
}

// bindInstanceParams builds one instance's parameter scope from the generate
// node's binding template: ${item.*} entries become this item's field values,
// ${params.*} entries copy the enclosing scope's resolved params, literals
// pass through.
func bindInstanceParams(bindings map[string]config.BindingDesc, it item, env *scopeEnv) (map[string]any, error) {
	params := make(map[string]any, len(bindings))
	for _, name := range sortedKeys(bindings) {
		bd := bindings[name]
		switch bd.Kind {
		case config.BindLiteral:
			params[name] = bd.Value
		case config.BindParam:
			v, ok := env.params[bd.Name]
			if !ok {
				return nil, fmt.Errorf("param %q: enclosing scope binds no %q", name, bd.Name)
			}
			params[name] = v
		case config.BindItem:
			v, ok := it.Fields[bd.Field]
			if !ok {
				return nil, fmt.Errorf("param %q: item lacks field %q", name, bd.Field)
			}
			params[name] = v
		default:
			return nil, fmt.Errorf("param %q: unknown binding kind %q", name, bd.Kind)
		}
	}
	return params, nil
}

// cloneRenamedIR deep-copies the pre-validated target IR and re-roots every
// node id from the target workflow's scope prefix onto the instance prefix —
// "task/implement" under prefix "epic/tasks[I-3]" becomes
// "epic/tasks[I-3]/implement". Edge endpoints, condition deps, selector
// candidates, nested sub-workflow graphs, and the canonical
// steps["<node-id>"] keys inside CEL sources are renamed with the same rule.
// Nothing is re-validated: the source IR was proven at validate time and
// renaming cannot change its structure.
func cloneRenamedIR(target *config.IR, prefix string) (*config.IR, error) {
	raw, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("clone target IR: %w", err)
	}
	var clone config.IR
	if err := json.Unmarshal(raw, &clone); err != nil {
		return nil, fmt.Errorf("clone target IR: %w", err)
	}
	oldPfx := target.Workflow + "/"
	newPfx := prefix + "/"
	rename := func(id string) string {
		if strings.HasPrefix(id, oldPfx) {
			return newPfx + id[len(oldPfx):]
		}
		return prefix + "/" + id
	}
	renameIR(&clone, rename, `steps["`+oldPfx, `steps["`+newPfx)
	return &clone, nil
}

// renameIR applies the id rename across one IR level and recurses into
// inlined sub-workflow graphs. celOld/celNew rewrite the desugarer's
// canonical steps["<node-id>"] references, whose ids all share the workflow
// scope prefix.
func renameIR(ir *config.IR, rename func(string) string, celOld, celNew string) {
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		n.ID = rename(n.ID)
		if n.When != nil {
			renameCond(n.When, rename, celOld, celNew)
		}
		if n.Sel != nil {
			for j := range n.Sel.Candidates {
				n.Sel.Candidates[j] = rename(n.Sel.Candidates[j])
			}
			if n.Sel.Exhausted != nil {
				renameCond(n.Sel.Exhausted, rename, celOld, celNew)
			}
		}
		if n.Sub != nil {
			renameIR(n.Sub, rename, celOld, celNew)
		}
	}
	for i := range ir.Edges {
		ir.Edges[i].From = rename(ir.Edges[i].From)
		ir.Edges[i].To = rename(ir.Edges[i].To)
	}
}

func renameCond(spec *config.CondSpec, rename func(string) string, celOld, celNew string) {
	for i := range spec.Deps {
		spec.Deps[i] = rename(spec.Deps[i])
	}
	spec.CEL = strings.ReplaceAll(spec.CEL, celOld, celNew)
}

// generatePayload is the generate node's ok payload: the item count and ids,
// in input order.
func generatePayload(instances []instance) json.RawMessage {
	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.itemID)
	}
	raw, err := json.Marshal(map[string]any{"count": len(instances), "ids": ids})
	if err != nil {
		// count/ids are a string slice and an int; this cannot fail.
		raw = []byte(fmt.Sprintf(`{"count":%d}`, len(instances)))
	}
	return raw
}

// sourceContractResult wraps a contract violation as the generate node's
// failed result record.
func sourceContractResult(err error) failure.Result {
	return failure.Result{
		Status:  failure.StatusFailed,
		Error:   &failure.ErrorRecord{Reason: failure.ReasonSourceContract, Detail: err.Error()},
		Attempt: 1,
	}
}

// sortedKeys returns a map's keys sorted (deterministic iteration).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
