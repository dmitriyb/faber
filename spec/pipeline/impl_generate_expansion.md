# Implementation: Generate expansion algorithm

Covers GenerateExpander.

## Types (internal/pipeline/generate.go)

```go
type GenerateExpander struct {
    runner  infra.CommandRunner            // opaque user command, host-side
    irs     map[string]*config.IR          // target workflows, pre-validated
    policy  GeneratePolicy                 // deferred seam; zero value = first pass
}

type item struct {
    ID     string         // required, unique, non-empty
    Deps   []string       // ids; out-of-set entries ignored
    Fields map[string]any // pass-through for ${item.field}
}

type Splice struct {
    Nodes []config.Node
    Edges []config.Edge  // instance-internal + inter-instance + rewires
}
```

`item` unmarshals via a custom `UnmarshalJSON`: decode into `map[string]any`,
lift `id`/`deps` with type checks, keep the rest in `Fields`. Extra fields are
data, not schema — the WiringChecker typed `${item.x}` bindings as declared by
the source or `any`.

## Expand

```go
func (e *GenerateExpander) Expand(ctx context.Context, n *config.Node,
        params map[string]any) (*Splice, error) {

    out, err := e.runner.Run(ctx, n.Gen.Source.Command, resolveArgs(n.Gen.Source.Args, params)...)
    if err != nil { return nil, contractErr(n, "data-source command failed", err) }

    items, err := parseItems(out.Stdout) // strict: {"items":[...]} shape
    if err != nil { return nil, contractErr(n, "malformed data-source output", err) }
    if err := validateItems(items, n.Gen.With); err != nil {
        return nil, contractErr(n, "item contract violation", err)
    }
    if len(items) == 0 { return &Splice{}, nil } // no-op: node settles ok

    sp := &Splice{}
    inst := map[string]instance{}
    for _, it := range items {                     // stable input order
        pfx := fmt.Sprintf("%s[%s]", n.ID, it.ID)  // epic/tasks[I-3]
        inst[it.ID] = e.instantiate(sp, pfx, n.Gen.Workflow, bind(n.Gen.With, it, params))
    }
    for _, it := range items {
        for _, d := range it.Deps {
            dep, ok := inst[d]
            if !ok { continue }                    // outside the set: ignored
            orderEdges(sp, dep.sinks, inst[it.ID].sources)
        }
    }
    return sp, nil
}
```

`validateItems` checks: ids non-empty and unique; every `${item.field}` the
binding template references exists on every item with a bindable type; and the
in-set dep edges are acyclic (Kahn over the item set) — a cyclic item set can
never splice into the DAG, so it is rejected as a contract violation naming
the cycle path, before any instance exists.

## instantiate

```go
func (e *GenerateExpander) instantiate(sp *Splice, pfx, wf string,
        params map[string]any) instance {
    src := e.irs[wf] // desugared + wiring-checked at faber validate
    for _, node := range src.Nodes {
        nn := node.Clone()
        nn.ID = pfx + "/" + node.ID          // epic/tasks[I-3]/review-cycle@2/fix
        nn.Bindings = resolveParams(nn.Bindings, params) // ${item.*} now literal
        sp.Nodes = append(sp.Nodes, nn)
    }
    for _, edge := range src.Edges { sp.Edges = append(sp.Edges, prefix(edge, pfx)) }
    return instance{sources: sourcesOf(src, pfx), sinks: sinksOf(src, pfx)}
}
```

Instantiation is mechanical: prefix IDs, re-point edges, bake this item's
param values into the binding descriptors. Nothing is re-validated — the
source IR was proven at validate time, prefixing cannot create a cycle, and
the inter-instance edges were just proven acyclic over the item set.

## Splice hand-off

The scheduler applies the splice atomically on its loop goroutine: append
nodes, index edges, seed in-degrees, and rewire every original dependent of
the generate node with ordering edges from each instance's sinks. Only then
does the generate node settle `ok` with payload `{count, ids}` — so a
dependent's in-degree already includes the instance sinks before the generate
node's own edge is satisfied. Instance sources with no in-set deps enter the
ready queue immediately, in node-ID order like everything else.

Deferred policy knobs (`GeneratePolicy`): empty-set-as-error, cascade
abort-fan-out, and a richer malformed-output error contract all live behind
the policy struct; the mechanics above do not change.
