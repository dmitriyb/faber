package security

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func identityBinding(agent *fakeAgent, logBuf *bytes.Buffer) *IdentityBinding {
	var logger *slog.Logger
	if logBuf != nil {
		logger = slog.New(slog.NewTextHandler(logBuf, nil))
	} else {
		logger = slog.New(slog.DiscardHandler)
	}
	return NewIdentityBinding(agent, logger)
}

// Verifies e47a00273f03: after Prepare the step's agent holds exactly one
// fingerprint — the step's own identity, not any other configured one — and
// only the socket is forwarded (mount + SSH_AUTH_SOCK, nothing else) — test
// scenario 2.
func TestIdentityOneKeyPerBox(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
	scratch := t.TempDir()
	c, err := b.Prepare(context.Background(), implementStep(scratch))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sock := filepath.Join(scratch, "ssh-agent", "agent.sock")
	want := []string{"-v", sock + ":/ssh-agent", "-e", "SSH_AUTH_SOCK=/ssh-agent"}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args:\nwant %q\ngot  %q", want, c.Args)
	}
	fps := agent.keys[sock]
	if len(fps) != 1 || !strings.Contains(fps[0], "implementer") {
		t.Fatalf("agent must hold exactly the implementer key, got %q", fps)
	}
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

// Verifies e47a00273f03: two steps prepared concurrently under different
// identities get distinct sockets, each agent seeing only its own key — no
// step ever shares an agent — test scenario 2. Run under -race.
func TestIdentityConcurrentStepsGetDisjointAgents(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
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
	sockA := filepath.Join(steps[0].ScratchDir, "ssh-agent", "agent.sock")
	sockB := filepath.Join(steps[1].ScratchDir, "ssh-agent", "agent.sock")
	if sockA == sockB {
		t.Fatal("concurrent steps must get distinct sockets")
	}
	if fps := agent.keys[sockA]; len(fps) != 1 || !strings.Contains(fps[0], "implementer") {
		t.Fatalf("implement agent keys: %q", fps)
	}
	if fps := agent.keys[sockB]; len(fps) != 1 || !strings.Contains(fps[0], "merger") {
		t.Fatalf("merge agent keys: %q", fps)
	}
	for i := range contribs {
		if err := contribs[i].Teardown(context.Background()); err != nil {
			t.Fatalf("Teardown %d: %v", i, err)
		}
	}
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("agents leaked: %q", live)
	}
}

// Verifies e47a00273f03: zero keys after load is a hard error (the loader
// failed silently) and the just-spawned agent is torn down — test scenario 2.
func TestIdentityZeroKeysFailsAndCleansUp(t *testing.T) {
	agent := newFakeAgent()
	agent.listFn = func(string) ([]string, error) { return nil, nil }
	b := identityBinding(agent, nil)
	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), implementStep(scratch))
	errContains(t, err, "no key")
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("failed Prepare leaked an agent: %q", live)
	}
	if _, serr := os.Stat(filepath.Join(scratch, "ssh-agent")); !os.IsNotExist(serr) {
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
	b := identityBinding(agent, &logBuf)
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare must not abort on >1 key: %v", err)
	}
	log := logBuf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "fp-one") || !strings.Contains(log, "fp-two") {
		t.Fatalf("want a warning naming both fingerprints, got %q", log)
	}
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

// Verifies e47a00273f03: a key-load failure after the agent spawned tears the
// agent down before Prepare returns — the error path leaks nothing.
func TestIdentityKeyLoadFailureStopsAgent(t *testing.T) {
	agent := newFakeAgent()
	agent.addErr = errBoom
	b := identityBinding(agent, nil)
	scratch := t.TempDir()
	_, err := b.Prepare(context.Background(), implementStep(scratch))
	errContains(t, err, "load identity key")
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("failed Prepare leaked an agent: %q", live)
	}
	if len(agent.stops) != 1 {
		t.Fatalf("want exactly one Stop, got %d", len(agent.stops))
	}
}

// Verifies e47a00273f03: teardown kills the agent and removes the socket
// directory, and the platform group-membership flag is emitted when
// configured (the macOS VM socket-ownership case).
func TestIdentityTeardownAndSocketGroup(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
	b.SocketGroup = "101"
	scratch := t.TempDir()
	c, err := b.Prepare(context.Background(), implementStep(scratch))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if i := slices.Index(c.Args, "--group-add"); i < 0 || c.Args[i+1] != "101" {
		t.Fatalf("want --group-add 101 in args, got %q", c.Args)
	}
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if live := agent.liveAgents(); len(live) != 0 {
		t.Fatalf("agent still live after teardown: %q", live)
	}
	if _, serr := os.Stat(filepath.Join(scratch, "ssh-agent")); !os.IsNotExist(serr) {
		t.Fatal("teardown must remove the socket directory")
	}
}

// Verifies e47a00273f03: a template with no identity contributes nothing —
// no agent is ever spawned.
func TestIdentityAbsentContributesNothing(t *testing.T) {
	agent := newFakeAgent()
	b := identityBinding(agent, nil)
	c, err := b.Prepare(context.Background(), StepSpec{Identity: nil, ScratchDir: t.TempDir()})
	if err != nil || len(c.Args) != 0 || c.Teardown != nil {
		t.Fatalf("want empty contribution, got %q err %v", c.Args, err)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("no agent must be spawned, got %q", agent.starts)
	}
}

// Verifies e47a00273f03: the identity binding refuses to run without a
// per-step scratch dir — the private socket directory has nowhere to live.
func TestIdentityRequiresScratchDir(t *testing.T) {
	b := identityBinding(newFakeAgent(), nil)
	_, err := b.Prepare(context.Background(), StepSpec{Identity: &config.IdentityDef{Key: "./keys/implementer"}})
	errContains(t, err, "scratch dir")
}

// Verifies e47a00273f03 (finding 5 regression): the internal failure teardown
// runs on a context detached from step cancellation — when Prepare fails
// under an already-cancelled step ctx, the agent's Stop still sees a live
// context (graceful SIGTERM window) rather than an immediate deadline kill.
func TestIdentityFailureTeardownDetachedFromCancel(t *testing.T) {
	agent := newFakeAgent()
	agent.addErr = errBoom // fail after the agent is spawned
	b := identityBinding(agent, nil)
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
