# Change Proposal: Postlude — a deterministic hook phase after the agent

## Context

The box phase order is context → prelude → agent → result: deterministic
hooks run *before* the agent, but nothing scriptable runs *after* it. Every
post-agent obligation therefore rides on agent behavior — e.g. "post your
review answers through the gate" — and when the obligation is not met, the
failure is one undifferentiated mystery: did the agent stall earlier, or
finish and simply skip its last step? There is no deterministic place to
distinguish those, and no home for post-steps the agent should not even know
exist.

## Proposed change

A fourth user hook, `postlude`, symmetric with `prelude`: order becomes
context → prelude → agent → **postlude** → result. Same contract as the other
hooks — runs in the box, cwd = the clone, bundle dir available, box-contract
env, fail-stop with its own phase name (`postlude`) and the standard handoff
record. Declared per template (`hooks.postlude`), optional; absent means the hook
execution is skipped (the phase still logs in the fixed order, like skills)
and behavior is otherwise identical to today.

The phase runs BEFORE result extraction, so a postlude can act on and
validate the agent's artifacts (an answers file, a state marker) and turn
"the agent's last step silently didn't happen" into exact diagnostics: file
absent (agent never wrote it), file malformed (contract violation, named),
file empty-but-legal (recorded, not an error) — three different remedies
instead of one investigation. It is also the home for deterministic
post-steps outside the agent's knowledge or reach.

## Impact expectation

- **config**: `hooks.postlude` in the template schema (types, validate,
  desugar/resolve, load path resolution); goldens/reference IR updated.
- **agent**: `contract.HookPostlude`; run-spec mount + validation; the phase
  slice in the box sequencer; handoff phase naming.
- **pipeline**: boxrun/reentry thread the resolved postlude path (reentry
  observes only — the debug shell replaces the sequencer, hooks never run).
- **spec/docs**: phase-order leaves, the hooks-contract leaf (now covering
  all three user hook slots), lifecycle test leaf, configuration/architecture
  docs.
