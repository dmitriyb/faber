package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// agentStartTimeout bounds how long Start waits for the agent's socket to
// appear before declaring the spawn failed.
const agentStartTimeout = 5 * time.Second

// execAgent is the production AgentController over the ssh-agent and ssh-add
// binaries. The agent runs foreground (-D) as a direct child, so its lifetime
// is exactly the handle's: Stop SIGTERMs it (SIGKILL on deadline) and reaps.
// ssh-add runs with a minimal environment carrying only the agent socket.
type execAgent struct {
	logger *slog.Logger
}

// NewAgentController returns the real ssh-agent/ssh-add adapter.
func NewAgentController(logger *slog.Logger) AgentController {
	return &execAgent{logger: childLogger(logger, "ssh-agent")}
}

// Start implements AgentController: ssh-agent -D -a <socket>, then wait for
// the socket to appear (the agent creates it before serving).
func (a *execAgent) Start(ctx context.Context, socket string) (AgentHandle, error) {
	// Deliberately not CommandContext: the agent must outlive Prepare's
	// caller frame and die only via Stop, which the BindingSet guarantees on
	// every exit path (with a context detached from step cancellation).
	cmd := exec.Command("ssh-agent", "-D", "-a", socket)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard // the "SSH_AUTH_SOCK=…; export …" echo; the path is already ours
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.NewTimer(agentStartTimeout)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(socket); err == nil {
			a.logger.DebugContext(ctx, "ssh-agent ready", "socket", socket, "pid", cmd.Process.Pid)
			return &execAgentHandle{cmd: cmd, done: done}, nil
		}
		select {
		case werr := <-done:
			return nil, fmt.Errorf("ssh-agent exited before creating its socket: %v: %s",
				werr, strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("waiting for ssh-agent socket: %w", ctx.Err())
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("ssh-agent socket %s did not appear within %s", socket, agentStartTimeout)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// AddKey implements AgentController: ssh-add <keySource> against the agent
// socket. Stdin is empty, so a passphrase-protected key fails instead of
// hanging; ssh-add's output names files and comments, never key material, so
// folding it into the error is safe.
func (a *execAgent) AddKey(ctx context.Context, socket, keySource string) error {
	cmd := exec.CommandContext(ctx, "ssh-add", keySource)
	cmd.Env = agentEnv(socket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh-add %s: %w: %s", keySource, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListKeys implements AgentController: ssh-add -l, one fingerprint line per
// key. Exit code 1 is ssh-add's "the agent has no identities" answer — an
// empty list, not an error.
func (a *execAgent) ListKeys(ctx context.Context, socket string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "ssh-add", "-l")
	cmd.Env = agentEnv(socket)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var xerr *exec.ExitError
		if errors.As(err, &xerr) && xerr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("ssh-add -l: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var fingerprints []string
	for line := range strings.Lines(stdout.String()) {
		if l := strings.TrimSpace(line); l != "" {
			fingerprints = append(fingerprints, l)
		}
	}
	return fingerprints, nil
}

// agentEnv is the minimal environment ssh-add runs against: tool lookup plus
// the one socket — never the full host environment.
func agentEnv(socket string) []string {
	env := []string{EnvSSHAuthSock + "=" + socket}
	for _, key := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// execAgentHandle is one live agent child process.
type execAgentHandle struct {
	cmd  *exec.Cmd
	done chan error
}

// Stop implements AgentHandle: SIGTERM, wait, SIGKILL on context deadline.
// An agent that already exited (e.g. the whole process group was signalled)
// is reaped without error.
func (h *execAgentHandle) Stop(ctx context.Context) error {
	// A Signal failure means the process is already gone; reaping below is
	// still required either way.
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		_ = h.cmd.Process.Kill()
		<-h.done
		return fmt.Errorf("ssh-agent pid %d ignored SIGTERM and was killed: %w", h.cmd.Process.Pid, ctx.Err())
	}
}
