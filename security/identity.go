package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// agentSocketDir is the name of the private socket directory the binding
// creates under the step's scratch dir; agentSocketFile is the socket inside
// it. Deterministic given the scratch dir, so repeated assembly of the same
// resolved step yields a byte-identical fragment — and per-attempt scratch
// dirs give retries a fresh socket path.
const (
	agentSocketDir  = "ssh-agent"
	agentSocketFile = "agent.sock"
)

// AgentController is the typed seam over the ssh-agent and ssh-add binaries.
// Unit tests substitute a fake; no test needs a real agent. Key material
// acquisition is behind AddKey: the identity's key source is an opaque,
// resolver-interpreted string — a file key in the sandbox, a
// hardware-resident key in production — and neither this binding nor the box
// can tell the difference. Only the fingerprint matters.
type AgentController interface {
	// Start spawns one ephemeral agent listening on socket and returns its
	// live handle. The socket's parent directory already exists, private to
	// the step.
	Start(ctx context.Context, socket string) (AgentHandle, error)

	// AddKey loads the identity's one key (from its opaque key source) into
	// the agent at socket.
	AddKey(ctx context.Context, socket, keySource string) error

	// ListKeys returns one fingerprint line per key the agent at socket
	// holds. Fingerprints are public material and safe to log.
	ListKeys(ctx context.Context, socket string) ([]string, error)
}

// AgentHandle is one live ephemeral ssh-agent process.
type AgentHandle interface {
	// Stop terminates the agent and reaps it. Called exactly once, on every
	// exit path, via the binding's teardown.
	Stop(ctx context.Context) error
}

// IdentityBinding is the mechanism behind "one key per container ⇒ role
// isolation by construction" (requirement e47a00273f03). Per step attempt it
// spawns a fresh ssh-agent on a private socket, loads exactly one key for the
// template's identity, forwards only the socket into the box, and destroys
// the agent the moment the step ends. The private key never enters the
// container — not as a file, not as env, not through the socket (an agent
// answers signing challenges; it does not export keys). Concurrent steps
// never share an agent, even under the same identity.
//
// Faber never learns what a key is for: fingerprint-to-role mapping and
// signature policy live in the user's gate service.
type IdentityBinding struct {
	agents AgentController
	logger *slog.Logger

	// SocketGroup, when non-empty, adds "--group-add <SocketGroup>" to the
	// contribution — the documented escape for platforms where the forwarded
	// socket's group ownership blocks the box's non-root user (the macOS VM
	// case). Empty on platforms that need nothing.
	SocketGroup string

	// Registry (role→fingerprint) and Locator (fingerprint→live key) resolve
	// an identity that carries no explicit key path. Both are consulted only
	// when ResolveIdentity falls through to the registry hop, so a path-form
	// identity — every existing config — needs neither and both may be nil.
	Registry Registry
	Locator  KeyLocator
}

// NewIdentityBinding wires the ephemeral-agent lifecycle to an
// AgentController (NewAgentController for the real binaries).
func NewIdentityBinding(agents AgentController, logger *slog.Logger) *IdentityBinding {
	return &IdentityBinding{agents: agents, logger: childLogger(logger, "identity-binding")}
}

// Name implements Binding.
func (b *IdentityBinding) Name() string { return "identity" }

// Prepare runs the per-attempt lifecycle: spawn, load exactly one key,
// verify, contribute the socket mount. Zero keys after load is a hard error
// (the loader failed silently); more than one logs a loud warning naming the
// extra fingerprints — role isolation is degraded, but a hardware-backed
// loader may legitimately surface adjacent credentials — and proceeds. Any
// failure after the spawn tears the agent down before returning, so a failed
// Prepare leaks nothing.
func (b *IdentityBinding) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if step.Identity == nil {
		return Contribution{}, nil
	}
	// Resolve the key source before anything is spawned: explicit path wins
	// (byte-identical to today), else the registry+locator hop. A role that
	// resolves to nothing fails here, with no agent left behind.
	keySource, expectFP, err := ResolveIdentity(ctx, b.Registry, b.Locator, step.IdentityRole, *step.Identity)
	if err != nil {
		return Contribution{}, err
	}
	if step.ScratchDir == "" {
		return Contribution{}, errors.New("identity binding requires a per-step scratch dir")
	}
	dir := filepath.Join(step.ScratchDir, agentSocketDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Contribution{}, fmt.Errorf("create agent socket dir: %w", err)
	}
	sock := filepath.Join(dir, agentSocketFile)

	handle, err := b.agents.Start(ctx, sock)
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			b.logger.WarnContext(ctx, "remove socket dir after failed agent spawn", "node", step.NodeID, "err", rerr)
		}
		return Contribution{}, fmt.Errorf("spawn ssh-agent: %w", err)
	}
	teardown := func(ctx context.Context) error {
		// A leaked agent is a leaked credential handle: attempt both halves
		// independently and join, never short-circuit.
		var errs []error
		if err := handle.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop ssh-agent: %w", err))
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove socket dir: %w", err))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (Contribution, error) {
		// Detach from step cancellation, mirroring BindingSet.teardownAll: a
		// Prepare failing *because* the step was cancelled must still give
		// the agent its graceful SIGTERM window instead of an immediate
		// SIGKILL with a misleading deadline error.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownGrace)
		defer cancel()
		if terr := teardown(tctx); terr != nil {
			b.logger.WarnContext(tctx, "teardown after failed identity setup", "node", step.NodeID, "err", terr)
		}
		return Contribution{}, err
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
			"node", step.NodeID, "fingerprints", fingerprints)
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
		role := step.IdentityRole
		if role == "" {
			role = "(inline fingerprint)"
		}
		return fail(fmt.Errorf("identity role %q: agent loaded a key whose fingerprint is not the pinned one: expected %s, agent holds %v",
			role, expectFP, fingerprints))
	}

	args := []string{
		"-v", sock + ":" + ContainerAgentSocket,
		"-e", EnvSSHAuthSock + "=" + ContainerAgentSocket,
	}
	if b.SocketGroup != "" {
		args = append(args, "--group-add", b.SocketGroup)
	}
	return Contribution{Args: args, Teardown: teardown}, nil
}
