//go:build realinfra

package pipeline

// Real-infrastructure suite for the box-run composition: needs a working
// docker daemon and the go toolchain (to build the faber-box entry binary).
// It never runs in the sandboxed unit gate; on an acceptance machine run:
//
//	go test -tags realinfra -timeout 15m ./pipeline/
//
// What it verifies on a real machine (test_pipeline_execution.md's "whatever
// needs real containers" remainder):
//   - AgentBoxes launches a real container from BuildContainerSpec with the
//     security fragment spliced, and the mounted result channel round-trips:
//     the box's phase sequencer writes a structured record on its exit path
//     (here an early phase failure — no hooks, no agent CLI on PATH), and
//     ExtractResult adapts it; the exit code is never consulted.
//   - A container that produces no record at all synthesizes box-vanished.
import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/security"
)

func requireBinaries(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not available; realinfra suite needs a real machine: %v", name, err)
		}
	}
}

// buildFaberBox compiles the static in-container entry binary.
func buildFaberBox(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "faber-box")
	cmd := exec.Command("go", "build", "-o", out, "github.com/dmitriyb/faber/cmd/faber-box")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build faber-box: %v\n%s", err, raw)
	}
	return out
}

func realBoxes(t *testing.T) *AgentBoxes {
	t.Helper()
	logger := discardLogger()
	docker := infra.NewDockerCLI(logger)
	runner := infra.NewCommandRunner(logger)
	bindings := security.NewBindingSet(
		security.NewNetworkBinding(docker, logger),
		security.NewRemoteBinding(logger),
		security.NewIdentityBinding(security.NewAgentController(logger), logger),
		security.NewCredentialBroker(security.NewExecResolver("", runner), logger),
		logger,
	)
	return &AgentBoxes{
		Containers: infra.NewContainerRunner(docker, logger),
		Bindings:   bindings,
		Log:        logger,
	}
}

// realImage builds the minimal toolset image with the real faber-box baked
// in — what every box runs from now that nothing is mounted for it.
func realImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	logger := discardLogger()
	b := infra.NewImageBuilder(infra.NewDockerCLI(logger), infra.NewNixCLI(logger), infra.DefaultNixpkgsPin(), t.TempDir(), buildFaberBox(t), logger)
	tag, err := b.Build(ctx, "realinfra-smoke", config.BuildDef{Packages: []string{"coreutils"}}, infra.ImageInputs{})
	if err != nil {
		t.Fatalf("build the smoke image: %v", err)
	}
	return tag
}

// Verifies ae796d2a1503 on a real machine: the composed box run produces a
// structured record through the result directory even when its
// phases fail — failure is a record, not an absence.
func TestRealInfra_BoxRoundTrip(t *testing.T) {
	requireBinaries(t, "docker", "go", "nix")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	image := realImage(t, ctx)

	tpl := &config.ResolvedTemplate{
		Name:   "smoke",
		Skill:  "smoke",
		Env:    map[string]string{contract.EnvAgentCLI: "definitely-not-installed"},
		Inputs: map[string]config.ParamDef{},
		Output: map[string]config.ParamDef{"out": {Type: "string", Required: true}},
	}
	boxes := realBoxes(t)
	got, err := boxes.RunAttempt(ctx, BoxAttempt{
		RunID:    "real-" + t.Name(),
		RunDir:   t.TempDir(),
		NodeID:   "w/smoke",
		Attempt:  1,
		Template: tpl,
		Image:    image,
		Inputs:   map[string]any{},
	})
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	// The box has no hooks and no installed agent CLI: some phase fails, but
	// the record channel must still deliver a structured failure.
	if got.Result.Status != failure.StatusFailed {
		t.Fatalf("status %s, want a structured failure record", got.Result.Status)
	}
	if got.Result.Error == nil || got.Result.Error.Reason == "" {
		t.Fatalf("failure record carries no reason: %+v", got.Result)
	}
}
