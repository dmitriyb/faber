# BindingSet — deterministic composition of per-step bindings

## What it is

The single assembly point for the security surface of one step: it takes
the per-step contributions of NetworkBinding, RemoteBinding,
IdentityBinding, and CredentialBroker, adds the isolation-runtime knob, and
hands infra's ContainerRunner four things — an ordered docker-run argv
fragment, the pre-run setup that already happened, the attach to run beside
the container while it lives, and the post-run teardown that must happen. ContainerRunner is the only place a `docker run`
argv is ever constructed, and it consumes the BindingSet's fragment
verbatim: security decides *what* the flags are; infra decides only where
they sit around image, mounts, and command.

## Composition order

Fixed and deterministic: **network, remote, identity, credentials,
runtime**. Each binding's contribution is internally ordered too, so the
same resolved step always yields the same argv fragment byte-for-byte —
golden-testable, diffable in logs, and stable under map iteration. Setup
hooks run in that same order (network preflight, host-key read, agent
spawn + key load, resolver calls / secrets-payload encode); teardown hooks
run in exact reverse and run *always* — after the container exits, after a
setup failure partway through (unwinding only what was set up), and on
context cancellation. No production binding leaves per-attempt host state
behind today: the identity binding's relay ends with the container and its
role agents are run-scoped (stopped by `Close`), and file mode's tokens live
only in the container tmpfs and die with it — the guarantee holds for
whatever a binding contributes. Teardown errors are collected and surfaced
together, never short-circuited.

## The attach

A binding whose mechanism lives beside the running container contributes an
`Attach(ctx, container)`; the set runs the contributed attaches concurrently
as one `Assembled.Attach`, joining their errors, and infra starts it with
the container and cancels it when the container ends. Today the identity
binding is the one contributor: it serves the signing proxy over `docker
exec` into the box's relay, so the box reaches its agent through no shared
host path. An attach failure is logged by the runner and never decides the
step; the box records the consequence itself (no agent at its signing
phase).

## The runtime knob

An optional `runtime` setting — template-level, overriding a
workflow-level default — maps to `--runtime=<value>` (e.g. `runsc` for
gVisor on Linux). It is the last contribution in the fragment, it changes
only the argv, and it never affects the image: the same tag runs under
either runtime, preserving "the image is a pure function of the toolset".
Platforms whose default runtime already provides VM isolation simply omit
it. This knob deliberately stays optional — the trust model treats the
container boundary as expendable (the gate and the egress lock are the
walls), so hardening it is a preference, not a pillar.

## The check composition

`faber check` runs a workflow's hooks for real and lets them reach
nothing. It is the same set in the same order with three members
substituted and the fourth absent (`NewCheckBindingSet`): the network
member contributes `--network none` whatever the config's section says —
companion services go with it, by intent; the remote member mounts the
check's named volume (`faber-check-<id>-remote`) read-write at
`/faber/remote` and, on a repository's first use, seeds it through a short
container of the step's own image running `faber-box seed-remote <repo>`
with a `git bundle` of the working directory's repository on its stdin —
nothing of the host is mounted — so the box's one remote is a local path
(no host-key policy applies to one) and its pushes land where nothing else
looks; the
identity member is the real IdentityBinding with every role resolved to one
key generated for the check, in path form — neither the registry, the
locator nor the operator's own keys are consulted, and commits are signed
with it through the same proxy and queue; there is no credential broker at
all. The runtime knob stays. The box learns the mode from one variable
(`FABER_CHECK`, the agent module's); the sandbox itself is entirely this
composition.

## Failure semantics

A binding's setup failure fails the step *before* any container exists:
the step gets a structured failure result (the failure module's contract)
whose error names the binding and the cause — missing network, unreadable
host key, resolver non-zero exit, agent spawn failure. Nothing is retried
inside the BindingSet; retry is a step-level concern, and each attempt
gets a completely fresh assembly (new agent, fresh resolver invocation,
fresh secrets payload), which is what makes between-attempt cleanup sound.

## Boundary notes

The BindingSet is per-step and stateless across steps: no pooling, no
caching of resolved secrets or spawned agents. Concurrency safety follows
from that — parallel steps compose disjoint resources (private socket
dirs, per-step temp files) and can only collide on the shared docker
network, which is read-only from faber's side. Bindings whose
configuration is absent (no `remote:`, no credentials, no runtime)
contribute nothing and appear nowhere in the fragment; an empty BindingSet
is legal and yields an empty fragment.

Requirements implemented: Isolation runtime knob.
