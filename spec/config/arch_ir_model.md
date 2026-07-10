# IRModel — the acyclic JSON intermediate representation

## What it is

The engine's backend contract: a uniform, acyclic, fully explicit graph that the
executor consumes. YAML is for people; the IR is for the machine. Everything the
frontend makes convenient (loops, references, compact bindings, workflow reuse)
the IR makes explicit (unrolled chains, typed edges, resolved templates).

## Node kinds

Exactly three in the first pass, discriminated by `kind`:

| kind | Meaning | Payload |
|------|---------|---------|
| `agent` | run one box | template ref, resolved input bindings, condition, retry/on_failure policy |
| `sub-workflow` | run a nested IR graph | the nested graph (inlined at desugar time), param bindings |
| `generate` | fan a named sub-workflow out over a data source at run time | source ref, sub-workflow name (kept by reference — expansion is run-time), item binding template |

The `kind` field is an open discriminator by design: the deferred non-agent step
kinds (human-approval, wait, pure-command — design edge case 1) will be new kinds
with the same port/edge grammar, requiring no change to edge encoding or
scheduling. That reservation is this component's contribution to the backlog
requirement; no such kind ships now.

There is deliberately **no Loop kind**: bounded loops exist only in the frontend
and arrive here already unrolled. An IR containing a cycle is invalid, full stop.

## Ports and edges

Every node declares typed input ports (from its template's `inputs`) and output
ports (from `output`). An edge is a triple:

```
{from: {node, port}, to: {node, port}}     data edge — carries a typed value
{from: node, to: node, order: true}        ordering edge — carries nothing
```

Data edges are *derived from references*, never authored: `${steps.X.field}` in
the YAML becomes `{from: {X, field}, to: {this, slot}}`. Ordering edges come only
from `depends_on`. The scheduler treats both identically for readiness; the
threading machinery reads values only over data edges.

Bindings that are not edges (literals, `${params.*}`, `${item.*}`) are stored on
the node as resolved-at-instantiation binding descriptors.

## Conditions and the post-loop selector

A node's `when:` is carried as a compiled-checkable CEL string plus the set of
step results it mentions (extracted at desugar time so the WiringChecker can
verify the references and the scheduler knows the condition's dependencies).

Loop unrolling introduces one synthetic node form the frontend cannot write: the
**selector** — an alias node whose output ports mirror a loop-body step's outputs
and whose value resolves, at run time, to the newest executed iteration's result
(a coalesce chain over `step@N .. step@1`). Post-loop references like
`${steps.review.verdict}` compile to an edge from the selector. The selector is
pure wiring — it runs no container.

## Serialization contract

- Canonical JSON: nodes sorted by id, keys emitted in fixed order, no
  insignificant whitespace variance — the same YAML must produce byte-identical
  IR (project requirement "Deterministic desugaring"; asserted by golden-file
  tests against the reference workflows).
- Node ids are path-like and human-readable: `task/implement`,
  `task/review-cycle@2/fix`, `epic/tasks` — iteration instances carry `@i`.
  Stable ids are what make the journal key (step-id, input-hash) meaningful
  across resumes.
- The IR embeds the resolved template definitions it needs (image spec, hooks,
  identity, I/O schemas) so the executor never re-reads the YAML.

Requirements implemented: Desugaring to JSON IR, Deferred: non-agent step kinds.
