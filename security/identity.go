package security

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/dmitriyb/faber/infra"
)

// agentSocketPattern names the private per-spawn MkdirTemp directory the
// binding creates for the agent socket; agentSocketFile is the socket inside
// it. The dir lives under the SYSTEM temp dir, never the step's scratch
// tree: a Unix socket path is capped at ~104–108 bytes (sun_path), and the
// scratch path scales with the project directory, run id, and node id — a
// deep-but-legal layout overflows the cap and ssh-agent cannot bind
// (unix_listener: path too long). MkdirTemp keeps the per-attempt freshness
// retries rely on (unique dir per spawn) at the cost of one attempt-unique
// host path in the assembled fragment.
const (
	agentSocketPattern = "faber-agent-"
	agentSocketFile    = "agent.sock"

	// maxSocketPath is the longest agent socket path Prepare accepts —
	// headroom under the smallest platform sun_path cap (~104 on macOS/BSD,
	// 108 on Linux). MkdirTemp honors TMPDIR, so a deep override would
	// reproduce the very overflow the temp dir exists to avoid; the guard
	// fails closed with a diagnosis instead of ssh-agent's cryptic
	// "unix_listener: path too long".
	maxSocketPath = 100
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
// isolation by construction" (requirement e47a00273f03). Per role per run it
// spawns one ssh-agent on a private host socket and loads exactly one key;
// per step attempt it serves a signing proxy onto that agent into the box
// through a relay the box runs on a socket of its own, over docker exec, for
// exactly the container's lifetime. Nothing of the host filesystem is shared
// (a mounted socket is unreachable from a box whose uid is remapped), and
// the private key never enters the container — not as a file, not as env,
// not through the protocol (an agent answers signing challenges; it does not
// export keys).
//
// Faber never learns what a key is for: fingerprint-to-role mapping and
// signature policy live in the user's gate service.
type IdentityBinding struct {
	agents AgentController
	logger *slog.Logger

	// roles holds the run-scoped agents, one per role (or per path-form key
	// source), spawned on first use and stopped by Close. queue is the sign
	// queue every proxy's Sign passes through: one signature at a time per
	// process, so a single hardware token behind every role never meets two
	// requests at once.
	mu    sync.Mutex
	roles map[string]*roleAgent
	queue sync.Mutex

	// Exec is the docker seam the attach execs the relay through (the
	// wiring's docker client). Nil fails the attach, naming it; Prepare
	// itself never needs it.
	Exec infra.Execer

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
	return &IdentityBinding{
		roles: map[string]*roleAgent{}, agents: agents, logger: childLogger(logger, "identity-binding")}
}

// Name implements Binding.
func (b *IdentityBinding) Name() string { return "identity" }

// Prepare runs the per-attempt lifecycle: the role's agent (spawned, loaded
// with exactly one key and verified on the role's first attempt), then the
// contribution — the agent tmpfs, SSH_AUTH_SOCK, and the attach that serves
// the signing proxy over the relay. Zero keys after load is a hard error
// (the loader failed silently); more than one logs a loud warning naming the
// extra fingerprints — role isolation is degraded, but a hardware-backed
// loader may legitimately surface adjacent credentials — and proceeds. Any
// failure after the spawn tears the agent down before returning, so a failed
// Prepare leaks nothing. Nothing is created on the host per attempt, so the
// contribution has no teardown.
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
	roleKey := step.IdentityRole
	if roleKey == "" {
		roleKey = "path:" + keySource
	}
	ra, err := b.roleAgentFor(ctx, roleKey, keySource, expectFP, step.IdentityRole, step.NodeID)
	if err != nil {
		return Contribution{}, err
	}
	proxy := &signProxy{keys: ra.keys, upstream: ra.client, queue: &b.queue}
	attach := func(ctx context.Context, container string) error {
		if b.Exec == nil {
			return errors.New("identity: no docker seam to attach the agent relay through")
		}
		return serveRelay(ctx, b.Exec, container, proxy, b.logger)
	}
	return Contribution{
		Args:   []string{"--tmpfs", ContainerAgentDir, "-e", EnvSSHAuthSock + "=" + ContainerAgentSocket},
		Attach: attach,
	}, nil
}
