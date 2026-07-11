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
//   - Package resolution proof against real nix eval: stock names resolve,
//     bogus names come back as field-path errors without any build.
//   - Docker structured queries against a real daemon: ImageExists /
//     NetworkExists answer negatively for absent objects without error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

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
	b := NewImageBuilder(docker, nix, DefaultNixpkgsPin(), t.TempDir(), logger)

	tag, err := b.Build(ctx, "realinfra-min", minimalToolset())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	exists, err := docker.ImageExists(ctx, tag)
	if err != nil || !exists {
		t.Fatalf("image %s not in daemon after build: exists=%v err=%v", tag, exists, err)
	}

	runner := NewContainerRunner(docker, logger)
	name := fmt.Sprintf("faber-realinfra-%d", time.Now().UnixNano())
	res, err := runner.Run(ctx, RunSpec{Name: name, Image: tag, Entry: []string{"ls", "/bin"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 || len(res.Output) == 0 {
		t.Fatalf("ls /bin: exit %d, %d output bytes", res.ExitCode, len(res.Output))
	}

	// Store paths are root-owned...
	res, err = runner.Run(ctx, RunSpec{Name: name + "-stat", Image: tag,
		Entry: []string{"stat", "-L", "-c", "%u", "/bin/ls"}})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Output)) != "0" {
		t.Fatalf("store path not root-owned: exit %d output %q", res.ExitCode, res.Output)
	}
	// ...and not writable by a non-root run user (bindings carry --user, the
	// verbatim-splice seam the security module will use).
	res, err = runner.Run(ctx, RunSpec{Name: name + "-write", Image: tag,
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
	tag2, err := b.Build(ctx, "realinfra-min", minimalToolset())
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
// from docker ps -a (--rm completed after kill), and the RunResult still
// carries partial output and timing.
func TestRealinfra_KillOnCancel(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer setupCancel()

	docker := NewDockerCLI(logger)
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), logger)
	tag, err := b.Build(setupCtx, "realinfra-min", minimalToolset())
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
	// yes provides continuous output so the partial-output capture is provable.
	res, err := runner.Run(ctx, RunSpec{Name: name, Image: tag, Entry: []string{"yes", "partial-line"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Fatalf("cancelled run returned after %v, outside any grace window", elapsed)
	}
	if !strings.Contains(string(res.Output), "partial-line") {
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
	b := NewImageBuilder(docker, NewNixCLI(logger), DefaultNixpkgsPin(), t.TempDir(), logger)
	tag, err := b.Build(ctx, "realinfra-min", minimalToolset())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner := NewContainerRunner(docker, logger)
	res, err := runner.Run(ctx, RunSpec{
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

// Verifies b329adcbbfe4 against real nix eval: stock names prove resolvable,
// a bogus name is a field-path error, and nothing is built either way.
func TestRealinfra_PackageResolutionProof(t *testing.T) {
	requireTools(t)
	logger := realLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	nix := &countingNix{inner: NewNixCLI(logger)}
	b := NewImageBuilder(NewDockerCLI(logger), nix, DefaultNixpkgsPin(), "", logger)

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
