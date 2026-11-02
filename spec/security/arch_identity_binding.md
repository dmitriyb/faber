# IdentityBinding — one single-key ssh-agent per role, one signing proxy per step

## What it is

The mechanism behind "one key per container ⇒ role isolation by
construction". Each template declares an `identity`; per run, the first
step of that identity makes faber spawn one ssh-agent on a private socket
and load exactly one key for it; every step of that identity then receives
its own private socket, served by faber's agent-protocol proxy onto that
agent, forwarded into the box as the only credential channel. The same key
signs commits and authenticates SSH, so one fingerprint is one role — and
the user's gate maps fingerprint to role server-side. Faber guarantees the
"one key" half; enforcement of what that key may do is the gate's.

The proxy exists for one reason: a hardware token is a single device. With
N boxes each holding their own agent on the same SK key, two boxes cloning
at the same instant make the token refuse the second (`agent refused
operation`) and that box dies before its first hook. Every proxy's `Sign`
passes one process-wide mutex — the sign queue — so the token never sees
two requests at once. With no-touch keys the queue is the entire fix.

## Lifecycle

Two scopes: the role agent lives for the run, the proxy for the attempt.

1. **Spawn (once per role per run).** On the identity's first attempt,
   `ssh-agent -a <sock>` with the socket in a fresh private temp directory
   (0700, owned by faber's user, `MkdirTemp` under the system temp dir —
   deliberately NOT under the run's scratch area: a Unix socket path is
   capped at ~104–108 bytes (`sun_path`), and the scratch path scales with
   the project directory, run id, and node id, so a deep-but-legal layout
   overflows the cap and the agent cannot bind). The agent is keyed by the
   registry role (or by the key source for a path-form identity) and reused
   by every later attempt of that role; concurrent attempts of different
   roles get different agents. `MkdirTemp` honors `TMPDIR`, so a deep
   override would reproduce the overflow; the binding guards the composed
   path against a ~100-byte budget and fails closed with an error naming
   `TMPDIR` rather than surfacing ssh-agent's cryptic bind failure.
2. **Resolve + load.** Exactly one key for the template's identity. The key
   source is resolved first (see "Resolving the key", below): either the
   config's explicit `identities.<name>.key` path, or — when the template
   declares an identity with no inline key — the RoleRegistry turns the role
   name into a fingerprint and the `KeyLocator` turns that fingerprint into
   a live host key. Either way the result is one opaque key source, loaded
   into the agent. The material may be a file key (sandbox) or a
   hardware-resident key whose private half never leaves the device
   (production) — the binding cannot tell, the box cannot tell, and neither
   needs to: only the fingerprint matters.
3. **Verify.** List the agent's keys. Zero keys is a hard error (the
   resolver failed silently). More than one key logs a warning naming the
   extra fingerprints — role isolation is degraded because the box could
   sign as either — but does not abort, since a hardware-backed loader may
   legitimately surface adjacent credentials. When resolution pinned a
   fingerprint (any branch but an explicit path), the binding also asserts
   the agent now holds *that* fingerprint and fails closed otherwise: the
   locator resolves a `.pub` to its private counterpart by naming convention,
   so a stale, copied, or planted pair whose private half differs is caught
   here — at prepare time — rather than late at the gate after a wasted run.
   After verification the binding connects to the role agent over its
   socket as an agent-protocol client and lists its keys: the single key a
   well-formed agent holds is what boxes will see; if a loader surfaced
   extra credentials, only the pinned fingerprint's key is exposed, so the
   degraded case narrows rather than widens.
4. **Proxy (per attempt).** A second private temp directory and socket,
   same `sun_path` guard, on which faber serves the agent protocol itself
   (`x/crypto/ssh/agent`'s `ServeAgent`): `List` answers the role's key,
   `Sign`/`SignWithFlags` forward to the role agent's client under the sign
   queue, `Add`/`Remove`/`RemoveAll`/`Lock`/`Unlock` are refused and
   extensions are unsupported — a box can sign as its role and do nothing
   else to the agent. The socket is 0600 like ssh-agent's own. No key
   material crosses faber: the protocol carries challenges and signatures.
5. **Contribute.** `-v <sock>:/ssh-agent -e SSH_AUTH_SOCK=/ssh-agent`. On
   platforms where the forwarded socket's group ownership blocks the box's
   non-root user (the macOS VM case), the binding adds `--group-add
   <SocketGroup>` **and passes the same gid to the box as `-e
   FABER_AGENT_SOCKET_GID=<SocketGroup>`**; the wiring fills the field from the
   host config file's `agent_socket_group` (see the explicit-host-inputs
   proposal — an explicit per-machine file, never process environment), and
   empty means neither is emitted. The env var is load-bearing, not redundant
   with `--group-add`: docker admits the still-root box to the group, but the
   box's privileged preamble later calls `setgroups`, which *replaces* the
   supplementary set — so the preamble must be told which group to keep, or the
   drop strips the grant and the box can no longer open the forwarded socket
   (see the phase-sequencer preamble). `SocketGroup` is therefore a numeric gid,
   so the box can hand it to `setgroups` directly. Optionally, when the identity declares a public
   key file, `-e` with the public key line — otherwise the box's prelude
   derives the signing key by listing the forwarded agent (`ssh-add -L`).
6. **Teardown (per attempt).** Close the proxy listener and remove its
   directory. This runs on every exit path — success, step failure, setup
   failure of a later binding, context cancellation — via the BindingSet's
   reverse-order teardown guarantee. A spawn that fails before a proxy
   exists (load, verify, connect) stops the just-spawned agent on the same
   detached context.
7. **Close (per run).** The cmd wiring calls `BindingSet.Close` once the
   run returns; the identity binding stops every role agent and removes
   its directory. A leaked agent is a leaked credential handle, so both
   teardown and close failures are surfaced loudly, never swallowed.

## Resolving the key: fingerprint before path

A template names an identity; the key behind it is resolved host-side,
before the agent is asked to hold anything, with explicit paths taking
precedence over the registry so existing configs never change behavior:

- **Explicit path** (`identities.<name>.key: ./keys/implementer`) — used
  verbatim, exactly as today. No registry lookup. Every current config and
  every `examples/` config takes this branch and is byte-identical.
- **Explicit fingerprint** (`identities.<name>.key: SHA256:…`) — resolved
  straight through the `KeyLocator`, skipping the role→fingerprint hop.
- **No inline key** — the role name is looked up in the RoleRegistry
  (`role → fingerprint`), then the `KeyLocator` finds the matching live key
  (running ssh-agent, then `~/.ssh/*.pub`, then YubiKey resident keys) and
  yields its key source.

Resolution reads only public material — agent listings, `.pub` files, token
metadata — never a private key into faber's memory. A role absent from the
registry, or a fingerprint with no matching local key, fails Prepare with a
clear error naming the role and the fingerprint, before any container
launches. The registry, its `add-key`/`list-keys` operations, and the
locator are specified in `arch_role_registry.md`.

## What never happens

The private key never enters the container: not as a file, not as env, not
readable through the socket (an agent answers signing challenges; it does
not export keys). Faber never verifies what a key is *for*: its registry
maps a role *name* to a fingerprint (which key), never a fingerprint to a
server-side role or permission (what that key may do), and it does no
signature checking — that reverse table and that policy belong to the gate.
And a box's agent never outlives its step: the proxy is scoped to one
container's lifetime, so a step's signing window is exactly as durable as
the step; the role agent behind it outlives the step but is reachable only
through faber's proxies, never shared, and dies with the run.

Requirements implemented: Identity binding role keys, Serialized signing.
