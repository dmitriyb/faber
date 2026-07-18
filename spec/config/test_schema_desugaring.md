# Test section: Schema and desugaring tests

Integration/acceptance scenarios spanning SchemaTypes, Loader, Desugarer, and
IRModel — the path from YAML text to canonical IR. (Unit tests live beside the
code; these are the module-level behaviors that must hold.)

## Fixtures

- `testdata/reference.yaml` — the full reference orchestrator.yaml from
  `spec/test_reference_workflows.md` (task + epic).
- `testdata/reference_task.ir.json`, `testdata/reference_epic.ir.json` — golden
  IR files, reviewed by hand once, regenerated only deliberately.
- A library of minimal invalid configs, one per Loader check.

## Scenarios

1. **Round trip.** Loading `reference.yaml` yields a Config whose every field
   survives a marshal/unmarshal cycle unchanged; template and workflow maps carry
   all four templates and both workflows.
2. **Union enforcement.** A step with both `use:` and `loop:` (and one with
   neither) each yield exactly one violation naming the step path and the union
   rule.
3. **Loader check catalog.** Each invalid fixture produces its expected
   field-path error and no others: duplicate step id inside a loop body, unknown
   identity in `run.identity`, `${item.x}` outside a generate binding,
   `${sources.x}` used as a `with:` value, remote with both `host_key_file` and
   `tofu`, unknown `mode` in credentials.
4. **Golden desugar (task).** Desugaring `task` matches the golden file
   byte-for-byte: 3 unrolled iterations with gate conditions
   `!(steps.review@1.verdict == "approved")` etc., ordering chain between
   iterations, selectors for `review` (referenced post-loop) with candidates
   `[review@3, review@2, review@1]`, merge wired to `implement.pr` via the data
   edge and to the selector via its `when` deps.
5. **Golden desugar (epic).** The generate node carries the source ref, workflow
   name `task`, and the item binding template; the `task` sub-IR is NOT inlined
   (run-time expansion) but was independently desugared and checked.
6. **Determinism.** Desugar the reference 100 times shuffling map iteration
   (`GODEBUG=randseed` variations or an explicit map-order fuzz harness): byte
   output identical. Two configs differing only in YAML key order and comments
   produce identical IR.
7. **Workflow-reference cycle.** `a` uses `b` uses `a`: desugar-time error naming
   the cycle path; no partial IR emitted.
8. **Unroll bound.** A loop with `max: 10000` over a 2-step body trips the
   sanity ceiling with an error naming the loop's step id.

## Composition and dual-mode scenarios

9. **Include merge.** A root project file with `include: [images.yaml, skills.yaml,
   hooks.yaml, templates.yaml, workflows.yaml]` assembles to a `Config` whose
   library maps are the union of the fragments; the assembled config validates and
   desugars to the same IR as the equivalent single-file config.
10. **Duplicate name across files.** Two included files each defining
    `templates.review`: Assemble **records** the violation (with both file paths)
    and returns it; `Validate` surfaces it in the collected report alongside any
    other errors — it is collected, not a hard stop, because a duplicate key still
    yields a mergeable `Config`. Same key in the *same* file is an ordinary
    YAML/last-key case, not this error.
11. **Include cycle (hard stop).** `a.yaml` includes `b.yaml` includes `a.yaml`:
    Assemble **hard-stops** with the cycle path (a cycle cannot yield a `Config`,
    so it is never collected). An unreadable/unparseable included file is the other
    hard stop. A diamond (`a`→`b`, `a`→`c`, both `b` and `c`→`d`) merges `d` once
    and succeeds.
12. **Declarer-relative paths.** An included `lib/images.yaml` with
    `overlay: ./overlay.nix` resolves to `lib/overlay.nix` (relative to the
    included file), not to the process CWD nor the root file's dir; the assembled
    `Config` carries the absolute path. Running the same single-file config from a
    different CWD produces byte-identical resolved paths.
13. **Dual-mode desugar equivalence (image/hooks/identity).** Two configs — one
    using `image:`/`hooks:` names/top-level `identity:`, the other the inline
    `build:`/`hooks: {…paths}`/`run.identity:` forms with identical underlying
    values — desugar to byte-identical IR for those aspects (the `ResolvedTemplate`
    image spec, hook paths, and identity are form-independent). The **skills** leg
    is the deliberate exception: the named and inline forms resolve to *different*
    `ResolvedSkills` shapes (`Sources` vs `Root`) by design (scenario 14), because
    `SkillDef.dir` and inline `skills.dir` are different directory levels — but both
    deliver the same mounted `/faber/skills` tree after run-prep staging.
14. **Skills assembly — named vs inline shapes.** A template `skill: implement`,
    `skills: [implement, go-expert]`, `skills_link: .claude/skills` desugars to a
    `ResolvedSkills` with `Sources` = `[(implement,→dir), (go-expert,→dir)]`
    (declared order, deduped), empty `Root`, `Primary: implement`,
    `Link: .claude/skills`. The inline `skills: {dir, link}` form instead yields
    `Root: <dir>` with **empty `Sources`** (a direct mount, no `<name>` wrapper) —
    proving single-skill delivery is byte-identical to today and does not
    double-nest. Run-prep staging (per-attempt copy of real files for `Sources`,
    direct mount for `Root`) is exercised in the pipeline module's tests.

15. **Per-image pin — validation and carry-through.** An `images.go-box.pin:
    {rev: "25.11", sha256: "…"}` desugars onto the resolved flat
    `ResolvedTemplate.Pin` unchanged; the inline `build.pin` form carries
    identically (dual-mode parity). A named `image:`'s pin survives `ResolveBuild`'s
    `ImageDef → BuildDef` projection (the explicit `img.Pin` copy is exercised: a
    named toolset with a pin must NOT resolve to `Pin == nil`). A template whose
    toolset declares no pin resolves to `ResolvedTemplate.Pin == nil` — the engine
    default is applied later in infra, never baked into the IR, so (the field being
    flat and `omitempty`) its IR is byte-identical to today. A **partial** pin
    (`rev` without `sha256`, or vice-versa) is a Loader field-path violation naming
    `….pin`; a fully-empty `pin: {}` normalizes to absent (`Pin == nil`) and passes;
    both-absent and both-present pins pass; a both-present pin with an off-charset
    `rev`/`sha256` is a Loader field-path violation. Two configs differing only in
    whether the toolset arrives via `image:` (a library entry with a pin) or inline
    `build:` (with the same pin) desugar to byte-identical `ResolvedTemplate.Pin`.

## Edge cases

- Empty `steps:` — Loader error, not a desugar panic.
- Loop with `max: 1` — body emitted once, no gate conditions, selector still
  emitted for post-loop refs.
- `when:` on a loop-step itself applies to every iteration's nodes (conjoined
  with gates).
- A template and workflow sharing a name — Loader rejects; the Desugarer never
  sees the ambiguity.
