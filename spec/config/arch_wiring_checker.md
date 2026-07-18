# WiringChecker — typed dataflow validation over the IR

## What it is

The gate between "parses" and "will run": every wiring, type, and structure error
a run could hit is surfaced here, at `faber validate`, against the desugared IR.
The design's central promise — a reference *is* the edge — is only safe because
this component checks every edge before anything executes.

### Division of labor with the Loader (library references)

The config redesign's library cross-references — `template.image ∈ images`,
`template.hooks.* ∈ hooks`, `template.identity ∈ identities`, and (in named-skills
mode only) `template.skills[*] ∈ skills` with `template.skill ∈ template.skills` —
plus dual-mode exclusivity, are **name-level** checks resolvable against the
assembled `Config` alone, so they belong to the Loader (see `arch_loader.md`) and
run *before* desugaring. The optional per-image nixpkgs pin's both-fields-or-neither
completeness (`images.<name>.pin` / inline `build.pin` requires both `rev` and
`sha256`), its fully-empty-`{}`→absent normalization, and its per-field charset
validation (`rev`/`sha256` restricted to the splice-safe charset, since they are
user-supplied splice material) are likewise Loader schema checks — within-node
field rules like inline-skills pairing, resolvable against the assembled `Config` —
not dataflow checks, so the WiringChecker never sees them. The duplicate-name-across-includes and substrate-placement
violations are recorded at Assemble and merged into the Loader's collected report;
an include cycle (or unreadable file) hard-stops Assemble before any of this. (When
`skills` is inline or absent, `template.skill` is a free-form prompt token, checked
by no one.) By the time the IR exists, every `TemplateDef` has been
resolved into a `ResolvedTemplate` with no dangling names left. The WiringChecker
therefore operates on already-resolved templates and keeps its existing job: the
*typed dataflow* over the IR (`step.use` edges, slot/type compatibility, cycles
over data+ordering edges, conditions, tool-subset). The two together are the
"resolve + check every cross-reference" contract; nothing about the checks below
changed.

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
