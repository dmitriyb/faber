# Change Proposal: Gated steps require an explicit committer email

## Context

The signing phase configures the box committer identity from `FABER_GIT_NAME`/
`FABER_GIT_EMAIL`, falling back to the synthetic `faber-<identity>` /
`faber-<identity>@box.invalid`. On a gated step the synthetic email is a silent
misconfiguration: commits sign correctly and pass the user's gate (its check is
key-based), but a forge that ties signature verification to a registered account
email (e.g. GitHub's "Verified" badge) can never resolve `@box.invalid` to an
account, so every commit surfaces as Unverified. Nothing fails until a human
notices the badge — the run itself completes green.

## Proposed change

The signing phase (phase 6, `configureSigning`) fails fast on a gated step when
no committer email is supplied: if `RemoteURL` is set and `FABER_GIT_EMAIL`
resolves empty, the box aborts with a signing-phase error naming the missing
variable, instead of falling back to `@box.invalid`. The synthetic email
fallback disappears (it was reachable only on gated steps — gateless steps skip
the signing phase entirely); the synthetic *name* fallback `faber-<identity>`
stays. Faber stays policy-free: it does not validate the email against any
forge, it only refuses to invent one.

## Impact expectation

- **agent**: PhaseSequencer signing phase — behavior change (fail-fast), spec
  leaves `arch_phase_sequencer.md`, `impl_phase_sequencing.md`,
  `test_box_lifecycle.md`; `configureSigning` in `agent/box/box.go` + tests.
- **docs**: `docs/deploy.md`, `docs/commands.md` env-var table.
- No config-module change: `FABER_GIT_EMAIL` remains a host-env input, not a
  schema field (template env cannot carry it — the `FABER_` namespace is
  engine-owned and screened at validate time).
