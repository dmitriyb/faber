package security

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// fakeLocator is a KeyLocator keyed by fingerprint, recording every lookup.
type fakeLocator struct {
	byFP    map[string]string
	lookups []string
}

func (f *fakeLocator) Locate(_ context.Context, fingerprint string) (string, error) {
	f.lookups = append(f.lookups, fingerprint)
	if src, ok := f.byFP[fingerprint]; ok {
		return src, nil
	}
	return "", errNoMatch
}

var errNoMatch = &ValidationError{msg: "no match"} // any non-nil error; kind unused here

// Verifies b145ab5182f4 (scenario 10): an explicit path key resolves verbatim
// with no registry read and no locator call.
func TestResolveExplicitPathVerbatim(t *testing.T) {
	loc := &fakeLocator{byFP: map[string]string{fpA: "/should/not/be/used"}}
	reg := Registry{"implementer": {Fingerprint: fpA}}
	got, fp, err := ResolveIdentity(context.Background(), reg, loc, "implementer", config.IdentityDef{Key: "./keys/implementer"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "./keys/implementer" {
		t.Fatalf("explicit path must resolve verbatim, got %q", got)
	}
	if fp != "" {
		t.Fatalf("explicit path must pin no fingerprint, got %q", fp)
	}
	if len(loc.lookups) != 0 {
		t.Fatalf("explicit path must not call the locator, got %v", loc.lookups)
	}
}

// Verifies b145ab5182f4 (scenario 10): an inline SHA256: key resolves straight
// through the locator, skipping the role→fingerprint hop.
func TestResolveInlineFingerprint(t *testing.T) {
	loc := &fakeLocator{byFP: map[string]string{fpA: "/home/u/.ssh/id_ed25519"}}
	got, fp, err := ResolveIdentity(context.Background(), nil, loc, "unused-role", config.IdentityDef{Key: fpA})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "/home/u/.ssh/id_ed25519" {
		t.Fatalf("got %q", got)
	}
	if fp != fpA {
		t.Fatalf("inline fingerprint must be pinned, want %q got %q", fpA, fp)
	}
	if !slices.Equal(loc.lookups, []string{fpA}) {
		t.Fatalf("locator lookups: %v", loc.lookups)
	}
}

// Verifies b145ab5182f4 (scenario 10): an identity with no inline key resolves
// role → registry fingerprint → locator.
func TestResolveViaRegistry(t *testing.T) {
	loc := &fakeLocator{byFP: map[string]string{fpA: "/home/u/.ssh/id_impl"}}
	reg := Registry{"implementer": {Fingerprint: fpA}}
	got, fp, err := ResolveIdentity(context.Background(), reg, loc, "implementer", config.IdentityDef{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "/home/u/.ssh/id_impl" {
		t.Fatalf("got %q", got)
	}
	if fp != fpA {
		t.Fatalf("registry fingerprint must be pinned, want %q got %q", fpA, fp)
	}
}

// Verifies b145ab5182f4 (scenario 10): a role absent from the registry is an
// error naming the role; a fingerprint the locator cannot match is an error
// naming both role and fingerprint.
func TestResolveFailuresNameRoleAndFingerprint(t *testing.T) {
	loc := &fakeLocator{byFP: map[string]string{}}
	reg := Registry{"implementer": {Fingerprint: fpA}}

	_, _, err := ResolveIdentity(context.Background(), reg, loc, "ghost", config.IdentityDef{})
	errContains(t, err, "ghost")
	errContains(t, err, "not in registry")

	_, _, err = ResolveIdentity(context.Background(), reg, loc, "implementer", config.IdentityDef{})
	errContains(t, err, "implementer")
	errContains(t, err, fpA)
	errContains(t, err, "no local key matches")
}

// registryStep is a resolved step whose identity declares no inline key — the
// registry path. IdentityRole is the registry lookup key.
func registryStep(scratch string) StepSpec {
	s := implementStep(scratch)
	s.Identity = &config.IdentityDef{} // no inline key
	s.IdentityRole = "implementer"
	return s
}

// Verifies b145ab5182f4 (scenario 10): the identity binding resolves a no-inline-key
// identity through the registry+locator, and the agent then holds exactly that
// one located key.
func TestIdentityBindingResolvesViaRegistry(t *testing.T) {
	agent := newFakeAgent()
	// The located private key reports the pinned fingerprint — the correct pair.
	agent.fpBySource = map[string]string{"/keys/located-impl": fpA}
	b := identityBinding(agent, nil)
	b.Registry = Registry{"implementer": {Fingerprint: fpA}}
	b.Locator = &fakeLocator{byFP: map[string]string{fpA: "/keys/located-impl"}}

	scratch := t.TempDir()
	c, err := b.Prepare(context.Background(), registryStep(scratch))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sock := filepath.Join(scratch, "ssh-agent", "agent.sock")
	fps := agent.keys[sock]
	// fakeAgent labels the key by the keySource basename it was handed.
	if len(fps) != 1 || !strings.Contains(fps[0], "located-impl") {
		t.Fatalf("agent must hold exactly the located key, got %q", fps)
	}
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

// Verifies b145ab5182f4 (scenario 10): a located key whose loaded fingerprint
// differs from the role's pinned one (a stale, copied, or planted
// pub/private pair) fails Prepare closed, naming the role, the expected
// fingerprint, and what the agent actually holds — and tears the agent down.
func TestIdentityBindingRejectsFingerprintMismatch(t *testing.T) {
	agent := newFakeAgent()
	// The registry pins fpA, the locator finds a key, but the loaded key
	// reports fpB — the pub/private pairing was not the pinned key.
	agent.fpBySource = map[string]string{"/keys/wrong-half": fpB}
	b := identityBinding(agent, nil)
	b.Registry = Registry{"implementer": {Fingerprint: fpA}}
	b.Locator = &fakeLocator{byFP: map[string]string{fpA: "/keys/wrong-half"}}

	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), registryStep(scratch))
	errContains(t, err, "implementer")
	errContains(t, err, fpA)
	errContains(t, err, fpB)
	errContains(t, err, "not the pinned one")
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("fingerprint mismatch must tear the agent down, leaked: %q", live)
	}
	if _, serr := os.Stat(filepath.Join(scratch, "ssh-agent")); !os.IsNotExist(serr) {
		t.Fatal("fingerprint mismatch must remove the socket directory")
	}
}

// Verifies b145ab5182f4 (scenario 10): a role absent from the registry fails
// Prepare before any agent is spawned or socket dir created.
func TestIdentityBindingUnknownRoleFailsBeforeSpawn(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
	b.Registry = Registry{}
	b.Locator = &fakeLocator{byFP: map[string]string{}}

	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), registryStep(scratch))
	errContains(t, err, "implementer")
	errContains(t, err, "not in registry")
	if len(agent.starts) != 0 {
		t.Fatalf("no agent must be spawned on a resolution failure, got %v", agent.starts)
	}
	if _, serr := os.Stat(filepath.Join(scratch, "ssh-agent")); !os.IsNotExist(serr) {
		t.Fatal("resolution failure must not create the socket directory")
	}
}

// Verifies b145ab5182f4 (scenario 10): a fingerprint the locator cannot match
// fails Prepare before any spawn, naming role and fingerprint.
func TestIdentityBindingLocatorMissFailsBeforeSpawn(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
	b.Registry = Registry{"implementer": {Fingerprint: fpA}}
	b.Locator = &fakeLocator{byFP: map[string]string{}} // no match

	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), registryStep(scratch))
	errContains(t, err, "implementer")
	errContains(t, err, fpA)
	if len(agent.starts) != 0 {
		t.Fatalf("no agent must be spawned on a locator miss, got %v", agent.starts)
	}
}

// Verifies b145ab5182f4 (scenario 11): the real locator searches running agent,
// then ~/.ssh/*.pub (sorted), then YubiKey resident keys — first match wins.
func TestKeyLocatorSearchOrder(t *testing.T) {
	sshDir := t.TempDir()
	// Two pub keys; a private counterpart exists only for id_b so the sorted
	// walk (id_a before id_b) still yields a stable, existing match for id_b.
	writePub(t, sshDir, "id_a")
	writePub(t, sshDir, "id_b")
	privB := filepath.Join(sshDir, "id_b")
	if err := os.WriteFile(privB, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A readable file used as the agent listing's comment, so the running-agent
	// source is usable.
	agentKey := filepath.Join(t.TempDir(), "agent_id")
	if err := os.WriteFile(agentKey, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	var opened []string
	l := &keyLocator{
		logger: slog.New(slog.DiscardHandler),
		sshDir: sshDir,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			switch {
			case name == "ssh-add" && slices.Equal(args, []string{"-l"}):
				// The running agent holds fpA under a readable path comment.
				return "256 " + fpA + " " + agentKey + " (ED25519)\n", nil
			case name == "ssh-keygen" && len(args) == 2 && args[0] == "-lf":
				opened = append(opened, args[1])
				// Map each pub file to a distinct fingerprint; id_b → fpB.
				if strings.HasSuffix(args[1], "id_b.pub") {
					return "256 " + fpB + " comment (ED25519)\n", nil
				}
				return "256 SHA256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz other (ED25519)\n", nil
			}
			t.Fatalf("unexpected command: %s %v", name, args)
			return "", nil
		},
		residents: func(context.Context) ([]resident, error) {
			return []resident{{fingerprint: fpA, keySource: "resident-handle"}}, nil
		},
	}

	// fpA lives on the agent AND as a resident: the agent wins.
	got, err := l.Locate(context.Background(), fpA)
	if err != nil {
		t.Fatalf("locate agent key: %v", err)
	}
	if got != agentKey {
		t.Fatalf("agent source must win, got %q", got)
	}

	// fpB lives only under ~/.ssh: the private counterpart path is returned.
	got, err = l.Locate(context.Background(), fpB)
	if err != nil {
		t.Fatalf("locate pub key: %v", err)
	}
	if got != privB {
		t.Fatalf("pub-dir source: want %q, got %q", privB, got)
	}
	// Only .pub files were read by ssh-keygen; the private key was never opened
	// (only stat'd), so faber read no private material.
	for _, p := range opened {
		if !strings.HasSuffix(p, ".pub") {
			t.Fatalf("locator read a non-.pub file: %q", p)
		}
	}
}

// Verifies b145ab5182f4 (scenario 11): when the agent holds the fingerprint but
// its listing comment is not a readable path (the common case for keys added
// without a path comment, forwarded agents, gnome-keyring, or cardno:/user@host
// comments), the running-agent hop yields no match and resolution falls through
// to the ~/.ssh/*.pub private counterpart.
func TestKeyLocatorAgentNonPathCommentFallsThrough(t *testing.T) {
	sshDir := t.TempDir()
	writePub(t, sshDir, "id_a")
	privA := filepath.Join(sshDir, "id_a")
	if err := os.WriteFile(privA, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := &keyLocator{
		logger: slog.New(slog.DiscardHandler),
		sshDir: sshDir,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			switch {
			case name == "ssh-add" && slices.Equal(args, []string{"-l"}):
				// The agent holds fpA, but the comment is a non-path string.
				return "256 " + fpA + " user@host (ED25519)\n", nil
			case name == "ssh-keygen" && len(args) == 2 && args[0] == "-lf":
				return "256 " + fpA + " comment (ED25519)\n", nil
			}
			t.Fatalf("unexpected command: %s %v", name, args)
			return "", nil
		},
		residents: func(context.Context) ([]resident, error) { return nil, nil },
	}

	// fromAgent must reject the non-path comment outright.
	if got := l.fromAgent(context.Background(), fpA); got != "" {
		t.Fatalf("agent hop must yield no match on a non-path comment, got %q", got)
	}

	// End to end, resolution falls through to the ~/.ssh private counterpart.
	got, err := l.Locate(context.Background(), fpA)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if got != privA {
		t.Fatalf("want fall-through to %q, got %q", privA, got)
	}
}

// Verifies b145ab5182f4 (scenario 11): the resident source is reached only when
// neither the agent nor ~/.ssh matches.
func TestKeyLocatorResidentFallback(t *testing.T) {
	l := &keyLocator{
		logger: slog.New(slog.DiscardHandler),
		sshDir: t.TempDir(), // empty: no *.pub
		run: func(_ context.Context, name string, args ...string) (string, error) {
			// Empty agent (exit-1 semantics folded into "no lines").
			return "", nil
		},
		residents: func(context.Context) ([]resident, error) {
			return []resident{{fingerprint: fpA, keySource: "yubikey-handle"}}, nil
		},
	}
	got, err := l.Locate(context.Background(), fpA)
	if err != nil {
		t.Fatalf("resident locate: %v", err)
	}
	if got != "yubikey-handle" {
		t.Fatalf("want resident handle, got %q", got)
	}
}

func writePub(t *testing.T, dir, base string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, base+".pub"), []byte("ssh-ed25519 AAAA "+base), 0o644); err != nil {
		t.Fatal(err)
	}
}
