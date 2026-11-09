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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
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
// by the run user inside the box. The box is a held one, set up for its run
// user by `faber-box receive` as a re-entry is (the passwd entry OpenSSH
// needs, the chowns), so the whole mount-free path is the real one.
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

	// A held box, as a re-entry starts one: the binding's fragment and the
	// run user's ids on the argv, `faber-box hold` as the entry.
	name := fmt.Sprintf("faber-realinfra-relay-%d", time.Now().UnixNano())
	args := []string{"run", "--rm", "-d", "--name", name, "--tmpfs", "/tmp", "--tmpfs", "/home/box",
		"-e", "FABER_RUN_UID=1000", "-e", "FABER_RUN_GID=1000"}
	args = append(args, c.Args...)
	args = append(args, tag, "/faber/bin/faber-box", "hold")
	if code, err := docker.ContainerRun(ctx, args, nil, io.Discard, os.Stderr); err != nil || code != 0 {
		t.Fatalf("start held box: exit %d err %v", code, err)
	}
	defer docker.Kill(context.WithoutCancel(ctx), name)
	actx, stopAttach := context.WithCancel(ctx)
	attached := make(chan error, 1)
	go func() { attached <- c.Attach(actx, name) }()
	defer func() {
		stopAttach()
		if err := <-attached; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("attach: %v", err)
		}
	}()

	// receive with an empty stream sets the box up for its run user: the
	// passwd and group entry OpenSSH needs, and the chowns.
	var stream bytes.Buffer
	contract.NewFrameWriter(&stream).End()
	var stderr bytes.Buffer
	if code, err := docker.Exec(ctx, name, []string{"/faber/bin/faber-box", "receive"}, &stream, io.Discard, &stderr); err != nil || code != 0 {
		t.Fatalf("receive: exit %d err %v stderr %q", code, err, stderr.String())
	}

	// As the run user, which the relay chowned the socket to: wait for the
	// socket as the clone phase does, list the agent, sign a message through
	// it and check the signature against the listed key; the key line is
	// the stdout the test reads. The exec runs as root (no --user); setpriv
	// drops the same way the preamble does. Exec takes an argv, so the
	// script needs no shell quoting.
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
	stderr.Reset()
	code, err := docker.Exec(ctx, name, []string{"setpriv", "--reuid=1000", "--regid=1000", "--clear-groups", "env", "HOME=/home/box", "bash", "-c", script}, nil, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("box: exit %d err %v stderr %q stdout %q", code, err, stderr.String(), stdout.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != wantKey {
		t.Fatalf("the box's agent lists %q, want the role key %q", got, wantKey)
	}
	if !strings.Contains(stderr.String(), "1000:1000 600") {
		t.Fatalf("the socket is not the run user's 0600: %q", stderr.String())
	}
}

// Verifies fb62c46ab1c1 on a real machine: the check remote binding seeds
// its named volume through `faber-box seed-remote` in a container of the
// step's image, from a bundle of this repository, and a box of that image
// finds the bare repository at the remote URL with this checkout's HEAD,
// owned by the run user.
func TestRealinfra_CheckRemoteSeed(t *testing.T) {
	requireBinaries(t, "git", "docker", "nix")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	top, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git repository: %v", err)
	}
	source := strings.TrimSpace(string(top))
	head, err := exec.CommandContext(ctx, "git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	docker := infra.NewDockerCLI(logger)
	builder := infra.NewImageBuilder(docker, infra.NewNixCLI(logger), infra.DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	// bash for the two-command entry (a nix image has no sh otherwise),
	// util-linux for setpriv: the read runs as the run user, as the box's
	// clone does — git refuses a repository owned by someone else.
	tag, err := builder.Build(ctx, "realinfra-seed", config.BuildDef{Packages: []string{"bash", "coreutils", "git", "util-linux"}}, infra.ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	volume := fmt.Sprintf("faber-check-realinfra-%d-remote", time.Now().UnixNano())
	defer docker.VolumeRemove(context.WithoutCancel(ctx), volume)
	runner := infra.NewContainerRunner(docker, logger)
	set := NewCheckBindingSet(NewIdentityBinding(NewAgentController(logger), logger), volume, source, "", runner, infra.NewCommandRunner(logger), logger)
	defer set.Close(context.WithoutCancel(ctx))
	a, err := set.Prepare(ctx, StepSpec{NodeID: "seed", Repo: "faber", Image: tag, ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer a.Teardown(context.WithoutCancel(ctx))
	if !strings.Contains(strings.Join(a.Args, " "), "-v "+volume+":"+ContainerCheckRemote) {
		t.Fatalf("fragment lacks the volume: %q", a.Args)
	}

	var stdout bytes.Buffer
	res, err := runner.Run(ctx, infra.ContainerSpec{
		Name:     fmt.Sprintf("faber-realinfra-seedread-%d", time.Now().UnixNano()),
		Image:    tag,
		Bindings: a.Args[2:], // the remote member's tokens: the volume and the URL; no network member needed here
		Stdout:   &stdout,
		Entry: []string{"setpriv", "--reuid=" + strconv.Itoa(os.Getuid()), "--regid=" + strconv.Itoa(os.Getgid()), "--clear-groups", "env", "HOME=/tmp",
			"bash", "-c", "git -C /faber/remote/faber.git rev-parse HEAD && stat -c %u /faber/remote/faber.git"},
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("read: exit %d err %v stderr %q stdout %q", res.ExitCode, err, res.Output, stdout.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != strings.TrimSpace(string(head)) || lines[1] != strconv.Itoa(os.Getuid()) {
		t.Fatalf("seeded repository: %q, want HEAD %s owned by %d", stdout.String(), strings.TrimSpace(string(head)), os.Getuid())
	}
}
