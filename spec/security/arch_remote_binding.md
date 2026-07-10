# RemoteBinding — the pinned gateway as the box's only git remote

## What it is

The component that tells the box where its repository lives and how to trust
that answer. From the workflow-level `remote:` section it derives the clone
URL the box will use and the host-key policy material that makes the SSH
connection fail closed. It contributes environment only — the actual clone
happens inside the box, over the identity binding's forwarded agent, across
the network binding's internal net.

## Contribution

- **Clone URL.** The configured URL prefix (`ssh://git@<host>/<path>`) plus
  the step's resolved `repo` input, suffixed `.git`, exported as the box's
  remote-URL env. `repo` is a run-time param — never baked into an image,
  optional at the workflow level, per-step overridable. When no repo param
  reaches the step, the binding contributes nothing and the box's clone
  phase is skipped by contract.
- **Host-key policy**, exactly one of three modes:
  - **pinned** (`host_key_file`): faber reads the gateway's public host-key
    line host-side and exports it; the box installs it in `known_hosts` and
    connects with `StrictHostKeyChecking=yes`. Any mismatch — including a
    first contact that doesn't match — fails closed. This is the production
    mode. An unreadable or empty key file fails the step before launch.
  - **tofu** (`tofu: true`): the box uses `accept-new`, trusting the first
    host key it sees. Sandbox-only, explicit opt-in, mutually exclusive with
    `host_key_file` (the loader rejects both).
  - **abort**: if a step needs the remote and neither mode is configured,
    the box's host-key policy phase aborts before any network contact. There
    is no silent default.

## The gate is not faber's

Everything trust-relevant about what happens *at* the remote is the user's
gate service, a companion container faber never learns the name of. Push
validation (signed commits verified against allowed signers, key-fingerprint
to role mapping, per-role content rules, default-branch protection),
holding the upstream forge credential, forwarding accepted refs, and
mediating pull-request actions — all of it is server-side policy behind the
git URL. Faber's whole contract with the gate is: the box can reach exactly
one git remote, authenticated by exactly one forwarded key, and whatever the
gate rejects surfaces in the box as an ordinary failed push with the gate's
structured rejection text in the step's error detail. Faber never holds the
forge credential and cannot: it only ever sees `ssh://git@…`.

This boundary is also what makes the box expendable: a fully compromised
agent can push garbage, and the worst case is a rejected push.

## Deferred seam: gateless trust boundary

A workflow with no push target — a research run producing only its typed
result — has nothing for a gate to validate, so "the gate is the wall"
stops describing the trust boundary. Backlog: define what the boundary *is*
for gateless runs and where terminal outputs durably live when there is no
repository to receive them. The first pass simply allows the configuration:
`remote:` may be absent, repo-less steps skip cloning, outputs live in the
step's typed result payload (journaled host-side), and the egress lock
remains the only enforced boundary.

Requirements implemented: Remote binding pinned gateway; Deferred: gateless
trust boundary.
