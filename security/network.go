package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/dmitriyb/faber/infra"
)

// wellKnownNoProxy are the exemptions the binding guarantees regardless of
// declaration, so intra-container loopback traffic never detours through the
// proxy. Declared entries keep their YAML order; these are appended only when
// missing.
var wellKnownNoProxy = []string{"localhost", "127.0.0.1"}

// NetworkBinding makes "the box has no route out" true — topologically, not
// by inspecting traffic (requirement 6e6d0bb46819). It attaches the step
// container to the workflow's named internal docker network and, in the
// default proxy mode, points every well-behaved client at the user's
// allow-listing egress proxy. The proxy, its allow-list, and every companion
// service on the network are user-owned and opaque: faber never reads the
// allow-list, never validates it, and a denied endpoint surfaces as an
// ordinary in-box failure (the deferred allow-list-ownership seam,
// requirement 989ba0fa773c, first pass: faber stays blind).
//
// Alternative mode — an explicit nftables: true — is the baked-nftables
// image: the contribution shrinks to the network attach plus NET_ADMIN (the
// image's root entrypoint loads its immutable rule set, then drops to the
// non-root agent user); no proxy environment is emitted. The rule content is
// build-side user config, never generated here. The two modes are mutually
// exclusive and one must be chosen; the loader enforces both rules and
// Prepare fails closed on defs that somehow bypass it — NET_ADMIN is never
// granted by omission, and an unnamed-but-populated network section never
// silently runs the step on the default bridge.
type NetworkBinding struct {
	docker infra.DockerClient
	logger *slog.Logger
}

// NewNetworkBinding wires the network preflight to the typed docker seam —
// the only docker call any binding makes.
func NewNetworkBinding(docker infra.DockerClient, logger *slog.Logger) *NetworkBinding {
	return &NetworkBinding{docker: docker, logger: childLogger(logger, "network-binding")}
}

// Name implements Binding.
func (b *NetworkBinding) Name() string { return "network" }

// Prepare verifies the named network exists (it is created out-of-band with
// the user's companion services; a missing network fails the step before
// launch) and emits the network attach plus, in proxy mode, the proxy
// environment.
func (b *NetworkBinding) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	n := step.Network
	if n == nil {
		return Contribution{}, nil
	}
	if n.Name == "" {
		if n.Proxy == "" && len(n.NoProxy) == 0 && !n.Nftables {
			return Contribution{}, nil // network section absent
		}
		// Fail closed: an unnamed-but-populated section means the egress
		// lock was intended, and skipping it would run the step on the
		// default bridge with unrestricted egress.
		return Contribution{}, errors.New("network section has no name; refusing to run on the default bridge")
	}
	exists, err := b.docker.NetworkExists(ctx, n.Name)
	if err != nil {
		return Contribution{}, fmt.Errorf("network preflight %q: %w", n.Name, err)
	}
	if !exists {
		return Contribution{}, fmt.Errorf("network %q does not exist; create it (with its companion services) before running", n.Name)
	}
	args := []string{"--network", n.Name}
	switch {
	case n.Proxy != "" && n.Nftables:
		// The loader rejects this; fail closed anyway rather than pick one.
		return Contribution{}, errors.New("both proxy and nftables configured; exactly one egress mode is allowed")
	case n.Nftables:
		// nftables mode (explicit opt-in): the root entrypoint needs
		// NET_ADMIN to load the baked rules; all proxy environment is
		// omitted.
		return Contribution{Args: append(args, "--cap-add", "NET_ADMIN")}, nil
	case n.Proxy != "":
		return Contribution{Args: append(args,
			"-e", "HTTPS_PROXY="+n.Proxy,
			"-e", "HTTP_PROXY="+n.Proxy,
			"-e", "NO_PROXY="+noProxyList(n.NoProxy),
		)}, nil
	default:
		return Contribution{}, fmt.Errorf("network %q declares neither proxy nor nftables; an egress mode must be chosen explicitly", n.Name)
	}
}

// noProxyList joins the declared exemptions in YAML order and appends the
// well-known loopback names when absent.
func noProxyList(declared []string) string {
	list := slices.Clone(declared)
	for _, req := range wellKnownNoProxy {
		if !slices.Contains(list, req) {
			list = append(list, req)
		}
	}
	return strings.Join(list, ",")
}

// childLogger derives a component logger, tolerating a nil root the same way
// infra does.
func childLogger(logger *slog.Logger, component string) *slog.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return logger.With("component", component)
}
