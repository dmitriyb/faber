# Loader — reading and schema-validating orchestrator.yaml

## What it is

The single entry point from bytes to a typed `*Config`: read the file, unmarshal
into SchemaTypes, then run schema-level validation. The Loader owns every check
that can be phrased against the YAML alone; checks that need the desugared graph
(reference resolution, cycles, type flow) belong to the WiringChecker.

## Two-phase contract

```
Load(path)      -> *Config, error        pure read + unmarshal, no validation
Validate(*Config) -> error               multi-error, field-path diagnostics
```

Kept separate (conductor's D2, carried over) so `faber validate` can report as much
as possible from a partially broken file, and so tests construct `Config` values
directly without YAML fixtures.

## What Validate checks

Structural, cross-reference, and vocabulary rules — all collected, none fatal-first:

- `version` supported; exactly one of the StepDef union forms per step.
- Required fields present: template `skill`, `build.packages` non-empty, workflow
  `steps` non-empty, param/field `type` in vocabulary.
- Name discipline: step ids unique within a workflow (including inside loop
  bodies); template/workflow/identity/source names are non-empty and disjoint
  where referenced.
- Reference existence (name level): every `use:` names a declared template or
  workflow; `run.identity` names a declared identity; a generate's `source:` names
  a declared source and its `workflow:` a declared workflow; `depends_on` entries
  name step ids in the same scope.
- Binding syntax: every `${...}` parses to one of the four legal roots; `${item.*}`
  only inside generate bindings; `${sources.*}` only as a generate source.
- Enum values: `credentials.services.*.mode` in {proxy, file, helper}; remote has
  exactly one of `host_key_file` / `tofu: true`; resource strings parse.

Every violation is `fmt.Errorf("workflows.epic.steps[0].with.repo: <reason>")` —
the field path is the contract, asserted by tests. Errors aggregate via
`errors.Join`.

## What Validate deliberately does not check

- Whether hook scripts, overlay files, or key files exist — run-machine concerns,
  checked at run start, not config validity (the config may be authored on a
  different machine than it runs on).
- Whether packages resolve in nixpkgs — that is infra's validate-time
  `nix eval` proof, orchestrated by the CLI after Validate passes.
- Type compatibility of `${steps.X.field}` against slots — WiringChecker, over
  the IR, where loop unrolling has already fixed instance identities.

## Failure mode

`Load` errors wrap the path and the yaml.v3 line context. `Validate` never stops
early: the user fixes a broken file in one round trip, not five (conductor's D6,
carried over).

Requirement implemented: Workflow schema definition.
