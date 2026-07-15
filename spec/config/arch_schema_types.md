# SchemaTypes — the orchestrator.yaml struct tree

## What it is

The Go type definitions that mirror `orchestrator.yaml` 1:1. Every other module
receives its configuration through these types; nothing else in the engine parses
YAML. The schema is the whole policy surface of faber — if a behavior cannot be
expressed here, it is either mechanism (faber's job) or it does not exist.

## Top-level shape

A `Config` is composed from a **project assembly** (the substrate) plus five
**named component libraries**. The project file carries the substrate and an
`include:` list; each included file contributes library entries. All included
files are merged into one flat `Config` before anything else runs (see
`arch_loader.md` for the assembly algorithm).

```
Config
├── Version      int
├── Include      []string          NEW: partial-config files to pull in (paths relative to THIS file)
│                                       — substrate (below) is honored only in the root project file
├── Network      NetworkDef        workflow-level binding (name, proxy, no_proxy)   ┐
├── Remote       RemoteDef         gateway URL prefix + host-key policy             │ substrate
├── Credentials  CredentialsDef    resolver command + per-service handle modes      │ (project
├── Identities   map[string]IdentityDef    role name -> key material source         │  assembly)
├── Images       map[string]ImageDef       NEW: named pure Nix toolsets            ┐┘
├── Skills       map[string]SkillDef       NEW: named skill definitions            │ five named
├── Hooks        map[string]HookDef        NEW: named hook executables             │ libraries
├── Templates    map[string]TemplateDef    the boxes — composition nodes           │ (union-merged
└── Workflows    map[string]WorkflowDef    the DAGs                                 ┘  across includes)
```

### The five libraries

```
ImageDef                      # a pure Nix toolset — exactly today's TemplateDef.Build
├── Packages []string         pinned Nix package names — the toolset IS the env
└── Overlay  string           optional path to a user overlay of derivations

SkillDef                      # a skill definition (a SKILL.md tree)
└── Dir      string           host dir of ONE skill's tree — SKILL.md sits at its root

HookDef                       # a hook executable
└── Path     string           opaque host script path
```

`Images`, `Skills`, and `Hooks` are keyed by name; templates reference them by
name. A library entry is inert data — faber never reads a `Dir`/`Path`/`Overlay`
at load time; they are opaque paths resolved relative to the file that declared
them (see "Path resolution", below).

### TemplateDef — a composition node

A template no longer inlines its toolset; it *references* library entries by name
and adds the per-box runtime knobs and the typed I/O contract. Every reference
resolves at desugar time into the existing `ResolvedTemplate` IR (see
`arch_desugarer.md` / `arch_ir_model.md`) — the resolved shape handed downstream
is unchanged.

```
TemplateDef  (named-reference form)
├── Image     string               ref → Images        (the box's toolset)
├── Identity  string               ref → Identities    (one key per box)
├── Skill     string               ref → Skills        the PRIMARY skill the box activates (/<skill>)
├── Skills    []string             refs → Skills       every skill delivered into the box (superset incl. Skill)
├── SkillsLink string              in-box $HOME-relative discovery path (was skills.link); required when Skills is non-empty
├── Hooks     HookSet              {context, prelude, on_failure} — each a ref → Hooks
├── Run       RunDef               resources, runtime, env, volumes (+ Identity, back-compat)
├── Inputs    map[string]ParamDef  typed slots
└── Output    map[string]FieldDef  typed output schema (validated at the boundary)
```

`Skill` names the leading `/<skill>` of the agent prompt. Its meaning depends on
which `skills` mode the template uses, and the two modes are the *only* place the
"`skill ∈ skills`" rule applies:

- **Named-skills mode** (`skills: [<name>…]`, a library reference list): `Skill`
  is *also* a library reference. The Loader requires every list entry `∈ Skills`
  and requires the primary `Skill` to be one of the listed names (a template that
  activates a skill must also deliver it). `Skills` is then the set of skill
  *definitions* delivered into the box's `/faber/skills` tree.
- **Inline mode** (`skills: {dir, link}`) **or no `skills` at all**: `Skill` is a
  **free-form prompt token** — the literal `/<skill>` the agent reads — NOT a
  library reference. There is no `skill ∈ skills` check and no library-existence
  check on it; this is exactly today's behavior. (Every current config sets
  `skill:` with no `skills:` library, and must keep validating.)

`SkillsLink` is agent-specific and opaque to faber: a claude box sets it to
`.claude/skills`, a different agent sets whatever it reads; faber never learns
`.claude`. See "Skills assembly", below, for how N named skills become the one
`/faber/skills` tree and how the inline form maps to a single direct mount.

### Dual-mode: inline forms remain valid (back-compat)

A template may express the `image`, `hooks`, and `skills` aspects EITHER by named
reference (above) OR with the current inline forms. Desugar resolves both to the
same `ResolvedTemplate`, so the smoke config and every `examples/` config keep
working unchanged.

```
TemplateDef  (inline form — current schema, still accepted)
├── Build     *BuildDef            inline {packages, overlay}     (instead of Image)
├── Hooks     HookSet              values are PATHS (instead of hook names)
├── Skills    *SkillsDef           inline {dir, link}             (instead of a name list + SkillsLink)
│                                    dir = a skills-ROOT holding <name>/SKILL.md subtrees (see below)
├── Skill     string               the primary skill token (a free-form /<skill>, not a ref, in this mode)
├── Run.Identity string            identity (instead of top-level Identity)
├── Inputs / Output                unchanged
```

Exclusivity — a template may not mix inline and named for the *same aspect*
(the Loader reports a field-pathed violation):

| Aspect | Named form | Inline form | Conflict error |
|--------|-----------|-------------|----------------|
| image | `image: <name>` | `build: {packages, overlay}` | both `image:` and `build:` set |
| skills | `skills: [<name>…]` (+ `skills_link`) | `skills: {dir, link}` | `skills` list *and* mapping — impossible in one node; `skills_link` set alongside inline `{dir,link}`; **`skills_link` set with an empty/absent `skills` leg** (a dangling discovery path with nothing to deliver) |
| hooks (per field) | bare name → Hooks | path (contains `/`, or begins `.`/`~`/`/`) | a bare name that resolves to no hook is a dangling-ref error, never a silent path |
| identity | top-level `identity: <name>` | `run.identity: <name>` | both set |

**Spelling asymmetry (intentional, verdict: correct).** The named form carries
the discovery path as the top-level template field `skills_link`, while the inline
form carries it as `skills.link` (a member of the `{dir, link}` mapping). This
asymmetry is deliberate — the named `skills:` value is a bare sequence with no room
for a sibling `link`, so it lives on the template; keep `skills_link` on the
template. Both spellings mean the same in-box `$HOME`-relative discovery path.

Disambiguation is by YAML node kind and lexical form, so it is deterministic:

- **image**: `image:` (a scalar) vs `build:` (a mapping) are distinct keys.
- **skills**: the `skills:` value is a *sequence* ⇒ named form; a *mapping*
  ⇒ inline `{dir, link}` form. One key, two node kinds, no ambiguity.
- **hooks**: each `hooks.{context,prelude,on_failure}` value is a **path** iff it
  contains a path separator `/` or begins with `.`, `~`, or `/`; otherwise it is a
  **hook name** resolved against the `Hooks` library. This path test is
  POSIX-oriented (it keys on `/`, `.`, `~`), which matches faber's Linux-container
  host convention. Existing configs use `./hooks/…` paths and are therefore always
  read as inline paths — unaffected.
- **identity**: top-level `identity:` is an alias for `run.identity:`; at most one
  may be set.

### Path resolution (supersedes the old CWD-relative rule)

`include:` paths AND **every declared file path** inside any config resolve
**relative to the file that declares them**, not the process CWD. The full list is:
image `overlay`, skill `dir`, hook `path` (and the inline `build.overlay`,
`skills.dir`, `hooks.*` path forms), the substrate paths `identities.*.key`
(e.g. `./keys/…`) and `credentials.resolver` and `remote.host_key_file` — so the
rule is uniform: *every* file path a config names is declarer-relative, with no
carve-outs. (Substrate lives in the root file, so the identity/credential paths are
declarer-relative to the root; the rule is stated uniformly so no reader has to
special-case them.) The Loader rewrites each to an absolute path at load time, so
the merged `Config` (and therefore every `ResolvedTemplate`) carries absolute
paths and downstream is unaffected in shape. This fixes the multi-file-composition gotcha and is the one
rule that makes `include:` sane. For a single-file config run from its own
directory the result is byte-identical to today's CWD-relative behavior, so
existing configs are unaffected. This **supersedes** the prior "CWD-relative,
resolved like the hook/overlay paths" rule stated for `skills.dir` and the hook
paths.

### Composition — the `include:` directive

`include: [<path>, …]` at the top level pulls in partial-config files. Each
included file is itself a `Config` fragment that may contribute any of the five
library maps (`images`/`skills`/`hooks`/`templates`/`workflows`). Assembly is the
**union** of the named maps across the root file and all included files.

- **Substrate is root-only** (reviewer decision iii — *kept*). `version`/`network`/
  `remote`/`credentials`/`identities` are honored only in the root project file; an
  included file that sets a substrate key is a violation, **recorded during
  Assemble with file provenance and surfaced through the collected `Validate`
  report** (an included file is a library, not a second project). *Rationale:* of
  the vocabularies, `identities` (unlike images/skills/hooks/templates/workflows)
  is locked to the root — identity definitions therefore cannot be modularized into
  a library file, so a template-library file that references an identity depends on
  the root project supplying it. Keeping the whole substrate root-only makes that
  dependency direction uniform and unambiguous.
- **Duplicate name across files is an error** — no silent last-wins. If two
  distinct files both define `templates.review` (or any same-key entry in the same
  library), it is **recorded during Assemble with both file paths and surfaced
  through the collected `Validate` report**, so assembly is deterministic and
  order-independent. (Duplicate keys need per-file provenance the merged `Config`
  no longer carries, which is why they are caught at Assemble, not Validate — see
  `arch_loader.md`.)
- **Nesting + cycles.** An included file may itself `include:`; resolution is
  transitive. Each file is identified by its resolved absolute path: a file
  re-encountered on the current include stack is a **cycle error** and one of the
  two Assemble **hard stops** (a cycle cannot yield a `Config`, so it cannot be
  collected), naming the path chain; a file reached twice by distinct paths (a
  diamond) is merged **once**, not flagged as a duplicate.
- **Paths are declarer-relative** (see "Path resolution").

### Skills assembly

This is the one genuinely-new delivery mechanism. It has two shapes that must not
be conflated, because `SkillDef.dir` and inline `skills.dir` are **different
directory levels**:

- **`SkillDef.dir`** (named-form source) is *one* skill's tree — `SKILL.md` sits
  at its root, with no `<name>/` wrapper.
- **inline `skills.dir`** is a **skills-root** — a directory whose children are the
  `<name>/SKILL.md` subtrees. It is already the exact shape `/faber/skills` expects.

Both resolve to the single read-only `/faber/skills` mount, but they get there
differently:

- **Named form** (`skills: [implement, go-expert]`): each name resolves via the
  `Skills` library to a single-skill `Dir`. Desugar records the ordered,
  name-deduped source set `[(name → Dir), …]` on `ResolvedSkills.Sources`, plus the
  primary `Skill` and `SkillsLink`. The **pipeline run-prep stager** (see
  `spec/pipeline/impl_scheduling.md`, "Skills staging") then composes those N
  single-skill dirs into one mountable tree by **copying** each source into
  `<stage>/<name>` as real, world-readable files (not a symlink farm — a symlink
  target would dangle across the read-only bind into the container) and passes
  `<stage>` as the single `/faber/skills` bind.
- **Inline form** (`skills: {dir, link}`): Desugar records the dir **directly** on
  `ResolvedSkills.Root` (NOT a wrapped one-entry `Sources`), because `dir` already
  is a `<name>/SKILL.md` root. The stager mounts that root at `/faber/skills` with
  **no `<name>` wrapper** — byte-identical to today's single-dir mount. Wrapping it
  as `<stage>/<name> → dir` would introduce a spurious extra level
  (`/faber/skills/summarize/summarize/SKILL.md`) and is explicitly *not* done.

In both cases the box sees one `/faber/skills` tree of `<name>/SKILL.md` entries,
bind-mounted read-only at the fixed neutral path (`contract.ContainerSkillsDir`),
and still symlinks `$HOME/<SkillsLink> → /faber/skills` from `FABER_SKILLS_LINK`.
The mount path, the read-only property, and the agent's symlink phase are all
**unchanged**; only the *host-side staging* is new, and only for the named form.

The only IR shape delta versus today is on the *source side* of the skills leg (a
single `{dir, link}` widens to `{sources | root, primary, link}`, exactly one of
`sources`/`root` populated). The mounted contract, infra's argv builder, and the
agent box are all byte-stable; the pipeline run-prep seam is the one thing that
changes. See the reviewer note in `arch_desugarer.md` and the unified
`SkillsRef{Names, Inline}` struct in `impl_schema_structs.md` — this section's
two-variant view (named `Sources` vs inline `Root`) is the *resolved* projection of
that single unmarshal-time `SkillsRef`.

```
WorkflowDef
├── Params   map[string]ParamDef   the typed params interface
├── Sources  map[string]SourceDef  data-source commands for generate
└── Steps    []StepDef             ordered list; order is documentation, edges are truth
```

`StepDef` is a tagged union with exactly three mutually exclusive forms:

| Form | Fields | Desugars to |
|------|--------|-------------|
| use-step | `id`, `use` (template or workflow name), `with`, `when`, `depends_on`, `retry`, `on_failure` | one `agent` or `sub-workflow` IR node |
| loop-step | `id`, `loop {max, until, steps}` | an unrolled conditional chain (see Desugarer) |
| generate-step | `id`, `generate {source, workflow, with}` | one `generate` IR node |

## The params typing vocabulary

`ParamDef = {type: string|int|bool|object, required: bool, default: any, enum: []}`.
The same vocabulary types workflow params, template inputs, and template output
fields — one type system end to end, so a `${steps.X.field}` reference can be
checked against a slot without conversion rules.

`repo` is deliberately not special: it is an ordinary optional param that templates
consume as an input slot. It is never baked into an image, and a step-level `with:`
can override it for multi-repo runs.

## The closed set of binding sources

A `with:` value is either a literal or one interpolated reference. References have
exactly four roots:

- `${params.name}` — a workflow param
- `${steps.id.field}` — a prior step's output field (**this is the DAG edge**)
- `${item.field}` — the current generate item (only inside a generate's `with:`)
- `${sources.name}` — a declared data source (only as a generate's `source:`)

Nothing else interpolates. There is no environment access, no host filesystem
access, no arbitrary expression in a binding — conditions (`when:`, `until:`) are
CEL and live in their own fields.

## Design rationale

- **Maps keyed by name, not lists with name fields**, for every library
  (images/skills/hooks/templates/workflows) and identities: names are identity,
  duplicates within one file are impossible by construction, and `$ref`-style
  reuse (`image: base`, `use: task`) is a map lookup. Duplicates *across* included
  files are the deterministic assembly error above.
- **Libraries, not inlining.** Splitting the pure toolset (`images`), the skill
  definitions (`skills`), and the hook executables (`hooks`) out of the template
  makes each reusable across templates and composable across files, while the
  template becomes a thin composition node. Dual-mode keeps the old inline forms
  valid so the change is additive, never a migration.
- **No domain fields.** The reference workflows (see `spec/test_reference_workflows.md`)
  express claim/review/merge purely through hook paths, skill names, and param
  names. Reviewers of schema changes should reject any field whose name encodes a
  tracker, gate, or spec-tool concept (see project requirement "Mechanism, not policy").
- **yaml.v3 struct tags only**; custom `UnmarshalYAML` is reserved for the StepDef
  union, the compact `with:` forms, and the `skills:` sequence-or-mapping
  disambiguation — everything else round-trips mechanically.

Requirements implemented: Workflow schema definition, Typed params interface,
Opaque policy seams.
