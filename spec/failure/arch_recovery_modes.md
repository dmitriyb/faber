# RecoveryModes — resume, fresh, interactive

## What it is

The three ways to re-enter a run that stopped — whether it failed, was killed,
or exhausted a budget. All three are entry points over the same two artifacts:
the run's journal and the deterministically re-derived IR. Nothing about a
stopped run is special state; recovery is just a different seeding of the
scheduler.

## resume

The default re-entry (`faber resume <run>`). Exclusivity comes first: resume
acquires the run's advisory lock (`run.lock`, see the Journal's same-run
exclusivity) before reading anything, and refuses loudly when another process
holds it — a live run cannot be resumed into, so there is never a second
journal appender and never a torn-tail repair against a live writer. Then the
journal header is checked, guards in fixed order, every refusal fail-closed
with `--fresh` as the escape:

1. **Format** — a journal whose `format` stamp is not this binary's refuses
   (no auto-migration; finish on the writer or start over).
2. **IR version** — a header journaled under a different IR schema refuses
   with a message naming the engine, not the operator's config.
3. **IR hash and params** — the config is re-loaded and re-desugared, and the
   resulting IR hash and supplied params must match the header — the config
   pipeline is deterministic, so a mismatch means the graph changed and
   journal keys cannot be trusted; resume refuses and points at fresh.
4. **Image inputs** — every reachable template's image tag is re-derived and
   compared against the header's recorded tags; a difference (a bumped
   default pin, image-schema change, or overlay edit — inputs the IR hash
   cannot see) refuses rather than silently invalidating every journal key.

On a match, execution proceeds normally except that when a step becomes
ready, its input-hash is computed and looked up: a journaled `ok` record with a matching
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

`--fresh`: ignore the journal entirely and run everything. A new run
directory and journal are created; the old journal is left untouched as a
record of the abandoned run. This is the escape hatch for "the world changed
in ways the hash cannot see" — a companion service was reconfigured, the
remote's state was manually repaired — and the recovery from a resume refusal
after config edits.

## interactive

Manual diagnosis: reconstruct the failed step's box and put the operator
inside it. Faber rebuilds exactly what the step ran with — the journaled
image tag (preferred over the current derivation, so an engine or pin upgrade
still reconstructs the box the step actually ran),
the same network, remote, and identity bindings (network attachment and
proxy env, remote and host-key material, a freshly spawned single-key agent
for the same identity), and the same resolved inputs exported as the step
env — but deliberately NO credential handles (the debug shell observes a
failed step and never runs the agent, so no token is resolved and none is
streamed; an operator who needs a secret sets it by hand), and with the
box's entry program replaced by an interactive shell on the operator's
terminal. The failed attempt's handoff directory (preserved
diagnostic state, per the result contract) is surfaced read-only inside the
box, so the operator sees what the agent saw, with the agent's exact toolset
on hand. Exiting the shell tears down the bindings as any run would. An
interactive session writes nothing to the journal — it is observation, not
execution — and the run's state is unchanged afterward; the operator then
chooses resume or fresh.

## upgrade-check: the pre-upgrade guard

`faber upgrade-check` is the read-only pre-flight encoding the rule "faber is
not upgraded mid-run": it enumerates journaled runs and refuses (non-zero)
while any is live (its run lock is held) or unfinished (no run-end marker in
its journal), listing them; `--force` acknowledges the list and exits 0 so a
deliberate upgrade can proceed. It never modifies a journal and never updates
faber — the binary swap is external (rebuild/release); this is the check an
operator or deploy step runs first. The audit scan is format-tolerant (it
probes record kinds only), because its whole job is to look at journals the
new binary may refuse to replay. Across a schema bump the consequence is
explicit: in-flight runs are finished on the old binary or restarted
`--fresh`; there is no auto-migration.

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
