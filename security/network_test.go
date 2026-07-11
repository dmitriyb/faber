package security

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func networkBinding(networks map[string]bool) (*NetworkBinding, *fakeDocker) {
	docker := &fakeDocker{networks: networks}
	return NewNetworkBinding(docker, slog.New(slog.DiscardHandler)), docker
}

// Verifies 6e6d0bb46819: proxy mode emits the network attach and the proxy
// environment in the fixed internal order, with the NO_PROXY list joined in
// declared YAML order.
func TestNetworkProxyModeFragment(t *testing.T) {
	b, _ := networkBinding(map[string]bool{"agents-internal": true})
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{
		"--network", "agents-internal",
		"-e", "HTTPS_PROXY=http://egress:8888",
		"-e", "HTTP_PROXY=http://egress:8888",
		"-e", "NO_PROXY=gateway,localhost,127.0.0.1",
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args:\nwant %q\ngot  %q", want, c.Args)
	}
	if c.Teardown != nil {
		t.Fatal("network binding must have nothing to tear down")
	}
}

// Verifies 6e6d0bb46819: the NO_PROXY list preserves declared order exactly
// and the well-known loopback exemptions are appended only when missing.
func TestNetworkNoProxyOrderAndLoopback(t *testing.T) {
	tests := []struct {
		name     string
		declared []string
		want     string
	}{
		{"declared order preserved", []string{"gateway", "localhost", "127.0.0.1"}, "gateway,localhost,127.0.0.1"},
		{"loopback appended when missing", []string{"svc-b", "svc-a"}, "svc-b,svc-a,localhost,127.0.0.1"},
		{"empty list still exempts loopback", nil, "localhost,127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := noProxyList(tt.declared); got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}
}

// Verifies 6e6d0bb46819: nftables mode (an explicit nftables: true) yields
// the network attach plus --cap-add NET_ADMIN and no proxy environment —
// test scenario 8.
func TestNetworkNftablesMode(t *testing.T) {
	b, _ := networkBinding(map[string]bool{"agents-internal": true})
	step := StepSpec{Network: &config.NetworkDef{Name: "agents-internal", Nftables: true}}
	c, err := b.Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"--network", "agents-internal", "--cap-add", "NET_ADMIN"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
}

// Verifies 6e6d0bb46819: the missing-network preflight applies in both modes
// and fails Prepare naming the network — test scenario 8's second half.
func TestNetworkMissingFailsPreflight(t *testing.T) {
	defs := []*config.NetworkDef{
		{Name: "agents-internal", Proxy: "http://egress:8888"},
		{Name: "agents-internal", Nftables: true},
	}
	for _, def := range defs {
		b, _ := networkBinding(nil)
		_, err := b.Prepare(context.Background(), StepSpec{Network: def})
		errContains(t, err, `"agents-internal"`)
		errContains(t, err, "does not exist")
	}
}

// Verifies 6e6d0bb46819 (finding 1 regression): the egress lock must not fail
// open — a populated network section with no name errors instead of silently
// running the step on the default bridge with unrestricted egress.
func TestNetworkUnnamedSectionFailsClosed(t *testing.T) {
	defs := map[string]*config.NetworkDef{
		"proxy without name":    {Proxy: "http://egress:8888"},
		"no_proxy without name": {NoProxy: []string{"gateway"}},
		"nftables without name": {Nftables: true},
	}
	for name, def := range defs {
		t.Run(name, func(t *testing.T) {
			b, docker := networkBinding(map[string]bool{"agents-internal": true})
			_, err := b.Prepare(context.Background(), StepSpec{Network: def})
			errContains(t, err, "default bridge")
			if len(docker.netCalls) != 0 {
				t.Fatalf("no preflight may run for an invalid section, got %q", docker.netCalls)
			}
		})
	}
}

// Verifies 6e6d0bb46819 (finding 3 regression): NET_ADMIN is never granted by
// omission — a network def with neither proxy nor nftables fails closed, and
// one with both is rejected rather than picking a mode.
func TestNetworkModeMustBeExplicit(t *testing.T) {
	t.Run("neither mode", func(t *testing.T) {
		b, _ := networkBinding(map[string]bool{"agents-internal": true})
		_, err := b.Prepare(context.Background(), StepSpec{Network: &config.NetworkDef{Name: "agents-internal"}})
		errContains(t, err, "neither proxy nor nftables")
	})
	t.Run("both modes", func(t *testing.T) {
		b, _ := networkBinding(map[string]bool{"agents-internal": true})
		def := &config.NetworkDef{Name: "agents-internal", Proxy: "http://egress:8888", Nftables: true}
		_, err := b.Prepare(context.Background(), StepSpec{Network: def})
		errContains(t, err, "exactly one egress mode")
	})
}

// Verifies 6e6d0bb46819: a step with no network section contributes nothing.
func TestNetworkAbsentContributesNothing(t *testing.T) {
	b, docker := networkBinding(nil)
	c, err := b.Prepare(context.Background(), StepSpec{})
	if err != nil || len(c.Args) != 0 {
		t.Fatalf("want empty contribution, got %q err %v", c.Args, err)
	}
	if len(docker.netCalls) != 0 {
		t.Fatalf("no preflight expected, got %q", docker.netCalls)
	}
}

// Verifies 989ba0fa773c (first pass only): faber stays blind to the egress
// allow-list — assembling the network binding makes exactly one docker call
// (the existence preflight) and consults nothing about the proxy beyond the
// configured URL. No cross-check of endpoint needs exists.
func TestNetworkAllowListStaysOpaque(t *testing.T) {
	b, docker := networkBinding(map[string]bool{"agents-internal": true})
	if _, err := b.Prepare(context.Background(), implementStep(t.TempDir())); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := []string{"agents-internal"}; !slices.Equal(docker.netCalls, want) {
		t.Fatalf("docker calls: want %q, got %q", want, docker.netCalls)
	}
}
