# AgentInvoker — headless skill invocation

## What it is

Phase 8 of the box: exactly one headless invocation of the agent CLI from the
template's pinned package set. Everything before it is deterministic setup;
everything after it is deterministic extraction; this is the only
nondeterministic phase, and it is atomic — there is no resuming into an
agent's chain of thought, only re-running the whole step.

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
            [--effort <FABER_EFFORT>] [--max-budget-usd <FABER_MAX_BUDGET>]
```

Effort level and budget bound are pass-throughs from config; when unset the
flags are omitted. The working directory is the workspace; the environment is
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
