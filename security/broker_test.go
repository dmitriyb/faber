package security

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func testBroker(resolver Resolver, logBuf *bytes.Buffer) *CredentialBroker {
	var logger *slog.Logger
	if logBuf != nil {
		logger = slog.New(slog.NewTextHandler(logBuf, nil))
	} else {
		logger = slog.New(slog.DiscardHandler)
	}
	return NewCredentialBroker(resolver, logger)
}

func serviceStep(scratch string, services map[string]config.ServiceDef) StepSpec {
	return StepSpec{NodeID: "task/implement", Services: services, ScratchDir: scratch}
}

// Verifies 0c5bc0f678b7: with agent-api in file mode the resolver runs
// host-side exactly once, the fragment carries exactly one --tmpfs
// /run/secrets and no -v mount and no env var carrying the token, the token
// rides Contribution.Secrets, no host file is written anywhere, and there is
// no teardown — test scenario 5.
func TestBrokerFileModeContainment(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	var logBuf bytes.Buffer
	b := testBroker(resolver, &logBuf)
	scratch := t.TempDir()
	c, err := b.Prepare(context.Background(), serviceStep(scratch, map[string]config.ServiceDef{"agent-api": {Mode: "file"}}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"--tmpfs", "/run/secrets"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
	for _, a := range c.Args {
		if strings.Contains(a, testToken) {
			t.Fatalf("fragment carries the raw token: %q", a)
		}
	}
	if resolver.callCount() != 1 {
		t.Fatalf("resolver must run exactly once, ran %d times", resolver.callCount())
	}
	// The token lives only on the Contribution's Secrets map, and its formatted
	// form is redacted.
	if got := c.Secrets["agent-api"].String(); got != redacted {
		t.Fatalf("secret not redacted in formatting: %q", got)
	}
	if string(c.Secrets["agent-api"].reveal()) != testToken {
		t.Fatalf("Secrets map does not carry the resolved token")
	}
	// No host file is written anywhere under the scratch dir.
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("file mode wrote host files: %v", entries)
	}
	if c.Teardown != nil {
		t.Fatal("file mode leaves no host residue; teardown must be nil")
	}
	// Degraded mode is deliberately noisy.
	if log := logBuf.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "agent-api") {
		t.Fatalf("want a degraded-path warning naming the service, got %q", log)
	}
}

// Verifies 0c5bc0f678b7: two file-mode services yield exactly one --tmpfs
// /run/secrets and a two-key Secrets map — the tmpfs is emitted once — test
// scenario 5.
func TestBrokerFileModeSingleTmpfs(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	b := testBroker(resolver, nil)
	c, err := b.Prepare(context.Background(), serviceStep(t.TempDir(), map[string]config.ServiceDef{
		"agent-api": {Mode: "file"},
		"other-api": {Mode: "file"},
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := []string{"--tmpfs", "/run/secrets"}; !slices.Equal(c.Args, want) {
		t.Fatalf("args: want exactly one --tmpfs, got %q", c.Args)
	}
	if len(c.Secrets) != 2 || c.Secrets["agent-api"].reveal() == nil || c.Secrets["other-api"].reveal() == nil {
		t.Fatalf("want a two-key Secrets map, got %v", c.Secrets)
	}
}

// Verifies 0c5bc0f678b7: switching the service to proxy mode invokes no
// resolver and emits only the base-URL env — faber never touches the secret
// at all — test scenario 5.
func TestBrokerProxyModeNeverResolves(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	b := testBroker(resolver, nil)
	c, err := b.Prepare(context.Background(), serviceStep(t.TempDir(),
		map[string]config.ServiceDef{"agent-api": {Mode: "proxy", Endpoint: "http://tokens:8080/anthropic"}}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"-e", "FABER_SERVICE_AGENT_API_URL=http://tokens:8080/anthropic"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
	if resolver.callCount() != 0 {
		t.Fatalf("proxy mode must not invoke the resolver, ran %d times", resolver.callCount())
	}
	if c.Teardown != nil {
		t.Fatal("proxy mode has nothing to tear down")
	}
}

// Verifies 0c5bc0f678b7: helper mode forwards the declaration's fields as
// FABER_HELPER_<NAME>_* env for the box hooks to install.
func TestBrokerHelperModePassthrough(t *testing.T) {
	b := testBroker(nil, nil)
	c, err := b.Prepare(context.Background(), serviceStep(t.TempDir(),
		map[string]config.ServiceDef{"git-lfs": {Mode: "helper", Endpoint: "http://helper:9000"}}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"-e", "FABER_HELPER_GIT_LFS_ENDPOINT=http://helper:9000"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
}

// Verifies 0c5bc0f678b7: services are walked in sorted name order so the
// fragment stays deterministic under map iteration.
func TestBrokerSortedServiceOrder(t *testing.T) {
	b := testBroker(nil, nil)
	c, err := b.Prepare(context.Background(), serviceStep(t.TempDir(), map[string]config.ServiceDef{
		"zeta":  {Mode: "proxy", Endpoint: "http://z"},
		"alpha": {Mode: "proxy", Endpoint: "http://a"},
		"mid":   {Mode: "helper", Endpoint: "http://m"},
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{
		"-e", "FABER_SERVICE_ALPHA_URL=http://a",
		"-e", "FABER_HELPER_MID_ENDPOINT=http://m",
		"-e", "FABER_SERVICE_ZETA_URL=http://z",
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
}

// Verifies 0c5bc0f678b7: a resolver failure fails the step before a container
// exists, names the binding's service but never stdout content, and leaves no
// host file behind — there is nothing to shred because nothing is ever written
// to disk.
func TestBrokerResolverFailureNoHostResidue(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken), errFor: map[string]error{"beta-api": errors.New("exit 1")}}
	b := testBroker(resolver, nil)
	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), serviceStep(scratch, map[string]config.ServiceDef{
		"agent-api": {Mode: "file"}, // sorts first, succeeds (token stays in RAM)
		"beta-api":  {Mode: "file"}, // fails
	}))
	errContains(t, err, "beta-api")
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token content: %q", err.Error())
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("no host file may be written in file mode, found: %v", entries)
	}
}

// Verifies 0c5bc0f678b7: fail-closed validation — unknown modes, missing
// endpoints, unusable service names, and file mode without a resolver all
// fail the step before launch.
func TestBrokerFailClosedValidation(t *testing.T) {
	tests := []struct {
		name     string
		resolver Resolver
		services map[string]config.ServiceDef
		want     string
	}{
		{"unknown mode", nil, map[string]config.ServiceDef{"svc": {Mode: "env"}}, "unknown credential mode"},
		{"proxy without endpoint", nil, map[string]config.ServiceDef{"svc": {Mode: "proxy"}}, "requires an endpoint"},
		{"helper without endpoint", nil, map[string]config.ServiceDef{"svc": {Mode: "helper"}}, "requires an endpoint"},
		{"file without resolver", nil, map[string]config.ServiceDef{"svc": {Mode: "file"}}, "none is configured"},
		{"unusable service name", &fakeResolver{token: []byte("t")}, map[string]config.ServiceDef{"../svc": {Mode: "file"}}, "invalid name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBroker(tt.resolver, nil)
			_, err := b.Prepare(context.Background(), serviceStep(t.TempDir(), tt.services))
			errContains(t, err, tt.want)
		})
	}
}

// Verifies 0c5bc0f678b7 (finding 4 regression): service names outside the
// closed charset fail closed in every mode, naming the service — a name like
// "tok:ro" must never reach a -v mount spec or an env-var-name position and
// surface as an opaque docker error instead. The resolver is never consulted
// for an invalid name.
func TestBrokerServiceNameCharsetEnforced(t *testing.T) {
	tests := []struct {
		name    string
		service string
		def     config.ServiceDef
	}{
		{"file mode with mount-option injection", "tok:ro", config.ServiceDef{Mode: "file"}},
		{"proxy mode with equals sign", "bad=name", config.ServiceDef{Mode: "proxy", Endpoint: "http://x"}},
		{"helper mode with space", "has space", config.ServiceDef{Mode: "helper", Endpoint: "http://x"}},
		{"proxy mode with uppercase", "Bad", config.ServiceDef{Mode: "proxy", Endpoint: "http://x"}},
		{"file mode with leading dash", "-svc", config.ServiceDef{Mode: "file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &fakeResolver{token: []byte(testToken)}
			b := testBroker(resolver, nil)
			_, err := b.Prepare(context.Background(), serviceStep(t.TempDir(), map[string]config.ServiceDef{tt.service: tt.def}))
			errContains(t, err, tt.service)
			errContains(t, err, "invalid name")
			if resolver.callCount() != 0 {
				t.Fatalf("resolver must not run for an invalid service name, ran %d times", resolver.callCount())
			}
		})
	}
}

// Verifies 0c5bc0f678b7: no declared services means no contribution and no
// teardown.
func TestBrokerAbsentServicesContributeNothing(t *testing.T) {
	b := testBroker(nil, nil)
	c, err := b.Prepare(context.Background(), StepSpec{ScratchDir: t.TempDir()})
	if err != nil || len(c.Args) != 0 || c.Teardown != nil {
		t.Fatalf("want empty contribution, got %q err %v", c.Args, err)
	}
}
