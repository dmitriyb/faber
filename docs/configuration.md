# Configuration

One `orchestrator.yaml` (plus any files it `include:`s) is the single config identity `faber validate`/`build`/`run` all read.
This document is the practical reference — the schema by example.
For the formal rules (desugaring, `$ref` resolution, wiring/type validation, the `include:` merge semantics), see `spec/config/arch_schema_types.md`, `arch_desugarer.md`, and `arch_wiring_checker.md`.

## Schema

```yaml
version: 1
include: [./shared/images.yaml]   # partial-config files, declarer-relative, union-merged

# --- substrate: root-only, never library-merged ---
network:     {name: agents-internal, proxy: http://egress:8888, no_proxy: [gateway]}  # egress lock; proxy XOR nftables required
remote:      {url: ssh://git@gateway/srv/git, host_key_file: ./keys/gw.pub}           # host_key_file XOR tofu required
credentials:
  resolver: ./hooks/get-token             # opaque: get_token(service) -> stdout
  services: {agent-api: {mode: proxy, endpoint: http://token-proxy:8402}}  # mode: proxy|file|helper
identities: {implementer: {key: ./keys/implementer}}   # one ssh-agent key per box

# --- component libraries: named, union-merged across include: files ---
images:
  base: {packages: [git, openssh, go, claude-code]}   # pinned nixpkgs set -> immutable image
skills:
  implement: {dir: ./skills/implement}                # one skill's tree, SKILL.md at its root
hooks:
  release: {path: ./hooks/release}                     # named hook, referenceable by bare name

templates:
  implement:
    image: base                       # named ref into images: (xor an inline build: {packages, overlay, pin})
    run:
      identity: implementer
      resources: {memory: 8g, cpus: 4}
      runtime: runsc                  # optional: switch container runtime (e.g. gVisor)
      env: {FABER_AGENT_CLI: claude}  # which agent binary the box invokes (required; no vendor default)
    skill: implement                  # the /<skill> prompt token
    hooks: {context: ./hooks/gather-context, on_failure: release}  # a bare name (library ref) or a path
    inputs:  {repo: {type: string, required: true}, item: {type: string, required: true}}
    output:  {branch: {type: string, required: true}, pr: {type: int, required: true}}

workflows:
  task:
    params: {repo: {type: string, required: true}, item: {type: string, required: true}}
    sources: {items: {command: ./hooks/list-items, args: ["--repo", "${params.repo}"]}}  # generate's data source
    steps:
      - id: implement
        use: implement
        with: {repo: "${params.repo}", item: "${params.item}"}
      - id: review-cycle
        loop: {max: 3, until: "steps.review.verdict == \"approved\"", steps: [ ... ]}
      - id: fan-out
        generate: {source: items, workflow: per-item, with: {item: "${source.item}"}}
      - id: merge
        use: merge
        when: "steps.review.verdict == \"approved\""
        depends_on: [fan-out]
        retry: 1
        on_failure: release
        with: {repo: "${params.repo}", pr: "${steps.implement.pr}"}
```

## Field-binding sources

A step's `with:` values and a `when`/`until` condition bind from a closed set: a workflow `params.*`, a `generate` item's `source.*` field, a literal, or `steps.<id>.<field>` from a completed step's typed output.
`${...}` references are resolved to graph edges at desugar time — an unresolvable, wrongly-typed, or cyclic reference is a `validate`-time error, never a run-time one.
`when`/`until` are CEL expressions over the same binding set, compiled once at validate time.

## Dual-mode aspects

`image`, `hooks.*`, and `skills` each support two forms: a named reference into the matching library (`images:`, `hooks:`, `skills:`) or an inline value (`build:`, a bare hook path, an inline `skills: {dir, link}`).
A template picks exactly one form per aspect — the loader rejects setting both.
Named references exist so one toolset/hook/skill tree can be shared across templates and `include:` files without repeating it.

## Typed params

`ParamDef` is one vocabulary used for workflow params, template inputs, and template outputs alike: `type`, `required`, `default`, `enum`.
An output field a step declares is exactly what `steps.<id>.<field>` downstream may reference — there is no separate output-typing system to keep in sync.

## Data-source commands and `generate`

`sources.<name>` names an opaque executable (`command` + `args`, argv only — never a shell) that emits items at run time; a `generate` step fans a named workflow out over those items, deriving one instance per item and inter-instance edges from each item's own `deps` field.
See `spec/pipeline/arch_generate_expander.md` for the expansion algorithm and node-count bound.
