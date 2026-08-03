# Change Proposal: Explicit host inputs — no configuration from process env

## Context

Faber read four host-side knobs from its process environment:
`FABER_STATE_DIR`, `FABER_BOX_BIN`, `FABER_GIT_NAME`, `FABER_GIT_EMAIL` (and
a fifth was about to join for the identity binding's socket group). For a
security orchestrator this is the wrong channel: ambient environment is
assembled invisibly — shell rc files, wrappers, parent processes — cannot be
audited before a run, and is inherited by everything. `FABER_BOX_BIN` is the
sharpest case: it selects the binary that runs as root inside every box, and
silent substitution through env is precisely the attack shape to refuse. The
gate-side precedent already got this right (portitor's gate-integrity config
is file-only with env explicitly excluded); faber's host side must match.

## Proposed change

Every host input moves to an explicit, auditable file; all `FABER_*` host env
reads are deleted. (The `FABER_*` names inside the BOX are unaffected — that
is the run contract faber itself constructs and injects per container.)

1. **Committer identity is role state.** The role registry (`roles.json`)
   entry gains `git_name`/`git_email`; `faber add-key` gains the flags. A
   forge verifies a signature only when the committer email belongs to the
   account owning the key, so the email is a property of the role's key
   binding — it lives beside the fingerprint. The wiring hands pipeline a
   role→identity map; each box receives ITS role's identity through the box
   contract env. The gated-step guard (proposal 2026-08-03-gated-committer-
   email) is unchanged in behavior: a role with no registered email aborts
   the signing phase instead of inventing an address.
2. **Machine knobs are host-config state.** New `host.json` beside
   `roles.json`: `box_bin` (absolute path required), `agent_socket_group`
   (the identity binding's `--group-add` escape for VM-mislabeled sockets;
   typically `0` on macOS, unset on Linux), `state_dir`. Strict decode
   (unknown keys refused), absent file = all defaults, malformed file
   refuses the invocation. The effective config is logged at run start so
   what a run uses is visible.

## Impact expectation

- **security**: registry Entry/AddKey/list output + validation; identity
  binding docs (socket group sourced from host config via the wiring).
- **pipeline**: `AgentBoxes.GitIdentities` (role→identity map) replaces the
  global GitName/GitEmail pair.
- **config**: `HostConfig` loader; `add-key` CLI flags.
- **cmd/faber**: wiring reads host.json + registry; all host env reads gone.
- **docs**: commands.md env table replaced by host-configuration section;
  deploy.md state-dir/box-bin/commit-identity/macOS notes reworked;
  gated examples register the email via add-key.
