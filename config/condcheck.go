package config

import (
	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
)

// checkCondRefs walks a compiled condition's AST and enforces the canonical
// reference discipline the textual pipeline (rewriteStepRefs + the regex
// field check) cannot see on its own:
//
//   - every use of `steps` is exactly steps["<node-id>"].<field> with a
//     literal id the desugarer derived as a dependency — a hand-written
//     steps["x"].f (which the rewriter leaves untouched) would otherwise
//     compile, extract no dep edge, and fail only mid-run;
//   - every use of `params` is params.<name> (or params["<name>"]) with a
//     declared name — an undeclared name compiles against the dyn map and
//     fails only at evaluation;
//   - `item` never appears — the run-time activation binds only steps and
//     params, so an item reference is a guaranteed evaluation error.
//
// Violations are reported through report against the already-compiled AST.
func checkCondRefs(ast *cel.Ast, scope condScope, report func(format string, args ...any)) {
	walkCondExpr(ast.NativeRep().Expr(), scope, report)
}

// condScope is the reference vocabulary one condition is checked against:
// the derived dep node ids, the scope's declared param names, and the subset
// of params guaranteed present at evaluation (required or defaulted — an
// optional param without a default may simply be absent from the activation,
// at the root scope and in sub/instance scopes alike).
type condScope struct {
	deps       map[string]bool
	declared   map[string]bool
	guaranteed map[string]bool
}

// checkParamName reports the right violation for a params.<name> reference.
func (s condScope) checkParamName(name string, report func(format string, args ...any)) {
	switch {
	case !s.declared[name]:
		report("condition reads params.%s, which is not a declared param of this scope", name)
	case !s.guaranteed[name]:
		report("condition reads params.%s, which is optional without a default and may be absent at evaluation — give it a default or make it required", name)
	}
}

// walkCondExpr recursively validates one expression node. Canonical
// subtrees (steps["id"].field, params.name) are consumed whole; any other
// appearance of the reserved roots is a violation.
func walkCondExpr(e celast.Expr, scope condScope, report func(format string, args ...any)) {
	switch e.Kind() {
	case celast.SelectKind:
		sel := e.AsSelect()
		op := sel.Operand()
		// params.<name>
		if op.Kind() == celast.IdentKind && op.AsIdent() == "params" {
			scope.checkParamName(sel.FieldName(), report)
			return
		}
		// steps["<id>"].<field>
		if id, ok := stepsIndexArg(op); ok {
			checkStepsID(op, id, scope.deps, report)
			return
		}
		walkCondExpr(op, scope, report)
	case celast.CallKind:
		call := e.AsCall()
		if call.FunctionName() == operators.Index && len(call.Args()) == 2 {
			target, index := call.Args()[0], call.Args()[1]
			if target.Kind() == celast.IdentKind {
				switch target.AsIdent() {
				case "steps":
					// steps[...] not under a field select: either a dynamic
					// index or a whole-payload read — both non-canonical.
					if id, ok := stepsIndexArg(e); ok {
						checkStepsID(e, id, scope.deps, report)
						report("condition reads steps[%q] without selecting a field; write steps.<step>.<field>", id)
					} else {
						report("condition indexes steps with a non-literal key; write steps.<step>.<field>")
					}
					return
				case "params":
					if index.Kind() == celast.LiteralKind {
						if name, ok := index.AsLiteral().Value().(string); ok {
							scope.checkParamName(name, report)
							return
						}
					}
					report("condition indexes params with a non-literal key; write params.<name>")
					return
				}
			}
		}
		for _, a := range call.Args() {
			walkCondExpr(a, scope, report)
		}
		if call.IsMemberFunction() {
			walkCondExpr(call.Target(), scope, report)
		}
	case celast.IdentKind:
		switch e.AsIdent() {
		case "steps":
			report("condition uses steps outside a steps.<step>.<field> reference")
		case "params":
			report("condition uses params outside a params.<name> reference")
		case "item":
			report("condition references item, which is not available in conditions; thread the value through a param")
		}
	case celast.ListKind:
		for _, el := range e.AsList().Elements() {
			walkCondExpr(el, scope, report)
		}
	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			walkCondExpr(me.Key(), scope, report)
			walkCondExpr(me.Value(), scope, report)
		}
	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			walkCondExpr(field.AsStructField().Value(), scope, report)
		}
	case celast.ComprehensionKind:
		comp := e.AsComprehension()
		walkCondExpr(comp.IterRange(), scope, report)
		walkCondExpr(comp.AccuInit(), scope, report)
		walkCondExpr(comp.LoopCondition(), scope, report)
		walkCondExpr(comp.LoopStep(), scope, report)
		walkCondExpr(comp.Result(), scope, report)
	}
}

// stepsIndexArg reports whether e is exactly steps["<literal string>"],
// returning the literal id.
func stepsIndexArg(e celast.Expr) (string, bool) {
	if e.Kind() != celast.CallKind {
		return "", false
	}
	call := e.AsCall()
	if call.FunctionName() != operators.Index || len(call.Args()) != 2 {
		return "", false
	}
	target, index := call.Args()[0], call.Args()[1]
	if target.Kind() != celast.IdentKind || target.AsIdent() != "steps" {
		return "", false
	}
	if index.Kind() != celast.LiteralKind {
		return "", false
	}
	s, ok := index.AsLiteral().Value().(string)
	return s, ok
}

// checkStepsID validates a canonical steps["<id>"] key against the derived
// dep set. The desugarer records a dep for every reference it rewrites, so a
// key outside the set was authored directly and carries no edge — it would
// read an unsettled or unknown node mid-run.
func checkStepsID(_ celast.Expr, id string, deps map[string]bool, report func(format string, args ...any)) {
	if !deps[id] {
		report("condition reads steps[%q], which the desugarer did not derive as a dependency; write steps.<step>.<field> so the reference becomes an edge", id)
	}
}
