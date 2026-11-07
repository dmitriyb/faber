//go:build realinfra

package infra

// Real-infrastructure integration suite: needs working docker and nix
// binaries plus network access to fetch the pinned nixpkgs. It never runs in
// the sandboxed unit gate (go test ./...); on an acceptance machine run:
//
//	go test -tags realinfra -timeout 30m ./infra/
//
// What this suite verifies on a real machine:
//   - Build-load-run round trip (test_infra.md scenario 8): a minimal
//     [coreutils] toolset builds via real nix, loads under the computed tag,
//     runs with output captured, has root-owned non-writable store paths,
//     and rebuilds are no-ops (daemon-as-cache).
//   - Kill on cancel (scenario 9): a cancelled run returns context.Canceled
//     within the grace window, --rm removes the deterministically named
//     container, and partial output/timing survive.
//   - Non-zero exit is data (scenario 10): entry ["false"] yields err == nil
//     and ExitCode == 1.
//   - Stdin secrets delivery (scenario 11): a StdinSecrets payload streamed
//     over docker run -i to entry ["cat"] is echoed back byte-for-byte, proving
//     the stream reaches the container intact and closes on EOF.
//   - Package resolution proof against real nix eval: stock names resolve,
//     bogus names come back as field-path errors without any build.
//   - Docker structured queries against a real daemon: ImageExists /
//     NetworkExists answer negatively for absent objects without error.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

func realLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker", "nix"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed; realinfra suite needs a real machine", tool)
		}
	}
}

// countingNix wraps a real NixClient to prove daemon-as-cache behavior.
type countingNix struct {
	inner  NixClient
	mu     sync.Mutex
	builds int
}

func (c *countingNix) Eval(ctx context.Context, exprFile string, args []string) (json.RawMessage, error) {
	return c.inner.Eval(ctx, exprFile, args)
}

func (c *countingNix) Build(ctx context.Context, exprFile string) ([]string, error) {
	c.mu.Lock()
	c.builds++
	c.mu.Unlock()
	return c.inner.Build(ctx, exprFile)
}

func (c *countingNix) buildCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builds
}

// minimalToolset is the smallest real build: coreutils, no overlay.
func minimalToolset() config.BuildDef {
	return config.BuildDef{Packages: []string{"coreutils"}}
}

// realBox compiles the real faber-box for the image's architecture (the
// Linux variant of the host arch, as the expression targets): the binary the
// builder bakes into every image.
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

// Verifies 0acac696dcf1 + 0c82c6478856 (scenario 8): build-load-run round
// trip with the minimal toolset, immutability of store paths at runtime, and
// no nix invocation on rebuild.
func TestRealinfra_BuildLoadRunRoundTrip(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	docker := NewDockerCLI(logger)
	nix := &countingNix{inner: NewNixCLI(logger)}
	b := NewImageBuilder(docker, nix, DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)

	tag, err := b.Build(ctx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	exists, err := docker.ImageExists(ctx, tag)
	if err != nil || !exists {
		t.Fatalf("image %s not in daemon after build: exists=%v err=%v", tag, exists, err)
	}

	runner := NewContainerRunner(docker, logger)
	name := fmt.Sprintf("faber-realinfra-%d", time.Now().UnixNano())
	// The two streams reach two writers: ls writes its listing to stdout,
	// nothing to stderr.
	var stdout bytes.Buffer
	res, err := runner.Run(ctx, ContainerSpec{Name: name, Image: tag, Entry: []string{"ls", "/bin"}, Stdout: &stdout})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 || stdout.Len() == 0 || len(res.Output) != 0 {
		t.Fatalf("ls /bin: exit %d, %d stdout bytes, stderr %q", res.ExitCode, stdout.Len(), res.Output)
	}

	// Store paths are root-owned...
	if out := runStdout(t, ctx, runner, ContainerSpec{Name: name + "-stat", Image: tag,
		Entry: []string{"stat", "-L", "-c", "%u", "/bin/ls"}}); out != "0" {
		t.Fatalf("store path not root-owned: %q", out)
	}
	// The baked faber-box is image content: present, root-owned, executable,
	// and it answers for itself.
	// Store paths are canonicalised to 0555, so the mode is r-x, never rwx.
	if out := runStdout(t, ctx, runner, ContainerSpec{Name: name + "-box", Image: tag,
		Entry: []string{"stat", "-L", "-c", "%u %A", "/faber/bin/faber-box"}}); !regexp.MustCompile(`^0 -r.xr.xr.x`).MatchString(out) {
		t.Fatalf("baked faber-box not root-owned and executable: %q", out)
	}
	if out := runStdout(t, ctx, runner, ContainerSpec{Name: name + "-boxv", Image: tag, Entry: []string{"/faber/bin/faber-box", "version"}}); !strings.HasPrefix(out, "faber-box ") {
		t.Fatalf("baked faber-box does not run: %q", out)
	}
	// ...and not writable by a non-root run user (bindings carry --user, the
	// verbatim-splice seam the security module will use).
	res, err = runner.Run(ctx, ContainerSpec{Name: name + "-write", Image: tag,
		Bindings: []string{"--user", "65534:65534"},
		Entry:    []string{"touch", "/bin/ls"}})
	if err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("non-root user could modify a store path")
	}

	// Rebuild performs no nix invocation: the daemon is the cache.
	before := nix.buildCalls()
	tag2, err := b.Build(ctx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if tag2 != tag {
		t.Fatalf("rebuild tag %q, want %q", tag2, tag)
	}
	if nix.buildCalls() != before {
		t.Fatalf("rebuild invoked nix %d more times", nix.buildCalls()-before)
	}
}

// Verifies 0c82c6478856 (scenario 9): kill on cancel — Run returns
// context.Canceled within the grace window, the deterministic name is gone
// from docker ps -a (--rm completed after kill), and the ContainerResult still
// carries partial output and timing.
func TestRealinfra_KillOnCancel(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer setupCancel()

	docker := NewDockerCLI(logger)
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	tag, err := b.Build(setupCtx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	runner := NewContainerRunner(docker, logger)
	name := fmt.Sprintf("faber-realinfra-cancel-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(setupCtx)
	go func() {
		time.Sleep(3 * time.Second)
		cancel()
	}()
	start := time.Now()
	// yes provides continuous output so the partial-output capture is
	// provable; it writes to stdout, so the Stdout writer (a bounded tail,
	// as the runner keeps for stderr) is where the partial output shows.
	partial := newTailBuffer(outputCap)
	res, err := runner.Run(ctx, ContainerSpec{Name: name, Image: tag, Entry: []string{"yes", "partial-line"}, Stdout: partial})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Fatalf("cancelled run returned after %v, outside any grace window", elapsed)
	}
	if !strings.Contains(string(partial.Bytes()), "partial-line") {
		t.Fatal("partial output lost on cancel")
	}
	if res.Duration <= 0 {
		t.Fatalf("timing lost on cancel: %v", res.Duration)
	}

	// --rm finishes asynchronously after the kill; poll briefly.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.CommandContext(setupCtx, "docker", "ps", "-a",
			"--filter", "name="+name, "--format", "{{json .Names}}").Output()
		if err != nil {
			t.Fatalf("docker ps: %v", err)
		}
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still listed after kill: %s", name, out)
		}
		time.Sleep(time.Second)
	}
}

// Verifies 0c82c6478856 (scenario 10, integration half): entry ["false"]
// returns err == nil and ExitCode == 1 against a real daemon.
func TestRealinfra_NonZeroExitIsData(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	docker := NewDockerCLI(logger)
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	tag, err := b.Build(ctx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner := NewContainerRunner(docker, logger)
	res, err := runner.Run(ctx, ContainerSpec{
		Name:  fmt.Sprintf("faber-realinfra-false-%d", time.Now().UnixNano()),
		Image: tag,
		Entry: []string{"false"},
	})
	if err != nil {
		t.Fatalf("box failure surfaced as a Go error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit code %d, want 1", res.ExitCode)
	}
}

// Verifies 0c82c6478856 (scenario 11, integration half): a StdinSecrets payload
// streamed to a real container over docker run -i reaches the process intact and
// the write end is closed on EOF — entry ["cat"] reads stdin to EOF and echoes
// the exact bytes back through captured output, which both proves delivery and
// proves the read terminated (cat would block forever without the close-on-EOF).
func TestRealinfra_StdinSecretsDelivery(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	docker := NewDockerCLI(logger)
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	tag, err := b.Build(ctx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner := NewContainerRunner(docker, logger)
	payload := []byte(`{"agent-api":"dG9rLXNlY3JldA=="}`)
	var stdout bytes.Buffer
	res, err := runner.Run(ctx, ContainerSpec{
		Name:         fmt.Sprintf("faber-realinfra-stdin-%d", time.Now().UnixNano()),
		Image:        tag,
		StdinSecrets: payload,
		Entry:        []string{"cat"},
		Stdout:       &stdout,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("cat exited %d (a hang would time out instead)", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), string(payload)) {
		t.Fatalf("stdin payload not echoed back: stdout %q, want %q", stdout.String(), payload)
	}
}

// Verifies b329adcbbfe4 against real nix eval: stock names prove resolvable,
// a bogus name is a field-path error, and nothing is built either way.
func TestRealinfra_PackageResolutionProof(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	nix := &countingNix{inner: NewNixCLI(logger)}
	b := NewImageBuilder(NewDockerCLI(logger), nix, DefaultNixpkgsPin(), "", realBox(t), logger)

	if err := b.ProvePackages(ctx, "ok", config.BuildDef{Packages: []string{"coreutils", "git"}}); err != nil {
		t.Fatalf("stock packages failed the proof: %v", err)
	}
	err := b.ProvePackages(ctx, "bad", config.BuildDef{Packages: []string{"coreutils", "faber-definitely-not-a-package"}})
	if err == nil {
		t.Fatal("bogus package proved resolvable")
	}
	if !strings.Contains(err.Error(), `templates.bad.build.packages: "faber-definitely-not-a-package" does not resolve`) {
		t.Fatalf("proof error %q lacks the field path", err)
	}
	if nix.buildCalls() != 0 {
		t.Fatal("the proof built something")
	}
}

// Verifies b8db21752444 against a real daemon: structured queries answer
// "absent" without error for missing objects.
func TestRealinfra_DockerStructuredQueries(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	docker := NewDockerCLI(logger)
	exists, err := docker.ImageExists(ctx, "faber/definitely-absent:000000000000")
	if err != nil || exists {
		t.Fatalf("absent image: exists=%v err=%v", exists, err)
	}
	netExists, err := docker.NetworkExists(ctx, "faber-definitely-absent-net")
	if err != nil || netExists {
		t.Fatalf("absent network: exists=%v err=%v", netExists, err)
	}
	netExists, err = docker.NetworkExists(ctx, "bridge")
	if err != nil || !netExists {
		t.Fatalf("bridge network: exists=%v err=%v", netExists, err)
	}
}

// Verifies 0acac696dcf1 on a real machine: an image built with a prelude
// hook and a one-skill named leg carries them as root-owned image content at
// the contract paths, next to the baked faber-box, and /faber stays a plain
// directory the daemon can mount into.
func TestRealinfra_BakedInputs(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	hooks := t.TempDir()
	prelude := filepath.Join(hooks, "prelude")
	if err := os.WriteFile(prelude, []byte("#!/bin/bash\necho prelude-ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := t.TempDir()
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("skill a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewImageBuilder(NewDockerCLI(logger), NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	// coreutils has no shell; bash gives the hook its interpreter and the probe its -c.
	tag, err := b.Build(ctx, "realinfra-inputs", config.BuildDef{Packages: []string{"coreutils", "bash"}}, ImageInputs{
		Hooks:  config.HookSet{Prelude: prelude},
		Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: skill}}, Primary: "a", Link: ".x/skills"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner := NewContainerRunner(NewDockerCLI(logger), logger)
	name := fmt.Sprintf("faber-realinfra-inputs-%d", time.Now().UnixNano())
	run := func(suffix string, spec ContainerSpec) string {
		t.Helper()
		spec.Name, spec.Image = name+suffix, tag
		return runStdout(t, ctx, runner, spec)
	}
	if out := run("-hook", ContainerSpec{Entry: []string{"stat", "-L", "-c", "%u %A", "/faber/hooks/prelude"}}); !regexp.MustCompile(`^0 -r.xr.xr.x`).MatchString(out) {
		t.Fatalf("baked prelude: %q", out)
	}
	if out := run("-hookrun", ContainerSpec{Entry: []string{"/faber/hooks/prelude"}}); out != "prelude-ran" {
		t.Fatalf("baked prelude does not run: %q", out)
	}
	if out := run("-skill", ContainerSpec{Entry: []string{"cat", "/faber/skills/a/SKILL.md"}}); out != "skill a" {
		t.Fatalf("baked skill: %q", out)
	}
	// /faber is a real directory: the daemon mounts the engine tmpfs into it.
	if out := run("-mount", ContainerSpec{
		Mounts: []Mount{{Kind: KindTmpfs, Container: "/faber/bundle"}, {Kind: KindTmpfs, Container: "/faber/result"}},
		Entry:  []string{"bash", "-c", "test -d /faber/bundle && test -d /faber/result && test -x /faber/bin/faber-box && echo ok"},
	}); out != "ok" {
		t.Fatalf("/faber mount points: %q", out)
	}
}

// runStdout runs the spec and returns its trimmed stdout, failing the test
// on an actuation error or a non-zero exit (with the stderr tail).
func runStdout(t *testing.T, ctx context.Context, runner *ContainerRunner, spec ContainerSpec) string {
	t.Helper()
	var stdout bytes.Buffer
	spec.Stdout = &stdout
	res, err := runner.Run(ctx, spec)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%s: exit %d err %v stderr %q stdout %q", spec.Name, res.ExitCode, err, res.Output, stdout.String())
	}
	return strings.TrimSpace(stdout.String())
}

// Verifies 0c82c6478856 and 87f006277d2c on a real machine: a held box
// receives a frame stream over docker exec into its handoff tmpfs and its
// session dir under HOME, owned by the run user along with the writable
// mounts a preamble would have chowned (HOME, /workspace, the path down to
// the session dir), and a second exec sees the files — the mount-free
// re-entry path up to the terminal attach, which needs an operator.
func TestRealinfra_HoldAndReceive(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	docker := NewDockerCLI(logger)
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), realBox(t), logger)
	tag, err := b.Build(ctx, "realinfra-min", minimalToolset(), ImageInputs{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	name := fmt.Sprintf("faber-realinfra-hold-%d", time.Now().UnixNano())
	spec := ContainerSpec{Name: name, Image: tag, Entry: []string{holdEntry, "hold"},
		Mounts: []Mount{
			{Kind: KindTmpfs, Container: "/faber/handoff"},
			{Kind: KindTmpfs, Container: "/home/box"},
			{Kind: KindVolume, Container: "/workspace"},
		},
		Env: map[string]string{"FABER_RUN_UID": "1000", "FABER_RUN_GID": "1000", "FABER_SESSIONS_DIR": "/home/box/.h/sessions"}}
	if code, err := docker.ContainerRun(ctx, detachArgs(spec), nil, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatalf("start held box: exit %d err %v", code, err)
	}
	defer docker.Kill(context.WithoutCancel(ctx), name)

	var stream bytes.Buffer
	w := contract.NewFrameWriter(&stream)
	w.Frame(contract.FrameFile, "handoff/handoff.json", time.Time{}, strings.NewReader(`{"phase":"agent"}`), 17)
	w.Frame(contract.FrameFile, "handoff/bundle/CONTEXT.md", time.Time{}, strings.NewReader("ctx\n"), 4)
	w.Frame(contract.FrameFile, "sessions/a.jsonl", time.Time{}, strings.NewReader("{}\n"), 3)
	w.End()
	var stderr bytes.Buffer
	if code, err := docker.Exec(ctx, name, []string{"/faber/bin/faber-box", "receive"}, &stream, io.Discard, &stderr); err != nil || code != 0 {
		t.Fatalf("receive: exit %d err %v stderr %q", code, err, stderr.String())
	}
	var stdout bytes.Buffer
	owned := []string{"/faber/handoff", "/faber/handoff/handoff.json", "/faber/handoff/bundle/CONTEXT.md",
		"/home/box", "/home/box/.h", "/home/box/.h/sessions", "/home/box/.h/sessions/a.jsonl", "/workspace", "/tmp"}
	if code, err := docker.Exec(ctx, name, append([]string{"stat", "-c", "%u:%g %n"}, owned...), nil, &stdout, &stderr); err != nil || code != 0 {
		t.Fatalf("stat: exit %d err %v stderr %q", code, err, stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if !strings.HasPrefix(line, "1000:1000 ") {
			t.Fatalf("not owned by the run user: %q (all: %q)", line, stdout.String())
		}
	}
	stdout.Reset()
	if code, err := docker.Exec(ctx, name, []string{"cat", "/faber/handoff/bundle/CONTEXT.md"}, nil, &stdout, &stderr); err != nil || code != 0 || stdout.String() != "ctx\n" {
		t.Fatalf("cat: exit %d err %v stdout %q", code, err, stdout.String())
	}
}
