package config

import (
	"fmt"
	"strings"
	"testing"
)

// checkRef desugars the (mutated) reference task workflow and wiring-checks it.
func checkRef(t *testing.T, cfg *Config, workflow string) error {
	t.Helper()
	ir, err := Desugar(cfg, workflow)
	if err != nil {
		return err
	}
	return CheckWiring(ir, cfg)
}

// Verifies 72d49cc06ac6: the acceptance defect quartet — each one-defect
// mutation of the reference config fails validation with exactly the expected
// violation class (the fourth, run-entry param checking, is exercised through
// the CLI in cli_test.go).
func TestWiringDefectQuartet(t *testing.T) {
	t.Run("undeclared output field with near-miss suggestion", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[2].With["pr"] = "${steps.implement.prs}"
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, "task/merge.with.pr: references task/implement.prs — output field does not exist (did you mean pr?)")
	})

	t.Run("slot type mismatch", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[1].Loop.Steps[0].With["pr"] = "${params.repo}"
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, "type mismatch: params.repo is string, slot wants int")
		for i := 1; i <= 3; i++ {
			wantErrContaining(t, err, fmt.Sprintf("task/review-cycle@%d/review.with.pr", i))
		}
	})

	t.Run("reference cycle reported as a concrete node path", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[1].Loop.Steps[1].DependsOn = []string{"merge"}
		cfg.Workflows["task"].Steps[2].DependsOn = []string{"review-cycle"}
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, "reference cycle: task/merge -> task/review-cycle@1/fix -> task/review-cycle@2/fix -> task/review-cycle@3/fix -> task/merge")
	})

	t.Run("missing required run param is checked at run entry", func(t *testing.T) {
		cfg := loadRef(t)
		_, err := CheckRunParams(cfg.Workflows["task"], map[string]string{"repo": "sandbox"})
		wantErrContaining(t, err, "params.item: required param missing")
	})
}

// Verifies 72d49cc06ac6: a config carrying several defects reports them all
// together, sorted by (node, path) — no first-error truncation.
func TestWiringAllViolationsInOnePass(t *testing.T) {
	cfg := loadRef(t)
	cfg.Workflows["task"].Steps[2].With["pr"] = "${steps.implement.prs}"       // undeclared output field
	cfg.Workflows["task"].Steps[1].Loop.Steps[0].With["pr"] = "${params.repo}" // type mismatch
	cfg.Workflows["task"].Steps[1].Loop.Steps[1].DependsOn = []string{"merge"} // half of the cycle
	cfg.Workflows["task"].Steps[2].DependsOn = []string{"review-cycle"}        // other half
	cfg.Workflows["task"].Steps[0].With["surplus"] = "x"                       // undeclared slot
	err := checkRef(t, cfg, "task")
	ordered := []string{
		"task/implement.with.surplus",
		"task/merge: reference cycle",
		"task/merge.with.pr: references task/implement.prs",
		"task/review-cycle@1/review.with.pr: type mismatch",
		"task/review-cycle@2/review.with.pr: type mismatch",
		"task/review-cycle@3/review.with.pr: type mismatch",
	}
	text := err.Error()
	last := -1
	for _, want := range ordered {
		at := strings.Index(text, want)
		if at < 0 {
			t.Fatalf("missing violation %q in:\n%s", want, text)
		}
		if at < last {
			t.Fatalf("violations must be sorted by (node, path); %q out of order in:\n%s", want, text)
		}
		last = at
	}
	if lines := strings.Split(text, "\n"); len(lines) < 6 {
		t.Fatalf("expected all defects reported together, got %d lines:\n%s", len(lines), text)
	}
}

// Verifies 72d49cc06ac6: slot discipline — double-binding a slot (edge plus a
// second edge) and binding an undeclared slot each produce their specific
// violations.
func TestWiringSlotDiscipline(t *testing.T) {
	t.Run("undeclared slot", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[2].With["prr"] = "1"
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, `task/merge.with.prr: binds undeclared input slot "prr" (did you mean pr?)`)
	})

	t.Run("double-bound slot in hand-authored IR", func(t *testing.T) {
		cfg := loadRef(t)
		ir, err := Desugar(cfg, "task")
		if err != nil {
			t.Fatalf("desugar: %v", err)
		}
		ir.Edges = append(ir.Edges, Edge{From: "task/implement", FromPort: "pr", To: "task/merge", ToPort: "pr"})
		err = CheckWiring(ir, cfg)
		wantErrContaining(t, err, "task/merge.with.pr: input slot bound 2 times — a slot is bound exactly once")
	})

	t.Run("edge plus binding descriptor on one slot", func(t *testing.T) {
		cfg := loadRef(t)
		ir, err := Desugar(cfg, "task")
		if err != nil {
			t.Fatalf("desugar: %v", err)
		}
		merge := nodeByID(t, ir, "task/merge")
		merge.Bindings["pr"] = BindingDesc{Kind: BindLiteral, Value: 7, Type: "int"}
		err = CheckWiring(ir, cfg)
		wantErrContaining(t, err, "task/merge.with.pr: input slot bound 2 times")
	})

	t.Run("unbound required slot", func(t *testing.T) {
		cfg := loadRef(t)
		delete(cfg.Workflows["task"].Steps[0].With, "item")
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, `task/implement.with.item: required input slot "item" is unbound`)
	})
}

// Verifies 72d49cc06ac6: condition sanity — a condition reading a
// non-predecessor is rejected, an until: referencing a step outside the loop
// body is rejected, and a CEL syntax error surfaces with the compile
// diagnostic and the IR node path.
func TestWiringConditionChecks(t *testing.T) {
	t.Run("condition reads a descendant", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[0].When = "steps.merge.merged"
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, `task/implement.when: condition reads "task/merge", which does not precede this node`)
	})

	t.Run("until references a step outside the loop body", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[1].Loop.Until = "steps.implement.pr > 0"
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, `until references step "implement" outside the loop body`)
	})

	t.Run("CEL syntax error carries the diagnostic and node path", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[2].When = `steps.review.verdict ==`
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, "task/merge.when: condition does not compile:")
	})
}

// Verifies 72d49cc06ac6: the tool-subset rule — a step declaring tool needs
// not provided by its template's package list reports the set difference.
func TestWiringToolSubset(t *testing.T) {
	cfg := loadRef(t)
	cfg.Workflows["task"].Steps[2].Tools = []string{"git", "jq"}
	err := checkRef(t, cfg, "task")
	wantErrContaining(t, err, `task/merge.tools: tools not provided by template "merge" packages: [jq]`)
}

// Verifies 72d49cc06ac6: the generate boundary — the target workflow's params
// are checked against the generate's binding template, and item fields carry
// the data-source contract types (id string, deps structured).
func TestWiringGenerateBoundary(t *testing.T) {
	t.Run("missing required target param", func(t *testing.T) {
		cfg := loadRef(t)
		delete(cfg.Workflows["epic"].Steps[0].Generate.With, "item")
		err := checkRef(t, cfg, "epic")
		wantErrContaining(t, err, `epic/tasks.with.item: required input slot "item" is unbound`)
	})

	t.Run("item.deps into an incompatible slot", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["epic"].Steps[0].Generate.With["item"] = "${item.deps}"
		err := checkRef(t, cfg, "epic")
		wantErrContaining(t, err, "epic/tasks.with.item: type mismatch: item.deps is object, slot wants string")
	})

	t.Run("pass-through item fields are untyped", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["epic"].Steps[0].Generate.With["item"] = "${item.label}"
		if err := checkRef(t, cfg, "epic"); err != nil {
			t.Fatalf("undeclared item fields pass through as any: %v", err)
		}
	})

	t.Run("unknown target param in the binding template", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["epic"].Steps[0].Generate.With["surplus"] = "${item.id}"
		err := checkRef(t, cfg, "epic")
		wantErrContaining(t, err, `epic/tasks.with.surplus: binds undeclared input slot "surplus"`)
	})
}

// Verifies 72d49cc06ac6: enum narrowing — an enum output satisfies a plain
// string slot, but a plain string source does not satisfy an enum slot.
func TestWiringEnumNarrowing(t *testing.T) {
	cfg := loadRef(t)
	// verdict (enum) into a plain string slot: legal.
	tp := cfg.Templates["merge"]
	tp.Inputs = map[string]ParamDef{
		"repo": {Type: "string", Required: true},
		"pr":   {Type: "int", Required: true},
		"note": {Type: "string"},
	}
	cfg.Templates["merge"] = tp
	cfg.Workflows["task"].Steps[2].With["note"] = "${steps.review.verdict}"
	if err := checkRef(t, cfg, "task"); err != nil {
		t.Fatalf("enum source must satisfy a plain string slot: %v", err)
	}

	// A plain string output into an enum slot: rejected.
	cfg = loadRef(t)
	tp = cfg.Templates["merge"]
	tp.Inputs = map[string]ParamDef{
		"repo": {Type: "string", Required: true},
		"pr":   {Type: "int", Required: true},
		"gate": {Type: "string", Enum: []string{"approved"}},
	}
	cfg.Templates["merge"] = tp
	cfg.Workflows["task"].Steps[2].With["gate"] = "${steps.implement.branch}"
	err := checkRef(t, cfg, "task")
	wantErrContaining(t, err, "task/merge.with.gate: type mismatch: task/implement.branch is a plain string, slot wants enum [approved]")
}

// Verifies 72d49cc06ac6: violation output stays sorted and deterministic even
// with 100+ errors.
func TestWiringManyViolationsDeterministic(t *testing.T) {
	cfg := minimalConfig()
	steps := make([]StepDef, 0, 120)
	for i := 0; i < 120; i++ {
		steps = append(steps, StepDef{
			ID:  fmt.Sprintf("s%03d", i),
			Use: "box",
			With: map[string]any{
				"input":   "${params.subject}",
				"surplus": "x", // undeclared slot, one violation per step
			},
		})
	}
	cfg.Workflows["flow"] = WorkflowDef{
		Params: map[string]ParamDef{"subject": {Type: "string", Required: true}},
		Steps:  steps,
	}
	first := checkRef(t, cfg, "flow")
	second := checkRef(t, cfg, "flow")
	if first == nil || first.Error() != second.Error() {
		t.Fatal("violation output must be deterministic across runs")
	}
	lines := strings.Split(first.Error(), "\n")
	if len(lines) < 120 {
		t.Fatalf("want 120 violations, got %d", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Fatalf("violations must be sorted, line %d out of order", i)
		}
	}
}

// Verifies 72d49cc06ac6: condition dependencies participate in cycle detection
// — a mutual when-cycle with no data edges must fail validate rather than
// deadlock the scheduler (fix pass finding 1).
func TestWiringConditionDependencyCycle(t *testing.T) {
	cfg := minimalConfig()
	cfg.Workflows["flow"] = WorkflowDef{
		Params: map[string]ParamDef{"subject": {Type: "string", Required: true}},
		Steps: []StepDef{
			{ID: "a", Use: "box", When: `steps.b.result == "x"`, With: map[string]any{"input": "${params.subject}"}},
			{ID: "b", Use: "box", When: `steps.a.result == "x"`, With: map[string]any{"input": "${params.subject}"}},
		},
	}
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("validate: %v", err)
	}
	err := checkRef(t, cfg, "flow")
	wantErrContaining(t, err, "reference cycle: flow/a -> flow/b -> flow/a")
}

// Verifies 72d49cc06ac6: fields a condition reads are validated against the
// dep's declared output schema — an undeclared field fails validate with a
// field-path error and near-miss suggestion, for when:, gates, and until:
// alike (fix pass finding 2).
func TestWiringConditionFieldRefs(t *testing.T) {
	t.Run("when reads an undeclared output field", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[2].When = `steps.review.verdicts == "approved"`
		err := checkRef(t, cfg, "task")
		wantErrContaining(t, err, "task/merge.when: references task/review.verdicts — output field does not exist (did you mean verdict?)")
	})

	t.Run("until reads an undeclared output field", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[1].Loop.Until = `steps.review.blessing == "granted"`
		err := checkRef(t, cfg, "task")
		// The selector's exhaustion rule and the iteration gates all carry the
		// rewritten until, so the violation surfaces with IR node paths.
		wantErrContaining(t, err, "task/review.until: references task/review-cycle@3/review.blessing — output field does not exist")
		wantErrContaining(t, err, "task/review-cycle@2/review.when: references task/review-cycle@1/review.blessing — output field does not exist")
	})

	t.Run("declared fields pass", func(t *testing.T) {
		cfg := loadRef(t)
		if err := checkRef(t, cfg, "task"); err != nil {
			t.Fatalf("pristine reference must pass: %v", err)
		}
	})
}

// Verifies 72d49cc06ac6 (IN-F1): the condition AST discipline — only the
// canonical steps["<dep>"].<field> and declared params.<name> forms survive
// to run time. Hand-authored index forms, undeclared params, bare roots, and
// item references all fail at validate, never as a mid-run condition error.
func TestWiringConditionASTDiscipline(t *testing.T) {
	cases := []struct {
		name string
		when string
		want string
	}{
		{"hand-authored steps index escapes dep extraction",
			`steps["task/implement"].pr > 0`,
			`which the desugarer did not derive as a dependency`},
		{"dynamic steps index",
			`steps[params.repo].pr > 0`,
			`indexes steps with a non-literal key`},
		{"whole-payload read",
			`steps.implement.pr > 0 && steps["task/implement"] == steps["task/implement"]`,
			`without selecting a field`},
		{"bare steps root",
			`size(steps) > 0`,
			`uses steps outside a steps.<step>.<field> reference`},
		{"undeclared param",
			`params.undeclared == "x"`,
			`params.undeclared, which is not a declared param`},
		{"undeclared param via index",
			`params["undeclared"] == "x"`,
			`params.undeclared, which is not a declared param`},
		{"item is not available in conditions",
			`item.id == "x"`,
			`item, which is not available in conditions`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadRef(t)
			cfg.Workflows["task"].Steps[2].When = tc.when
			err := checkRef(t, cfg, "task")
			wantErrContaining(t, err, tc.want)
		})
	}

	t.Run("canonical forms pass", func(t *testing.T) {
		cfg := loadRef(t)
		cfg.Workflows["task"].Steps[2].When = `steps.implement.pr > 0 && params.repo != ""`
		if err := checkRef(t, cfg, "task"); err != nil {
			t.Fatalf("canonical condition must validate: %v", err)
		}
	})
}

// Verifies 72d49cc06ac6 (IN-F2): duplicate node ids anywhere in the flattened
// graph are a wiring error — the per-level index would silently collapse
// them and the executor would abort at run start.
func TestWiringFlatDuplicateNodeIDs(t *testing.T) {
	ir := &IR{
		IRVersion: IRVersion,
		Workflow:  "flow",
		Nodes: []Node{
			{ID: "a/x", Kind: KindAgent, Bindings: map[string]BindingDesc{}},
			{ID: "sub", Kind: KindSubWorkflow, Bindings: map[string]BindingDesc{}, Sub: &IR{
				IRVersion: IRVersion,
				Workflow:  "inner",
				Nodes:     []Node{{ID: "a/x", Kind: KindAgent, Bindings: map[string]BindingDesc{}}},
			}},
		},
	}
	err := CheckWiring(ir, &Config{Workflows: map[string]WorkflowDef{"flow": {}, "inner": {}}})
	wantErrContaining(t, err, `duplicate node id "a/x"`)
}

// Verifies 72d49cc06ac6 (IN-F1, M1): a condition may reference only params
// guaranteed present at evaluation — an optional param without a default may
// be absent from the activation (root and sub scopes alike), so referencing
// it is a validate error; defaulted params are guaranteed because the
// desugarer bakes sub/instance-scope defaults into the scope bindings.
func TestWiringConditionParamGuarantee(t *testing.T) {
	cfg := loadRef(t)
	wf := cfg.Workflows["task"]
	wf.Params["maybe"] = ParamDef{Type: "string"} // optional, no default
	wf.Params["tuned"] = ParamDef{Type: "string", Default: "x"}
	cfg.Workflows["task"] = wf

	cfg.Workflows["task"].Steps[2].When = `params.maybe == "y"`
	err := checkRef(t, cfg, "task")
	wantErrContaining(t, err, "optional without a default and may be absent at evaluation")

	cfg.Workflows["task"].Steps[2].When = `params.tuned == "y"`
	if err := checkRef(t, cfg, "task"); err != nil {
		t.Fatalf("defaulted param must be referenceable: %v", err)
	}
}

// Verifies 255893ae16eb (M1): the desugarer bakes declared defaults into
// sub-workflow entry and generate bindings, so those scopes materialize
// defaults exactly like the run entry does for root params.
func TestDesugarBakesScopeDefaults(t *testing.T) {
	cfg := loadRef(t)
	wf := cfg.Workflows["task"]
	wf.Params["tuned"] = ParamDef{Type: "string", Default: "fallback"}
	cfg.Workflows["task"] = wf

	ir, err := Desugar(cfg, "epic") // epic generates over task
	if err != nil {
		t.Fatal(err)
	}
	var gen *Node
	for i := range ir.Nodes {
		if ir.Nodes[i].Kind == KindGenerate {
			gen = &ir.Nodes[i]
		}
	}
	if gen == nil {
		t.Fatal("epic has no generate node")
	}
	b, ok := gen.Gen.Bindings["tuned"]
	if !ok || b.Kind != BindLiteral || b.Value != "fallback" {
		t.Fatalf("generate bindings must carry the baked default, got %+v", gen.Gen.Bindings["tuned"])
	}
	// An explicit binding wins over the default.
	epic := cfg.Workflows["epic"]
	epic.Steps[0].Generate.With["tuned"] = "explicit"
	cfg.Workflows["epic"] = epic
	ir, err = Desugar(cfg, "epic")
	if err != nil {
		t.Fatal(err)
	}
	for i := range ir.Nodes {
		if ir.Nodes[i].Kind == KindGenerate {
			if got := ir.Nodes[i].Gen.Bindings["tuned"]; got.Value != "explicit" {
				t.Fatalf("explicit binding must win over the default, got %+v", got)
			}
		}
	}
}
