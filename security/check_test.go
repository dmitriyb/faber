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
	"github.com/dmitriyb/faber/infra"
)

// fakeSeeder records the seed containers the check remote runs and answers
// each with a fixed exit.
type fakeSeeder struct {
	specs []infra.ContainerSpec
	code  int
	out   string
}

func (f *fakeSeeder) Run(_ context.Context, spec infra.ContainerSpec) (infra.ContainerResult, error) {
	f.specs = append(f.specs, spec)
	return infra.ContainerResult{ExitCode: f.code, Output: []byte(f.out)}, nil
}

// Verifies fb62c46ab1c1: the check composition hands the box no network,
// a bare repository in the check's volume as its only remote — seeded from
// a bundle of the working directory's repository through a container of
// the step's image — the generated key as every role's identity, and no
// credential at all — in the fixed binding order.
func TestCheckBindingSetSandbox(t *testing.T) {
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	agent := newFakeAgent()
	runner := &fakeRunner{write: "# v2 git bundle\nbundle-bytes"}
	seeder := &fakeSeeder{}
	const volume = "faber-check-123-remote"
	key := filepath.Join(t.TempDir(), "check_ed25519")
	set := NewCheckBindingSet(NewIdentityBinding(agent, logger), volume, "/work/project", key, seeder, runner, logger)
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
		Image:        "faber/impl:abc",
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
	// remote: the volume at the mount point, the bare repository's URL under it
	wantMount := "-v " + volume + ":" + ContainerCheckRemote
	wantURL := "-e " + EnvRemoteURL + "=" + ContainerCheckRemote + "/sandbox.git"
	if !strings.Contains(args, wantMount) || !strings.Contains(args, wantURL) || strings.Contains(args, EnvHostKey) || strings.Contains(args, "gateway") {
		t.Errorf("remote contribution: %q", args)
	}
	// seeded once: a bundle of the working directory's repository into a
	// container of the step's image with the volume mounted
	if len(runner.calls) != 1 || runner.calls[0].Path != "git" || len(runner.calls[0].Args) != 7 ||
		!slices.Equal(runner.calls[0].Args[:5], []string{"-C", "/work/project", "bundle", "create", "--quiet"}) ||
		!strings.Contains(runner.calls[0].Args[5], "faber-check-bundle-") || runner.calls[0].Args[6] != "--all" {
		t.Errorf("bundle call: %+v", runner.calls)
	}
	if len(seeder.specs) != 1 {
		t.Fatalf("seed containers: %d, want 1", len(seeder.specs))
	}
	seed := seeder.specs[0]
	if seed.Image != "faber/impl:abc" || !slices.Equal(seed.Entry, []string{ContainerEntry, "seed-remote", "sandbox"}) ||
		string(seed.StdinSecrets) != "# v2 git bundle\nbundle-bytes" || !strings.HasPrefix(seed.Name, "faber-check-seed-sandbox-") {
		t.Errorf("seed container: %+v", seed)
	}
	if len(seed.Mounts) != 1 || seed.Mounts[0] != (infra.Mount{Kind: infra.KindNamed, Name: volume, Container: ContainerCheckRemote}) {
		t.Errorf("seed mounts: %+v", seed.Mounts)
	}
	if seed.Env["FABER_RUN_UID"] == "" || seed.Env["FABER_RUN_GID"] == "" {
		t.Errorf("seed env lacks the run ids: %v", seed.Env)
	}
	// The seed container is a check container too: no network, the step's runtime.
	if !slices.Equal(seed.Bindings, []string{"--network", "none", "--runtime=runsc"}) {
		t.Errorf("seed bindings = %q", seed.Bindings)
	}
	if _, err := os.Stat(runner.calls[0].Args[5]); !os.IsNotExist(err) {
		t.Errorf("the bundle file was left behind: %s", runner.calls[0].Args[5])
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
	runner := &fakeRunner{write: "bundle"}
	seeder := &fakeSeeder{}
	set := NewCheckBindingSet(NewIdentityBinding(newFakeAgent(), logger), "vol", "", "/k", seeder, runner, logger)
	t.Cleanup(func() { set.Close(context.Background()) })

	a, err := set.Prepare(context.Background(), StepSpec{NodeID: "task/bare", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("repo-less step: %v", err)
	}
	if got := strings.Join(a.Args, " "); got != "--network none" {
		t.Errorf("repo-less, identity-less step: %q", got)
	}
	a.Teardown(context.Background())

	_, err = set.Prepare(context.Background(), StepSpec{NodeID: "task/x", Repo: "sandbox", Image: "img", ScratchDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "needs a git repository in the working directory") {
		t.Fatalf("repo-bound step without a source repository: %v", err)
	}
	if len(seeder.specs) != 0 {
		t.Fatalf("no seed may run without a source repository, got %d", len(seeder.specs))
	}

	// one seeding per repository: a second step of the same repository
	// runs no container, another repository runs one more
	set2 := NewCheckBindingSet(NewIdentityBinding(newFakeAgent(), logger), "vol", "/work/project", "/k", seeder, runner, logger)
	t.Cleanup(func() { set2.Close(context.Background()) })
	for _, repo := range []string{"sandbox", "sandbox", "acme/widgets"} {
		a, err = set2.Prepare(context.Background(), StepSpec{NodeID: "task/" + repo, Repo: repo, Image: "img", ScratchDir: t.TempDir()})
		if err != nil {
			t.Fatalf("repo %s: %v", repo, err)
		}
		a.Teardown(context.Background())
	}
	if len(seeder.specs) != 2 || seeder.specs[0].Entry[2] != "sandbox" || seeder.specs[1].Entry[2] != "acme/widgets" {
		t.Errorf("seed containers: %+v", seeder.specs)
	}
	// A nested repository keeps its slash in the entry and loses it in the
	// container name, which docker would refuse.
	if n := seeder.specs[1].Name; !strings.HasPrefix(n, "faber-check-seed-acme-widgets-") || strings.Contains(n, "/") {
		t.Errorf("nested repo seed name: %q", n)
	}
	if len(seeder.specs[1].Bindings) != 2 {
		t.Errorf("a step without a runtime must seed with --network none only, got %q", seeder.specs[1].Bindings)
	}

	// a seed container that fails fails the step before launch, naming why
	failing := &fakeSeeder{code: 1, out: "seed-remote: git clone --bare exited 128"}
	set3 := NewCheckBindingSet(NewIdentityBinding(newFakeAgent(), logger), "vol", "/work/project", "/k", failing, runner, logger)
	t.Cleanup(func() { set3.Close(context.Background()) })
	_, err = set3.Prepare(context.Background(), StepSpec{NodeID: "task/x", Repo: "sandbox", Image: "img", ScratchDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "exited 1") || !strings.Contains(err.Error(), "git clone --bare exited 128") {
		t.Fatalf("failing seed: %v", err)
	}
}
