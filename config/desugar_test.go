package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func desugarRef(t *testing.T, workflow string) *IR {
	t.Helper()
	ir, err := Desugar(loadRef(t), workflow)
	if err != nil {
		t.Fatalf("desugar %s: %v", workflow, err)
	}
	return ir
}

func nodeByID(t *testing.T, ir *IR, id string) *Node {
	t.Helper()
	for i := range ir.Nodes {
		if ir.Nodes[i].ID == id {
			return &ir.Nodes[i]
		}
	}
	t.Fatalf("node %s not in IR", id)
	return nil
}

func hasEdge(ir *IR, e Edge) bool {
	for _, got := range ir.Edges {
		if got == e {
			return true
		}
	}
	return false
}

// Verifies 014aaac22619 and 0ebbdd8f836b: desugaring the reference task
// workflow matches the golden file byte-for-byte — three unrolled iterations
// with gate conditions, ordering chain between iterations, a selector for the
// post-loop-referenced body step, and the final step wired to the pre-loop
// output via a data edge and to the selector via its condition deps.
func TestGoldenDesugarTask(t *testing.T) {
	ir := desugarRef(t, "task")
	got, err := EncodeIR(ir)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want, err := os.ReadFile("testdata/reference_task.ir.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("IR differs from golden file testdata/reference_task.ir.json:\n%s", got)
	}

	// The structural facts the golden pins, asserted explicitly so a
	// deliberate regeneration still has to satisfy them.
	review2 := nodeByID(t, ir, "task/review-cycle@2/review")
	if review2.When == nil || review2.When.CEL != `!(steps["task/review-cycle@1/review"].verdict == "approved")` {
		t.Fatalf("iteration 2 must carry the not-until@1 gate, got %+v", review2.When)
	}
	if nodeByID(t, ir, "task/review-cycle@1/review").When != nil {
		t.Fatal("iteration 1 carries no gate")
	}
	sel := nodeByID(t, ir, "task/review")
	if sel.Kind != KindSelector {
		t.Fatalf("post-loop reference target must be a selector, got %q", sel.Kind)
	}
	wantCands := []string{"task/review-cycle@3/review", "task/review-cycle@2/review", "task/review-cycle@1/review"}
	if len(sel.Sel.Candidates) != 3 {
		t.Fatalf("selector candidates: %v", sel.Sel.Candidates)
	}
	for i, c := range wantCands {
		if sel.Sel.Candidates[i] != c {
			t.Fatalf("selector candidates must be newest-first, got %v", sel.Sel.Candidates)
		}
	}
	if sel.Sel.Exhausted == nil || !strings.Contains(sel.Sel.Exhausted.CEL, "task/review-cycle@3/review") {
		t.Fatalf("selector must carry the loop-exhaustion rule against the final iteration, got %+v", sel.Sel.Exhausted)
	}
	if !hasEdge(ir, Edge{From: "task/implement", FromPort: "pr", To: "task/merge", ToPort: "pr"}) {
		t.Fatal("merge must be wired to implement.pr via a data edge")
	}
	merge := nodeByID(t, ir, "task/merge")
	if merge.When == nil || len(merge.When.Deps) != 1 || merge.When.Deps[0] != "task/review" {
		t.Fatalf("merge's condition must depend on the selector, got %+v", merge.When)
	}
	if !hasEdge(ir, Edge{From: "task/review-cycle@1/review", To: "task/review-cycle@2/fix"}) {
		t.Fatal("iterations must be chained by ordering edges")
	}
	for _, n := range ir.Nodes {
		if n.Kind != KindAgent && n.Kind != KindSelector {
			t.Fatalf("the executed IR never contains a Loop op or other kinds here, got %q", n.Kind)
		}
	}
}

// Verifies 014aaac22619 and 0ebbdd8f836b: the epic generate node carries the
// source ref, the target workflow by name, and the item binding template; the
// task sub-IR is NOT inlined (expansion is run time).
func TestGoldenDesugarEpic(t *testing.T) {
	ir := desugarRef(t, "epic")
	got, err := EncodeIR(ir)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want, err := os.ReadFile("testdata/reference_epic.ir.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("IR differs from golden file testdata/reference_epic.ir.json:\n%s", got)
	}

	gen := nodeByID(t, ir, "epic/tasks")
	if gen.Kind != KindGenerate || gen.Sub != nil {
		t.Fatalf("generate keeps its target by reference, never inlined: %+v", gen)
	}
	if gen.Gen.Workflow != "task" || gen.Gen.Source != "members" {
		t.Fatalf("generate payload: %+v", gen.Gen)
	}
	if b := gen.Gen.Bindings["item"]; b.Kind != BindItem || b.Field != "id" {
		t.Fatalf("item binding template: %+v", gen.Gen.Bindings)
	}
	// The target workflow desugars and wiring-checks independently.
	task := desugarRef(t, "task")
	if err := CheckWiring(task, loadRef(t)); err != nil {
		t.Fatalf("generate target must validate independently: %v", err)
	}
}

// Verifies 0ebbdd8f836b: desugaring is pure and deterministic — repeated runs
// over freshly loaded configs (fresh Go map iteration order every time) yield
// byte-identical IR, and a config differing only in YAML key order and
// comments produces identical bytes.
func TestDeterministicIREmission(t *testing.T) {
	base, err := EncodeIR(desugarRef(t, "task"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := EncodeIR(desugarRef(t, "task"))
		if err != nil {
			t.Fatalf("encode run %d: %v", i, err)
		}
		if !bytes.Equal(base, got) {
			t.Fatalf("run %d produced different IR bytes", i)
		}
	}

	reordered, _, err := Load("testdata/reference_reordered.yaml")
	if err != nil {
		t.Fatalf("load reordered: %v", err)
	}
	if err := Validate(reordered, nil); err != nil {
		t.Fatalf("validate reordered: %v", err)
	}
	for _, wf := range []string{"task", "epic"} {
		a, err := Desugar(loadRef(t), wf)
		if err != nil {
			t.Fatalf("desugar %s: %v", wf, err)
		}
		b, err := Desugar(reordered, wf)
		if err != nil {
			t.Fatalf("desugar reordered %s: %v", wf, err)
		}
		ab, _ := EncodeIR(a)
		bb, _ := EncodeIR(b)
		if !bytes.Equal(ab, bb) {
			t.Fatalf("YAML key order / comments changed the %s IR bytes", wf)
		}
	}
}

// Verifies 014aaac22619: a workflow that transitively includes itself is a
// desugar-time error naming the cycle path, with no partial IR emitted.
func TestWorkflowReferenceCycle(t *testing.T) {
	cfg := minimalConfig()
	cfg.Workflows["a"] = WorkflowDef{Steps: []StepDef{{ID: "s", Use: "b"}}}
	cfg.Workflows["b"] = WorkflowDef{Steps: []StepDef{{ID: "s", Use: "a"}}}
	ir, err := Desugar(cfg, "a")
	if ir != nil {
		t.Fatal("no partial IR on a workflow-reference cycle")
	}
	wantErrContaining(t, err, "workflow reference cycle: a -> b -> a")
}

// Verifies 014aaac22619: a loop whose unrolled node count exceeds the sanity
// ceiling is rejected with an error naming the offending loop.
func TestUnrollBound(t *testing.T) {
	cfg := minimalConfig()
	cfg.Workflows["flow"] = WorkflowDef{
		Params: map[string]ParamDef{"subject": {Type: "string", Required: true}},
		Steps: []StepDef{{ID: "wide", Loop: &LoopDef{Max: 10000, Until: `steps.one.result == "done"`, Steps: []StepDef{
			{ID: "one", Use: "box", With: map[string]any{"input": "${params.subject}"}},
			{ID: "two", Use: "box", With: map[string]any{"input": "${params.subject}"}},
		}}}},
	}
	_, err := Desugar(cfg, "flow")
	wantErrContaining(t, err, `unrolling loop "wide" would exceed the 10000-node sanity ceiling`)
}

// Verifies 014aaac22619: loop with max: 1 emits the body once with no gate
// conditions; the selector is still emitted for post-loop references.
func TestLoopMaxOne(t *testing.T) {
	cfg := loadRef(t)
	cfg.Workflows["task"].Steps[1].Loop.Max = 1
	ir, err := Desugar(cfg, "task")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	review := nodeByID(t, ir, "task/review-cycle@1/review")
	if review.When != nil {
		t.Fatalf("single iteration carries no gate, got %+v", review.When)
	}
	for _, n := range ir.Nodes {
		if strings.Contains(n.ID, "@2") {
			t.Fatalf("max 1 must emit exactly one iteration, found %s", n.ID)
		}
	}
	sel := nodeByID(t, ir, "task/review")
	if sel.Kind != KindSelector || len(sel.Sel.Candidates) != 1 {
		t.Fatalf("selector still emitted for post-loop refs, got %+v", sel.Sel)
	}
	if err := CheckWiring(ir, cfg); err != nil {
		t.Fatalf("wiring: %v", err)
	}
}

// Verifies 014aaac22619: a when: on the loop-step itself applies to every
// iteration's nodes, conjoined with the gates.
func TestLoopStepWhenAppliesToAllIterations(t *testing.T) {
	cfg := loadRef(t)
	cfg.Workflows["task"].Steps[1].When = `steps.implement.branch != ""`
	ir, err := Desugar(cfg, "task")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	loopWhen := `steps["task/implement"].branch != ""`
	for _, id := range []string{"task/review-cycle@1/review", "task/review-cycle@2/review", "task/review-cycle@3/fix"} {
		n := nodeByID(t, ir, id)
		if n.When == nil || !strings.Contains(n.When.CEL, loopWhen) {
			t.Fatalf("%s must carry the loop-step when conjoined, got %+v", id, n.When)
		}
	}
	two := nodeByID(t, ir, "task/review-cycle@2/review")
	if !strings.HasPrefix(two.When.CEL, `!(steps["task/review-cycle@1/review"]`) {
		t.Fatalf("gate must still lead iteration 2's condition, got %q", two.When.CEL)
	}
	if err := CheckWiring(ir, cfg); err != nil {
		t.Fatalf("wiring: %v", err)
	}
}

// Verifies 014aaac22619: named sub-workflow reuse via use: inlines the target
// workflow's desugared graph as a sub-workflow node with param bindings.
func TestSubWorkflowInlining(t *testing.T) {
	cfg := minimalConfig()
	cfg.Workflows["inner"] = WorkflowDef{
		Params: map[string]ParamDef{"input": {Type: "string", Required: true}},
		Steps:  []StepDef{{ID: "act", Use: "box", With: map[string]any{"input": "${params.input}"}}},
	}
	cfg.Workflows["flow"] = WorkflowDef{
		Params: map[string]ParamDef{"subject": {Type: "string", Required: true}},
		Steps:  []StepDef{{ID: "nested", Use: "inner", With: map[string]any{"input": "${params.subject}"}}},
	}
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("validate: %v", err)
	}
	ir, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	n := nodeByID(t, ir, "flow/nested")
	if n.Kind != KindSubWorkflow || n.Sub == nil {
		t.Fatalf("use of a workflow becomes a sub-workflow node, got %+v", n)
	}
	if n.Sub.Workflow != "inner" || len(n.Sub.Nodes) != 1 || n.Sub.Nodes[0].ID != "flow/nested/act" {
		t.Fatalf("sub graph must be inlined under the step's scope, got %+v", n.Sub)
	}
	if b := n.Bindings["input"]; b.Kind != BindParam || b.Name != "subject" {
		t.Fatalf("with: entries become the sub-IR's param bindings, got %+v", n.Bindings)
	}
	if err := CheckWiring(ir, cfg); err != nil {
		t.Fatalf("wiring: %v", err)
	}
}

// Verifies 83bbad3814ba (first-pass behavior only): the node-kind
// discriminator reserves the extension point — the first pass ships exactly
// agent, sub-workflow, generate, and the synthetic selector; a hand-authored
// non-agent kind is rejected by the wiring checker rather than silently
// scheduled.
func TestNodeKindDiscriminatorReserved(t *testing.T) {
	cfg := loadRef(t)
	ir := desugarRef(t, "task")
	seen := map[string]bool{}
	for _, n := range ir.Nodes {
		seen[n.Kind] = true
	}
	for kind := range seen {
		switch kind {
		case KindAgent, KindSubWorkflow, KindGenerate, KindSelector:
		default:
			t.Fatalf("unexpected node kind %q shipped", kind)
		}
	}
	ir.Nodes = append(ir.Nodes, Node{ID: "task/zzz-hold", Kind: "human-approval", Bindings: map[string]BindingDesc{}})
	err := CheckWiring(ir, cfg)
	wantErrContaining(t, err, `task/zzz-hold: unsupported node kind "human-approval"`)
}
