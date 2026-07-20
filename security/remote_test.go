package security

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func remoteBinding() *RemoteBinding {
	return NewRemoteBinding(slog.New(slog.DiscardHandler))
}

// Verifies 14a0498eb362: pinned mode reads the gateway host-key line
// host-side and exports it alongside the spliced clone URL; nothing
// tofu-related appears — test scenario 4.
func TestRemotePinnedHostKey(t *testing.T) {
	c, err := remoteBinding().Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{
		"-e", "FABER_REMOTE_URL=ssh://git@gateway/srv/git/sandbox.git",
		"-e", "FABER_HOST_KEY=" + hostKeyLine,
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args:\nwant %q\ngot  %q", want, c.Args)
	}
	for _, a := range c.Args {
		if strings.Contains(a, "TOFU") {
			t.Fatalf("pinned mode must carry nothing tofu-related, got %q", a)
		}
	}
}

// Verifies 14a0498eb362: tofu mode (sandbox-only explicit opt-in) emits the
// flag env and no key line — test scenario 4.
func TestRemoteTOFUMode(t *testing.T) {
	step := implementStep(t.TempDir())
	step.Remote = &config.RemoteDef{URL: "ssh://git@gateway/srv/git", TOFU: true}
	c, err := remoteBinding().Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{
		"-e", "FABER_REMOTE_URL=ssh://git@gateway/srv/git/sandbox.git",
		"-e", "FABER_HOST_KEY_TOFU=1",
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args: want %q, got %q", want, c.Args)
	}
}

// Verifies 14a0498eb362: with neither pinned key nor tofu configured the
// binding aborts with a clear error — there is no silent default.
func TestRemoteNeitherModeAborts(t *testing.T) {
	step := implementStep(t.TempDir())
	step.Remote = &config.RemoteDef{URL: "ssh://git@gateway/srv/git"}
	_, err := remoteBinding().Prepare(context.Background(), step)
	errContains(t, err, "no host-key policy")
	errContains(t, err, "host_key_file")
	errContains(t, err, "tofu")
}

// Verifies 14a0498eb362: both modes configured is a loader rejection;
// assembly never picks one — it fails closed.
func TestRemoteBothModesFailClosed(t *testing.T) {
	step := implementStep(t.TempDir())
	step.Remote = &config.RemoteDef{URL: "ssh://git@gateway/srv/git", HostKeyFile: "testdata/gateway_host_key.pub", TOFU: true}
	_, err := remoteBinding().Prepare(context.Background(), step)
	errContains(t, err, "exactly one")
}

// Verifies 14a0498eb362: an unreadable, empty, or multi-line host key file
// fails the step before launch — pinned mode fails closed — test scenario 4.
func TestRemoteBadHostKeyFileFailsBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pub")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	multi := filepath.Join(dir, "multi.pub")
	if err := os.WriteFile(multi, []byte("line-one\nline-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, file, want string
	}{
		{"unreadable", filepath.Join(dir, "absent.pub"), "read host key file"},
		{"empty", empty, "is empty"},
		{"multi-line", multi, "single host-key line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := implementStep(dir)
			step.Remote = &config.RemoteDef{URL: "ssh://git@gateway/srv/git", HostKeyFile: tt.file}
			_, err := remoteBinding().Prepare(context.Background(), step)
			errContains(t, err, tt.want)
		})
	}
}

// Verifies 14a0498eb362 and 18e9f712c810 (first pass only): a repo-less step
// or an absent remote section contributes nothing at all — the box's clone
// phase is skipped by contract, the configuration is legal, and outputs live
// in the step's typed result with the egress lock as the only enforced
// boundary. No gateless trust-boundary machinery exists.
func TestRemoteGatelessContributesNothing(t *testing.T) {
	noRepo := implementStep(t.TempDir())
	noRepo.Repo = ""
	noRemote := implementStep(t.TempDir())
	noRemote.Remote = nil
	for name, step := range map[string]StepSpec{"repo-less step": noRepo, "no remote section": noRemote} {
		c, err := remoteBinding().Prepare(context.Background(), step)
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		if len(c.Args) != 0 || c.Teardown != nil {
			t.Fatalf("%s: want empty contribution, got %q", name, c.Args)
		}
	}
}

// Verifies 14a0498eb362: the clone URL is the configured prefix plus the
// resolved repo input suffixed .git, tolerant of a trailing slash.
func TestRemoteCloneURLSplice(t *testing.T) {
	tests := []struct {
		prefix, repo, want string
	}{
		{"ssh://git@gateway/srv/git", "sandbox", "ssh://git@gateway/srv/git/sandbox.git"},
		{"ssh://git@gateway/srv/git/", "sandbox", "ssh://git@gateway/srv/git/sandbox.git"},
	}
	for _, tt := range tests {
		if got := cloneURL(tt.prefix, tt.repo); got != tt.want {
			t.Fatalf("cloneURL(%q, %q): want %q, got %q", tt.prefix, tt.repo, tt.want, got)
		}
	}
}

// Verifies 14a0498eb362 (L-P3i): the repo input — possibly an upstream box's
// output — must be a plain repository path before it is spliced into the
// clone URL; traversal and URL-reshaping shapes fail the step before launch.
func TestRemoteRepoShape(t *testing.T) {
	remote := &config.RemoteDef{URL: "ssh://git@gw/srv/git", TOFU: true}
	b := NewRemoteBinding(nil)
	for _, bad := range []string{"../up", "a/../b", "a b", "a;rm", "a\nb", "/abs", "a//b", ".", "..", "a?x=1", "a#f", "org/../../top"} {
		if _, err := b.Prepare(context.Background(), StepSpec{Remote: remote, Repo: bad}); err == nil {
			t.Errorf("repo %q must be refused", bad)
		}
	}
	for _, good := range []string{"sandbox", "org/repo", "a.b/c-d_e"} {
		c, err := b.Prepare(context.Background(), StepSpec{Remote: remote, Repo: good})
		if err != nil {
			t.Errorf("repo %q must pass: %v", good, err)
			continue
		}
		if len(c.Args) == 0 || !strings.Contains(c.Args[1], good+".git") {
			t.Errorf("repo %q did not splice: %v", good, c.Args)
		}
	}
}
