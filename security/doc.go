// Package security is faber's run-time security surface: the per-step binding
// contributions that make every real control external to the container. It
// produces the ordered, pre-tokenized docker-run argv fragment that infra's
// ContainerRunner splices verbatim between its engine flags and the image tag
// (RunSpec.Bindings), plus the setup/teardown lifecycle hooks that must
// bracket the container's life.
//
// Four bindings and one knob compose, in a fixed order, into a BindingSet:
//
//   - NetworkBinding — the egress lock: attach to a named internal docker
//     network and point clients at the user's allow-listing proxy (or, in the
//     alternative nftables mode, grant NET_ADMIN for a baked rule set).
//   - RemoteBinding — the pinned gateway: the box's only git remote URL and
//     the host-key policy material (pinned fail-closed, or sandbox TOFU).
//   - IdentityBinding — an ephemeral single-key ssh-agent per step, with only
//     the socket forwarded and guaranteed teardown on every exit path.
//   - CredentialBroker — credential handles, never contents: proxy endpoints,
//     helper config passthrough, and the degraded opt-in raw-token file mode
//     (streamed over the container's stdin into a container tmpfs, 0600, dies
//     with the container — no host file, no shred).
//   - the isolation runtime knob — an optional --runtime=<value> flag.
//
// Everything opinionated — the proxy's allow-list, the gateway's push
// validation, what a resolver command does — is a user-owned companion
// service or opaque command; this package contributes mechanism only.
//
// Secrets are opaque: resolver output is typed as Secret, whose every
// formatting and marshalling path yields "[redacted]". Raw bytes are
// reachable only inside this package, and only the stdin-payload encoder uses
// them, at the single moment of encoding the file-mode secrets payload.
package security
