# Desugarer — the frontend-to-IR compiler

## What it is

A pure function `Desugar(*Config, workflowName) -> IR`. It performs four
transformations, in order, and nothing else: resolve reuse, unroll loops, expand
bindings into edges, and emit canonical JSON. All policy questions (is this
reference type-correct? is the graph acyclic?) are left to the WiringChecker —
the Desugarer is a mechanical translator that must be trivially predictable.

## The four transformations

1. **Reuse resolution.** A use-step naming a workflow becomes a `sub-workflow`
   node with the referenced workflow's graph desugared recursively and inlined,
   its params bound from the step's `with:`. A use-step naming a template becomes
   an `agent` node carrying the resolved template. Name collisions between
   templates and workflows are already rejected by the Loader. Recursion depth is
   bounded by the workflow-reference graph being acyclic (checked here — a
   workflow that transitively includes itself is a desugar-time error, since
   unbounded structures cannot unroll).

2. **Loop unrolling.** `loop {max: N, until: P, steps: B}` becomes N copies of
   the body, `B@1 .. B@N`, chained linearly:
   - Every node in `B@i` (i > 1) carries the gate condition `!P@(i-1)` conjoined
     with its own `when:` — a settled loop skips all later iterations.
   - `until: P` is evaluated against iteration i's own instances (`P@i` rewrites
     `steps.X.field` to `steps.X@i.field`).
   - In-body references to a body step resolve within the same iteration;
     references to pre-loop steps pass through unchanged.
   - After the chain, a **selector node** is emitted per referenced body step,
     coalescing `X@N .. X@1` so post-loop references read the final executed
     iteration (see IRModel).
   - Loop exhaustion semantics (all N gates ran, `P@N` false => the loop settles
     failed) are encoded as the selector's failure rule, consumed by the failure
     module's semantics — the Desugarer only wires it.

3. **Binding expansion.** Each `with:` entry becomes either a data edge
   (`${steps...}`), a param/item binding descriptor, or a literal. `depends_on`
   becomes ordering edges. `when:` strings are parsed to extract their step
   references (recorded as condition dependencies) but are carried as CEL source
   — compilation is the ConditionEvaluator's job at validate time.

4. **Canonical emission.** Deterministic node ordering and key ordering; see
   IRModel's serialization contract. `--emit-ir` prints exactly these bytes.

## Why unrolling (and not a Loop op)

Carried over from the design decision: a DAG with no loop operator keeps every
downstream consumer simple — the scheduler needs no back-edges, the journal needs
no iteration bookkeeping beyond the `@i` in the node id, resume needs no loop
state reconstruction, and validation is plain reachability. The cost — N copies
of a small body — is trivial at workflow scale (N is single digits; bodies are a
few nodes). `max` is required on every loop precisely so unrolling is always
finite.

## Determinism rules

- No maps iterated without sorting; no timestamps; no random ids.
- The only inputs are the `*Config` value and the workflow name; the output IR
  embeds everything downstream needs, so desugaring never happens twice per run.
- Byte-stable across faber versions within a major IR version; the IR carries
  `ir_version: 1`.

Requirements implemented: Desugaring to JSON IR, Deterministic IR emission.
