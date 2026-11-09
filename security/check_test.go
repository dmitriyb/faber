package security

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// Verifies fb62c46ab1c1: the check composition hands the box no network,
// a bare repository cloned from the working directory's as its only
// remote, the generated key as every role's identity, and no credential
// at all — in the fixed binding order.
func TestCheckBindingSetSandbox(t *testing.T) {
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	agent := newFakeAgent()
	runner := &fakeRunner{}
	remoteDir := t.TempDir()
	key := filepath.Join(t.TempDir(), "check_ed25519")
	set := NewCheckBindingSet(NewIdentityBinding(agent, logger), remoteDir, "/work/project", key, runner, logger)
	t.Cleanup(func() { set.Close(context.Background()) })

	step := StepSpec{
		NodeID:       "task/implement",
		Network:      &config.NetworkDef{Name: "agents-internal", Proxy: "http://egress:8888"},
		Remote:       &config.RemoteDef{URL: "ssh://git@gateway/srv/git", HostKeyFile: "testdata/gateway_host_key.pub"},
		Identity:     &config.IdentityDef{Key: "SHA256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefa"},
		IdentityRole: "implementer",
		Services:     map[string]config.ServiceDef{"agent-api": {Mode: "proxy"}},
		Runtime:      "runsc",
		Repo:         "sandbox",
		ScratchDir:   t.TempDir(),
	}
	a, err := set.Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer a.Teardown(context.Background())
	args := strings.Join(a.Args, " ")

	// network: none, not the configured one; no proxy environment
	if !strings.HasPrefix(args, "--network none ") || strings.Contains(args, "agents-internal") || strings.Contains(args, "HTTPS_PROXY") {
		t.Errorf("network contribution: %q", args)
	}
	// remote: the bare repository under the mount, cloned from the working directory
	wantMount := "-v " + remoteDir + ":" + ContainerCheckRemote
	wantURL := "-e " + EnvRemoteURL + "=" + ContainerCheckRemote + "/sandbox.git"
	if !strings.Contains(args, wantMount) || !strings.Contains(args, wantURL) || strings.Contains(args, EnvHostKey) || strings.Contains(args, "gateway") {
		t.Errorf("remote contribution: %q", args)
	}
	if len(runner.calls) != 1 || runner.calls[0].Path != "git" ||
		!slices.Equal(runner.calls[0].Args, []string{"clone", "--bare", "--quiet", "/work/project", filepath.Join(remoteDir, "sandbox.git")}) {
		t.Errorf("clone call: %+v", runner.calls)
	}
	// identity: the real binding, loaded with the check key, not the role's fingerprint
	if !strings.Contains(args, strings.Join(identityArgs, " ")) || a.Attach == nil {
		t.Fatalf("no agent contributed: %q (attach nil: %v)", args, a.Attach == nil)
	}
	keys, _ := agent.ListKeys(context.Background(), agent.starts[0])
	if len(keys) != 1 || !strings.Contains(keys[0], "fp-check_ed25519") {
		t.Errorf("agent keys: %v, want the check key only", keys)
	}
	// credentials: none; the runtime knob stays
	if strings.Contains(args, "/run/secrets") || strings.Contains(args, "FABER_SERVICE") {
		t.Errorf("a credential reached the box: %q", args)
	}
	if !strings.HasSuffix(args, "--runtime=runsc") {
		t.Errorf("runtime knob lost: %q", args)
	}
	if len(a.SecretsStdin) != 0 {
		t.Errorf("secrets payload must be empty, got %d bytes", len(a.SecretsStdin))
	}
}

// Verifies fb62c46ab1c1: a repo-less step contributes no remote and no
// identity when the template declares none; a repo-bound step with no
// repository in the working directory fails before launch, naming why; the
// bare repository is cloned once, not per step.
func TestCheckBindingSetEdges(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	runner := &fakeRunner{}
	remoteDir := t.TempDir()
	set := NewCheckBindingSet(NewIdentityBinding(newFakeAgent(), logger), remoteDir, "", "/k", runner, logger)
	t.Cleanup(func() { set.Close(context.Background()) })

	a, err := set.Prepare(context.Background(), StepSpec{NodeID: "task/bare", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("repo-less step: %v", err)
	}
	if got := strings.Join(a.Args, " "); got != "--network none" {
		t.Errorf("repo-less, identity-less step: %q", got)
	}
	a.Teardown(context.Background())

	_, err = set.Prepare(context.Background(), StepSpec{NodeID: "task/x", Repo: "sandbox", ScratchDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "needs a git repository in the working directory") {
		t.Fatalf("repo-bound step without a source repository: %v", err)
	}

	// an already-present bare repository is reused, no clone call
	set2 := NewCheckBindingSet(NewIdentityBinding(newFakeAgent(), logger), remoteDir, "/work/project", "/k", runner, logger)
	t.Cleanup(func() { set2.Close(context.Background()) })
	os.MkdirAll(filepath.Join(remoteDir, "sandbox.git"), 0o755)
	a, err = set2.Prepare(context.Background(), StepSpec{NodeID: "task/x", Repo: "sandbox", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("present bare repo: %v", err)
	}
	a.Teardown(context.Background())
	if len(runner.calls) != 0 {
		t.Errorf("no clone expected for a present repository, got %+v", runner.calls)
	}
}
