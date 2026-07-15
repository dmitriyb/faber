# Desugarer — the frontend-to-IR compiler

## What it is

A pure function `Desugar(*Config, workflowName) -> IR`. It performs five
transformations, in order, and nothing else: resolve template references, resolve
reuse, unroll loops, expand bindings into edges, and emit canonical JSON. All
policy questions (is this reference type-correct? is the graph acyclic?) are left
to the WiringChecker — the Desugarer is a mechanical translator that must be
trivially predictable. The `*Config` it receives is already assembled (includes
merged, paths absolute) and Loader-validated, so every named reference is known to
resolve; desugar performs the resolution, not the checking.

## The five transformations

0. **Template reference resolution (dual-mode → `ResolvedTemplate`).** Each
   `TemplateDef` is resolved into the existing `ResolvedTemplate` shape by looking
   up its references in the assembled libraries, so the IR the executor consumes
   is unchanged. Both the named and inline forms collapse to the same result:

   - **image**: `image: <name>` → `images[<name>]` → `BuildDef{packages, overlay,
     pin}`; inline `build:` → the same `BuildDef` directly. The shared `ResolveBuild`
     (see `impl_desugaring.md`) does the projection; because `ImageDef` and `BuildDef`
     are distinct types, its named branch **copies `img.Pin → BuildDef.Pin`**
     explicitly. The pin then lands on the **flat** `ResolvedTemplate.Pin` field
     (`*PinDef`, json `pin,omitempty`) — there is no `ResolvedTemplate.Build`
     sub-struct, so it is `ResolvedTemplate.Pin`, never `ResolvedTemplate.Build.Pin`.
     Desugar is mechanical and does NOT substitute the engine default; the default is
     resolved later, in infra, per build. Because that field is `omitempty` and nil
     when the toolset declares no pin, it serializes to nothing — that is precisely
     what makes the IR of a pin-less toolset byte-for-byte what it is today. Only a
     single flat omitempty carrier makes the "absent ⇒ byte-identical IR" argument
     close.
   - **hooks**: for each of `context`/`prelude`/`on_failure`, a bare name →
     `hooks[<name>].path`; a path form → that path verbatim. Both yield the
     resolved absolute hook path already carried today.
   - **identity**: top-level `identity:` or `run.identity:` → the identity name
     (unchanged string field).
   - **skills** (named form): the named list `skills: [n1, n2, …]` → the ordered,
     name-deduped source set `[(n → skills[n].dir)]`, with the primary `skill` and
     `skills_link`. Each source dir is *one skill's* `SKILL.md` tree; the run-prep
     stager (see `spec/pipeline/impl_scheduling.md`, "Skills staging") composes
     them into the single `/faber/skills` tree by **copying** each source into
     `<stage>/<name>` as real, world-readable files — not a symlink farm, whose
     targets would be host paths that dangle across the read-only bind into the
     container. In the named form the Loader guarantees the primary `skill` is a
     member of the set.
   - **skills** (inline form): `skills: {dir, link}` → a *direct* skills-root
     `{root: dir, link}` (NOT a wrapped one-entry set). Here `dir` already is a
     root of `<name>/SKILL.md` subtrees, so the stager mounts it directly at
     `/faber/skills` with no `<name>` wrapper — byte-identical to today. The
     primary `skill` in the inline form is a free-form prompt token, not a
     library reference, so no set-membership check applies.
   - **skills** (absent): no skills leg (current default).

   See `arch_schema_types.md` "Skills assembly" for the two shapes and the
   direct-mount special case.

   Everything else on the template (resources, runtime, env, volumes, inputs,
   output) passes through unchanged. Desugar reads no files — the paths are already
   absolute from assembly; it only rearranges resolved values into the IR struct.

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

## IR stability under the config redesign (reviewer note)

The library/dual-mode redesign is confined to the *config surface and its
resolution*. Transformation 0 resolves every new reference into the existing
`ResolvedTemplate`, so the image spec, hook paths, identity, resources, runtime,
env, volumes, and I/O schemas the executor sees are byte-for-byte what they were.
The image, hooks, and identity aspects require **zero** IR change.

The **one** shape delta is the source side of the skills leg: today's single
`ResolvedTemplate.Skills{dir, link}` widens to the resolved
`{sources: [(name, dir)…], root, primary, link}` (named form populates `sources`,
inline form populates `root` — exactly one). This lets the run-prep stager compose
N named skill dirs into the one `/faber/skills` tree.

What is byte-stable and what changes, stated plainly:

- **infra's argv builder is byte-stable** — it still emits a single read-only
  `/faber/skills` bind mount from one host path; it never learns there are N
  sources.
- **the agent box's symlink phase is byte-stable** — it still sees one
  `/faber/skills` tree of `<name>/SKILL.md` entries and symlinks
  `$HOME/<SkillsLink> → /faber/skills` from `FABER_SKILLS_LINK`.
- **the pipeline run-prep seam changes** — the scheduler `steps` closure (the
  `infra.RunSpec` assembly seam) now reads `ResolvedSkills.Sources`, builds a
  per-attempt staging tree by **copying** each source into `<stage>/<name>` (real,
  world-readable files — a symlink farm would dangle across the read-only bind into
  the container), and sets that single stage dir as the one `Mount{Host}` for
  `/faber/skills`. For the
  inline `root` form it mounts the root directly (no staging, byte-identical to
  today). This stager is specified in `spec/pipeline/impl_scheduling.md`
  ("Skills staging").

The image, hooks, and identity aspects require **zero** change anywhere. The
decision is settled: option **(a)** — a single staged mount that preserves the
`/faber/skills` mount contract and leaves infra untouched. (The rejected
alternative of mounting each source at `/faber/skills/<name>`, which would have
changed infra's argv builder to emit N mounts, is not adopted.) No other part of
the IR changes.

## Determinism rules

- No maps iterated without sorting; no timestamps; no random ids. The skills
  source set is emitted in the template's declared list order (deduped), not map
  order, so it is deterministic.
- The only inputs are the `*Config` value and the workflow name; the output IR
  embeds everything downstream needs, so desugaring never happens twice per run.
- Byte-stable across faber versions within a major IR version; the IR carries
  `ir_version: 1`.

Requirements implemented: Desugaring to JSON IR, Deterministic IR emission.
