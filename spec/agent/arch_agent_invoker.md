# AgentInvoker — headless skill invocation

## What it is

Phase 9 of the box: exactly one headless invocation of the agent CLI from the
template's pinned package set. Everything before it is deterministic setup;
everything after it is deterministic extraction; this is the only
nondeterministic phase, and it is atomic — there is no resuming into an
agent's chain of thought, only re-running the whole step.

## The prelude may skip it

On a template that declares itself agent-skippable (`agent_optional: true`,
carried into the box as `FABER_AGENT_OPTIONAL=1`), a prelude that has already
made the step's decision can skip the invocation entirely: it writes
`FABER_SKIP_AGENT=1` into `bundle.env`, and this phase becomes a logged no-op
— no process, no prompt, no model call. The phase order is otherwise
unchanged: the postlude still runs, and the result phase still enforces the
declared output contract (the prelude or the postlude must have written
`output.json`, or the step fails `missing-output` with the skip named in the
detail). The attempt record carries `agent_skipped: true` so journal, cost
accounting, and report all show that no agent ran. On a template that did
NOT opt in, the signal is ignored with a logged warning and the agent runs —
skipping is a reviewable property of the template, never emergent behavior.
The sidecar key is consumed by the engine (it is FABER-namespaced and never
exported into any child environment); any value other than `1` is a bundle
contract error.

## Prompt assembly

The prompt is three parts, concatenated with blank-line separators:

```
/<skill>

<contents of $FABER_BUNDLE_DIR/CONTEXT.md>

ADDITIONAL INSTRUCTION: <FABER_EXTRA_INSTRUCTION>     (only when set)
```

The leading slash-command line activates the configured skill; the bundle body
is passed verbatim (the hooks authored it, faber does not touch it); the
optional trailer is an operator note passed through the box environment for a
single run — clearly delimited so the skill can weigh it against its own
instructions.

## Invocation

The invoker executes the agent CLI (the binary is part of the template's
package set — validate-time package proof guarantees it resolves) with:

```
<agent-cli> -p <prompt> --permission-mode bypassPermissions
            [--model <FABER_MODEL>] [--effort <FABER_EFFORT>]
            [--max-budget-usd <FABER_MAX_BUDGET>]
```

Model, effort level and budget bound are pass-throughs from config; when unset
the flags are omitted. Config validation makes model and effort mandatory per
template — the agent's cost/quality knobs are pinned in config, never left to
float on a vendor default — so in engine-launched boxes both flags are always
present; the omission path serves direct sequencer invocations. The working directory is the workspace; the environment is
the box environment plus the bundle's sidecar values, so anything the prelude
derived (the branch name, a resolved record id) is visible to the skill.
stdout and stderr stream to the container's log — they are never parsed. The
result file is the only machine-readable channel out of this phase.

## Why permission bypass

The agent runs unrestricted *inside* because the sealed environment is the
restriction: a pinned, root-owned, immutable toolset; an internal network
whose only egress is the user's allow-listing proxy; a single-role key behind
a forwarded socket; the gateway as the only reachable remote; no secret
material, only handles. A second in-container permission gate would be a
control enforced *by* the untrusted thing it is meant to control — exactly
what the untrusted-box principle forbids relying on. In-box policy files are
at most fast feedback for the agent; the wall is the environment and the
user's gate service behind it. Consequently there is nothing for faber to
configure here: no allow-lists, no tool gates, no interactive prompts — the
box either has a tool (it is in the package set) or it does not.

## Exit mapping

- Exit 0 → proceed to the result phase. Success of the *process* says nothing
  about the outcome: a valid-but-unfavorable payload (a review verdict of
  `changes`) is an ok result, and a missing result file is handled by the
  extractor's fallback — neither re-enters this phase.
- Nonzero exit → the fail-stop path: handoff record with `phase: agent`, the
  exit code, and a stderr tail; failed attempt record; the result phase's
  extraction never runs. A budget-bound abort surfaces this way too — the
  bound is a hard cost stop, and interpreting it (defer, re-admit) is the
  host-side meter's business, not the box's.

Requirements implemented: Unrestricted agent invocation.
