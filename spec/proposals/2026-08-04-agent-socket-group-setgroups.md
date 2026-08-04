# Change Proposal: Keep the forwarded-agent socket group across the box's privilege drop

## Context

The identity binding forwards its ephemeral ssh-agent into the box as a bind-
mounted socket. On the macOS Docker VM the forwarded socket is presented
root-owned (`srw-rw---- root:root`), so the binding admits the box to the
socket's group with `--group-add <agent_socket_group>` (`0` on macOS; unset on
Linux, where the socket is owned by the box uid directly). This grant is applied
while the container is still root.

The box's privileged preamble (`enterRunUser`) then drops to the run user with
`setgroups([]int{RunGID})`, `setgid`, `setuid`. `setgroups` **replaces** the
supplementary set, so it strips the very group `--group-add` granted. The
dropped box (uid = host uid, gid = RunGID, groups = {RunGID}) can no longer open
the root-owned agent socket: `ssh-add -l` reports `Permission denied`, the clone
offers no key, and the box dies with `git@<gate>: Permission denied
(publickey)` → `clone-failed`, with every downstream step skipped. The binding's
own `--group-add` escape is silently undone a few instructions later by the
box's own `setgroups`. Linux is unaffected (the box uid owns the socket, so no
supplementary group is needed).

## Proposed change

Carry the socket gid to the box and preserve it across the drop.

1. **Security (emit).** When the identity binding emits `--group-add
   <SocketGroup>`, it also passes the same gid to the box as `-e
   FABER_AGENT_SOCKET_GID=<SocketGroup>` (a new box-contract env name owned by
   the security module, beside `SSH_AUTH_SOCK`). Empty `SocketGroup` ⇒ neither
   is emitted — byte-identical to today on Linux. `agent_socket_group` is a
   numeric gid so the box can name it to `setgroups` directly.
2. **Agent (preserve).** `BoxEnv` gains `AgentSocketGID int`, parsed from
   `FABER_AGENT_SOCKET_GID`; absent or blank ⇒ `-1` (none). The preamble builds
   its supplementary set as `{RunGID}`, plus `AgentSocketGID` when it is set
   (`>= 0`) and differs from `RunGID`, then calls `setgroups` with that list.
   Unset ⇒ `{RunGID}` alone, exactly as before.

The env var is load-bearing, not redundant with `--group-add`: docker admits the
still-root box to the group, but the preamble's `setgroups` replaces the set, so
the preamble must be told which group to keep. This is additive and box-
tolerated when absent, so it does not bump the contract version.

## Impact expectation

- **security**: `EnvAgentSocketGID` in `boxenv.go`; the identity binding's
  `Prepare` emits `-e FABER_AGENT_SOCKET_GID=<SocketGroup>` alongside
  `--group-add`; binding-assembly test asserts both flags appear together and
  neither appears when `SocketGroup` is empty.
- **agent**: `BoxEnv.AgentSocketGID` parse in the box env (`-1` sentinel); the
  preamble's `setgroups` group-list construction; box-lifecycle test covers the
  socket-gid-present and absent paths.
- **spec/docs**: phase-sequencer preamble leaves (arch + impl) and identity-
  binding leaves (arch + impl).
