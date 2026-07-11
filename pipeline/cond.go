package pipeline

import (
	"fmt"

	"github.com/dmitriyb/faber/config"
	"github.com/google/cel-go/cel"
)

// condEval is the run-time half of the condition lifecycle. Compilation
// happens once per node when its graph (or splice) is loaded — never
// mid-dispatch — in an environment declaring exactly the variables the
// config module's validate-time environment declares, and every expression
// is first re-checked through config.CompileCondition so validate-time
// compilation and run-time evaluation cannot drift.
type condEval struct {
	env *cel.Env
}

// newCondEval builds the run-time CEL environment. It mirrors the config
// module's condition environment: steps and params as string-keyed dyn maps,
// item as dyn. No custom functions, no extra variables.
func newCondEval() (*condEval, error) {
	env, err := cel.NewEnv(
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("item", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("pipeline: build CEL environment: %w", err)
	}
	return &condEval{env: env}, nil
}

// compile checks the expression through the config module's shared entry
// point, then compiles the retained program. Called at graph load and at
// generate splice; nothing compiles on the dispatch path.
func (c *condEval) compile(spec *config.CondSpec) (cel.Program, error) {
	if spec == nil {
		return nil, nil
	}
	if err := config.CompileCondition(spec.CEL); err != nil {
		return nil, fmt.Errorf("pipeline: condition %q: %w", spec.CEL, err)
	}
	ast, iss := c.env.Compile(spec.CEL)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("pipeline: condition %q: %w", spec.CEL, iss.Err())
	}
	prog, err := c.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("pipeline: condition %q: %w", spec.CEL, err)
	}
	return prog, nil
}

// eval answers run-or-skip for one compiled condition. The activation maps
// each dep node id (the key the desugarer rewrote the expression to read) to
// its settled payload. The skipped-dependency short-circuit comes first: a
// skipped step has no payload, so any skipped dep makes the condition false
// without evaluating CEL — this single rule is what terminates a settled
// loop's unrolled tail. An evaluation error against a well-typed activation
// is returned, never silently treated as false.
func (c *condEval) eval(prog cel.Program, spec *config.CondSpec, dep func(id string) *execNode, params map[string]any) (bool, error) {
	steps := map[string]any{}
	for _, id := range spec.Deps {
		d := dep(id)
		if d == nil {
			return false, fmt.Errorf("pipeline: condition %q reads unknown node %q", spec.CEL, id)
		}
		if !d.terminal() {
			return false, fmt.Errorf("pipeline: condition %q reads unsettled node %q", spec.CEL, id)
		}
		if d.status == StateSkippedCondition || d.status == StateSkippedDependency {
			return false, nil // skip propagates; no evaluation
		}
		steps[id] = d.payload
	}
	if params == nil {
		params = map[string]any{}
	}
	out, _, err := prog.Eval(map[string]any{"steps": steps, "params": params})
	if err != nil {
		return false, fmt.Errorf("pipeline: condition %q: %w", spec.CEL, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("pipeline: condition %q evaluated to %T, want bool", spec.CEL, out.Value())
	}
	return b, nil
}
