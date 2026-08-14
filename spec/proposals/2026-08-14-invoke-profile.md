# Change Proposal: Declarative invocation profile

## Context

faber promises it hardcodes no agent vendor — `FABER_AGENT_CLI` has no
default — but the one nondeterministic phase does. `agent/box/invoke.go`
assembles the agent command line in Claude Code's headless dialect: the
`/{skill}` prompt prefix, `-p`, `--permission-mode bypassPermissions`, and
the `--model` / `--effort` / `--max-budget-usd` flags. So `FABER_AGENT_CLI`
swaps the binary name, not the invocation shape: pointing it at another
harness emits flags that harness rejects. This is a policy leak in a
mechanism-only engine — the dialect of one vendor's CLI is exactly the
kind of domain knowledge faber must receive as config, not carry as code.

## Proposed change

Make the invocation shape data: a named, user-defined **invocation
profile**, compiled into the IR and expanded by the box. The engine keeps
zero vendor names; it keeps one anonymous built-in default whose field
values reproduce today's behavior byte-for-byte.

- **Authoring.** A new top-level `invoke_profiles:` map in
  `orchestrator.yaml` defines named profiles. A template opts in with
  `invoke: {profile: <name>, …}`; inline fields override the named
  profile's; a template may also inline a full profile without naming one.
  A template with no `invoke:` gets the built-in default — existing
  configs desugar to identical IR (golden-guarded).
- **Fields** (all optional; defaults are the built-in dialect):
  - `subcommand` — argv tokens between the CLI and the prompt (default
    none).
  - `prompt_flag` — flag carrying the prompt string (default `-p`).
  - `skill_mode: prefix | flag` — `prefix` injects the skill into the
    prompt via `{skill}`; `flag` passes it as a separate argument
    (default `prefix`).
  - `skill_flag` — the argument name when `skill_mode: flag`.
  - `prompt_template` — template over `{skill}`, `{body}`, `{extra}`
    (default `"/{skill}\n\n{body}{extra}"`); `{extra}` expands to the
    current ADDITIONAL INSTRUCTION trailer verbatim, or empty.
  - `fixed_flags` — literal argv tail always appended (default
    `["--permission-mode", "bypassPermissions"]`).
  - `model_flag`, `effort_flag`, `budget_flag` — flags for
    `FABER_MODEL` / `FABER_EFFORT` / `FABER_MAX_BUDGET` (defaults
    `--model`, `--effort`, `--max-budget-usd`); an empty value omits the
    pair from argv, exactly as today.
- **Compilation.** Desugar resolves the named profile plus inline
  overrides into a concrete profile on the resolved template. Validation
  happens at `faber validate`, never mid-run: unknown profile name,
  `prompt_template` missing `{body}`, `skill_mode: flag` without
  `skill_flag`, `skill_mode: prefix` with `{skill}` unreachable, and
  engine-owned collisions all surface with field paths.
- **Transport.** The host marshals the resolved profile as JSON into a
  new engine-owned env var, `FABER_INVOKE_PROFILE`. The box parses it into
  its env; when the var is absent or empty the box uses the built-in
  default, so direct sequencer invocations keep working. This is an
  additive env var with a tolerant default: per the contract's own rule,
  **no `ContractVersion` bump**.
- **Expansion.** `invoke.go` becomes a pure expander over the profile:
  `[CLI] + subcommand + [prompt_flag, expand(prompt_template)] +
  skill args + fixed_flags + model/effort/budget args`. Every vendor
  literal is deleted from the file; a byte-for-byte table test pins the
  default profile's argv and prompt to today's output.

Vendor dialects for other harnesses (Goose or anything else) ship as
`invoke_profiles:` entries in user configuration — never as engine
presets. What is already vendor-neutral stays untouched: the skills
symlink (`FABER_SKILLS_LINK`), the `output.json` result convention, and
template env for vendor-specific variables.

## Impact expectation

- **config**: `invoke_profiles` schema and template `invoke:` block;
  desugar resolution and default injection; resolved profile on the IR
  template; validation rules; golden IR updates.
- **agent**: `FABER_INVOKE_PROFILE` in the contract (engine-owned, no
  version bump); runspec emission; box env parsing with the absent ⇒
  default rule; `invoke.go` rewritten as a profile expander with the
  byte-for-byte default guard.
- **spec/docs**: agent phase-sequencing and env-contract leaves, config
  schema leaves; test sections for both modules; an `examples/` variant
  exercising a non-default profile.
