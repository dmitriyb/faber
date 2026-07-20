# GenerateExpander — run-time fan-out over data-source items

## What it is

The component that turns a `generate` node into a subgraph while the run is
live. Expansion is deliberately run-time — the item set is data the engine
cannot know at validate time — but everything *except* the items is proven at
validate time: the target workflow's IR was independently desugared and
wiring-checked, and the item binding template was checked against the item
contract. Expansion therefore instantiates a proven graph; it never compiles or
validates workflow structure mid-run.

## The data-source contract

When the generate node becomes ready, the expander invokes the node's declared
data-source command (an opaque user command, run host-side via infra's
CommandRunner with the node's resolved args) and reads stdout as:

```json
{"items": [{"id": "I-1", "deps": ["I-0"], ...}]}
```

`id` (non-empty, unique in the set, and inside the closed identifier grammar
`[A-Za-z0-9][A-Za-z0-9_.-]*` — ids embed into instance node ids and into the
canonical `steps["…"]` CEL keys, so the namespacing characters, quotes, and
control bytes are contract violations) and `deps` (list of ids) are the
guaranteed fields; any other fields pass through for `${item.field}` binding. The engine
never learns what an item *is* — in the reference config it happens to be a
work-item ID, but that fact lives entirely in the user's command and params.

Contract enforcement, in order: command failure, unparseable JSON, a missing
`items` key, a duplicate, empty, or out-of-grammar `id`, an `${item.field}` the
binding template references that some item lacks, a dependency cycle within
the set, or an expansion over the node ceiling (items x per-instance nodes
bounded by the same 10000-node limit the desugarer's loop unrolling obeys;
the data source's stdout is itself read bounded) — each
fails the generate node with a structured data-source contract error naming
the offending item. Nothing has been launched; standard failure propagation
handles the node's dependents.

## Instantiation and splice

For each item, in stable input order:

1. Clone the target workflow's IR with every node ID prefixed
   `<generate-node-id>[<item-id>]` — e.g. `epic/tasks[I-3]/implement`,
   `epic/tasks[I-3]/review-cycle@2/fix` — keeping instance IDs path-like,
   collision-free, and legible in journal and report.
2. Bind the instance's params from the node's `with:` template — `${item.*}`
   entries become literals from this item's fields; `${params.*}` entries copy
   the enclosing workflow's resolved params.
3. Derive **inter-instance ordering edges** from `deps`: for each dep naming
   another item in the set, an ordering edge from every sink node of the dep's
   instance to every source node of this instance. Deps naming ids outside the
   set are ignored — the source may describe a wider world than this run.

The splice hands the scheduler the new nodes and edges in one atomic update:
in-degrees seeded, instance sources with no deps immediately ready. Any
original dependent of the generate node is rewired to also depend on every
instance's sinks, so "after the fan-out" means after every instance settles.
The generate node itself then settles `ok` with a payload recording the item
count and ids.

## Edge-case policy (first pass)

An **empty item set is a no-op**: the generate node settles `ok` with zero
instances and its dependents proceed. **Malformed output fails the generate
node** with the contract error above. **Cascade follows standard skip
semantics**: one item's failed instance marks its transitive dependents —
including dependent instances via the derived ordering edges — as
`skipped-dependency`; independent instances complete, and a partial fan-out is
an ordinary set of failure records, not a special run state.

## Deferred seam

The edge-case policies are reserved for a policy pass: a flag choosing whether
an empty set is a no-op or an error, a richer validation-error contract for
malformed sources, and a cascade policy (skip dependents only vs. abort the
whole fan-out). The first-pass behaviors above are the defaults that pass will
make configurable; the seam is the expander's policy struct, not the splice
mechanics.

Requirements implemented: Generate expansion, Reference workflow execution,
Deferred: generate edge cases.
