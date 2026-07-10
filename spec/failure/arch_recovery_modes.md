# RecoveryModes — resume, fresh, interactive

## What it is

The three ways to re-enter a run that stopped — whether it failed, was killed,
or exhausted a budget. All three are entry points over the same two artifacts:
the run's journal and the deterministically re-derived IR. Nothing about a
stopped run is special state; recovery is just a different seeding of the
scheduler.

## resume

The default re-entry (`faber resume <run>`). The journal header is checked
first: the config is re-loaded and re-desugared, and the resulting IR hash and
supplied params must match the header — the config pipeline is deterministic,
so a mismatch means the graph changed and journal keys cannot be trusted;
resume refuses with an explanation and points at fresh. On a match, execution
proceeds normally except that when a step becomes ready, its input-hash is
computed and looked up: a journaled `ok` record with a matching
`(step-id, input-hash)` is skipped, its payload reused for downstream
threading exactly as if the step had just run. A failed or absent record means
the step runs. Execution therefore restarts at the first failed or absent
step — not by finding it upfront, but as the natural consequence of
readiness-time lookup: everything before it hits, everything after it was
never journaled.

Reuse is exact, not fuzzy: the hash covers resolved input values, template
identity, and image tag, so an upstream step that re-ran and produced a
different output invalidates its dependents' keys automatically.

## fresh

`--no-cache`: ignore the journal entirely and run everything. A new run
directory and journal are created; the old journal is left untouched as a
record of the abandoned run. This is the escape hatch for "the world changed
in ways the hash cannot see" — a companion service was reconfigured, the
remote's state was manually repaired — and the recovery from a resume refusal
after config edits.

## interactive

Manual diagnosis: reconstruct the failed step's box and put the operator
inside it. Faber rebuilds exactly what the step ran with — the same image tag,
the same security bindings (network attachment and proxy env, remote and
host-key material, a freshly spawned single-key identity agent for the same
identity, credential handles), the same resolved inputs exported as the step
env — but replaces the box's entry program with an interactive shell on the
operator's terminal. The failed attempt's handoff directory (preserved
diagnostic state, per the result contract) is surfaced read-only inside the
box, so the operator sees what the agent saw, with the agent's exact toolset
on hand. Exiting the shell tears down the bindings as any run would. An
interactive session writes nothing to the journal — it is observation, not
execution — and the run's state is unchanged afterward; the operator then
chooses resume or fresh.

## Generate items are not special

A failed generate item is a normal failure plus journal entry — there is no
"partial run" state distinct from any other partly-completed graph. On resume,
the generate node re-invokes its data-source command (expansion is run-time),
instances are re-instantiated, and the same readiness-time lookup applies per
instance: completed items hit the journal and skip, the failed item re-runs,
items the source no longer emits simply do not exist in the new expansion.
Fail-stop cascade within the fan-out (the failed item's dependents were
skipped; siblings completed) falls out of the same rules as everywhere else.

Requirement implemented: Recovery modes.
