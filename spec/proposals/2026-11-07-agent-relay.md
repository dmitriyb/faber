# Change Proposal: the role agent over an in-box relay on `docker exec` — no mounted socket, no socket group, contract v3

## Context

After the framed-outputs change the box shares one host path: the identity
binding's per-attempt agent socket, bind mounted at `/ssh-agent`
(`-v <tmpdir>/agent.sock:/ssh-agent -e SSH_AUTH_SOCK=/ssh-agent`). The socket
is a 0600 file owned by faber's host user. Inside a box that drops to
`FABER_RUN_UID` it is reachable only when that uid is the host user's, which is
exactly what userns-remap breaks (container 1000 is host 101000), and what the
macOS docker VM already mislabels, hence the `agent_socket_group` host knob
(`--group-add 0`) and the `FABER_AGENT_SOCKET_GID` env the preamble keeps
across its `setgroups`. The knob is a per-platform patch over a shared host
path; under remap there is no group to add.

The host already reaches a running container through a channel that needs no
shared path: `docker exec -i`, which the re-entry choreography uses for
`faber-box receive`. The agent protocol is a strict request–reply stream of
length-prefixed messages, so it carries over a pair of pipes as well as over a
unix socket. Settled with the operator: the proxy stays on the host, the socket
moves into the box, and a relay the box itself serves joins the two over an
exec; the knob and the env go.

## Proposed change

- **The relay (agent/box, `faber-box relay`).** A third verb beside `hold` and
  `receive`, started by the host through `docker exec -i <container>
  /faber/bin/faber-box relay` as soon as the container runs. It listens on
  `/faber/agent/agent.sock` (a tmpfs the identity binding mounts), chowns the
  directory and the socket to `FABER_RUN_UID:FABER_RUN_GID` from the
  container's own environment, drops to that user, and then pumps messages:
  for each client connection, read one agent message (4-byte big-endian
  length, at most 256 KiB, then the body), write it to its stdout, read one
  reply from its stdin, write it to the client. One message exchange at a
  time across all connections (a mutex around the pair), so the single stream
  to the host stays request–reply aligned; a client that sends a malformed
  length is disconnected, the others are not. EOF on stdin ends the relay, and
  the exec ends with the container. Nothing but length-checked bytes crosses
  it: no key material in either direction, as before.
- **The host end (security).** `IdentityBinding.Prepare` no longer creates a
  per-attempt socket directory, listener or mount. It returns the same
  `signProxy{keys, upstream, queue}` as the contribution's **attach**: a
  function `Attach(ctx, container) error` that waits until the container is
  running, execs the relay with two pipes, and serves the proxy on them with
  `agent.ServeAgent` (`x/crypto/ssh/agent`, which takes any `io.ReadWriter`)
  until the exec ends or the context is cancelled. The contribution's args
  become `--tmpfs /faber/agent -e SSH_AUTH_SOCK=/faber/agent/agent.sock`,
  byte-stable across attempts: the one attempt-unique host path in the
  fragment is gone, and so are `--group-add` and `FABER_AGENT_SOCKET_GID`. The
  role agents (one per role per run, the sign queue, `Close`) are unchanged.
  `Contribution` and `Assembled` gain `Attach`; the set composes the
  contributed attaches into one that runs them concurrently and joins their
  errors. `SocketGroup` and `EnvAgentSocketGID` are removed;
  `ContainerAgentSocket` moves under `/faber`, which is already reserved, so
  `/ssh-agent` leaves the reserved paths.
- **Attach as a run-primitive input (infra).** `ContainerSpec.Attach
  func(ctx, container string) error`. `Run` starts it in a goroutine beside
  `docker run` and cancels it when the run returns; `Enter` starts it right
  after the held box is up and cancels it before the kill. `DockerClient`
  gains `Running(ctx, name) (bool, error)` — `docker inspect --format
  '{{json .State.Running}}'`, "No such container" is false — and the package
  offers `ExecWhenRunning`, which polls `Running` and then calls `Exec` once,
  so the attach never matches daemon prose and never feeds stdin to an exec
  that failed to start. Both are pure argv plus a streaming call, like the
  verbs beside them. The attach's error is logged, never the run's result: a
  box without its relay fails its own signing phase and says so in the record.
- **The box waits for its socket (agent/box).** With `SSH_AUTH_SOCK` set and a
  remote to clone, the clone phase first waits for the socket to appear,
  bounded (30 s); past the bound the step fails with the signing reason
  naming the missing relay. The preamble's `setgroups` drops to the run group
  alone: `runUserGroups`, `AgentSocketGID` and its parsing go. The signing
  phase (`ssh-add -L` over `SSH_AUTH_SOCK`) is untouched. `ContractVersion`
  becomes 3: an image baked before this change expects a mounted socket and
  would fail late, so it is refused at start with "run faber build".
- **The host knob goes (config, cmd, docs).** `agent_socket_group` leaves
  `HostConfig`; a `host.json` that still carries it is refused with a message
  naming it retired and what replaced it (the strict decoder would otherwise
  say "unknown key"). The wiring hands the identity binding the docker client
  for its execs. The macOS socket-group paragraphs in `deploy.md` and
  `commands.md` go; the 2026-08-04 proposal stays as history.
- **Re-entry and check.** Re-entry sets the same attach on its held box, so the
  operator's shell has a working `SSH_AUTH_SOCK`; `faber check`'s identity
  member wraps the real binding and attaches the same way.
- **Tests.** security: the proxy served over a pipe pair to `agent.NewClient`
  (list, sign, read-only refusals, the sign queue) in place of the unix
  socket; the golden fragment with the tmpfs and the env and no `-v`, now
  byte-identical across attempts; an attach against a fake docker whose exec
  speaks the relay's half in-process; the retired-knob refusal. agent/box: the
  relay against an in-process host (two concurrent clients over the socket,
  one aligned stream, a malformed client disconnected alone); the clone
  phase's wait and its bound; the lifecycle fixture unchanged (it points
  `SSH_AUTH_SOCK` at a real agent). infra: `Running`'s argv and decode,
  `ExecWhenRunning` against a fake that is not running for the first calls,
  `Run` and `Enter` starting and cancelling the attach. realinfra: a held box
  on a nix image with openssh, the real binding attached, `ssh-add -L` inside
  the box as the run user listing exactly the throwaway key, and a signature
  made through it verifying against it.

## Impact expectation

- **security**: requirements "Identity binding role keys" (the socket is the
  box's own, served through a relay; nothing host-owned is shared) and
  "Serialized signing" (unchanged in substance, the queue sits behind the same
  proxy); components IdentityBinding, BindingSet; leaves
  `arch_identity_binding.md` (steps 4–7), `impl_bindings.md` (the identity
  row, `Attach`, the determinism paragraph), `arch_binding_set.md`,
  `flow_binding_assembly.md`, `test_security_bindings.md` (1–3, 12–15, new
  relay scenarios).
- **agent**: requirement "Fixed box phase order" (the preamble's group drop,
  the clone phase's wait), component PhaseSequencer; leaves
  `arch_phase_sequencer.md`, `impl_phase_sequencing.md`,
  `test_box_lifecycle.md`; the contract's version.
- **infra**: requirements "Container run primitive" (the attach) and "Typed
  subprocess interfaces" (`Running`, `ExecWhenRunning`); components
  ContainerRunner, ExecAdapters; leaves `arch_container_runner.md`,
  `impl_run_argv.md`, `arch_exec_adapters.md`, `impl_exec_adapters.md`,
  `test_infra.md`.
- **config**: `arch_cli.md`, `impl_cli.md` (`host.json` without the knob, the
  retired-key refusal); `/ssh-agent` leaves the reserved container paths.
- **pipeline**: `impl_scheduling.md` (the attach set on the attempt's spec and
  on re-entry's held box).
- **project**: the security requirement's wording ("an ssh-agent socket" is
  now the relay).
- **docs**: `deploy.md` (platform table, identities), `commands.md`
  (`host.json` fields).
- **cmd**: the wiring passes the docker client to the identity binding and
  drops the knob.
