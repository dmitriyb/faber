# Loader — reading and schema-validating orchestrator.yaml

## What it is

The single entry point from bytes to a typed `*Config`: read the root project
file, resolve and merge its `include:` closure into one assembled `Config`, then
run schema-level validation. The Loader owns every check that can be phrased
against the YAML alone (now including library-reference existence and dual-mode
exclusivity); checks that need the desugared graph (typed reference flow, cycles
over the DAG, type compatibility) belong to the WiringChecker.

## Three-phase contract

```
Assemble(path)   -> *Config, []AssemblyViolation, error   read root + transitive includes, merge, resolve paths
Load(path)       -> *Config, []AssemblyViolation, error   = Assemble (kept as the public name)
Validate(*Config, []AssemblyViolation) -> error           multi-error, field-path diagnostics
```

`Assemble`/`Load` runs no schema validation, but it is the *only* phase with
per-file provenance, so it must catch the two cross-file rules the merged `Config`
can no longer express — **duplicate library key across files** and **substrate key
on a non-root file** — recording each as an `AssemblyViolation{path, msg}` and
returning the slice alongside the assembled `Config`. `Validate` then **merges
those recorded violations into its collected report**, so the user sees assembly
and schema errors together in one round trip. A single-file config with no
`include:` assembles to itself with an empty violation slice, so callers and tests
unaware of composition are unaffected. Tests may still construct `Config` values
directly (conductor's D2, carried over) and pass a nil violation slice to
`Validate`.

Only two conditions **hard-stop** `Assemble` (they cannot yield a `Config`, so
there is nothing to collect): an **unreadable/unparseable file** (root or any
include) and an **include cycle**. Everything else — including the two recorded
violations above — is non-fatal and flows into the collected `Validate` report.

## Assembly: include resolution + merge

`Assemble(root)` walks the include DAG and folds every file into one `Config`:

```
assemble(path, stack, seen, viols) -> partial *Config:
  abs := absolutize(path)                       # relative to the DECLARING file's dir
  if abs in stack: HARD STOP "include cycle: a.yaml -> b.yaml -> a.yaml"
  if abs in seen:  return nil                    # diamond: already merged once, skip
  cfg := unmarshal(read(abs))                    # HARD STOP on unreadable/unparseable
  resolvePaths(cfg, dir(abs))                    # rewrite every declared file path to absolute (list below)
  if abs != root and cfg.hasSubstrate():
    viols.add(abs, "included files may only contribute libraries")   # RECORDED, not fatal
  for inc in cfg.Include:                         # inc is relative to abs's dir
    merge(cfg, assemble(inc, stack+abs, seen, viols), viols)   # merge RECORDS dup keys into viols
  seen.add(abs)
  return cfg
```

- **Declarer-relative paths.** `resolvePaths` rewrites **every file path a config
  declares** to an absolute path anchored at the declaring file's directory, and
  likewise each `include:` entry. The full list: `images.*.overlay`, `skills.*.dir`,
  `hooks.*.path`, the inline `templates.*.build.overlay` / `skills.dir` /
  `hooks.*` path values, and the substrate paths `identities.*.key`,
  `credentials.resolver`, and `remote.host_key_file`. No file path is left CWD- or
  root-relative — the rule is uniform. The merged `Config` therefore carries only
  absolute paths; downstream (and every `ResolvedTemplate`) is unaffected in shape.
  This supersedes the old CWD-relative rule; a single-file config run from its own
  dir is byte-identical to before.
- **Merge = union of the named library maps** (`images`/`skills`/`hooks`/
  `templates`/`workflows`). A key present in two distinct files is a **duplicate
  violation** naming both files (`templates.review: defined in a.yaml and b.yaml`)
  — no silent last-wins. Because the merged map cannot express which file a key
  came from, this is **recorded into `viols` at merge time** (with both paths) and
  surfaced through `Validate`, keeping assembly order-independent and deterministic.
- **Substrate is root-only** (reviewer decision iii — *kept*). `version`/`network`/
  `remote`/`credentials`/`identities` from a non-root file are a **recorded
  violation** (with the offending file's path), surfaced through `Validate`; only
  the root project file's substrate is honored. Rationale: `identities` cannot be
  modularized into a library file, so a template-library file depends on the root
  supplying the identities it references — locking the whole substrate to the root
  makes that dependency direction uniform (see `arch_schema_types.md`).
- **Cycle detection** is by the DFS `stack`; a diamond (same file reached twice by
  different paths) is merged once via `seen`, not flagged. A cycle **hard-stops**
  Assemble — it cannot yield a `Config`, so it is never a collected violation.

## What Validate checks

Structural, cross-reference, and vocabulary rules — all collected, none fatal-first:

- `version` supported; exactly one of the StepDef union forms per step.
- Required fields present: template `skill`, a toolset present (exactly one of
  `image` or `build`; when `build`, `build.packages` non-empty; when `image`, the
  referenced image's packages non-empty), workflow `steps` non-empty, param/field
  `type` in vocabulary.
- **Dual-mode exclusivity** (per template, per aspect): not both `image` and
  `build`; not both top-level `identity` and `run.identity`; `skills` not both a
  named list and an inline mapping; `skills_link` present iff `skills` is the
  **named list** — so `skills_link` set with an inline `{dir,link}` leg, or with an
  empty/absent `skills` leg (a discovery path pointing at nothing), is a violation.
  Field-pathed, collected.
- Name discipline: step ids unique within a workflow (including inside loop
  bodies); template/workflow/identity/image/skill/hook/source names non-empty and
  disjoint where referenced. Library keys, and every referenced name faber turns
  into a filesystem path component (most sharply a skill name, which run-prep
  stages under `<stage>/<name>`, but the same rule for image/hook/template/
  workflow/identity keys and each `template.skills[*]` reference), must be a
  **safe single path segment**: no `/` (or `\`), no `..` segment, not absolute, no
  leading `.` or `~` — the same discipline `serviceNamePattern` enforces on
  credential service names. A violation is a field-pathed validate error
  (`skills."../x": name must be a safe identifier (no "/", "..", or leading ".")`),
  so an escaping name is caught at `faber validate` and never reaches the staging
  copy mid-run.
- **Cross-file assembly** (the recorded `AssemblyViolation`s merged in from
  Assemble): no duplicate library key across included files; no substrate key on a
  non-root file. The include cycle and unreadable-file cases are **not** in this
  list — they hard-stop Assemble and never reach a collected report.
- Reference existence (name level, over the merged libraries): every `use:` names a
  declared template or workflow; `template.image` names a declared image; each
  bare-name `template.hooks.*` names a declared hook; `template.identity`
  (top-level or `run.identity`) names a declared identity; a generate's `source:`
  names a declared source and its `workflow:` a declared workflow; `depends_on`
  entries name step ids in the same scope.
- **Skills references — named mode only.** When `skills` is a **named list**, every
  `template.skills[*]` must name a declared skill AND the primary `template.skill`
  must be a member of that list (`template.skill ∈ template.skills`). When `skills`
  is **inline `{dir,link}` or absent**, `template.skill` is a **free-form prompt
  token** (`/<skill>`), not a library reference — it is neither resolved against the
  `Skills` library nor checked for set membership. This scoping is what keeps every
  existing config (which sets `skill:` with no `skills:` library) valid.
- Binding syntax: every `${...}` parses to one of the four legal roots; `${item.*}`
  only inside generate bindings; `${sources.*}` only as a generate source.
- Enum values: `credentials.services.*.mode` in {proxy, file, helper}; remote has
  exactly one of `host_key_file` / `tofu: true`; resource strings parse.
- **Identifier grammars.** Step ids match `[A-Za-z][A-Za-z0-9_-]*` — the
  node-id namespacing characters (`/`, `@`, `[`, `]`, `"`) are reserved, so an
  authored id can never collide with a desugared one, and the grammar is
  reference-total (no dots, letter-first) so every declared id is
  addressable from `${steps...}` bindings and CEL conditions alike. Slot and param names
  match `[A-Za-z][A-Za-z0-9_-]*` and must be **env-token disjoint** per
  declaration map: the canonical `EnvToken` mapping (uppercase, `-`→`_`, the
  single source the box contract's `SlotToken` delegates to) is lossy, and two
  names sharing a token would silently misbind one value to the other's
  `FABER_INPUT_*` variable. Credential service names must be env-token disjoint and must not export an
  engine- or runner-owned variable (`PATH`, `HOME`, `GIT_SSH_COMMAND`,
  `SSH_AUTH_SOCK`, the `FABER_` namespace) — the box refuses the same names
  at its secrets phase as defense in depth.
- **Run-contract mirrors.** The statically checkable half of the run-spec
  assembler's refusals: `run.env.FABER_AGENT_CLI` must name the agent CLI
  (faber defaults no vendor), template env may not claim engine- or
  security-owned names (`EngineOwnedEnv`, the shared rule), and template
  volumes may not overlap the reserved container paths
  (`ReservedContainerPaths` — asserted in agreement with the run-spec
  assembler's own list by a cross-package test). A template that could not
  launch any box fails `faber validate`, not each step at run time.

Every violation is `fmt.Errorf("workflows.epic.steps[0].with.repo: <reason>")` —
the field path is the contract, asserted by tests. Errors aggregate via
`errors.Join`.

## What Validate deliberately does not check

- Whether hook scripts, overlay files, skill dirs, or key files exist — run-machine
  concerns, checked at run start, not config validity (the config may be authored
  on a different machine than it runs on). Note this is about *file existence*:
  the paths are still resolved to absolute (declarer-relative) at assembly, and
  an `include:` target that cannot be read is a hard `Load`/`Assemble` error, since
  a missing include means the config is not fully known.
- Whether packages resolve in nixpkgs — that is infra's validate-time
  `nix eval` proof, orchestrated by the CLI after Validate passes.
- Type compatibility of `${steps.X.field}` against slots — WiringChecker, over
  the IR, where loop unrolling has already fixed instance identities.

## Failure mode

`Load`/`Assemble` errors wrap the path and the yaml.v3 line context; the two hard
stops are an unreadable/unparseable file (root or any include) and an include
cycle. `Validate` never stops early: the user fixes a broken assembly in one round
trip, not five (conductor's D6, carried over), and duplicate-name / dangling-ref /
exclusivity violations across the whole include closure are reported together.

Requirement implemented: Workflow schema definition.
