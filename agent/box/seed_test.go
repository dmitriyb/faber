package box

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedRepo makes a throwaway repository with one commit of the given
// message (its own HOME, so no global git config reaches it) and returns
// its bundle and HEAD.
func seedRepo(t *testing.T, msg string) (bundle []byte, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "src")
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "HOME="+root, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("-c", "user.name=t", "-c", "user.email=t@t.invalid", "commit", "-q", "--allow-empty", "-m", msg)
	head = git("rev-parse", "HEAD")
	bundle = []byte(git("bundle", "create", "-", "--all"))
	if !bytes.HasPrefix(bundle, []byte("# v2 git bundle")) {
		t.Fatalf("not a bundle: %q", bundle[:min(len(bundle), 20)])
	}
	return bundle, head
}

// Verifies 93ba0858d75f (test scenario 27): seed-remote turns the bundle on
// stdin into <repo>.git under the remote dir and leaves no bundle behind;
// a second seeding leaves the first repository alone; an empty stdin, a bad
// name and a corrupt bundle are errors that leave no repository.
func TestSeedRemote(t *testing.T) {
	bundle, head := seedRepo(t, "init")
	remote := filepath.Join(t.TempDir(), "remote")
	env := &BoxEnv{RemoteDir: remote}
	runner := NewExecRunner(io.Discard, io.Discard)
	if err := SeedRemote(context.Background(), env, "sandbox", bytes.NewReader(bundle), runner, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bare := filepath.Join(remote, "sandbox.git")
	out, err := exec.Command("git", "-C", bare, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != head {
		t.Fatalf("seeded HEAD = %q err %v, want %s", out, err, head)
	}
	entries, _ := os.ReadDir(remote)
	for _, e := range entries {
		if e.Name() != "sandbox.git" {
			t.Fatalf("left behind under the remote dir: %s", e.Name())
		}
	}

	// A second seeding — another bundle — leaves the first repository alone.
	other, otherHead := seedRepo(t, "another")
	if otherHead == head {
		t.Fatal("the fixture made the same commit twice")
	}
	if err := SeedRemote(context.Background(), env, "sandbox", bytes.NewReader(other), runner, nil); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	out, _ = exec.Command("git", "-C", bare, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(out)) != head {
		t.Fatalf("a present repository was replaced: HEAD %q", out)
	}

	for name, in := range map[string]io.Reader{
		"empty stdin":    strings.NewReader(""),
		"corrupt bundle": strings.NewReader("# v2 git bundle\ngarbage\n"),
	} {
		if err := SeedRemote(context.Background(), env, "fresh", in, runner, nil); err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if _, err := os.Stat(filepath.Join(remote, "fresh.git")); !os.IsNotExist(err) {
			t.Fatalf("%s: a repository was left behind", name)
		}
	}
	for _, bad := range []string{"../x", "/abs", "a//b", "a/./b", ""} {
		if err := SeedRemote(context.Background(), env, bad, bytes.NewReader(bundle), runner, nil); err == nil {
			t.Fatalf("repo %q: want a refusal", bad)
		}
	}

	// A nested repository path, as the remote binding's clone URL allows,
	// lands under its directories.
	if err := SeedRemote(context.Background(), env, "acme/widgets", bytes.NewReader(bundle), runner, nil); err != nil {
		t.Fatalf("nested repo: %v", err)
	}
	out, err = exec.Command("git", "-C", filepath.Join(remote, "acme", "widgets.git"), "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != head {
		t.Fatalf("nested HEAD = %q err %v", out, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(remote, "acme")); len(entries) != 1 {
		t.Fatalf("left behind beside the nested repository: %d entries", len(entries))
	}
}

// Verifies 93ba0858d75f (test scenario 27, the race): a clone finding the
// target already occupied — another seeder or an earlier run got there
// first — is not an error and does not replace it; an empty or missing
// target takes the clone.
func TestInstallBare(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, files ...string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			os.WriteFile(filepath.Join(p, f), []byte(f), 0o644)
		}
		return p
	}
	occupied := mk("occupied.git", "HEAD")
	first, err := installBare(mk("tmp1", "config"), occupied)
	if err != nil || first {
		t.Fatalf("occupied target: first=%v err=%v, want the other's repository kept", first, err)
	}
	if _, err := os.Stat(filepath.Join(occupied, "HEAD")); err != nil {
		t.Fatal("the occupied repository was replaced")
	}
	first, err = installBare(mk("tmp2", "config"), filepath.Join(dir, "fresh.git"))
	if err != nil || !first {
		t.Fatalf("fresh target: first=%v err=%v", first, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.git", "config")); err != nil {
		t.Fatal("the clone did not land")
	}
	if _, err := installBare(filepath.Join(dir, "missing-tmp"), filepath.Join(dir, "x.git")); err == nil {
		t.Fatal("a missing source must be an error")
	}
}
