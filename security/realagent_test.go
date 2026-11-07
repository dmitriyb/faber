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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
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

// realBox compiles faber-box for the image's architecture, as the infra
// suite does: the binary the builder bakes and the relay is exec'd from.
func realBox(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "faber-box")
	cmd := exec.Command("go", "build", "-o", out, "github.com/dmitriyb/faber/cmd/faber-box")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build faber-box: %v\n%s", err, raw)
	}
	return out
}

// Verifies e47a00273f03 and 1b0f6b9009af on a real machine: the real
// identity binding, attached to a real container through docker exec and
// `faber-box relay`, gives the box's run user an agent that lists exactly
// the role's key and signs with it — no host path shared, the socket owned
// by the run user inside the box.
func TestRealinfra_AgentRelay(t *testing.T) {
	requireBinaries(t, "ssh-agent", "ssh-add", "ssh-keygen", "docker", "nix")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if out, err := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "throwaway", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	wantKey := strings.Join(strings.Fields(string(pub))[:2], " ")

	docker := infra.NewDockerCLI(logger)
	builder := infra.NewImageBuilder(docker, infra.NewNixCLI(logger), infra.DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	// bash for the script, util-linux for setpriv: the image holds only the
	// declared packages.
	tag, err := builder.Build(ctx, "realinfra-relay", config.BuildDef{Packages: []string{"bash", "coreutils", "util-linux", "openssh"}}, infra.ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	binding := NewIdentityBinding(NewAgentController(logger), logger)
	binding.Exec = docker
	defer binding.Close(context.WithoutCancel(ctx))
	c, err := binding.Prepare(ctx, StepSpec{NodeID: "relay", Identity: &config.IdentityDef{Key: key}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The box waits for its socket as the clone phase does, then — as the
	// run user, which the relay chowned the socket to — lists the agent,
	// signs a message through it and checks the signature against the
	// listed key; the key line is the stdout the test reads.
	script := `set -e
for i in $(seq 200); do [ -S "$SSH_AUTH_SOCK" ] && break; sleep 0.1; done
stat -c '%u:%g %a' "$SSH_AUTH_SOCK" >&2
ssh-add -L > /tmp/keys
[ "$(wc -l < /tmp/keys)" = 1 ]
echo message > /tmp/m
ssh-keygen -Y sign -q -n faber -f /tmp/keys /tmp/m
ssh-keygen -Y check-novalidate -n faber -s /tmp/m.sig < /tmp/m >&2
cut -d' ' -f1,2 /tmp/keys`
	var stdout bytes.Buffer
	spec := infra.ContainerSpec{
		Name:     fmt.Sprintf("faber-realinfra-relay-%d", time.Now().UnixNano()),
		Image:    tag,
		Mounts:   []infra.Mount{{Kind: infra.KindTmpfs, Container: "/tmp"}},
		Env:      map[string]string{"FABER_RUN_UID": "1000", "FABER_RUN_GID": "1000", "HOME": "/tmp"},
		Bindings: c.Args,
		Attach:   c.Attach,
		Stdout:   &stdout,
		// The entry runs as root (no --user); the dropped user is what the
		// relay gives the socket to, so the script drops the same way. Entry
		// is an argv, so the script needs no shell quoting.
		Entry: []string{"setpriv", "--reuid=1000", "--regid=1000", "--clear-groups", "bash", "-c", script},
	}
	res, err := infra.NewContainerRunner(docker, logger).Run(ctx, spec)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("box: exit %d err %v stderr %q stdout %q", res.ExitCode, err, res.Output, stdout.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != wantKey {
		t.Fatalf("the box's agent lists %q, want the role key %q", got, wantKey)
	}
	if !strings.Contains(string(res.Output), "1000:1000 600") {
		t.Fatalf("the socket is not the run user's 0600: %q", res.Output)
	}
}
