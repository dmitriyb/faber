# Change Proposal: Prelude may skip the agent

## Context

Box phase order is clone → signing → context hook → prelude hook → agent →
postlude → result. The agent always runs. For steps whose decision is already
made before the agent starts, that is pure waste: a merge step whose prelude
has established that the PR is mergeable needs only its deterministic
postlude — "no judgement lives here" — yet today it must still spend an agent
turn invoking a script.

## Proposed change

- **Signal.** The prelude signals the skip through the existing bundle
  sidecar convention: `FABER_SKIP_AGENT=1` in `bundle.env`, beside the values
  hooks already write. Not a magic exit code — exit status already means
  pass/fail and is consumed by `set -e` in shell hooks. The key is consumed
  by the engine (popped before the reserved-name check and never exported to
  the agent environment); any value other than `1` is a bundle contract
  error (`bundle-malformed`), mirroring the TOFU discipline.
- **Opt-in per template.** A template declares `agent_optional: true`; the
  resolved template carries it into the IR (omitempty — absent templates'
  IR bytes are unchanged) and the host emits `FABER_AGENT_OPTIONAL=1`. A
  template that does not opt in ignores the signal and says so (a warning
  log; the agent runs normally). Skipping is a reviewable property of the
  template, not emergent behavior.
- **Phase order unchanged.** The agent phase becomes a logged no-op when
  skipped; the postlude still runs; the result phase still runs, so the
  step's declared output contract must be satisfied — by the prelude or the
  postlude writing `output.json`. An unsatisfied contract fails exactly as
  today (`missing-output`), with the detail additionally noting the agent
  was skipped by the prelude.
- **Record honesty.** The attempt record carries `agent_skipped: true`
  through the contract, the failure result, the journal, and the report
  (an `(agent-skipped)` marker on the step line). No usage sidecar exists
  for a skipped agent, so reported-tier metering sees no agent cost.
- **Resume.** Skipping does not change inputs, template identity, or image
  tag, so the journal key is unchanged; a resumed run adopts the prior ok
  record verbatim (cached), agent_skipped marker included. Flipping
  agent_optional between runs changes the IR hash, so resume refuses with
  the standard drift message rather than silently changing whether the
  agent ran.

## Impact expectation

- config: TemplateDef.agent_optional (types, round-trip), ResolvedTemplate
  (IR, omitempty), desugar copy; schema/impl leaves.
- agent: contract env name + bundle key + record field; runspec emission;
  box env parse (strict "1"); prelude pop/warn; agent phase no-op; result
  phase marker + enriched missing-output detail; extract pass-through.
- pipeline: adaptResult marker copy; report line marker.
- failure: Result.AgentSkipped field (journal shape, additive).
- spec/docs: schema, phase-order, hooks-contract, result leaves; test
  sections; configuration doc.
