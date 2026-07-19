package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// epicParams supplies the reference epic workflow's declared params.
func epicParams() config.Params {
	return config.Params{
		"group": {Type: "string", Value: "G-7"},
		"repo":  {Type: "string", Value: "gw/repo"},
	}
}

// epicHarness wires the reference epic IR with the task IR as the generate
// target and a scripted three-item source (I-3 depends on I-1 and I-2; I-2
// carries an out-of-set dep that must be ignored).
func epicHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.params = epicParams()
	h.exec.Workflows = map[string]*config.IR{"task": loadReferenceIR(t, "reference_task.ir.json")}
	src := &fakeSource{stdout: itemsJSON(
		map[string]any{"id": "I-1"},
		map[string]any{"id": "I-2", "deps": []string{"X-9"}},
		map[string]any{"id": "I-3", "deps": []string{"I-1", "I-2"}},
	)}
	h.exec.Source = src
	h.boxes.deflt = func(box BoxAttempt) failure.Result {
		switch {
		case strings.HasSuffix(box.NodeID, "/review"):
			return verdict("approved")
		case strings.HasSuffix(box.NodeID, "/implement"):
			return prPayload()
		default:
			return okPayload(map[string]any{"status": "done"})
		}
	}
	return h
}

// Verifies 7c30f5aac83f and ae796d2a1503: the reference epic's generate node
// fans the task workflow out over the data-source items with
// dependency-respecting order — [I-1] and [I-2] run, [I-3]'s sources become
// ready only after both instances' sinks settle, each instance settles
// through its own merge, and the generate payload records the item count.
func TestGenerate_EpicFanOutRespectsItemDeps(t *testing.T) {
	h := epicHarness(t)
	ir := loadReferenceIR(t, "reference_epic.ir.json")

	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"epic/tasks":                         StateOK,
		"epic/tasks[I-1]/implement":          StateOK,
		"epic/tasks[I-1]/merge":              StateOK,
		"epic/tasks[I-2]/merge":              StateOK,
		"epic/tasks[I-3]/implement":          StateOK,
		"epic/tasks[I-3]/merge":              StateOK,
		"epic/tasks[I-3]/review":             StateOK,
		"epic/tasks[I-1]/review-cycle@2/fix": StateSkippedCondition,
	})

	// The generate node's payload records {count: 3, ids: [...]}.
	gen := h.record(t, "run-test", "epic/tasks")
	var payload struct {
		Count int      `json:"count"`
		IDs   []string `json:"ids"`
	}
	if err := json.Unmarshal(gen.Result.Payload, &payload); err != nil {
		t.Fatalf("decode generate payload: %v", err)
	}
	if payload.Count != 3 || len(payload.IDs) != 3 {
		t.Errorf("generate payload %+v, want count 3 with 3 ids", payload)
	}

	// I-3's first box starts only after I-1's and I-2's sinks settled: its
	// implement start index is after every I-1/I-2 start.
	order := h.boxes.startOrder()
	firstI3 := -1
	lastOthers := -1
	for i, id := range order {
		switch {
		case strings.HasPrefix(id, "epic/tasks[I-3]") && firstI3 < 0:
			firstI3 = i
		case strings.HasPrefix(id, "epic/tasks[I-1]") || strings.HasPrefix(id, "epic/tasks[I-2]"):
			lastOthers = i
		}
	}
	if firstI3 < 0 {
		t.Fatalf("no I-3 box ran; order %v", order)
	}
	if firstI3 < lastOthers {
		t.Errorf("I-3 started at %d before I-1/I-2 finished dispatching (last at %d): %v", firstI3, lastOthers, order)
	}
}

// Verifies 175c2f02a73d and a0f44481f57b: one item's failed instance cascades
// through the derived inter-instance edges — [I-1] and its dependent [I-3]
// settle skipped-dependency with [I-1]'s implement as the named root cause —
// while the independent [I-2] completes ok and the rollup reads the partial
// fan-out.
func TestGenerate_FanOutCascade(t *testing.T) {
	h := epicHarness(t)
	h.boxes.script("epic/tasks[I-1]/implement", failedResult("agent", "box died"))
	ir := loadReferenceIR(t, "reference_epic.ir.json")

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want a failed-run error, got %v", err)
	}
	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"epic/tasks":                StateOK,
		"epic/tasks[I-1]/implement": StateFailed,
		"epic/tasks[I-1]/merge":     StateSkippedDependency,
		"epic/tasks[I-1]/review":    StateSkippedDependency,
		"epic/tasks[I-2]/merge":     StateOK,
		"epic/tasks[I-3]/implement": StateSkippedDependency,
		"epic/tasks[I-3]/merge":     StateSkippedDependency,
	})
	root := "epic/tasks[I-1]/implement"
	for _, id := range []string{"epic/tasks[I-1]/merge", "epic/tasks[I-3]/implement", "epic/tasks[I-3]/merge"} {
		rec := h.record(t, "run-test", id)
		if rec.Result.Error.Detail != root {
			t.Errorf("%s names ancestor %q, want %q", id, rec.Result.Error.Detail, root)
		}
	}
	if !strings.Contains(h.text.String(), "3 items: 1 ok, 1 failed, 1 skipped") {
		t.Errorf("rollup summary missing from report:\n%s", h.text.String())
	}
}

// genTestIR is a tiny generate IR: gen -> after (ordering dependent).
func genTestIR(bindings map[string]config.BindingDesc) *config.IR {
	if bindings == nil {
		bindings = map[string]config.BindingDesc{}
	}
	gen := config.Node{
		ID:   "w/gen",
		Kind: config.KindGenerate,
		Gen: &config.GenSpec{
			Source:   "src",
			Command:  "./list-items",
			Args:     []string{"${params.filter}"},
			Workflow: "unit",
			Bindings: bindings,
		},
		Bindings: map[string]config.BindingDesc{},
	}
	return testIR("w",
		[]config.Node{gen, agentNode("w/after", "out")},
		[]config.Edge{orderEdge("w/gen", "w/after")},
	)
}

// unitWorkflow is the one-node target workflow for generate unit tests.
func unitWorkflow() *config.IR {
	n := agentNode("unit/step", "out")
	n.Bindings = map[string]config.BindingDesc{"p": {Kind: config.BindParam, Name: "p"}}
	return testIR("unit", []config.Node{n}, nil)
}

func genUnitHarness(t *testing.T, stdout []byte, bindings map[string]config.BindingDesc) (*harness, *config.IR) {
	t.Helper()
	h := newHarness(t)
	h.params = config.Params{"filter": {Type: "string", Value: "all"}}
	h.exec.Workflows = map[string]*config.IR{"unit": unitWorkflow()}
	h.exec.Source = &fakeSource{stdout: stdout}
	return h, genTestIR(bindings)
}

// Verifies 175c2f02a73d: an empty item set is a no-op — the generate node
// settles ok with zero instances, dependents proceed, the report shows the
// zero-item fan-out summary, and the run exits success.
func TestGenerate_EmptySetIsNoOp(t *testing.T) {
	h, ir := genUnitHarness(t, itemsJSON(), nil)
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/gen":   StateOK,
		"w/after": StateOK,
	})
	gen := h.record(t, "run-test", "w/gen")
	if !strings.Contains(string(gen.Result.Payload), `"count":0`) {
		t.Errorf("generate payload %s, want count 0", gen.Result.Payload)
	}
	if !strings.Contains(h.text.String(), "0 items") {
		t.Errorf("report lacks the zero-item summary:\n%s", h.text.String())
	}
}

// Verifies 175c2f02a73d and 7c30f5aac83f: every data-source contract
// violation — malformed output, duplicate ids, a missing bound item field, an
// in-set dep cycle, a failing command — fails the generate node with a
// structured error naming the violation, no instance node ever exists, and
// standard propagation handles the dependents.
func TestGenerate_ContractViolations(t *testing.T) {
	itemBind := map[string]config.BindingDesc{"p": {Kind: config.BindItem, Field: "size"}}
	tests := []struct {
		name    string
		stdout  []byte
		cmdErr  error
		binds   map[string]config.BindingDesc
		wantMsg string
	}{
		{"malformed JSON", []byte("not json"), nil, nil, "malformed data-source output"},
		{"missing items key", []byte(`{"other": []}`), nil, nil, "malformed data-source output"},
		{"empty id", itemsJSON(map[string]any{"id": ""}), nil, nil, "empty id"},
		{"duplicate ids", itemsJSON(map[string]any{"id": "I-1"}, map[string]any{"id": "I-1"}), nil, nil, "duplicate id"},
		{"missing item field", itemsJSON(map[string]any{"id": "I-1"}), nil, itemBind, `lacks field "size"`},
		{"in-set dep cycle", itemsJSON(
			map[string]any{"id": "I-1", "deps": []string{"I-2"}},
			map[string]any{"id": "I-2", "deps": []string{"I-1"}},
		), nil, nil, "dependency cycle"},
		{"command failure", nil, errors.New("exit 3"), nil, "data-source command failed"},
		{"bracket in item id", itemsJSON(map[string]any{"id": "a]/b"}), nil, nil, "must match"},
		{"quote in item id", itemsJSON(map[string]any{"id": `a"b`}), nil, nil, "must match"},
		{"newline in item id", itemsJSON(map[string]any{"id": "a\nb"}), nil, nil, "must match"},
		{"expansion over the node ceiling", oversizedItemsJSON(), nil, nil, "over the 10000-node ceiling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, ir := genUnitHarness(t, tt.stdout, tt.binds)
			h.exec.Source = &fakeSource{stdout: tt.stdout, err: tt.cmdErr}
			err := h.run(t, ir, config.RunOptions{})
			if err == nil {
				t.Fatalf("want a failed-run error")
			}
			states := h.states(t, "run-test")
			wantStates(t, states, map[string]string{
				"w/gen":   StateFailed,
				"w/after": StateSkippedDependency,
			})
			rec := h.record(t, "run-test", "w/gen")
			if rec.Result.Error.Reason != failure.ReasonSourceContract {
				t.Errorf("reason %q, want %q", rec.Result.Error.Reason, failure.ReasonSourceContract)
			}
			// The failed record is clock-stamped, not zero-timed.
			if !rec.Result.Timing.Started.Equal(testBase) {
				t.Errorf("failed generate record timing %v, want the scheduler clock's %v", rec.Result.Timing.Started, testBase)
			}
			if !strings.Contains(rec.Result.Error.Detail, tt.wantMsg) {
				t.Errorf("detail %q does not name the violation %q", rec.Result.Error.Detail, tt.wantMsg)
			}
			for id := range states {
				if strings.Contains(id, "[") {
					t.Errorf("instance node %s exists after a contract violation", id)
				}
			}
			if h.boxes.attempts("w/gen") != 0 {
				t.Errorf("generate node launched a box")
			}
		})
	}
}

// Verifies 7c30f5aac83f: deps naming ids outside the item set are ignored —
// no edge, no error — and ${item.*}/${params.*} bindings bake into the
// instance's params.
func TestGenerate_ItemBindingAndOutOfSetDeps(t *testing.T) {
	binds := map[string]config.BindingDesc{
		"p": {Kind: config.BindItem, Field: "id"},
	}
	h, ir := genUnitHarness(t, itemsJSON(
		map[string]any{"id": "I-1", "deps": []string{"ELSEWHERE"}},
	), binds)
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/gen":           StateOK,
		"w/gen[I-1]/step": StateOK,
		"w/after":         StateOK,
	})
}

// Verifies 7c30f5aac83f: instantiation renames node ids, edges, condition
// deps, selector candidates, and the CEL steps["..."] keys onto the instance
// prefix without touching the source IR.
func TestGenerate_CloneRenamesEverything(t *testing.T) {
	target := loadReferenceIR(t, "reference_task.ir.json")
	clone, err := cloneRenamedIR(target, "epic/tasks[I-9]")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	byID := map[string]*config.Node{}
	for i := range clone.Nodes {
		byID[clone.Nodes[i].ID] = &clone.Nodes[i]
	}
	merge := byID["epic/tasks[I-9]/merge"]
	if merge == nil {
		t.Fatalf("renamed merge node missing; ids: %v", sortedNodeIDs(clone))
	}
	if want := `steps["epic/tasks[I-9]/review"].verdict == "approved"`; merge.When.CEL != want {
		t.Errorf("merge condition CEL %q, want %q", merge.When.CEL, want)
	}
	if merge.When.Deps[0] != "epic/tasks[I-9]/review" {
		t.Errorf("merge condition dep %q not renamed", merge.When.Deps[0])
	}
	sel := byID["epic/tasks[I-9]/review"]
	if sel == nil || sel.Sel.Candidates[0] != "epic/tasks[I-9]/review-cycle@3/review" {
		t.Errorf("selector candidates not renamed: %+v", sel)
	}
	for _, e := range clone.Edges {
		if !strings.HasPrefix(e.From, "epic/tasks[I-9]/") || !strings.HasPrefix(e.To, "epic/tasks[I-9]/") {
			t.Errorf("edge %v not renamed", e)
		}
	}
	// The source IR is untouched.
	for i := range target.Nodes {
		if strings.Contains(target.Nodes[i].ID, "I-9") {
			t.Errorf("source IR mutated: %s", target.Nodes[i].ID)
		}
	}
}

// Verifies 7c30f5aac83f: the data-source argv resolves ${params.*} from the
// enclosing scope and passes literals through.
func TestGenerate_ResolveArgs(t *testing.T) {
	env := &scopeEnv{params: map[string]any{"filter": "open", "n": 3}}
	got, err := resolveArgs([]string{"--filter", "${params.filter}", "${params.n}"}, env)
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	want := []string{"--filter", "open", "3"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := resolveArgs([]string{"${params.missing}"}, env); err == nil {
		t.Errorf("unbound param arg resolved without error")
	}
}

func sortedNodeIDs(ir *config.IR) []string {
	out := make([]string, 0, len(ir.Nodes))
	for i := range ir.Nodes {
		out = append(out, ir.Nodes[i].ID)
	}
	return out
}

// Verifies 7c30f5aac83f and 175c2f02a73d: a self-dep is ignored under the
// same leniency as out-of-set deps — no edge, no cycle error — matching
// expand()'s edge derivation.
func TestGenerate_SelfDepIgnored(t *testing.T) {
	h, ir := genUnitHarness(t, itemsJSON(
		map[string]any{"id": "I-1", "deps": []string{"I-1"}},
	), nil)
	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run (self-dep must be ignored, not a cycle): %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/gen":           StateOK,
		"w/gen[I-1]/step": StateOK,
		"w/after":         StateOK,
	})
}

// oversizedItemsJSON renders an item document whose expansion would exceed
// maxExpandedNodes (the target workflow has >= 1 node per instance).
func oversizedItemsJSON() []byte {
	items := make([]map[string]any, maxExpandedNodes+1)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprintf("i%d", i)}
	}
	return itemsJSON(items...)
}
