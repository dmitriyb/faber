# Test section: Wiring and CLI tests

Integration/acceptance scenarios spanning WiringChecker and CLI — validation
outcomes as the user experiences them through `faber validate` and run-entry
param checking.

## Fixtures

- `testdata/reference.yaml` plus a set of one-defect mutations of it, generated
  by patching the valid config (so each fixture stays minimal and current).
- An in-process CLI harness calling `run(args, stderr)` and capturing exit code
  + stderr.

## Scenarios

1. **Acceptance defect quartet** (pins the "Broken wiring caught at validate"
   project scenario). Four mutations of the reference config, each failing
   `faber validate` with exit 1 and exactly the expected violation:
   - `${steps.implement.prs}` (undeclared output field; message suggests `pr`)
   - review's `pr` slot bound from `${params.repo}` (string into int slot)
   - a `depends_on` edge from merge back into the loop body creating a cycle
     (reported as a concrete node path);
   - `faber run task` with `--param repo=sandbox` only (missing required
     `item`; run-entry error, nothing built or launched).
2. **All-violations-in-one-pass.** A config carrying all four defects reports
   all four together, sorted by (node, path) — no first-error truncation.
3. **Slot discipline.** Double-binding a slot (edge + literal) and binding an
   undeclared slot each produce their specific violations.
4. **Condition checks.** `when: steps.merge.merged` on a node that precedes
   merge is rejected (condition reads a non-predecessor); an `until:` referencing
   a step outside the loop body is rejected; a CEL syntax error surfaces with the
   compile diagnostic and the IR node path.
5. **Tool subset.** A step declaring tool needs `[git, jq]` against a template
   whose packages lack `jq` reports the set difference.
6. **Generate boundary.** The epic's generate `with:` omitting the target
   workflow's required `item` param fails; `${item.deps}` binding into a
   `[]string`-incompatible slot fails; the target workflow is wiring-checked
   even when only reachable via generate.
7. **CLI contract.** `faber` with no args exits 2 with usage; `validate` on the
   pristine reference exits 0 silently (logs only); `--emit-ir` writes canonical
   bytes to stdout and nothing else to stdout; `validate` exit codes and stderr
   shape are stable across runs.
8. **Resume guard.** `faber resume <id>` after the config changed (different IR
   hash) refuses with the mismatch message; `--fresh` proceeds.

## Edge cases

- Unknown `--param name=v` (not in params interface): rejected, listing declared
  params.
- `--param item=` (empty string for required param): accepted as a value —
  presence, not truthiness, is the contract.
- `--log-format json` on a TTY produces JSON (explicit beats auto-detection).
- Violation output with 100+ errors remains sorted and deterministic.
