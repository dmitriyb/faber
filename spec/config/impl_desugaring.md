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
overlay hash + the optional per-toolset nixpkgs pin, resolved hook paths, the
optional skills leg, identity, resources, runtime, I/O schemas) so the run phase never consults the YAML again — and its
shape is preserved by the config redesign (see arch_desugarer.md "IR stability").

`ResolvedTemplate` is **flat** — there is no `Build` sub-struct — so the toolset
lives directly as `Packages []string`, `Overlay string`, and now the nixpkgs pin
as `Pin *PinDef` (json `pin,omitempty`). The pin is therefore `ResolvedTemplate.Pin`,
never `ResolvedTemplate.Build.Pin`. It is nil when the toolset declares no pin, and
because the field is `omitempty` a nil pin serializes to nothing: that is exactly
what makes a pin-less template's IR byte-identical to today. The
`omitempty`→nil ⇒ byte-identical-IR-when-absent property only closes because the
carrier is this single flat, omitempty field.

The one field that widens is the skills leg, from a single `{dir, link}` to the
resolved delivery set:

```go
type ResolvedSkills struct {
    // Exactly one of Sources / Root is populated (named vs inline form).
    Sources []SkillSource `json:"sources,omitempty"` // NAMED form: ordered, name-deduped single-skill trees {Name, Dir(abs)}
    Root    string        `json:"root,omitempty"`    // INLINE form: a skills-ROOT (<name>/SKILL.md subtrees), mounted DIRECTLY
    Primary string        `json:"primary"`           // == template.skill (a Sources member in named mode; a free-form token in inline mode)
    Link    string        `json:"link"`              // in-box $HOME-relative discovery path (unchanged)
}
type SkillSource struct { Name, Dir string }
```

The delivered contract is unchanged (one `/faber/skills` tree + one
`FABER_SKILLS_LINK`); only the source representation widens so run-prep can stage
N dirs into that one tree. The **named** form yields `Sources` (the pipeline
run-prep stager farms them under `<name>/`); the **inline `{dir, link}`** form
yields `Root` — the dir is already a `<name>/SKILL.md` root, so run-prep mounts it
directly with no `<name>` wrapper, byte-identical to today. Emitting a wrapped
one-entry `Sources` for the inline case would double-nest it
(`/faber/skills/<name>/<name>/SKILL.md`); that is why the two forms resolve to
different fields. `GenSpec` carries the source
command/args, the target workflow *name* (expansion is run-time), and the item
binding template. `SelSpec` lists the coalesced candidates newest-first.

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
      use(template):  node(kind=agent, id=scope+"/"+step.ID,
                           template=resolveTemplate(cfg, step.Use))
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

### resolveTemplate

Collapses a dual-mode `TemplateDef` into `ResolvedTemplate`; both forms produce
the same value (Loader has already proven every reference resolves and exclusivity
holds):

```
resolveTemplate(cfg, name):
  t := cfg.Templates[name]
  build   := ResolveBuild(cfg, t)   # inline *t.Build verbatim, else the named
                                     # cfg.Images[t.Image] PROJECTED into a BuildDef.
                                     # ImageDef and BuildDef are DISTINCT types, so the
                                     # named form must COPY img.Pin -> build.Pin explicitly
                                     # (a field copy, NOT struct passthrough); result:
                                     # {Packages, Overlay(abs), Pin}. Pin is nil when the
                                     # toolset declares none; the engine-default fallback
                                     # is applied later, in infra.

  ident   := t.Identity            if t.Identity != "" else t.Run.Identity
  hooks   := for f in {context,prelude,on_failure}:
               isPath(v) ? v : cfg.Hooks[v].Path                        # v already absolute if a path
  skills  := t.Skills.Names != nil
               ? ResolvedSkills{Sources: dedupOrdered(n -> {n, cfg.Skills[n].Dir}),
                                Primary: t.Skill, Link: t.SkillsLink}   # named: N single-skill trees to farm
               : t.Skills.Inline != nil
                 ? ResolvedSkills{Root: t.Skills.Inline.Dir,           # inline: a skills-root mounted DIRECTLY
                                  Primary: t.Skill, Link: t.Skills.Inline.Link}
                 : nil                                                  # no skills leg
  return ResolvedTemplate{Packages: build.Packages, Overlay: build.Overlay,
                          Pin: build.Pin,          # flat *PinDef, json "pin,omitempty";
                                                   # nil ⇒ omitted ⇒ byte-identical IR
                          Identity: ident, Skill: t.Skill,
                          Hooks: hooks, Skills: skills, Run: t.Run,
                          Inputs: t.Inputs, Output: t.Output}
```

`ResolveBuild(cfg, t)` is the shared toolset resolver — desugar and infra's build
seam (`infra/seams.go`) both call it, so both see the same `BuildDef` regardless of
how the toolset was declared. It returns the inline `*t.Build` verbatim when set,
else projects the named `cfg.Images[t.Image]` into a fresh `BuildDef`. Because
`ImageDef` and `BuildDef` are **distinct types**, the named branch must **copy
`img.Pin → build.Pin`** explicitly (a field copy, not a struct passthrough) — omit
that copy and a named `image:`'s pin is silently dropped before it ever reaches the
`ResolvedTemplate` or infra. `ResolveBuild` returns `(BuildDef{}, false)` only when
neither form is set (a Loader-reported error).

`isPath(v)` is the same lexical test the schema uses (contains `/`, or begins
`.`/`~`/`/`). No file is read; paths are already absolute from assembly.
`dedupOrdered` keeps first occurrence, preserving declared list order.

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
