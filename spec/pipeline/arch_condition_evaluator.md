# ConditionEvaluator — CEL compilation and skip decisions

## What it is

The one place a `when:` (and a desugared loop gate or selector-exhaustion
condition — the same `CondSpec` shape) is compiled and evaluated. It has two
lives: at **validate time** it is the `CompileCondition` entry point the config
module's WiringChecker calls to prove every expression compiles against the
schemas it references; at **run time** it answers the scheduler's first
per-node question — run or skip.

## Compilation (validate time)

`CompileCondition(expr)` builds a cel-go environment declaring the condition
variables (`steps` and `params` as dyn maps, `item`) and compiles the
expression. The WiringChecker then walks the checked AST (config's
`checkCondRefs`) and enforces the canonical reference discipline: every use
of `steps` must be exactly the desugarer's rewritten
`steps["<node-id>"].<field>` form with the id in the condition's derived dep
set (a hand-authored index compiles but carries no edge, so it is rejected);
every use of `params` must name a param the enclosing scope declares; `item`
never appears (the run-time activation binds only steps and params). A
separate pass over the canonical form checks each referenced field against
the dep's declared output schema (field exists, with a did-you-mean hint).
Referenced steps must be predecessors of the condition's node — the Desugarer
guarantees this by construction (a `when:` reference *is* an edge), so by the
time an expression evaluates, every step it reads has settled. Compiled
programs are retained keyed by node ID; nothing is compiled mid-run. Result
typing stays a run-time check: the maps are dyn-typed, so a non-bool result
surfaces as a structured condition failure at evaluation, not at compile.

## The activation (run time)

At readiness the scheduler asks `Evaluate(node, state)`. The evaluator builds
the activation from the node's `CondSpec.Deps` — the node IDs the expression
reads, mapped back to their source-level names (inside iteration *i*,
`steps.review` resolved to `review@i` at desugar time; after the loop it
resolved to the selector node):

```
{
  steps:  { "<name>": { "<field>": <value>, ... }, ... },   // settled payloads
  params: { "<name>": <resolved value>, ... }
}
```

Payloads come from completed result records (in-memory or journal-adopted —
identical by construction). A missing entry is impossible: conditions only read
predecessors, and predecessors have settled before the node is ready.

## Skip decisions and propagation

- Expression evaluates `false` ⇒ the node settles **`skipped-condition`** — a
  distinct terminal state, never a failure.
- **Skipped dependency short-circuit**: if any step the expression reads
  settled as skipped (either flavor), the condition is `false` without
  evaluating CEL — a skipped step has no payload, and skip propagates. This
  single rule is what terminates a settled loop early: iteration *i*'s gate
  `!(until@i-1)` reads `review@i-1`; once one iteration's gate goes false, every
  later iteration's gate short-circuits false in turn, and the whole tail
  settles `skipped-condition` with no scheduler special-casing.
- A `failed` dependency never reaches the evaluator — failure propagation marks
  the node `skipped-dependency` before it becomes ready.

Evaluation is pure and instantaneous (host-side, no I/O), so it runs on the
scheduler's dispatch path before any journal, meter, or container work.

## Boundaries

The evaluator owns no scheduling and no state: it receives settled payloads and
returns a boolean (or the short-circuit). It never sees YAML — expressions
arrive as `CondSpec{cel, deps}` in the IR — and it never invents defaults: an
evaluation error against a well-typed activation is a bug surfaced as a node
failure, not silently treated as false. cel-go is one of the three permitted
external dependencies; the environment enables no custom functions, no macros
beyond the defaults, and no access beyond the two declared variables.

Requirement implemented: Conditional steps via CEL.
