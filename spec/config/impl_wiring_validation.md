# Implementation: Wiring validation algorithm

Covers WiringChecker.

## Entry point (internal/config/wiring.go)

```go
func CheckWiring(ir *IR, cfg *Config) error          // multi-error, IR-path diagnostics
func CheckRunParams(wf WorkflowDef, supplied map[string]string) (Params, error)
```

`CheckWiring` runs at `faber validate` (and again, cheaply, at `run` entry since
`run` embeds validate). `CheckRunParams` runs only at `run`/`resume` entry.

The library cross-references introduced by the config redesign
(`image`/`skill`/`skills`/`hooks`/`identity` name resolution, duplicate-name and
dual-mode exclusivity, include cycles) are resolved earlier, at Loader/assembly
time, so `CheckWiring` sees a fully-resolved IR and needs no new pass. Its input
`*Config` is the assembled config; `outputs`/`inputs` are read from the already
resolved `ResolvedTemplate`s carried on the nodes, so the tool-subset check reads
`template.Build.Packages` regardless of whether the toolset arrived via `image:`
or inline `build:`.

## Pass structure

One index-building pass, then independent check passes appending to a shared
violation list:

```
index := build:
  byID    map[nodeID]*Node
  outputs map[nodeID]map[field]FieldDef   # selector nodes proxy their target's schema
  inputs  map[nodeID]map[slot]ParamDef
  inbound map[nodeID]map[slot][]Edge

pass 1 — resolution:
  every edge: From node exists; FromPort ∈ outputs[From]
              To node exists;   ToPort ∈ inputs[To]
  near-miss: levenshtein ≤ 2 against declared fields -> "did you mean X?"

pass 2 — slot discipline:
  for each node, each required input slot: exactly one of
  (inbound edge | binding descriptor | declared default); duplicates and
  unknown-slot bindings are violations

pass 3 — types:
  edge:    outputs[From][FromPort].Type must equal inputs[To][ToPort].Type
           enum source satisfies plain string slot; plain string does NOT
           satisfy enum slot; no other coercions
  param:   cfg param decl type vs slot type
  item:    source item contract type (id: string, deps: []string, else any)
  literal: yamlType(value) vs slot type

pass 4 — acyclicity (Kahn):
  in-degree over data+ordering edges; on leftover nodes, DFS extracts one
  concrete cycle and reports it as "a -> b -> c -> a"

pass 5 — conditions:
  for each CondSpec: every dep node exists and is not a descendant of the
  condition's own node (a condition may only read completed predecessors);
  CEL compilation delegated to pipeline.CompileCondition with an environment
  typed from the deps' output schemas — compile errors are violations here

pass 6 — tool subset:
  for each agent node with declared tool needs: needs ⊆ template.Build.Packages
  (set difference reported)
```

## Generate nodes

A generate node is checked as a boundary, not expanded (expansion is run-time):

- its `source` exists and its `workflow` exists (Loader already guaranteed
  name-level existence; here the target workflow's params are checked against
  the generate's `with:` template — every required param bound from a legal
  source, `${item.*}` permitted);
- the target workflow's own IR was desugared and wiring-checked independently
  (every named workflow is validated whether or not it is an entry point).

This is what makes run-time expansion safe: an instance created later binds only
`${item.*}` values into a graph whose internal wiring was already proven.

## Violation type

```go
type WiringError struct {
    Node string // IR node id (or "params" for run-entry errors)
    Path string // e.g. "with.pr" / "when" / "output.verdict"
    Msg  string
}
```

Sorted by (Node, Path) before joining, so output order is stable for tests and
users. The acceptance scenario "Broken wiring caught at validate" pins four
concrete violation classes: missing required param, undeclared output field,
slot type mismatch, reference cycle.
