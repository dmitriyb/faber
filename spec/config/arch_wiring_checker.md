# WiringChecker — typed dataflow validation over the IR

## What it is

The gate between "parses" and "will run": every wiring, type, and structure error
a run could hit is surfaced here, at `faber validate`, against the desugared IR.
The design's central promise — a reference *is* the edge — is only safe because
this component checks every edge before anything executes.

## Checks

Run in order, all violations collected:

1. **Reference resolution.** Every data edge's `from` names an existing node and
   a field declared in that node's output schema. Selector nodes expose their
   underlying step's schema. Generate item bindings (`${item.field}`) are checked
   against the data-source item contract (`id` and `deps` guaranteed; other
   fields pass-through typed as declared by the source, or `any` if undeclared).

2. **Slot discipline.** Every required input slot of every node is bound exactly
   once; no binding targets an undeclared slot; no two bindings target the same
   slot. Optional slots with defaults may be unbound.

3. **Type compatibility.** The bound source type must equal the slot type
   (string/int/bool/object, plus enum-narrowing: an enum field satisfies a string
   slot, but a string source does not satisfy an enum slot). Literals are typed
   by YAML tag. Params are typed by their declaration. There are no implicit
   conversions.

4. **Acyclicity.** Kahn's algorithm over data + ordering edges; on failure the
   cycle path is reported node-by-node (`task/a -> task/b -> task/a`). The IR
   must already be acyclic if the Desugarer is correct — this check is
   defense-in-depth and catches hand-authored IR.

5. **Condition sanity.** Every `when:`/`until:` CEL expression compiles in an
   environment typed from the step results it references (delegated to the
   pipeline module's ConditionEvaluator compilation entry point); referenced
   steps must precede the condition's node.

6. **Tool-subset rule.** A step's declared tool needs (if the step declares any)
   must be a subset of its template's `build.packages`. Validation is
   one-directional by design: the image is built *from* the list, so the list is
   ground truth; the check only proves no step assumes a tool its box will not
   have.

7. **Param completeness (run entry).** At `faber run`, the supplied `--param`
   bindings are checked against the workflow's params interface: every required
   param present, every value type-correct, no unknown names. Missing required
   params are a hard error — there is no implicit repo or any other fallback.

## Error contract

Same field-path discipline as the Loader, but paths address IR nodes:
`task/review-cycle@2/fix.with.pr: references steps.implement.prs — output field
does not exist (did you mean pr?)`. Near-miss suggestions are included when the
edit distance is 1–2; the acceptance scenario "Broken wiring caught at validate"
asserts the exact failure classes: missing required param, undeclared output
field, slot type mismatch, reference cycle.

Requirement implemented: Reference wiring validation.
