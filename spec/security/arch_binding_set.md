# BindingSet — deterministic composition of per-step bindings

## What it is

The single assembly point for the security surface of one step: it takes
the per-step contributions of NetworkBinding, RemoteBinding,
IdentityBinding, and CredentialBroker, adds the isolation-runtime knob, and
hands infra's ContainerRunner three things — an ordered docker-run argv
fragment, the pre-run setup that already happened, and the post-run
teardown that must happen. ContainerRunner is the only place a `docker run`
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
run in exact reverse (kill the agent, nothing for credentials, remote, or
network) and run *always* — after the container exits, after a setup
failure partway through (unwinding only what was set up), and on context
cancellation. File mode leaves no host-side residue to undo — its tokens
live only in the container tmpfs and die with the container — so its
teardown is a no-op. Teardown errors are collected and surfaced together,
never short-circuited: a failed hook must not skip killing the agent.

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
