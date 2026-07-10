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
2. **Load.** Exactly one key for the template's identity, acquired through
   the user's resolver seam (the `identities:` declaration). The material
   may be a file key (sandbox) or a hardware-resident key whose private
   half never leaves the device (production) — the binding cannot tell, the
   box cannot tell, and neither needs to: only the fingerprint matters.
3. **Verify.** List the agent's keys. Zero keys is a hard error (the
   resolver failed silently). More than one key logs a warning naming the
   extra fingerprints — role isolation is degraded because the box could
   sign as either — but does not abort, since a hardware-backed loader may
   legitimately surface adjacent credentials.
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

## What never happens

The private key never enters the container: not as a file, not as env, not
readable through the socket (an agent answers signing challenges; it does
not export keys). Faber never verifies what a key is *for* — no
fingerprint-to-role table, no signature checking; that vocabulary belongs
to the gate. And the agent never outlives its step: identity is scoped to
one container's lifetime, which is what makes a step's credential exposure
exactly as durable as the step.

Requirements implemented: Identity binding role keys.
