# Change Proposal: Session transcripts and session-resuming re-entry

## Context

The agent's `$HOME` (`/home/box`) is a tmpfs dropped with the `--rm`
container, so the harness's native session state — the full transcript of
the agent's reasoning, tool use, and per-message token usage — vanishes
when the box exits. The agent's stdout is streamed to the container log
and never written to disk. Nothing faber persists lets an operator answer
"where did the tokens go" or "how did the agent reason", which is exactly
what tuning context/prelude hooks and skills for token efficiency
requires. The advisory `usage.json` carries totals at best, never the
trace.

Interactive re-entry has the same gap: `resume --interactive` rebuilds
the failed step's box but replaces the entry program with a bare shell —
the session that produced the failure is gone, so the operator starts
from an empty harness in a reconstructed container.

This proposal builds on the invocation profile
(`2026-08-14-invoke-profile.md`): where the harness keeps its sessions
and how it resumes one are vendor dialect, so they are profile data,
never engine code.

## Proposed change

- **Profile fields.** The invocation profile gains two optional fields:
  - `session_dir` — the path, relative to the container `$HOME`, where
    the harness writes session transcripts.
  - `resume_argv` — the argv that resumes the harness's most recent
    session in that directory.
  Both are opaque to the engine. Config guidance (not enforcement): point
  `session_dir` at the narrow transcript subpath, not the harness's whole
  state directory — wider paths can carry checkpoint copies of the
  workspace and drag repo-class I/O through the host bind.
- **Capture: a live per-attempt host bind.** When session saving is
  enabled and the template's profile defines `session_dir`, the runspec
  binds a host directory — `<runDir>/boxes/<step>/attempt-<n>/sessions/`
  — read-write at `$HOME/<session_dir>`, pre-owned to the run uid like
  the result dir. Consequences, all by construction:
  - *Step isolation*: each attempt gets its own empty directory; a box
    never sees another step's sessions, exactly as today.
  - *Strict identification*: run/step/attempt addressing falls out of
    the existing attempt-dir layout.
  - *Crash safety*: the transcript is on the host the moment the harness
    writes it; a container that dies mid-step leaves the record behind —
    the most valuable case for analysis.
  - *No workspace impact*: `/workspace` stays an anonymous native
    volume; only append-scale transcript writes cross the bind, beside
    the result-dir bind every box already has.
- **Enablement.** `--sessions` on `faber run`, recorded in the journal
  header so resume inherits it; `faber resume --sessions` enables it for
  the remainder of a run that started without it. Off by default —
  transcripts are an observability artifact of a run, not workflow
  semantics, so the toggle is per-run, not in `orchestrator.yaml`.
- **Interactive re-entry into the session.** When the failed step's
  attempt has a saved session and the profile defines `resume_argv`,
  re-entry copies the saved session into the reconstructed container's
  tmpfs `$HOME` at `session_dir` and launches `resume_argv` instead of
  `/bin/sh` — the operator lands inside the conversation that produced
  the failure, with the handoff mounted as today. The archived record is
  copied, never bound: the ephemeral session may diverge freely and the
  host record stays immutable. A `--shell` flag keeps the bare-shell
  behavior; when no session or no `resume_argv` exists, the shell is the
  fallback. Re-entry scope stays failed-only.
- **Analysis is out of scope.** The engine's job ends at durable,
  step-addressed transcripts on the host; reading them — token
  accounting, prompt archaeology, feeding a meter — is user-side work
  over the run directory.

## Impact expectation

- **config**: `session_dir` / `resume_argv` profile fields and their
  validation; `--sessions` on run and resume; header field
  documentation.
- **agent**: runspec sessions mount (reserved-path checks, ownership),
  carried on the resolved profile.
- **pipeline**: sessions directory lifecycle in the box attempt flow
  (survives the pre-attempt scrub of its own attempt dir only); reentry
  session copy and `resume_argv` entry with the `--shell` fallback.
- **failure**: sessions flag in the journal header; interactive target
  plumbing for the saved-session location.
- **spec/docs**: box-lifecycle, recovery-modes, and profile leaves; test
  sections for agent and pipeline; `docs/commands.md`.
