# Implementation: Desugaring algorithm

Covers Desugarer and IRModel.

## IR types (internal/config/ir.go)

```go
type IR struct {
    IRVersion int              `json:"ir_version"`
    Workflow  string           `json:"workflow"`
    Nodes     []Node           `json:"nodes"` // sorted by ID
    Edges     []Edge           `json:"edges"` // sorted by (To, ToPort)
}

type Node struct {
    ID       string            `json:"id"`   // path-like: "task/review-cycle@2/fix"
    Kind     string            `json:"kind"` // agent | sub-workflow | generate | selector
    Template *ResolvedTemplate `json:"template,omitempty"` // agent nodes
    Sub      *IR               `json:"sub,omitempty"`       // sub-workflow nodes
    Gen      *GenSpec          `json:"gen,omitempty"`       // generate nodes
    Sel      *SelSpec          `json:"sel,omitempty"`       // selector nodes
    Bindings map[string]BindingDesc `json:"bindings"`  // literals, params, items
    When     *CondSpec         `json:"when,omitempty"`
    Retry    int               `json:"retry,omitempty"`
    OnFailure string           `json:"on_failure,omitempty"`
}

type Edge struct {
    From     string `json:"from"`
    FromPort string `json:"from_port,omitempty"` // empty => ordering edge
    To       string `json:"to"`
    ToPort   string `json:"to_port,omitempty"`
}

type CondSpec struct {
    CEL  string   `json:"cel"`
    Deps []string `json:"deps"` // node IDs the expression reads
}
```

`ResolvedTemplate` embeds everything the executor needs (image spec = packages +
overlay hash, hooks, the optional skills leg `{dir, link}`, identity, resources,
runtime, I/O schemas) so the run phase never consults the YAML again. `GenSpec`
carries the source command/args, the target workflow *name* (expansion is
run-time), and the item binding template.
`SelSpec` lists the coalesced candidates newest-first.

## Algorithm

```
Desugar(cfg, wf):
  checkWorkflowRefsAcyclic(cfg)              # DFS over use:->workflow references
  g := emit(cfg, wf, scope="<wf>", params=declared)
  canonicalize(g)                            # sort nodes/edges, fixed key order
  return g

emit(cfg, wf, scope, params):
  for step in wf.Steps:
    switch step form:
      use(template):  node(kind=agent, id=scope+"/"+step.ID)
                      bindings/edges from step.With  (expandBinding per entry)
      use(workflow):  node(kind=sub-workflow, sub=emit(cfg, target, scope+"/"+step.ID, ...))
                      with: entries become the sub-IR's param bindings
      generate:       node(kind=generate, gen={source, workflow, with-template})
      loop:           unrollLoop(step)
    when: -> CondSpec{cel, deps=stepRefsIn(cel)}
    depends_on -> ordering edges
```

### unrollLoop

```
unrollLoop(l, scope):
  for i in 1..l.Max:
    for s in l.Steps:
      inst := instantiate(s, suffix "@"+i)
      # rewrite in-body refs steps.X -> steps.X@i
      if i > 1:
        inst.When = AND(NOT(rewrite(l.Until, @i-1)), inst.When)
        ordering edge from every @i-1 body node to inst   # linear chain
  for each body step X referenced outside the loop or by Until:
    emit selector node scope+"/"+X (Sel = [X@Max .. X@1])
  selector failure rule: if X@Max executed and rewrite(Until,@Max) is false
    => loop exhausted => selector reports failed (consumed by failure module)
```

The gate condition `NOT(until@i-1)` is attached to *every* node of iteration i,
so a settled loop marks all later iterations skipped-by-condition without
scheduler special-casing. Skip propagation through the chain is ordinary
condition evaluation.

### expandBinding

```
expandBinding(slot, value):
  Literal        -> BindingDesc{kind: literal, value, type: yamlType(value)}
  ${params.p}    -> BindingDesc{kind: param, name: p}
  ${item.f}      -> BindingDesc{kind: item, field: f}     # generate scope only
  ${steps.X.f}   -> data Edge{from: resolveInScope(X), from_port: f, to: this, to_port: slot}
```

`resolveInScope` maps a step id to the current scope's instance (inside loop body
iteration i => `X@i`; after the loop => the selector node `X`).

## Determinism

- Iterate `Workflows`/`Templates` maps only through sorted key slices.
- Node IDs derive purely from scope paths; no counters shared across scopes.
- `canonicalize` sorts and emits via a fixed-order `MarshalJSON` on every IR type
  (hand-rolled field order, `json.Encoder` with `SetEscapeHTML(false)`).
- Golden test: desugaring `spec/test_reference_workflows.md`'s YAML twice, and
  across runs, yields byte-identical output.

## Size bounds

Unrolling multiplies loop bodies by Max: the reference task workflow (body of 2,
max 3) emits 6 body nodes + 2 selectors + implement + merge = 10 nodes. A guard
rejects configs whose unrolled node count exceeds a sanity ceiling (10_000) with
a clear error naming the offending loop, rather than desugaring unboundedly.
