# SchemaTypes — the orchestrator.yaml struct tree

## What it is

The Go type definitions that mirror `orchestrator.yaml` 1:1. Every other module
receives its configuration through these types; nothing else in the engine parses
YAML. The schema is the whole policy surface of faber — if a behavior cannot be
expressed here, it is either mechanism (faber's job) or it does not exist.

## Top-level shape

```
Config
├── Version      int
├── Network      NetworkDef        workflow-level binding (name, proxy, no_proxy)
├── Remote       RemoteDef         gateway URL prefix + host-key policy
├── Credentials  CredentialsDef    resolver command + per-service handle modes
├── Identities   map[string]IdentityDef    role name -> key material source
├── Templates    map[string]TemplateDef    the boxes
└── Workflows    map[string]WorkflowDef    the DAGs
```

`TemplateDef` is split exactly along the build/run boundary:

```
TemplateDef
├── Build
│   ├── Packages []string          pinned Nix package names — the toolset IS the env
│   └── Overlay  string            optional path to a user overlay of derivations
├── Run
│   ├── Identity  string           key into Identities; one key per box
│   ├── Resources ResourceDef      memory, cpus
│   ├── Runtime   string           optional isolation runtime (e.g. runsc)
│   ├── Env       map[string]string   plain env for the box (e.g. module-proxy-off knobs)
│   └── Volumes   map[string]string   named volume -> mount path (e.g. pre-warmed dep cache)
├── Skill     string               agent skill invoked headlessly in the box
├── Hooks     HookSet              context, prelude, on_failure — opaque script paths
├── Inputs    map[string]ParamDef  typed slots
└── Output    map[string]FieldDef  typed output schema (validated at the boundary)
```

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

- **Maps keyed by name, not lists with name fields**, for templates/workflows/
  identities: names are identity, duplicates are impossible by construction, and
  `$ref`-style reuse (`use: task`) is a map lookup.
- **No domain fields.** The reference workflows (see `spec/test_reference_workflows.md`)
  express claim/review/merge purely through hook paths, skill names, and param
  names. Reviewers of schema changes should reject any field whose name encodes a
  tracker, gate, or spec-tool concept (see project requirement "Mechanism, not policy").
- **yaml.v3 struct tags only**; custom `UnmarshalYAML` is reserved for the StepDef
  union and compact forms — everything else round-trips mechanically.

Requirements implemented: Workflow schema definition, Typed params interface,
Opaque policy seams.
