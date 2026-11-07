package security

// Shared fakes and fixtures: docker and the resolver are faked via infra's
// typed seams, the ssh-agent binaries via this package's AgentController
// seam, so no test here needs docker, a network, or a real agent. The
// realinfra-tagged suite is the only place real binaries run.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"crypto/ed25519"
	"crypto/rand"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"net"
	"sync/atomic"
	"time"
)

// fakeDocker implements infra.DockerClient. Only NetworkExists is expected
// from this package; ContainerRun counts calls so tests can assert no
// container was ever started.
type fakeDocker struct {
	mu        sync.Mutex
	networks  map[string]bool
	netErr    error
	netCalls  []string
	runCalls  int
	killCalls []string
}

func (f *fakeDocker) ImageExists(context.Context, string) (bool, error) { return false, nil }

func (f *fakeDocker) Load(context.Context, string) (string, error) { return "", nil }

func (f *fakeDocker) NetworkExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netCalls = append(f.netCalls, name)
	if f.netErr != nil {
		return false, f.netErr
	}
	return f.networks[name], nil
}

func (f *fakeDocker) ContainerRun(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls++
	return 0, nil
}

func (f *fakeDocker) Exec(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func (f *fakeDocker) ExecTerminal(context.Context, string, []string, map[string]string, string, string) error {
	return nil
}

func (f *fakeDocker) Kill(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls = append(f.killCalls, name)
	return nil
}

func (f *fakeDocker) Remove(context.Context, string) error { return nil }

// fakeAgent is an in-process AgentController: Start serves a real keyring
// over the socket with x/crypto's ServeAgent (so the proxy's protocol path
// is exercised end to end, no binaries), AddKey mints a throwaway ed25519
// key named by the key source's basename, and ListKeys reports the
// `ssh-add -l`-shaped lines the binding verifies against (overridable, so
// tests can pin or break the fingerprint join). Stops are recorded with the
// ctx.Err() observed, and a per-sign delay plus a concurrency counter let
// tests prove the sign queue serializes.
type fakeAgent struct {
	mu       sync.Mutex
	keys     map[string][]string // socket -> fingerprint lines
	rings    map[string]agent.Agent
	running  map[string]bool
	starts   []string
	stops    []string
	stopCtx  []error // ctx.Err() observed at each Stop
	startErr error
	addErr   error
	listFn   func(socket string) ([]string, error) // overrides the tracked keys
	// fpBySource maps a keySource to the real fingerprint the loaded key
	// reports, mirroring how a correct pub/private pair yields its pinned
	// fingerprint. When a keySource is absent the fake synthesizes one from the
	// basename (the path-form default, where no fingerprint is pinned).
	fpBySource map[string]string

	signDelay time.Duration // held inside every upstream Sign
	inSign    int32
	maxInSign int32
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{keys: map[string][]string{}, rings: map[string]agent.Agent{}, running: map[string]bool{}}
}

func (f *fakeAgent) Start(_ context.Context, socket string) (AgentHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, socket)
	if f.startErr != nil {
		return nil, f.startErr
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	ring := agent.NewKeyring()
	f.rings[socket] = ring
	f.running[socket] = true
	served := &countingAgent{Agent: ring, f: f}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(served, conn)
			}()
		}
	}()
	return &fakeAgentHandle{f: f, socket: socket, ln: ln}, nil
}

// countingAgent wraps the keyring to observe sign concurrency and to hold
// each signature for signDelay, so an unqueued caller would overlap.
type countingAgent struct {
	agent.Agent
	f *fakeAgent
}

func (c *countingAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	n := atomic.AddInt32(&c.f.inSign, 1)
	for {
		m := atomic.LoadInt32(&c.f.maxInSign)
		if n <= m || atomic.CompareAndSwapInt32(&c.f.maxInSign, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&c.f.inSign, -1)
	if c.f.signDelay > 0 {
		time.Sleep(c.f.signDelay)
	}
	return c.Agent.Sign(key, data)
}

func (f *fakeAgent) AddKey(_ context.Context, socket, keySource string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	line := "256 SHA256:fp-" + filepath.Base(keySource) + " (ED25519)"
	if fp, ok := f.fpBySource[keySource]; ok {
		line = "256 " + fp + " " + filepath.Base(keySource) + " (ED25519)"
	}
	f.keys[socket] = append(f.keys[socket], line)
	if ring := f.rings[socket]; ring != nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: filepath.Base(keySource)}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeAgent) ListKeys(_ context.Context, socket string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(socket)
	}
	return f.keys[socket], nil
}

func (f *fakeAgent) liveAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []string
	for sock, up := range f.running {
		if up {
			live = append(live, sock)
		}
	}
	return live
}

type fakeAgentHandle struct {
	f      *fakeAgent
	socket string
	ln     net.Listener
}

func (h *fakeAgentHandle) Stop(ctx context.Context) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if h.ln != nil {
		_ = h.ln.Close()
	}
	h.f.running[h.socket] = false
	h.f.stops = append(h.f.stops, h.socket)
	h.f.stopCtx = append(h.f.stopCtx, ctx.Err())
	return nil
}

// fakeResolver implements Resolver with a fixed token, optional per-service
// failures, and an optional block-until-cancelled mode.
type fakeResolver struct {
	mu        sync.Mutex
	calls     []string
	token     []byte
	errFor    map[string]error
	blockCtx  bool // emulate a slow resolver: wait for ctx cancellation
	globalErr error
}

func (f *fakeResolver) GetToken(ctx context.Context, service string) (Secret, error) {
	f.mu.Lock()
	f.calls = append(f.calls, service)
	blocking := f.blockCtx
	err := f.globalErr
	if e, ok := f.errFor[service]; ok {
		err = e
	}
	tok := f.token
	f.mu.Unlock()
	if blocking {
		<-ctx.Done()
		return Secret{}, ctx.Err()
	}
	if err != nil {
		return Secret{}, err
	}
	return NewSecret(tok), nil
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeRunner implements infra.CommandRunner for ExecResolver tests.
type fakeRunner struct {
	mu    sync.Mutex
	calls []infra.CmdSpec
	res   infra.CmdResult
	err   error
}

func (f *fakeRunner) Run(_ context.Context, spec infra.CmdSpec) (infra.CmdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	return f.res, f.err
}

// hostKeyLine is the single line testdata/gateway_host_key.pub holds.
const hostKeyLine = "gateway ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIF4kAqRy0MOYCsaJctPyStnJDGvXUvIXu5DYSPCLxfXn"

const testToken = "tok-agent-api-31337"

// harness wires a BindingSet from fakes, mirroring the reference
// orchestrator's network/remote/credentials/identities sections.
type harness struct {
	docker   *fakeDocker
	agent    *fakeAgent
	resolver *fakeResolver
	broker   *CredentialBroker
	set      *BindingSet
	logBuf   *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	docker := &fakeDocker{networks: map[string]bool{"agents-internal": true}}
	agent := newFakeAgent()
	resolver := &fakeResolver{token: []byte(testToken)}
	broker := NewCredentialBroker(resolver, logger)
	return &harness{
		docker:   docker,
		agent:    agent,
		resolver: resolver,
		broker:   broker,
		logBuf:   logBuf,
		set: NewBindingSet(
			NewNetworkBinding(docker, logger),
			NewRemoteBinding(logger),
			NewIdentityBinding(agent, logger),
			broker,
			logger,
		),
	}
}

// implementStep is the reference implement step resolved into a StepSpec.
func implementStep(scratch string) StepSpec {
	return StepSpec{
		NodeID: "task/implement",
		Network: &config.NetworkDef{
			Name:    "agents-internal",
			Proxy:   "http://egress:8888",
			NoProxy: []string{"gateway", "localhost", "127.0.0.1"},
		},
		Remote:     &config.RemoteDef{URL: "ssh://git@gateway/srv/git", HostKeyFile: "testdata/gateway_host_key.pub"},
		Identity:   &config.IdentityDef{Key: "./keys/implementer"},
		Services:   map[string]config.ServiceDef{"agent-api": {Mode: "file"}},
		Repo:       "sandbox",
		ScratchDir: scratch,
	}
}

// mergeStep is the reference merge step: a different identity, no services.
func mergeStep(scratch string) StepSpec {
	s := implementStep(scratch)
	s.NodeID = "task/merge"
	s.Identity = &config.IdentityDef{Key: "./keys/merger"}
	s.Services = nil
	return s
}

// implementFragment is the exact ordered argv fragment the implement step
// must assemble to, given the attempt's agent socket host path (the one
// attempt-unique value — a per-spawn MkdirTemp dir, see the identity
// binding). File mode contributes exactly one --tmpfs /run/secrets and no
// token flag — the token rides Assembled.SecretsStdin, not the argv.
func implementFragment(sock string) []string {
	return []string{
		"--network", "agents-internal",
		"-e", "HTTPS_PROXY=http://egress:8888",
		"-e", "HTTP_PROXY=http://egress:8888",
		"-e", "NO_PROXY=gateway,localhost,127.0.0.1",
		"-e", "FABER_REMOTE_URL=ssh://git@gateway/srv/git/sandbox.git",
		"-e", "FABER_HOST_KEY=" + hostKeyLine,
		"-v", sock + ":/ssh-agent",
		"-e", "SSH_AUTH_SOCK=/ssh-agent",
		"--tmpfs", "/run/secrets",
	}
}

// errContains fails the test unless err is non-nil and mentions want.
func errContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", want)
	}
	if !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Fatalf("want error containing %q, got %q", want, err.Error())
	}
}

var errBoom = errors.New("boom")

// ed25519Gen mints a throwaway key pair for tests that need a key the
// binding never loaded.
func ed25519Gen(r io.Reader) (interface{}, interface{}, error) {
	pub, priv, err := ed25519.GenerateKey(r)
	return pub, priv, err
}
