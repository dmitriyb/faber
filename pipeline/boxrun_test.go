package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/security"
)

// fakeContainers is a scripted ContainerRunner: it captures the RunSpec and
// writes a canned record into the mounted result directory — no docker.
type fakeContainers struct {
	mu     sync.Mutex
	specs  []infra.RunSpec
	record *contract.Result // nil writes nothing (box vanished)
	usage  map[string]int64
}

func (f *fakeContainers) Run(ctx context.Context, spec infra.RunSpec) (infra.RunResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	resultDir := ""
	for _, m := range spec.Mounts {
		if m.Container == contract.ContainerResultDir {
			resultDir = m.Host
		}
	}
	if f.record != nil && resultDir != "" {
		if err := contract.WriteResultFile(resultDir, *f.record); err != nil {
			return infra.RunResult{}, err
		}
	}
	if f.usage != nil && resultDir != "" {
		raw, _ := json.Marshal(f.usage)
		os.WriteFile(filepath.Join(resultDir, contract.UsageFile), raw, 0o644)
	}
	return infra.RunResult{ExitCode: 0, Started: testBase, Duration: 2e9}, nil
}

// fakeBindings records Prepare calls and returns a fixed argv fragment.
type fakeBindings struct {
	mu        sync.Mutex
	steps     []security.StepSpec
	teardowns int
}

func (f *fakeBindings) Prepare(ctx context.Context, step security.StepSpec) (security.Assembled, error) {
	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	return security.Assembled{
		Args: []string{"--network=none", "-e", "HANDLE=x"},
		Teardown: func(context.Context) error {
			f.mu.Lock()
			f.teardowns++
			f.mu.Unlock()
			return nil
		},
	}, nil
}

func boxAttempt(t *testing.T) BoxAttempt {
	t.Helper()
	tpl := testTemplate("worker", "out")
	tpl.Env = map[string]string{contract.EnvAgentCLI: "agent-cli"}
	tpl.Inputs = map[string]config.ParamDef{"out": {Type: "string", Required: true}}
	return BoxAttempt{
		RunID:    "run-b",
		RunDir:   t.TempDir(),
		NodeID:   "w/x",
		Attempt:  1,
		Template: tpl,
		Image:    "img/worker:test",
		Inputs:   map[string]any{"out": "v"},
	}
}

// Verifies ae796d2a1503: the production box composition — BuildRunSpec, the
// security fragment spliced verbatim, container run, teardown, host-side
// result extraction — adapts an ok record to the failure module's shape and
// reads the usage sidecar.
func TestBoxRun_ComposesAndAdaptsOK(t *testing.T) {
	containers := &fakeContainers{
		record: &contract.Result{Status: contract.StatusOK, Payload: map[string]any{"out": "done"}, Attempt: 1},
		usage:  map[string]int64{"tokens": 42},
	}
	bindings := &fakeBindings{}
	boxes := &AgentBoxes{Containers: containers, Bindings: bindings, EntryBinary: "/usr/local/bin/faber-box"}

	got, err := boxes.RunAttempt(context.Background(), boxAttempt(t))
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got.Result.Status != failure.StatusOK {
		t.Fatalf("status %s, want ok", got.Result.Status)
	}
	if !strings.Contains(string(got.Result.Payload), `"out":"done"`) {
		t.Errorf("payload %s does not carry the record's output", got.Result.Payload)
	}
	if got.Usage["tokens"] != 42 {
		t.Errorf("usage sidecar %v, want tokens=42", got.Usage)
	}
	spec := containers.specs[0]
	if len(spec.Bindings) != 3 || spec.Bindings[0] != "--network=none" {
		t.Errorf("security fragment not spliced verbatim: %v", spec.Bindings)
	}
	if spec.Entry[0] != contract.ContainerEntry {
		t.Errorf("entry %v, want the faber-box binary", spec.Entry)
	}
	if bindings.teardowns != 1 {
		t.Errorf("teardown ran %d times, want exactly 1", bindings.teardowns)
	}
	if bindings.steps[0].ScratchDir == "" {
		t.Errorf("bindings prepared without a scratch dir")
	}
}

// Verifies a0f44481f57b: the container exit code is never authoritative — a
// run that leaves no readable record synthesizes a box-vanished failure, and
// a failed record's handoff pointer is re-rooted under the run directory.
func TestBoxRun_VanishedAndHandoff(t *testing.T) {
	// No record written: box vanished.
	containers := &fakeContainers{}
	boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}
	got, err := boxes.RunAttempt(context.Background(), boxAttempt(t))
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got.Result.Status != failure.StatusFailed || got.Result.Error.Reason != contract.ReasonBoxVanished {
		t.Fatalf("want a synthesized box-vanished failure, got %+v", got.Result)
	}

	// Failed record with a handoff: the pointer resolves under the run dir.
	attempt := boxAttempt(t)
	containers2 := &fakeContainers{record: &contract.Result{
		Status:  contract.StatusFailed,
		Error:   &contract.ResultError{Reason: "agent-failed", Detail: "died", Handoff: contract.HandoffFile},
		Attempt: 1,
	}}
	boxes2 := &AgentBoxes{Containers: containers2, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}
	got2, err := boxes2.RunAttempt(context.Background(), attempt)
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got2.Result.Error.Handoff == "" {
		t.Fatalf("handoff pointer lost in adaptation")
	}
	full := filepath.Join(attempt.RunDir, got2.Result.Error.Handoff)
	if !strings.HasSuffix(full, contract.HandoffFile) {
		t.Errorf("handoff %q does not resolve to the handoff file", full)
	}
	if strings.HasPrefix(got2.Result.Error.Handoff, "/") {
		t.Errorf("handoff %q is absolute, want run-dir relative", got2.Result.Error.Handoff)
	}
}

// fakeInteractive captures the interactive run spec.
type fakeInteractive struct {
	spec *infra.RunSpec
}

func (f *fakeInteractive) RunInteractive(ctx context.Context, spec infra.RunSpec) (err error) {
	f.spec = &spec
	return nil
}

// Verifies a0f44481f57b: interactive re-entry reconstructs the failed step's
// box from the journal and its handoff record — same image and inputs, the
// entry program replaced by a shell, the handoff state mounted read-only —
// and refuses steps that did not fail.
func TestBoxRun_InteractiveReentry(t *testing.T) {
	store := failure.NewStore(t.TempDir(), nil)
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	hash, _ := config.HashIR(ir)
	seed, err := store.Fresh(failure.Header{RunID: "run-i", Workflow: "w", IRHash: hash, Started: testBase})
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}

	// Journal a failed record whose handoff resolves under the run dir.
	handoffRel := filepath.Join("boxes", "x", "attempt-1", "result")
	handoffDir := filepath.Join(store.RunDir("run-i"), handoffRel)
	if err := os.MkdirAll(handoffDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := contract.WriteHandoffFile(handoffDir, contract.Handoff{
		Phase:  "agent",
		Reason: "agent-failed",
		Inputs: map[string]string{"out": "v"},
	}); err != nil {
		t.Fatalf("write handoff: %v", err)
	}
	rec := failure.ResultRecord{StepID: "w/x", InputHash: "h", Result: failure.Result{
		Status:  failure.StatusFailed,
		Error:   &failure.ErrorRecord{Reason: "agent-failed", Detail: "died", Handoff: filepath.Join(handoffRel, contract.HandoffFile)},
		Attempt: 1,
	}}
	if err := seed.Journal.AppendResult(rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	okRec := failure.ResultRecord{StepID: "w/y", InputHash: "h2", Result: failure.Result{
		Status: failure.StatusOK, Payload: json.RawMessage(`{"out":"v"}`), Attempt: 1,
	}}
	if err := seed.Journal.AppendResult(okRec); err != nil {
		t.Fatalf("append: %v", err)
	}
	seed.Journal.Close()

	// The IR node needs the agent CLI env and the input slot for BuildRunSpec.
	ir.Nodes[0].Template.Env = map[string]string{contract.EnvAgentCLI: "agent-cli"}
	ir.Nodes[0].Template.Inputs = map[string]config.ParamDef{"out": {Type: "string", Required: true}}
	interactive := &fakeInteractive{}
	re := &Reentry{
		IR:          ir,
		Images:      fakeTags{},
		Bindings:    &fakeBindings{},
		Interactive: interactive,
		EntryBinary: "/usr/local/bin/faber-box",
	}

	if err := store.Interactive(context.Background(), "run-i", "w/x", re); err != nil {
		t.Fatalf("interactive: %v", err)
	}
	spec := interactive.spec
	if spec == nil {
		t.Fatalf("interactive runner never invoked")
	}
	if spec.Entry[0] != "/bin/sh" {
		t.Errorf("entry %v, want the interactive shell", spec.Entry)
	}
	if spec.Env[contract.InputEnv("out")] != "v" {
		t.Errorf("inputs not re-exported: %v", spec.Env)
	}
	foundHandoff := false
	for _, m := range spec.Mounts {
		if m.Container == containerHandoffDir {
			foundHandoff = true
			if !m.ReadOnly {
				t.Errorf("handoff mount is writable")
			}
		}
	}
	if !foundHandoff {
		t.Errorf("handoff state not mounted: %v", spec.Mounts)
	}

	// A step that settled ok refuses re-entry (the failure store's guard).
	if err := store.Interactive(context.Background(), "run-i", "w/y", re); err == nil {
		t.Errorf("interactive re-entry accepted an ok step")
	}

	// The same flow through the Executor's interactive mode: nothing is
	// scheduled, the reconstruction seam is invoked.
	interactive.spec = nil
	exec := &Executor{Store: store, Reentry: re}
	err = exec.Execute(context.Background(), ir, config.Params{},
		config.RunOptions{Mode: "interactive", RunID: "run-i", InteractiveStep: "w/x"}, nil)
	if err != nil {
		t.Fatalf("executor interactive mode: %v", err)
	}
	if interactive.spec == nil {
		t.Errorf("executor interactive mode never reached the re-entry seam")
	}
}

// Verifies 595a2a6fcc5b: run-time condition compilation goes through the
// config module's shared CompileCondition gate, so an expression validate
// would reject never reaches a program.
func TestCond_CompileSharedGate(t *testing.T) {
	ce, err := newCondEval()
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if _, err := ce.compile(&config.CondSpec{CEL: `steps["a"].v == "x"`}); err != nil {
		t.Fatalf("valid condition rejected: %v", err)
	}
	if _, err := ce.compile(&config.CondSpec{CEL: `nonsense ===`}); err == nil {
		t.Fatalf("invalid condition compiled")
	}
	if _, err := ce.compile(&config.CondSpec{CEL: `undeclared_var == 1`}); err == nil {
		t.Fatalf("condition over an undeclared variable compiled")
	}
}

// Verifies 990c3d8a7888 and a0f44481f57b (defense in depth): box-authored
// failure reasons that collide with the pipeline's reserved journal
// vocabulary are namespaced at the adaptResult boundary, so even the raw
// reason string in the journal cannot masquerade as a scheduler record.
func TestBoxRun_ReservedReasonsSanitized(t *testing.T) {
	for _, reserved := range []string{reasonSkippedCondition, reasonSkippedDependency, reasonDeferred, reasonCached} {
		containers := &fakeContainers{record: &contract.Result{
			Status:  contract.StatusFailed,
			Error:   &contract.ResultError{Reason: reserved, Detail: "hostile"},
			Attempt: 1,
		}}
		boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}
		got, err := boxes.RunAttempt(context.Background(), boxAttempt(t))
		if err != nil {
			t.Fatalf("run attempt: %v", err)
		}
		if want := "box:" + reserved; got.Result.Error.Reason != want {
			t.Errorf("reason %q, want %q", got.Result.Error.Reason, want)
		}
	}
	// Non-reserved reasons pass through untouched (the rate-limit contract
	// depends on it).
	containers := &fakeContainers{record: &contract.Result{
		Status:  contract.StatusFailed,
		Error:   &contract.ResultError{Reason: "rate-limit", Detail: `{"reset":0}`},
		Attempt: 1,
	}}
	boxes := &AgentBoxes{Containers: containers, Bindings: &fakeBindings{}, EntryBinary: "/usr/local/bin/faber-box"}
	got, err := boxes.RunAttempt(context.Background(), boxAttempt(t))
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if got.Result.Error.Reason != "rate-limit" {
		t.Errorf("non-reserved reason mangled: %q", got.Result.Error.Reason)
	}
}
