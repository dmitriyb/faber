# Implementation: Conditions and reporting

Covers ConditionEvaluator and RunReporter.

## ConditionEvaluator (internal/pipeline/cond.go)

```go
type ConditionEvaluator struct {
    progs  map[string]cel.Program // keyed by node ID; filled at validate/load
    params map[string]any         // resolved run params, set once
}

// CompileCondition is the validate-time entry point the config module's
// WiringChecker calls for every when:/until:/exhaustion expression.
func CompileCondition(expr string, refs CondRefs) (cel.Program, error) {
    env, err := cel.NewEnv(
        cel.Variable("steps", cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.DynType))),
        cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
    )
    ast, iss := env.Compile(expr)
    // post-checks on the checked AST:
    //   - result type is bool
    //   - every steps.X.f select: X in refs.Steps, f in X's output schema,
    //     enum comparisons against declared enum values
    //   - every params.p: p declared in refs.Params
    return env.Program(ast)
}
```

`refs` carries only declarations (output schemas, param types) — compilation
is pure and needs no results. Programs are compiled once per node and reused
for every evaluation; nothing compiles mid-run.

```go
func (c *ConditionEvaluator) Evaluate(n *config.Node, results resultLookup) (bool, error) {
    steps := map[string]map[string]any{}
    for name, depID := range n.When.Deps { // source-level name -> node ID
        r := results(depID) // settled by construction: deps precede readiness
        if r.Skipped() { return false, nil } // skip propagates, no CEL eval
        steps[name] = r.Payload
    }
    out, _, err := c.progs[n.ID].Eval(map[string]any{"steps": steps, "params": c.params})
    if err != nil { return false, err } // node fails; never silently false
    return out.Value().(bool), nil
}
```

The skipped-dep short-circuit before `Eval` is the entire loop-early-exit
mechanism: iteration *i*'s gate reads `review@i-1`, so one false gate cascades
`skipped-condition` down the remaining chain.

## RunReporter (internal/pipeline/report.go)

```go
type RunReporter struct{}

type RunReport struct {
    Run      RunHeader     // workflow, config/IR hash, params, totals, wall time
    Steps    []StepLine    // canonical IR node order
    Generate []GenRollup   // one per generate node
}

type StepLine struct {
    ID       string         // task/review-cycle@2/fix
    Status   string         // ok|failed|halted|skipped-condition|skipped-dependency|skipped-halt|absent
    Cached   bool           // journal hit
    AgentSkipped bool       // the attempt's agent phase was skipped by the prelude
    Duration time.Duration
    Attempts int
    Deferred int            // count; DeferredFor total wait
    Outputs  map[string]any // key payload fields
    Error    *failure.ErrorRecord `json:",omitempty"` // reason, detail, handoff path
    Halt     *failure.HaltRecord  `json:",omitempty"` // halted step's reason/detail/phase
    Ancestor string         `json:",omitempty"`       // skipped-dependency / skipped-halt root cause
    Chose    string         `json:",omitempty"`       // selector's resolved candidate
}

func (RunReporter) Report(j failure.JournalReader, ir *config.IR) (*RunReport, error)
```

Construction is a pure join: read the journal header and records into a map,
walk `ir.Nodes` in canonical order emitting a `StepLine` per node (`absent`
when no record exists), then fold lines whose IDs match a generate-instance
prefix `gen[item]/` into per-item rollups under their `GenRollup` (aggregate
status = failed if any failed, else skipped-dependency if any, else ok).

Rendering is two thin functions over the same struct:

- `Text(w io.Writer)` — aligned per-step lines, generate rollups with nested
  instance lines, failure blocks (reason, detail, handoff pointer, attempts,
  `faber resume --interactive <id>` hint), halt blocks (halting step, halt
  reason, detail — named without the reader parsing JSON), run footer with
  per-state totals.
- `JSON(w io.Writer)` — `json.Encoder` with `SetEscapeHTML(false)` over the
  struct; field order fixed, slices pre-sorted, so output is diff-stable.

Exit-code mapping lives with the executor's own counters: failed > 0 ⇒ the
failure error (exit 1); else halted > 0 ⇒ the typed halt error (exit 3).
The reporter performs no I/O beyond the journal reader and the writer it is
handed, holds no state, and never inspects scheduler memory — reporting a
crashed run and a settled run is the same code path.
