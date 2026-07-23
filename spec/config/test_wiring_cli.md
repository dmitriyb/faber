# Test section: Wiring and CLI tests

Integration/acceptance scenarios spanning WiringChecker and CLI — validation
outcomes as the user experiences them through `faber validate` and run-entry
param checking.

## Fixtures

- `testdata/reference.yaml` plus a set of one-defect mutations of it, generated
  by patching the valid config (so each fixture stays minimal and current).
- An in-process CLI harness calling `RunWithDeps(args, stdout, stderr, deps)`
  (`config/cli_test.go`'s `runCLI` helper) and capturing exit code, stdout, and
  stderr — no subprocess spawned, per `arch_cli.md`'s "No hidden state".

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
   shape are stable across runs. `faber --help`, `faber -h`, and `faber help`
   print usage to stdout and exit 0 — the case that was broken before the
   cobra migration (`faber --help` used to fall through to the unknown-command
   path: `unknown command "--help"`, exit 2, on stderr).
8. **Resume guard.** `faber resume <id>` after the config changed (different IR
   hash) refuses with the mismatch message; `--fresh` proceeds.

## Library-reference and dual-mode scenarios

9. **Dangling library refs.** Each fails `faber validate` with exit 1 and its
   field-pathed error: `template.image` naming an undeclared image; a
   `template.skills[*]` naming an undeclared skill; a bare `hooks.context` naming
   an undeclared hook (distinguished from a path because it has no separator);
   `identity` naming an undeclared identity. Near-miss suggestions where distance
   ≤ 2.
10. **Primary skill — named mode only.** In named mode, `skill: review` with
    `skills: [implement]` fails: the activated skill must be a member of the
    delivered set. But `skill: review` with **no `skills:` library** (or with an
    inline `skills: {dir, link}`) is **valid** — `skill` is then a free-form prompt
    token, not a library reference, so no membership or existence check fires. This
    is the case every current config hits and it must keep passing `faber validate`.
11. **Dual-mode conflict.** A template setting both `image:` and `build:` fails;
    both top-level `identity:` and `run.identity:` fails; `skills_link:` alongside
    an inline `skills: {dir, link}` fails — each a field-pathed exclusivity error.
12. **Assembly errors — collected vs hard stop.** A duplicate library key across
    two included files and a substrate key on a non-root included file are
    **recorded during Assemble and collected into the `faber validate` report**
    (exit 1) alongside every schema error — sorted, no first-error truncation. An
    include cycle and an unreadable/unparseable included file instead **hard-stop**
    Assemble (exit 1) with a single cycle/parse message and no collected report,
    because neither can yield a `Config` to validate.
13. **Unsafe path-component name.** A config whose `skills` library declares a key
    like `"../../etc/x"` and whose `template.skills[*]` references it fails `faber
    validate` (exit 1): both the library key (`skills."../../etc/x"`) and the
    reference (`templates.box.skills[0]`) report the safe-identifier violation,
    collected in one report — the escaping name is rejected at validate, never
    joined into a stage path mid-run. The staging seam re-checks the same
    discipline (belt-and-suspenders) so a bypassed validation still cannot write
    outside the per-attempt tree.

## Edge cases

- Unknown `--param name=v` (not in params interface): rejected, listing declared
  params.
- `--param item=` (empty string for required param): accepted as a value —
  presence, not truthiness, is the contract.
- `--log-format json` on a TTY produces JSON (explicit beats auto-detection).
- Violation output with 100+ errors remains sorted and deterministic.
