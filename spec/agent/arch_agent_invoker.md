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

## The invocation profile

The invocation's *shape* is data, not code: a concrete invocation profile —
compiled by config from `invoke_profiles:` / a template's `invoke:` block and
delivered as the engine-owned `FABER_INVOKE_PROFILE` JSON — tells the invoker
where the prompt rides, how the skill is activated, and which flags carry
model/effort/budget. The invoker is a pure expander over it; **no vendor
literal appears in the phase**. When the variable is absent or empty (direct
sequencer invocations, pre-profile hosts) the box uses the anonymous built-in
default profile, whose field values reproduce the historical behavior
byte-for-byte (a pinned table test guards the bytes).

## Prompt assembly

The prompt is the profile's `prompt_template` expanded over the closed
placeholder set — under the default template, byte-identical to the historical
three-part prompt:

```
{skill}  ->  the configured skill token (default template renders it as the
             leading /<skill> slash-command line; a flag-mode profile carries
             it as a separate argument instead and omits it here)
{body}   ->  <contents of $FABER_BUNDLE_DIR/CONTEXT.md>, verbatim
{extra}  ->  "\n\nADDITIONAL INSTRUCTION: <FABER_EXTRA_INSTRUCTION>", or empty
```

The bundle body is passed verbatim (the hooks authored it, faber does not
touch it — substituted text is never re-scanned for placeholders, so bundle
bytes can never inject into the template); the optional trailer is an operator
note passed through the box environment for a single run — clearly delimited
so the skill can weigh it against its own instructions.

## Invocation

The invoker executes the agent CLI (the binary is part of the template's
package set — validate-time package proof guarantees it resolves) with the
profile-expanded argv:

```
<agent-cli> <subcommand…> [<prompt_flag>] <expanded prompt>
            [<skill_flag> <skill>]                 (skill_mode: flag only)
            <fixed_flags…>
            [<model_flag> <FABER_MODEL>] [<effort_flag> <FABER_EFFORT>]
            [<budget_flag> <FABER_MAX_BUDGET>]
```

Under the default profile that is exactly the historical
`<agent-cli> -p <prompt> --permission-mode bypassPermissions [--model …]
[--effort …] [--max-budget-usd …]`. An empty `prompt_flag` makes the prompt a
bare positional argument. Model, effort level and budget bound are
pass-throughs from config; a pair is omitted when its value is unset *or* its
profile flag is empty (a harness without that knob). Config validation makes
model and effort mandatory per template — the agent's cost/quality knobs are
pinned in config, never left to float on a vendor default — so in
engine-launched boxes both values are always present; the omission paths serve
direct sequencer invocations and knob-less harnesses. The working directory is
the workspace; the environment is
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
