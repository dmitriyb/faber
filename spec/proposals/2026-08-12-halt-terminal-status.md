# Change Proposal: Halt — a first-class terminal status

## Context

faber has no halt concept. What looks like halting today is a convention
layered on loop conditions: a template declares a `halt_reason` output, a hook
writes `{"done":true,"halt_reason":…}`, and the actual stopping is done by
`done:true` satisfying a loop's `until`. That works only because the halting
step happens to be the one the loop condition reads; it does not generalise —
a merge-time halt (CI stuck past its retry budget, findings posted, operator
must triage) cannot ride a loop condition that reads a different step.

There is also a semantic gap: today the only way for a step to stop the run is
to fail, and a failure fail-stops the dependency chain into hundreds of
`skipped-dependency` rows and a nonzero exit. "Operator must triage" and
"something broke" are different outcomes and should be machine-distinguishable.

## Proposed change

A third terminal status, `halted`, beside `ok` and `failed`:

- **Result contract.** `Result` gains `status: halted` carrying a
  `halt {reason, detail, phase}` record — reason a stable machine word chosen
  by the halter, detail human text, phase the box phase that requested it.
  A halted record carries no payload and no error; the union is enforced by
  `Validate` like the other two arms.
- **Requesting a halt.** Inside the box, a user phase (context, prelude,
  agent via its skill, postlude) writes `halt.json`
  (`{"reason": …, "detail": …}`) into `$FABER_RESULT_DIR`. After each user
  phase that exits 0, the sequencer checks for the file; when present it
  stops the phase order and emits the halted attempt record — later phases
  (the agent for a prelude halt, the postlude for an agent halt) never run.
  Not a magic exit code: exit status keeps meaning pass/fail, and a phase
  that fails after writing `halt.json` is a failure (the halt request is
  honored only from an orderly exit). A malformed `halt.json` fails the
  step loudly (reason `halt-invalid`).
- **Not an error.** A halt bypasses the failure policy's retry loop and
  `on_failure` cleanup: the step settled decisively; nothing is broken to
  clean up, and re-running it would repeat the very state that asked for an
  operator.
- **Skip attribution.** Downstream steps settle `skipped-halt` naming the
  halting step — distinct from `skipped-dependency`, so a triage stop never
  reads as a failure cascade. Independent branches keep running.
- **Exit code.** A run with no failed steps and at least one halted step
  exits 3 (0 = success, 1 = failure, 2 = usage); a run with failures stays
  exit 1 even when steps also halted. A supervising script branches on the
  code, never on report text.
- **Report.** The run report names the halting step, its reason, and detail
  in a dedicated block (like the failure blocks), plus `halted` /
  `skipped-halt` totals in the footer.
- **Resume.** A halted record is never a journal reuse hit (only `ok`
  records are), so `faber resume` re-enters a halted run at the halted step
  exactly the way it re-enters a failed one. Interactive re-entry stays
  failed-only: a halt preserves no handoff state.

The existing `blocking_drift` convention keeps working unchanged: it stops a
loop through its `until` condition and never produced a halted status. The
new mechanism is additive; templates migrate by writing `halt.json` instead
of threading sentinels through loop conditions.

## Impact expectation

- **failure**: `StatusHalted` + `HaltRecord` in the result contract and
  `Validate`; the attempt loop returns a halted result without cleanup or
  retry; `RunEndRecord` counts halted steps.
- **agent**: `halt.json` convention in the contract (name, shape, reasons);
  the sequencer's post-phase halt check and halted record emission;
  host-side extract passes a halted record through without threading.
- **pipeline**: `halted` / `skipped-halt` terminal states, halt propagation,
  journal skip encoding, report lines/blocks/totals, exit-code mapping
  (a typed `HaltedError` carrying exit 3), defensive skip handling in input
  resolution and condition evaluation.
- **config**: CLI exit-code contract documentation.
- **spec/docs**: result-contract, failure-policy, recovery, scheduler,
  reporter, phase-order, and hooks leaves; test sections for all three
  modules.
