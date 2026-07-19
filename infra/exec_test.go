package infra

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript writes an executable shell script for CommandRunner tests —
// user commands are opaque files executed directly, so a script stands in for
// any resolver/data-source/hook without needing docker or nix.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Verifies b8db21752444: a failing invocation surfaces *ExecError via
// errors.As with the exit code and a bounded stderr tail.
func TestExecErrorContract(t *testing.T) {
	script := writeScript(t, `echo "diagnostic detail" >&2`+"\n"+`exit 3`)
	runner := NewCommandRunner(testLogger())
	res, err := runner.Run(context.Background(), CmdSpec{Path: script})
	if err == nil {
		t.Fatal("non-zero exit returned no error")
	}
	var xerr *ExecError
	if !errors.As(err, &xerr) {
		t.Fatalf("error %T is not *ExecError", err)
	}
	if xerr.ExitCode != 3 || res.ExitCode != 3 {
		t.Fatalf("exit codes %d/%d, want 3", xerr.ExitCode, res.ExitCode)
	}
	if !strings.Contains(xerr.Stderr, "diagnostic detail") {
		t.Fatalf("stderr tail %q missing diagnostics", xerr.Stderr)
	}
	if len(xerr.Stderr) > stderrTailLimit {
		t.Fatalf("stderr tail unbounded: %d bytes", len(xerr.Stderr))
	}
}

// Verifies b8db21752444: the stderr tail is bounded even when the command
// floods stderr.
func TestExecErrorStderrBounded(t *testing.T) {
	script := writeScript(t, `i=0
while [ $i -lt 2000 ]; do echo "spam line $i" >&2; i=$((i+1)); done
echo "final stderr line" >&2
exit 1`)
	_, err := NewCommandRunner(testLogger()).Run(context.Background(), CmdSpec{Path: script})
	var xerr *ExecError
	if !errors.As(err, &xerr) {
		t.Fatalf("error %T is not *ExecError", err)
	}
	if len(xerr.Stderr) > stderrTailLimit {
		t.Fatalf("stderr tail %d bytes exceeds bound %d", len(xerr.Stderr), stderrTailLimit)
	}
	if !strings.HasSuffix(xerr.Stderr, "final stderr line") {
		t.Fatalf("tail %q lost the newest stderr", xerr.Stderr[max(0, len(xerr.Stderr)-80):])
	}
}

// Verifies b8db21752444: CommandRunner secret hygiene — the error text
// contains the command path but neither the argument list nor any stdout
// bytes; stdout still reaches the caller in the result.
func TestCommandRunnerSecretHygiene(t *testing.T) {
	script := writeScript(t, `echo "TOPSECRET-TOKEN-VALUE"`+"\n"+`echo "resolver failed" >&2`+"\n"+`exit 1`)
	res, err := NewCommandRunner(testLogger()).Run(context.Background(), CmdSpec{
		Path: script,
		Args: []string{"SECRET-ARG-SERVICE"},
	})
	if err == nil {
		t.Fatal("non-zero exit returned no error")
	}
	msg := err.Error()
	if !strings.Contains(msg, script) {
		t.Fatalf("error %q does not carry the command path", msg)
	}
	if strings.Contains(msg, "TOPSECRET-TOKEN-VALUE") {
		t.Fatalf("error %q leaks stdout", msg)
	}
	if strings.Contains(msg, "SECRET-ARG-SERVICE") {
		t.Fatalf("error %q leaks the argv", msg)
	}
	var xerr *ExecError
	if errors.As(err, &xerr) && len(xerr.Args) != 1 {
		t.Fatalf("ExecError args %q not redacted to path-only", xerr.Args)
	}
	if !strings.Contains(string(res.Stdout), "TOPSECRET-TOKEN-VALUE") {
		t.Fatal("stdout did not reach the caller")
	}
}

// Verifies b8db21752444: user commands run against a minimal base env plus
// the declared entries — never the full host environment — with stdin, args,
// and dir honored, executed directly with no shell interpolation.
func TestCommandRunnerSpec(t *testing.T) {
	t.Setenv("FABER_LEAK_CANARY", "must-not-leak")
	dir := t.TempDir()
	script := writeScript(t, `printf 'arg1=%s\n' "$1"`+"\n"+`cat`+"\n"+`env`+"\n"+`pwd`)
	res, err := NewCommandRunner(testLogger()).Run(context.Background(), CmdSpec{
		Path:  script,
		Args:  []string{"$(hostname)"}, // stays literal: no shell interpolation
		Stdin: []byte("stdin-payload\n"),
		Env:   []string{"DECLARED=yes"},
		Dir:   dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "arg1=$(hostname)") {
		t.Fatalf("args were shell-interpolated: %q", out)
	}
	if !strings.Contains(out, "stdin-payload") {
		t.Fatalf("stdin not delivered: %q", out)
	}
	if !strings.Contains(out, "DECLARED=yes") {
		t.Fatalf("declared env missing: %q", out)
	}
	if strings.Contains(out, "must-not-leak") {
		t.Fatalf("host env leaked into the user command: %q", out)
	}
	if !strings.Contains(out, "PATH=") {
		t.Fatalf("base env missing PATH: %q", out)
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("working dir not honored: %q", out)
	}
}

// Verifies b8db21752444: context cancellation kills a hung user command
// (process group SIGTERM) and returns the context error via errors.Is.
func TestCommandRunnerCancel(t *testing.T) {
	script := writeScript(t, `sleep 300`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewCommandRunner(testLogger()).Run(ctx, CmdSpec{Path: script})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > killGrace+5*time.Second {
		t.Fatalf("cancelled command outlived the grace period: %v", elapsed)
	}
}

// Verifies b8db21752444: the git adapter resolves refs through plumbing
// against a real local repository and distinguishes absent refs (ErrRefAbsent)
// from failures.
func TestGitLsRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false",
			"commit", "--allow-empty", "-q", "-m", "x"},
	} {
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	git := NewGitCLI(testLogger())
	sha, err := git.LsRemote(context.Background(), dir, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !isHexSHA(sha) {
		t.Fatalf("resolved sha %q is not an object name", sha)
	}
	_, err = git.LsRemote(context.Background(), dir, "refs/heads/definitely-absent")
	if !errors.Is(err, ErrRefAbsent) {
		t.Fatalf("absent ref error %v, want ErrRefAbsent", err)
	}
	_, err = git.LsRemote(context.Background(), filepath.Join(dir, "nope"), "HEAD")
	if err == nil || errors.Is(err, ErrRefAbsent) {
		t.Fatalf("broken remote error %v must not be ErrRefAbsent", err)
	}
}

// Verifies b8db21752444: recorded real tool outputs (testdata fixtures) parse
// to the expected values, and truncated/malformed fixtures produce wrapped
// parse errors — never a scraping fallback.
func TestStructuredParseCatalog(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("docker inspect id", func(t *testing.T) {
		id, err := parseJSONString(read("docker_inspect_id.json"))
		if err != nil {
			t.Fatal(err)
		}
		if id != "sha256:3f57d9401f8d42f986df300f0c69192fc41da28ccc8d797829467780db3dd741" {
			t.Fatalf("id %q", id)
		}
		if _, err := parseJSONString([]byte("sha256:not-json")); err == nil {
			t.Fatal("free-text accepted")
		}
	})

	t.Run("docker load", func(t *testing.T) {
		tag, err := parseLoadedTag(read("docker_load.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if tag != "faber/review:0a1b2c3d4e5f" {
			t.Fatalf("tag %q", tag)
		}
		if _, err := parseLoadedTag(read("docker_load_untagged.txt")); err == nil {
			t.Fatal("untagged load output accepted")
		}
	})

	t.Run("git ls-remote", func(t *testing.T) {
		sha, err := parseLsRemote(read("git_ls_remote.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if sha != "8d1b2f0e9a3c4d5e6f708192a3b4c5d6e7f80912" {
			t.Fatalf("sha %q", sha)
		}
		if _, err := parseLsRemote(read("git_ls_remote_malformed.txt")); err == nil {
			t.Fatal("malformed plumbing output accepted")
		}
	})

	t.Run("nix build", func(t *testing.T) {
		paths, err := parseNixBuildOut(read("nix_build.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != "/nix/store/8ml9gcjjgcsgkl3cra5s0zmr26nlq6z9-docker-image-faber-review.tar.gz" {
			t.Fatalf("paths %q", paths)
		}
		if _, err := parseNixBuildOut(read("nix_build_truncated.json")); err == nil {
			t.Fatal("truncated build output accepted")
		}
		if _, err := parseNixBuildOut([]byte(`[]`)); err == nil {
			t.Fatal("empty build output accepted")
		}
	})

	t.Run("nix eval proof", func(t *testing.T) {
		m, err := decodeProof(json.RawMessage(read("nix_eval_proof.json")))
		if err != nil {
			t.Fatal(err)
		}
		if !m["git"] || m["custom-cli-a"] || m["custom-cli-b"] {
			t.Fatalf("proof %v", m)
		}
		if _, err := decodeProof(json.RawMessage(read("nix_eval_proof_truncated.json"))); err == nil {
			t.Fatal("truncated proof accepted")
		}
	})
}

// Verifies b8db21752444: every adapter interface is fakeable — the fakes used
// across this suite satisfy the interfaces at compile time.
func TestAdaptersAreFakeable(t *testing.T) {
	var _ DockerClient = (*fakeDocker)(nil)
	var _ NixClient = (*fakeNix)(nil)
	var _ GitClient = NewGitCLI(nil)
	var _ CommandRunner = NewCommandRunner(nil)
}

// Verifies b8db21752444 (IN-F3): a user command's captured stdout is bounded —
// a data source streaming without limit fails loudly instead of buffering
// host memory — and the refusal names the bound, never the bytes.
func TestUserCmdStdoutBounded(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	r := NewCommandRunner(testLogger())
	_, err := r.Run(context.Background(), CmdSpec{
		Path: "/bin/sh",
		Args: []string{"-c", "head -c 70000000 /dev/zero"},
	})
	if err == nil || !strings.Contains(err.Error(), "stdout exceeded") {
		t.Fatalf("want the stdout bound refusal, got %v", err)
	}

	// Under the bound: bytes flow untouched.
	res, err := r.Run(context.Background(), CmdSpec{
		Path: "/bin/sh",
		Args: []string{"-c", "printf hello"},
	})
	if err != nil || string(res.Stdout) != "hello" {
		t.Fatalf("bounded run mangled stdout: %q, %v", res.Stdout, err)
	}
}
