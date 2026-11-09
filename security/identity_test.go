package security

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh/agent"
)

// identityBinding wires a binding to the fake agent and a fresh fake relay
// exec (returned, so tests can attach through it).
func identityBinding(t *testing.T, agent *fakeAgent, logBuf *bytes.Buffer) (*IdentityBinding, *fakeRelayExec) {
	t.Helper()
	var logger *slog.Logger
	if logBuf != nil {
		logger = slog.New(slog.NewTextHandler(logBuf, nil))
	} else {
		logger = slog.New(slog.DiscardHandler)
	}
	b := NewIdentityBinding(agent, logger)
	relay := newFakeRelayExec(t)
	b.Exec = relay
	return b, relay
}

// proxyKeyComments dials the relay's socket the way a box's ssh would and
// lists what the attached proxy exposes, by key comment.
func proxyKeyComments(t *testing.T, sock string) []string {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", sock, err)
	}
	defer conn.Close()
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		t.Fatalf("list via proxy: %v", err)
	}
	var out []string
	for _, k := range keys {
		out = append(out, k.Comment)
	}
	return out
}

// Verifies e47a00273f03: after Prepare and the attach the box's agent
// socket answers with exactly one key — the step's own identity, not any
// other configured one — and the contribution is the agent tmpfs, the env
// var and the attach, nothing else: no mount, no teardown — test scenario 2.
func TestIdentityOneKeyPerBox(t *testing.T) {
	agent := newFakeAgent()
	b, relay := identityBinding(t, agent, nil)
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !slices.Equal(c.Args, identityArgs) {
		t.Fatalf("args:\nwant %q\ngot  %q", identityArgs, c.Args)
	}
	if c.Teardown != nil {
		t.Fatal("nothing is created on the host per attempt; the contribution must carry no teardown")
	}
	sock, stop := attachRelay(t, relay, c.Attach, "box-1")
	if got := proxyKeyComments(t, sock); len(got) != 1 || got[0] != "implementer" {
		t.Fatalf("the box's agent must expose exactly the implementer key, got %q", got)
	}
	stop()
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("role agent still live after Close: %q", live)
	}
}

// Verifies e47a00273f03: two steps prepared concurrently under different
// identities attach to distinct relays, each seeing only its own key — no
// step ever shares an agent — test scenario 2. Run under -race.
func TestIdentityConcurrentStepsGetDisjointAgents(t *testing.T) {
	agent := newFakeAgent()
	b, relay := identityBinding(t, agent, nil)
	steps := []StepSpec{implementStep(t.TempDir()), mergeStep(t.TempDir())}
	contribs := make([]Contribution, len(steps))
	var wg sync.WaitGroup
	for i, step := range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := b.Prepare(context.Background(), step)
			if err != nil {
				t.Errorf("Prepare %s: %v", step.NodeID, err)
				return
			}
			contribs[i] = c
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	sockA, stopA := attachRelay(t, relay, contribs[0].Attach, "box-a")
	defer stopA()
	sockB, stopB := attachRelay(t, relay, contribs[1].Attach, "box-b")
	defer stopB()
	if sockA == sockB {
		t.Fatal("concurrent steps must get distinct sockets")
	}
	if got := proxyKeyComments(t, sockA); len(got) != 1 || got[0] != "implementer" {
		t.Fatalf("implement box sees %q", got)
	}
	if got := proxyKeyComments(t, sockB); len(got) != 1 || got[0] != "merger" {
		t.Fatalf("merge box sees %q", got)
	}
	// Two roles, two run-scoped agents: alive until the run closes them.
	if live := agent.liveAgents(); len(live) != 2 {
		t.Fatalf("want two role agents live until Close, got %q", live)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("agents leaked past Close: %q", live)
	}
}

// Verifies e47a00273f03: zero keys after load is a hard error (the loader
// failed silently) and the just-spawned agent is torn down — test scenario 2.
func TestIdentityZeroKeysFailsAndCleansUp(t *testing.T) {
	agent := newFakeAgent()
	agent.listFn = func(string) ([]string, error) { return nil, nil }
	b, _ := identityBinding(t, agent, nil)
	_, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	errContains(t, err, "no key")
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("failed Prepare leaked an agent: %q", live)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("want exactly one spawn, got %d", len(agent.starts))
	}
	if _, serr := os.Stat(filepath.Dir(agent.starts[0])); !os.IsNotExist(serr) {
		t.Fatal("failed Prepare must remove the socket directory")
	}
}

// Verifies e47a00273f03: more than one key logs a warning naming the extra
// fingerprints (role isolation degraded) but does not abort, since a
// hardware-backed loader may legitimately surface adjacent credentials —
// test scenario 2.
func TestIdentityMultipleKeysWarnsWithFingerprints(t *testing.T) {
	agent := newFakeAgent()
	agent.listFn = func(string) ([]string, error) {
		return []string{"256 SHA256:fp-one (ED25519)", "256 SHA256:fp-two (ED25519)"}, nil
	}
	var logBuf bytes.Buffer
	b, _ := identityBinding(t, agent, &logBuf)
	if _, err := b.Prepare(context.Background(), implementStep(t.TempDir())); err != nil {
		t.Fatalf("Prepare must not abort on >1 key: %v", err)
	}
	log := logBuf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "fp-one") || !strings.Contains(log, "fp-two") {
		t.Fatalf("want a warning naming both fingerprints, got %q", log)
	}
}

// Verifies e47a00273f03: a key-load failure after the agent spawned tears the
// agent down before Prepare returns — the error path leaks nothing.
func TestIdentityKeyLoadFailureStopsAgent(t *testing.T) {
	agent := newFakeAgent()
	agent.addErr = errBoom
	b, _ := identityBinding(t, agent, nil)
	_, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	errContains(t, err, "load identity key")
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("failed Prepare leaked an agent: %q", live)
	}
	if len(agent.stops) != 1 {
		t.Fatalf("want exactly one Stop, got %d", len(agent.stops))
	}
}

// Verifies e47a00273f03 (test scenario 16): the attach waits for the
// container through the running query and then execs the relay exactly
// once, with a stdin attached; cancelling it ends the attach; an exec that
// fails while the container runs surfaces its exit; one that ends with the
// container gone is no failure; a binding with no exec seam fails the
// attach naming it.
func TestIdentityAttachLifecycle(t *testing.T) {
	agent := newFakeAgent()
	b, relay := identityBinding(t, agent, nil)
	relay.notRunning = 2
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer b.Close(context.Background())
	_, stop := attachRelay(t, relay, c.Attach, "box-1")
	stop()
	relay.mu.Lock()
	polls, execs, stdinNil := relay.polls, relay.execs, relay.stdinNil
	relay.mu.Unlock()
	if polls < 3 {
		t.Fatalf("the attach must poll until the container runs, got %d polls", polls)
	}
	if len(execs) != 1 || !slices.Equal(execs[0], []string{"/faber/bin/faber-box", "relay"}) || stdinNil {
		t.Fatalf("execs = %q (stdin nil: %v), want one relay exec with stdin", execs, stdinNil)
	}

	failing := newFakeRelayExec(t)
	failing.execCode, failing.execStderr = 126, "exec: relay: permission denied"
	b.Exec = failing
	err = c.Attach(context.Background(), "box-2")
	errContains(t, err, "exited 126")

	// The relay dying with its container is every run's ordinary end, not
	// a failure of the attach.
	gone := newFakeRelayExec(t)
	gone.execCode, gone.goneAfter = 137, true
	b.Exec = gone
	if err := c.Attach(context.Background(), "box-gone"); err != nil {
		t.Fatalf("a relay killed with its container must not fail the attach: %v", err)
	}

	b.Exec = nil
	err = c.Attach(context.Background(), "box-3")
	errContains(t, err, "no docker seam")
}

// Verifies e47a00273f03: a template with no identity contributes nothing —
// no agent is ever spawned, no attach.
func TestIdentityAbsentContributesNothing(t *testing.T) {
	agent := newFakeAgent()
	b, _ := identityBinding(t, agent, nil)
	c, err := b.Prepare(context.Background(), StepSpec{Identity: nil, ScratchDir: t.TempDir()})
	if err != nil || len(c.Args) != 0 || c.Teardown != nil || c.Attach != nil {
		t.Fatalf("want empty contribution, got %q err %v", c.Args, err)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("no agent must be spawned, got %q", agent.starts)
	}
}

// Verifies e47a00273f03 (sun_path regression): the role agent's socket path
// is short and independent of the scratch dir's depth — a deep-but-legal
// project layout must not push it past the ~104–108 byte Unix socket path
// cap that made ssh-agent fail with "unix_listener: path too long".
func TestIdentitySocketPathShortEvenWithDeepScratch(t *testing.T) {
	t.Setenv("TMPDIR", "") // measure the code, not an ambient deep TMPDIR override
	agent := newFakeAgent()
	b, _ := identityBinding(t, agent, nil)
	deep := filepath.Join(t.TempDir(), strings.Repeat("deep-scratch-segment/", 8), "attempt-1", "scratch")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Prepare(context.Background(), implementStep(deep)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer b.Close(context.Background())
	sock := agent.starts[0]
	if len(sock) > 100 {
		t.Fatalf("socket path %q is %d bytes — over the sun_path headroom", sock, len(sock))
	}
	if strings.HasPrefix(sock, deep) {
		t.Fatalf("socket %q must not derive from the scratch dir", sock)
	}
}

// Verifies e47a00273f03 (sun_path guard): a deep TMPDIR override reproduces
// the overflow the temp dir exists to avoid — Prepare must fail closed with
// an error naming TMPDIR before any agent is spawned, leaving no temp dir
// behind.
func TestIdentityDeepTMPDIRFailsClosed(t *testing.T) {
	root := filepath.Join(os.TempDir(), "faber-deep-tmpdir-test")
	deep := filepath.Join(root, strings.Repeat("deep-tmpdir-segment/", 6))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMPDIR", deep)
	agent := newFakeAgent()
	b, _ := identityBinding(t, agent, nil)
	_, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	errContains(t, err, "TMPDIR")
	if len(agent.starts) != 0 {
		t.Fatalf("no agent must be spawned past the path-length guard, got %v", agent.starts)
	}
	entries, rerr := os.ReadDir(deep)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "faber-agent-") {
			t.Fatalf("path-length refusal left the temp dir behind: %s", e.Name())
		}
	}
}

// Verifies e47a00273f03 (finding 5 regression): the internal failure teardown
// runs on a context detached from step cancellation — when Prepare fails
// under an already-cancelled step ctx, the agent's Stop still sees a live
// context (graceful SIGTERM window) rather than an immediate deadline kill.
func TestIdentityFailureTeardownDetachedFromCancel(t *testing.T) {
	agent := newFakeAgent()
	agent.addErr = errBoom // fail after the agent is spawned
	b, _ := identityBinding(t, agent, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the step is already cancelled when the failure path runs
	_, err := b.Prepare(ctx, implementStep(t.TempDir()))
	errContains(t, err, "load identity key")
	if len(agent.stopCtx) != 1 {
		t.Fatalf("want exactly one Stop, got %d", len(agent.stopCtx))
	}
	if agent.stopCtx[0] != nil {
		t.Fatalf("teardown ran under the cancelled step context: %v", agent.stopCtx[0])
	}
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("agent leaked: %q", live)
	}
}
