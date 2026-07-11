package security

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	b := NewCredentialBroker(resolver, logger)
	b.isTmpfs = func(string) (bool, error) { return true, nil }
	return b
}

func serviceStep(scratch string, services map[string]config.ServiceDef) StepSpec {
	return StepSpec{NodeID: "task/implement", Services: services, ScratchDir: scratch}
}

// Verifies 0c5bc0f678b7: with agent-api in file mode the resolver runs
// host-side exactly once, the fragment carries the read-only mount and no env
// var carrying the token, and the mount source is a 0600 file holding the
// token inside the scratch dir — test scenario 5.
func TestBrokerFileModeContainment(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	var logBuf bytes.Buffer
	b := testBroker(resolver, &logBuf)
	scratch := t.TempDir()
	c, err := b.Prepare(context.Background(), serviceStep(scratch, map[string]config.ServiceDef{"agent-api": {Mode: "file"}}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	src := filepath.Join(scratch, "agent-api")
	want := []string{"-v", src + ":/run/secrets/agent-api:ro"}
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
	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("mount source: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mount source mode: want 0600, got %o", info.Mode().Perm())
	}
	content, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != testToken {
		t.Fatalf("mount source content mismatch")
	}
	// Degraded mode is deliberately noisy.
	if log := logBuf.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "agent-api") {
		t.Fatalf("want a degraded-path warning naming the service, got %q", log)
	}
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, serr := os.Stat(src); !os.IsNotExist(serr) {
		t.Fatal("teardown must remove the token file")
	}
}

// Verifies 0c5bc0f678b7: a non-tmpfs scratch dir refuses assembly — the raw
// token must never touch disk — test scenario 5.
func TestBrokerNonTmpfsScratchRefused(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	b := testBroker(resolver, nil)
	b.isTmpfs = func(string) (bool, error) { return false, nil }
	_, err := b.Prepare(context.Background(), serviceStep(t.TempDir(), map[string]config.ServiceDef{"agent-api": {Mode: "file"}}))
	errContains(t, err, "not tmpfs-backed")
	if resolver.callCount() != 0 {
		t.Fatal("no token may be resolved for an unusable scratch dir")
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
// exists, names the binding's service but never stdout content, and shreds
// any file material already written for earlier services.
func TestBrokerResolverFailureShredsEarlierFiles(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken), errFor: map[string]error{"beta-api": errors.New("exit 1")}}
	b := testBroker(resolver, nil)
	var shredded []string
	realShred := b.shred
	b.shred = func(path string) error {
		shredded = append(shredded, path)
		return realShred(path)
	}
	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), serviceStep(scratch, map[string]config.ServiceDef{
		"agent-api": {Mode: "file"}, // sorts first, succeeds
		"beta-api":  {Mode: "file"}, // fails
	}))
	errContains(t, err, "beta-api")
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token content: %q", err.Error())
	}
	first := filepath.Join(scratch, "agent-api")
	if !slices.Contains(shredded, first) {
		t.Fatalf("earlier file %q must be shredded on failure, shredded: %q", first, shredded)
	}
	if _, serr := os.Stat(first); !os.IsNotExist(serr) {
		t.Fatal("earlier token file must be gone after failed Prepare")
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

// Verifies 0c5bc0f678b7 (finding 2 regression): a token write that fails
// after touching the filesystem — a partial write on a size-limited tmpfs
// (ENOSPC) — is shredded before Prepare returns, even though the file never
// reached the teardown list; nothing raw survives the failed attempt.
func TestBrokerFailedWriteStillShredded(t *testing.T) {
	resolver := &fakeResolver{token: []byte(testToken)}
	b := testBroker(resolver, nil)
	b.writeFile = func(path string, data []byte, perm os.FileMode) error {
		// Emulate ENOSPC mid-write: half the token lands, then the write fails.
		if err := os.WriteFile(path, data[:len(data)/2], perm); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}
	var shredded []string
	realShred := b.shred
	b.shred = func(path string) error {
		shredded = append(shredded, path)
		return realShred(path)
	}
	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), serviceStep(scratch, map[string]config.ServiceDef{"agent-api": {Mode: "file"}}))
	errContains(t, err, "agent-api")
	errContains(t, err, "write token file")
	partial := filepath.Join(scratch, "agent-api")
	if !slices.Contains(shredded, partial) {
		t.Fatalf("partial write must be shredded, shredded: %q", shredded)
	}
	if _, serr := os.Stat(partial); !os.IsNotExist(serr) {
		t.Fatal("partial token file must be gone after the failed write")
	}
}

// Verifies 0c5bc0f678b7: shredFile overwrites the file's content with zeros
// before removing it (observed through an independently held descriptor) —
// the instrumented half of test scenario 3's shred assertion.
func TestShredZeroesBeforeRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := shredFile(path); err != nil {
		t.Fatalf("shredFile: %v", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatal("shred must remove the file")
	}
	content, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != len(testToken) || !bytes.Equal(content, make([]byte, len(testToken))) {
		t.Fatalf("content must be zeroed before removal, got %q", content)
	}
	// Idempotent on the second call: nothing left to leak.
	if err := shredFile(path); err != nil {
		t.Fatalf("shredFile on absent file: %v", err)
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
