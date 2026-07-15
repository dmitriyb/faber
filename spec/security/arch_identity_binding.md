# IdentityBinding — an ephemeral single-key ssh-agent per step

## What it is

The mechanism behind "one key per container ⇒ role isolation by
construction". Each template declares an `identity`; for every step launched
from that template, faber spawns a fresh ssh-agent on a private socket,
loads exactly one key for that identity, forwards only the socket into the
box, and destroys the agent the moment the step ends. The same key signs
commits and authenticates SSH, so one fingerprint is one role — and the
user's gate maps fingerprint to role server-side. Faber guarantees the
"one key" half; enforcement of what that key may do is the gate's.

## Lifecycle (per step attempt)

1. **Spawn.** `ssh-agent -a <sock>` with the socket in a fresh private
   directory (0700, owned by faber's user, under the run's scratch area).
   No step ever shares an agent — concurrent steps under the same identity
   get separate agents, so teardown of one cannot orphan another.
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
4. **Contribute.** `-v <sock>:/ssh-agent -e SSH_AUTH_SOCK=/ssh-agent`. On
   platforms where the forwarded socket's group ownership blocks the box's
   non-root user (the macOS VM case), the binding adds the documented
   group-membership flag. Optionally, when the identity declares a public
   key file, `-e` with the public key line — otherwise the box's prelude
   derives the signing key by listing the forwarded agent (`ssh-add -L`).
5. **Teardown.** Kill the agent process and remove the socket directory.
   This runs on every exit path — success, step failure, setup failure of a
   later binding, context cancellation — via the BindingSet's reverse-order
   teardown guarantee. A leaked agent is a leaked credential handle, so
   teardown failures are surfaced loudly, never swallowed.

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
And the agent never outlives its step: identity is scoped to
one container's lifetime, which is what makes a step's credential exposure
exactly as durable as the step.

Requirements implemented: Identity binding role keys.
