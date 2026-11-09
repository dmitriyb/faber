package security

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmitriyb/faber/infra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// roleAgent is one run-scoped ssh-agent holding exactly one role's key: the
// process behind every proxy socket handed to that role's boxes. It is
// spawned on the role's first attempt and stopped by Close at run end.
type roleAgent struct {
	handle AgentHandle
	dir    string
	sock   string
	conn   net.Conn
	client agent.ExtendedAgent
	keys   []*agent.Key // what boxes may see: the pinned key only
}

// errAgentReadOnly answers every agent request that would change what a
// box can sign with: the proxy is a signing window onto a fixed key, never
// a keyring the box can grow, shrink, or lock.
var errAgentReadOnly = errors.New("agent: refused — the box's agent is read-only")

// signProxy is the per-attempt agent a box talks to, served over the relay.
// List answers the role's key; Sign forwards to the role agent through the
// sign queue — one process-wide mutex, so a hardware token never sees two
// requests at once; everything that mutates is refused. The box cannot tell
// it from a real agent: the protocol and the env var are those of one.
type signProxy struct {
	keys     []*agent.Key
	upstream agent.ExtendedAgent
	queue    *sync.Mutex
}

func (p *signProxy) List() ([]*agent.Key, error) {
	out := make([]*agent.Key, len(p.keys))
	copy(out, p.keys)
	return out, nil
}

func (p *signProxy) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return p.SignWithFlags(key, data, 0)
}

func (p *signProxy) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	if !p.holds(key) {
		return nil, errors.New("agent: refused — key is not this box's identity")
	}
	p.queue.Lock()
	defer p.queue.Unlock()
	return p.upstream.SignWithFlags(key, data, flags)
}

func (p *signProxy) Signers() ([]ssh.Signer, error) {
	p.queue.Lock()
	defer p.queue.Unlock()
	return p.upstream.Signers()
}

func (p *signProxy) Add(agent.AddedKey) error   { return errAgentReadOnly }
func (p *signProxy) Remove(ssh.PublicKey) error { return errAgentReadOnly }
func (p *signProxy) RemoveAll() error           { return errAgentReadOnly }
func (p *signProxy) Lock([]byte) error          { return errAgentReadOnly }
func (p *signProxy) Unlock([]byte) error        { return errAgentReadOnly }
func (p *signProxy) Extension(string, []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}

// holds reports whether key is one of the proxy's exposed keys.
func (p *signProxy) holds(key ssh.PublicKey) bool {
	want := key.Marshal()
	for _, k := range p.keys {
		if string(k.Marshal()) == string(want) {
			return true
		}
	}
	return false
}

// serveRelay serves the proxy into container over `faber-box relay`: it
// waits for the container to run, execs the relay with a pipe each way and
// runs the agent server on the pair until the exec ends or ctx is
// cancelled. The relay's stdin is an os.Pipe — an *os.File, which os/exec
// hands to the process directly, so the exec's wait never blocks on a
// copier that has nothing left to copy (the host only ever writes replies);
// its stdout is an io.Pipe the server reads, closed here once the exec is
// over so the server returns. The relay's stderr goes to the log.
func serveRelay(ctx context.Context, exec infra.Execer, container string, p *signProxy, logger *slog.Logger) error {
	toRelay, hostOut, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("agent relay pipe: %w", err)
	}
	defer toRelay.Close()
	defer hostOut.Close()
	fromRelay, relayOut := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- agent.ServeAgent(p, struct {
			io.Reader
			io.Writer
		}{fromRelay, hostOut})
	}()
	code, xerr := infra.ExecWhenRunning(ctx, exec, container, []string{ContainerEntry, "relay"}, toRelay, relayOut, slogWriter(logger, container))
	relayOut.Close()
	hostOut.Close()
	serr := <-served
	if xerr != nil || code != 0 {
		// The relay dies with its container (killed with the pid namespace,
		// or never exec'd into a box that already exited): the ordinary end
		// of every run, not a failure of the attach.
		if containerGone(ctx, exec, container) {
			return nil
		}
		if xerr != nil {
			return fmt.Errorf("agent relay into %s: %w", container, xerr)
		}
		return fmt.Errorf("agent relay into %s exited %d", container, code)
	}
	if serr != nil && !errors.Is(serr, io.EOF) && !errors.Is(serr, io.ErrClosedPipe) {
		return fmt.Errorf("agent relay into %s: serve: %w", container, serr)
	}
	return nil
}

// containerGone reports whether the container is no longer running, on a
// context of its own: the attach's may already be cancelled.
func containerGone(ctx context.Context, exec infra.Execer, container string) bool {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	up, err := exec.Running(qctx, container)
	return err == nil && !up
}

// slogWriter turns a container's relay stderr into debug lines.
func slogWriter(logger *slog.Logger, container string) io.Writer {
	return &lineLogger{logger: logger, container: container}
}

type lineLogger struct {
	logger    *slog.Logger
	container string
}

func (l *lineLogger) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			l.logger.Debug("agent relay", "container", l.container, "line", line)
		}
	}
	return len(p), nil
}

// roleAgentFor returns the live agent for key, spawning and verifying it on
// first use. key is the registry role, or the opaque key source for a
// path-form identity (no role). The verification — exactly one pinned key —
// happens here, once per role per run, exactly as it used to per attempt.
func (b *IdentityBinding) roleAgentFor(ctx context.Context, key, keySource, expectFP, role, nodeID string) (*roleAgent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ra := b.roles[key]; ra != nil {
		return ra, nil
	}
	dir, sock, err := b.socketDir(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	handle, err := b.agents.Start(ctx, sock)
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			b.logger.WarnContext(ctx, "remove socket dir after failed agent spawn", "node", nodeID, "err", rerr)
		}
		return nil, fmt.Errorf("spawn ssh-agent: %w", err)
	}
	ra := &roleAgent{handle: handle, dir: dir, sock: sock}
	fail := func(err error) (*roleAgent, error) {
		// Detach from step cancellation, mirroring BindingSet.teardownAll: a
		// spawn failing *because* the step was cancelled must still give
		// the agent its graceful SIGTERM window.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownGrace)
		defer cancel()
		if terr := ra.stop(tctx); terr != nil {
			b.logger.WarnContext(tctx, "teardown after failed identity setup", "node", nodeID, "err", terr)
		}
		return nil, err
	}
	if err := b.agents.AddKey(ctx, sock, keySource); err != nil {
		return fail(fmt.Errorf("load identity key: %w", err))
	}
	fingerprints, err := b.agents.ListKeys(ctx, sock)
	if err != nil {
		return fail(fmt.Errorf("list agent keys: %w", err))
	}
	switch {
	case len(fingerprints) == 0:
		return fail(errors.New("agent holds no key after load (the key loader failed silently)"))
	case len(fingerprints) > 1:
		b.logger.WarnContext(ctx, "agent holds more than one key; role isolation degraded",
			"node", nodeID, "fingerprints", fingerprints)
	}
	// The fingerprint is the entire cross-system join: the registry pins a role
	// to a fingerprint, the gate authorizes signatures by fingerprint. Resolving
	// a *.pub to its private counterpart trusts a naming convention, so once the
	// key is actually loaded we confirm the agent holds the pinned fingerprint —
	// a stale, mismatched, or planted pub/private pair fails closed here, at
	// prepare time with a clear error, rather than late at the gate after a
	// wasted box run. expectFP is "" only for the explicit-path branch, where no
	// fingerprint was pinned and there is nothing to check against.
	if expectFP != "" && !fingerprintHeld(fingerprints, expectFP) {
		if role == "" {
			role = "(inline fingerprint)"
		}
		return fail(fmt.Errorf("identity role %q: agent loaded a key whose fingerprint is not the pinned one: expected %s, agent holds %v",
			role, expectFP, fingerprints))
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return fail(fmt.Errorf("connect to role agent: %w", err))
	}
	ra.conn = conn
	ra.client = agent.NewClient(conn)
	keys, err := ra.client.List()
	if err != nil {
		return fail(fmt.Errorf("list role agent keys over the protocol: %w", err))
	}
	ra.keys = exposedKeys(keys, expectFP)
	if len(ra.keys) == 0 {
		return fail(errors.New("role agent exposes no key over the protocol"))
	}
	b.roles[key] = ra
	return ra, nil
}

// exposedKeys picks what a box may see: the single key a well-formed agent
// holds, or — when a loader surfaced extra credentials — only the pinned
// fingerprint's key, so the degraded case narrows rather than widens.
func exposedKeys(keys []*agent.Key, expectFP string) []*agent.Key {
	if len(keys) <= 1 || expectFP == "" {
		return keys
	}
	var out []*agent.Key
	for _, k := range keys {
		if ssh.FingerprintSHA256(k) == expectFP {
			out = append(out, k)
		}
	}
	return out
}

// socketDir creates the private socket directory and checks the composed
// path against the sun_path budget.
func (b *IdentityBinding) socketDir(ctx context.Context, nodeID string) (dir, sock string, err error) {
	dir, err = os.MkdirTemp("", agentSocketPattern)
	if err != nil {
		return "", "", fmt.Errorf("create agent socket dir: %w", err)
	}
	sock = filepath.Join(dir, agentSocketFile)
	if len(sock) > maxSocketPath {
		if rerr := os.Remove(dir); rerr != nil {
			b.logger.WarnContext(ctx, "remove socket dir after path-length refusal", "node", nodeID, "err", rerr)
		}
		return "", "", fmt.Errorf(
			"agent socket path %q is %d bytes — over the %d-byte Unix socket (sun_path) budget; point TMPDIR at a shorter directory",
			sock, len(sock), maxSocketPath)
	}
	return dir, sock, nil
}

// stop ends the role agent: connection, process, directory — each attempted
// regardless of the others, errors joined.
func (ra *roleAgent) stop(ctx context.Context) error {
	var errs []error
	if ra.conn != nil {
		if err := ra.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close role agent connection: %w", err))
		}
	}
	if err := ra.handle.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop ssh-agent: %w", err))
	}
	if err := os.RemoveAll(ra.dir); err != nil {
		errs = append(errs, fmt.Errorf("remove socket dir: %w", err))
	}
	return errors.Join(errs...)
}

// Close stops every role agent the run spawned. The cmd wiring calls it
// once the run returns; a leaked agent is a leaked credential handle, so
// every error is surfaced.
func (b *IdentityBinding) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var errs []error
	for key, ra := range b.roles {
		if err := ra.stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("role agent %s: %w", key, err))
		}
		delete(b.roles, key)
	}
	return errors.Join(errs...)
}
