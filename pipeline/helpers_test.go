package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
)

// testBase is the fixed timestamp all canned records derive from, keeping
// reports byte-deterministic.
var testBase = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeClock is a deterministic Clock: Now is fixed, AfterFunc records the
// delay and fires the callback immediately on its own goroutine (simulated
// wake — tests assert the recorded delay, not wall time).
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	delays []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: testBase} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) {
	c.mu.Lock()
	c.delays = append(c.delays, d)
	c.mu.Unlock()
	go f()
}

func (c *fakeClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

// scripted is one node's canned behavior in the fake box runner.
type scripted struct {
	// results are returned per attempt (1-based); the last entry repeats.
	results []failure.Result
	latency time.Duration
	// onAttempt runs before returning (e.g. cancel the run context).
	onAttempt func(attempt int)
}

// fakeBoxes is the scripted BoxRunner: canned result records per node id,
// optional latency, and a recorded start/finish log for order and
// concurrency assertions. No containers anywhere.
type fakeBoxes struct {
	mu      sync.Mutex
	scripts map[string]*scripted
	deflt   func(box BoxAttempt) failure.Result
	starts  []string
	calls   map[string]int
	spans   map[string][2]time.Time // real wall-clock span per node (concurrency checks)
}

func newFakeBoxes() *fakeBoxes {
	return &fakeBoxes{
		scripts: map[string]*scripted{},
		calls:   map[string]int{},
		spans:   map[string][2]time.Time{},
	}
}

func okPayload(fields map[string]any) failure.Result {
	raw, _ := json.Marshal(fields)
	return failure.Result{
		Status:  failure.StatusOK,
		Payload: raw,
		Timing:  failure.Timing{Started: testBase, Finished: testBase.Add(2 * time.Second)},
	}
}

func failedResult(reason, detail string) failure.Result {
	return failure.Result{
		Status: failure.StatusFailed,
		Error:  &failure.ErrorRecord{Reason: reason, Detail: detail},
		Timing: failure.Timing{Started: testBase, Finished: testBase.Add(time.Second)},
	}
}

func (f *fakeBoxes) script(node string, results ...failure.Result) *scripted {
	s := &scripted{results: results}
	f.scripts[node] = s
	return s
}

func (f *fakeBoxes) RunAttempt(ctx context.Context, box BoxAttempt) (BoxResult, error) {
	f.mu.Lock()
	f.calls[box.NodeID]++
	attempt := f.calls[box.NodeID]
	f.starts = append(f.starts, box.NodeID)
	started := time.Now()
	s := f.scripts[box.NodeID]
	f.mu.Unlock()

	var res failure.Result
	var hook func(int)
	latency := time.Duration(0)
	switch {
	case s != nil:
		idx := attempt - 1
		if idx >= len(s.results) {
			idx = len(s.results) - 1
		}
		res = s.results[idx]
		latency = s.latency
		hook = s.onAttempt
	case f.deflt != nil:
		res = f.deflt(box)
	default:
		res = okPayload(map[string]any{"out": box.NodeID})
	}
	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
		}
	}
	if hook != nil {
		hook(attempt)
	}
	f.mu.Lock()
	f.spans[box.NodeID] = [2]time.Time{started, time.Now()}
	f.mu.Unlock()
	return BoxResult{Result: res}, nil
}

func (f *fakeBoxes) startOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

func (f *fakeBoxes) attempts(node string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[node]
}

// fakeTags is a fixed ImageTagger.
type fakeTags struct{}

func (fakeTags) Tag(t *config.ResolvedTemplate) (string, error) {
	return "img/" + t.Name + ":test", nil
}

// fakeSource is a scripted infra.CommandRunner for generate data sources.
type fakeSource struct {
	mu     sync.Mutex
	stdout []byte
	err    error
	calls  []infra.CmdSpec
}

func (f *fakeSource) Run(ctx context.Context, spec infra.CmdSpec) (infra.CmdResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
	if f.err != nil {
		return infra.CmdResult{ExitCode: 1}, f.err
	}
	return infra.CmdResult{Stdout: f.stdout}, nil
}

// itemsJSON renders a data-source document (an empty call renders an empty
// list, not null).
func itemsJSON(items ...map[string]any) []byte {
	if items == nil {
		items = []map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"items": items})
	return raw
}

// testTemplate builds a minimal resolved template with the given output
// fields (all typed string unless the name suggests otherwise).
func testTemplate(name string, outputs ...string) *config.ResolvedTemplate {
	out := map[string]config.ParamDef{}
	for _, f := range outputs {
		out[f] = config.ParamDef{Type: "string", Required: true}
	}
	return &config.ResolvedTemplate{
		Name:   name,
		Skill:  name,
		Inputs: map[string]config.ParamDef{},
		Output: out,
	}
}

// agentNode builds an agent IR node.
func agentNode(id string, outputs ...string) config.Node {
	return config.Node{
		ID:       id,
		Kind:     config.KindAgent,
		Template: testTemplate("tpl-"+strings.ReplaceAll(id, "/", "-"), outputs...),
		Bindings: map[string]config.BindingDesc{},
	}
}

// orderEdge is a pure ordering edge.
func orderEdge(from, to string) config.Edge { return config.Edge{From: from, To: to} }

// testIR assembles a small hand-written IR.
func testIR(workflow string, nodes []config.Node, edges []config.Edge) *config.IR {
	return &config.IR{IRVersion: config.IRVersion, Workflow: workflow, Nodes: nodes, Edges: edges}
}

// harness bundles one executor run's collaborators.
type harness struct {
	exec   *Executor
	boxes  *fakeBoxes
	store  *failure.Store
	clock  *fakeClock
	text   *strings.Builder
	json   *strings.Builder
	params config.Params
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	boxes := newFakeBoxes()
	clock := newFakeClock()
	store := failure.NewStore(t.TempDir(), nil)
	text := &strings.Builder{}
	jsonOut := &strings.Builder{}
	return &harness{
		exec: &Executor{
			Store:   store,
			Boxes:   boxes,
			Images:  fakeTags{},
			Meta:    RunMeta{ConfigPath: "orchestrator.yaml", ConfigHash: "cfg-hash", Supplied: map[string]string{}},
			Out:     text,
			JSONOut: jsonOut,
			Clock:   clock,
		},
		boxes: boxes,
		store: store,
		clock: clock,
		text:  text,
		json:  jsonOut,
	}
}

// run executes the IR through the executor with a fixed run id.
func (h *harness) run(t *testing.T, ir *config.IR, opts config.RunOptions) error {
	t.Helper()
	return h.runCtx(context.Background(), t, ir, opts)
}

func (h *harness) runCtx(ctx context.Context, t *testing.T, ir *config.IR, opts config.RunOptions) error {
	t.Helper()
	if opts.RunID == "" && opts.Mode == "" {
		opts.RunID = "run-test"
	}
	params := h.params
	if params == nil {
		params = config.Params{}
	}
	return h.exec.Execute(ctx, ir, params, opts, nil)
}

// states loads the run's journal and maps step id -> decoded terminal state.
func (h *harness) states(t *testing.T, runID string) map[string]string {
	t.Helper()
	rp, err := h.store.Load(runID)
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	out := map[string]string{}
	for id, rec := range rp.LastByStep {
		out[id] = recordState(rec)
	}
	return out
}

// recordState decodes a journal record's terminal state. Like the reporter,
// it decodes skip reasons only on hashless records — the scheduler's skip
// signature — never on executed failures.
func recordState(rec failure.ResultRecord) string {
	switch {
	case rec.Result.Status == failure.StatusOK:
		return StateOK
	case isSkipRecord(rec, reasonSkippedCondition):
		return StateSkippedCondition
	case isSkipRecord(rec, reasonSkippedDependency):
		return StateSkippedDependency
	default:
		return StateFailed
	}
}

// record returns a step's last journal record.
func (h *harness) record(t *testing.T, runID, step string) failure.ResultRecord {
	t.Helper()
	rp, err := h.store.Load(runID)
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	rec, ok := rp.LastByStep[step]
	if !ok {
		t.Fatalf("no journal record for %s", step)
	}
	return rec
}

// wantStates asserts a subset of the terminal-state map.
func wantStates(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for id, state := range want {
		if got[id] != state {
			t.Errorf("step %s: state %q, want %q", id, got[id], state)
		}
	}
}

// loadReferenceIR reads one of the config module's golden IR fixtures.
func loadReferenceIR(t *testing.T, name string) *config.IR {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "config", "testdata", name))
	if err != nil {
		t.Fatalf("read reference IR: %v", err)
	}
	var ir config.IR
	if err := json.Unmarshal(raw, &ir); err != nil {
		t.Fatalf("parse reference IR: %v", err)
	}
	return &ir
}

// verdict builds the reference review payload.
func verdict(v string) failure.Result {
	return okPayload(map[string]any{"verdict": v})
}

// prPayload builds the reference implement payload.
func prPayload() failure.Result {
	return okPayload(map[string]any{"branch": "work/x", "pr": 17})
}

// chain builds n agent nodes id0 -> id1 -> ... under the given prefix.
func chain(prefix string, n int) ([]config.Node, []config.Edge) {
	var nodes []config.Node
	var edges []config.Edge
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s/s%d", prefix, i)
		nodes = append(nodes, agentNode(id, "out"))
		if i > 0 {
			edges = append(edges, orderEdge(fmt.Sprintf("%s/s%d", prefix, i-1), id))
		}
	}
	return nodes, edges
}
