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
invoke_profiles:                    # named agent-CLI invocation dialects (all fields optional; absent inherits the built-in default)
  goose:
    subcommand: [run]               # argv tokens between the CLI and the prompt
    prompt_flag: -t                 # flag carrying the prompt; "" makes the prompt positional
    skill_mode: flag                # prefix (skill injected into the prompt via {skill}) | flag (its own argument pair)
    skill_flag: --recipe            # the skill's argument name in flag mode
    prompt_template: "{body}{extra}"  # over {skill} {body} {extra}; default "/{skill}\n\n{body}{extra}"
    fixed_flags: []                 # literal argv tail; default ["--permission-mode", "bypassPermissions"]
    model_flag: ""                  # "" drops the pair (harness without the knob); defaults --model/--effort/--max-budget-usd
    effort_flag: ""
    budget_flag: ""
    session_dir: .local/share/goose/sessions  # $HOME-relative transcript dir; captured per attempt under the run dir with `faber run --sessions`
    resume_argv: [goose, session, resume, --last]  # entry for `resume --interactive`: lands the operator inside the failed attempt's session

templates:
  implement:
    image: base                       # named ref into images: (xor an inline build: {packages, overlay, pin})
    run:
      identity: implementer
      resources: {memory: 8g, cpus: 4}
      runtime: runsc                  # optional: switch container runtime (e.g. gVisor)
      env: {FABER_AGENT_CLI: claude}  # which agent binary the box invokes (required; no vendor default)
    skill: implement                  # the /<skill> prompt token
    model: claude-sonnet-5            # agent model, passed through via the profile's model flag (required; no vendor default)
    effort: high                      # agent effort level, passed through via the profile's effort flag (required)
    invoke: {profile: goose}          # invocation dialect: a named profile ref, inline fields, or both (inline overrides field-by-field); absent = the built-in default dialect
    agent_optional: true              # optional: the prelude may skip the agent by writing FABER_SKIP_AGENT=1 into bundle.env (default false — the agent always runs)
    hooks: {context: ./hooks/gather-context, on_failure: release}  # a bare name (library ref) or a path; also prelude (pre-agent) and postlude (post-agent, pre-result)
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

## Invocation profiles

An `invoke_profiles:` entry describes one agent CLI's headless dialect; a template opts in with `invoke: {profile: <name>, …}`, inline fields overriding the named profile's field-by-field (they compose — this aspect is deliberately not either/or).
Because a template is a role's box, this is the harness↔role seam: one template's package set + `FABER_AGENT_CLI` pick the binary, its `invoke:` picks the dialect, and its opaque `run.env` carries provider/endpoint knobs — so different roles in one workflow can run different harnesses against different endpoints (see `examples/mixed-harness/`).
A template with no `invoke:` gets the anonymous built-in default, byte-identical to the invocation faber always emitted; faber ships no vendor profile.
Explicit emptiness is meaningful: `prompt_flag: ""` makes the prompt positional, `model_flag`/`effort_flag`/`budget_flag: ""` drop the pair, `fixed_flags: []` drops the tail; an *absent* field inherits instead.
The resolved profile lands in the IR and reaches the box as `FABER_INVOKE_PROFILE`; `faber validate` checks profile rules (a `{body}`-carrying prompt template, skill injected exactly once, a `$HOME`-contained `session_dir`) at compile time, never mid-run.
The optional session dialect — `session_dir` (where the harness writes session transcripts) and `resume_argv` (how it resumes the latest one) — powers per-attempt transcript capture (`faber run --sessions`) and session-resuming interactive re-entry (`faber resume --interactive`); see `docs/commands.md`.

## Typed params

`ParamDef` is one vocabulary used for workflow params, template inputs, and template outputs alike: `type`, `required`, `default`, `enum`.
An output field a step declares is exactly what `steps.<id>.<field>` downstream may reference — there is no separate output-typing system to keep in sync.

## Data-source commands and `generate`

`sources.<name>` names an opaque executable (`command` + `args`, argv only — never a shell) that emits items at run time; a `generate` step fans a named workflow out over those items, deriving one instance per item and inter-instance edges from each item's own `deps` field.
See `spec/pipeline/arch_generate_expander.md` for the expansion algorithm and node-count bound.
