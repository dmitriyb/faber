//go:build realinfra

package security

// Real-binary integration suite for the ssh-agent adapter: needs ssh-agent,
// ssh-add, and ssh-keygen on PATH. It never runs in the sandboxed unit gate;
// on an acceptance machine run:
//
//	go test -tags realinfra ./security/
//
// What it verifies against the real binaries (test scenario 2 with a real
// agent): spawn on a private socket, load exactly one throwaway file key,
// list exactly one fingerprint, and tear down without leaking the process or
// the socket.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func requireBinaries(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not available: %v", name, err)
		}
	}
}

// Verifies e47a00273f03: the real agent lifecycle — ephemeral spawn, exactly
// one key loaded and listed, guaranteed teardown — against the actual
// ssh-agent and ssh-add binaries.
func TestRealAgentLifecycle(t *testing.T) {
	requireBinaries(t, "ssh-agent", "ssh-add", "ssh-keygen")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if out, err := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "throwaway", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}

	agents := NewAgentController(slog.New(slog.DiscardHandler))
	sock := filepath.Join(dir, "agent.sock")
	handle, err := agents.Start(ctx, sock)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = handle.Stop(ctx)
		}
	}()

	if fps, err := agents.ListKeys(ctx, sock); err != nil || len(fps) != 0 {
		t.Fatalf("fresh agent must hold no keys, got %q err %v", fps, err)
	}
	if err := agents.AddKey(ctx, sock, key); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	fps, err := agents.ListKeys(ctx, sock)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(fps) != 1 {
		t.Fatalf("want exactly one key, got %q", fps)
	}

	if err := handle.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
	// The agent removes its socket on clean shutdown; either way the process
	// is reaped, so a follow-up list must fail or answer empty.
	if _, err := os.Stat(sock); err == nil {
		if fps, lerr := agents.ListKeys(ctx, sock); lerr == nil && len(fps) > 0 {
			t.Fatalf("agent still answering after Stop: %q", fps)
		}
	}
}
