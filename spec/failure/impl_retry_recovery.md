# Implementation: Retry and recovery algorithms

Covers FailurePolicy and RecoveryModes.

## Attempt loop (internal/failure/policy.go)

The Scheduler hands FailurePolicy a launch function; the policy owns the loop:

```go
// StepRunner launches one attempt of the step and returns its result record.
type StepRunner func(ctx context.Context, attempt int) (Result, error)

type Policy struct {
    Journal *Journal
    Hooks   HookRunner
    Log     *slog.Logger
}

func (p *Policy) RunStep(ctx context.Context, st StepSpec, run StepRunner) Result {
    var history []AttemptInfo
    max := 1 + st.Retry
    for attempt := 1; ; attempt++ {
        res, err := run(ctx, attempt)
        if err != nil { // launch-level error: still a record, reason "launch"
            res = failedResult("launch", err, attempt)
        }
        res.Attempt, res.Attempts = attempt, history
        if res.Status == StatusOK || attempt == max {
            return res // final record: journaled + propagated by the caller
        }
        history = append(history, attemptInfoOf(res))
        p.cleanup(ctx, st, res) // between attempts: the idempotency guarantee
    }
}
```

Intermediate failures are not journaled individually; the final record carries
them as `attempts`. Exactly one result record per step per (resumed) run
reaches the journal.

## Cleanup hook (internal/failure/hooks.go)

```go
type HookRunner interface {
    // RunOnFailure execs the opaque user script host-side.
    RunOnFailure(ctx context.Context, script string, inputs map[string]any, e ErrorRecord) error
}
```

The exec implementation: `exec.CommandContext(ctx, script)`, env = the parent
env plus `FABER_STEP_ID`, `FABER_ATTEMPT`, and one `FABER_INPUT_<SLOT>` per
resolved input (scalars verbatim, objects as JSON); stdin = the error record
as JSON; stdout/stderr captured. `cleanup` wraps it:

```go
func (p *Policy) cleanup(ctx context.Context, st StepSpec, res Result) {
    if st.OnFailure == "" {
        return
    }
    err := p.Hooks.RunOnFailure(ctx, st.OnFailure, st.Inputs, *res.Error)
    p.Journal.Append(CleanupRecord{StepID: st.ID, InputHash: st.InputHash,
        OK: err == nil, Detail: detailOf(err)})
    // reported, never masks: res is untouched regardless of err
}
```

Cleanup also runs once after a final failure (the terminal invocation the
requirement's examples describe — release the item, delete the branch), before
the record is returned for journaling.

## Resume seeding (internal/failure/recover.go)

```go
func Resume(ctx context.Context, runDir string, cfg *config.Config) (*RunSeed, error) {
    lock, err := AcquireRunLock(runDir) // exclusivity first: a live run refuses loudly
    // (every refusal below releases the lock; success hands it to the Journal)
    hdr, done, costs, err := Load(filepath.Join(runDir, "journal.jsonl"))
    // re-derive: deterministic pipeline => byte-stable IR
    ir := desugarAndCheck(cfg, hdr.Workflow)
    if hashOf(ir) != hdr.IRHash || !maps.Equal(normParams(cfg), hdr.Params) {
        return nil, fmt.Errorf("run %s: config drift (IR/params changed); resume unsafe, use --fresh", hdr.RunID)
    }
    // Prior seeds readiness-time lookup; Costs seed the metering admitter's
    // spent ledger (the executor folds them), so a budget bounds the whole
    // logical run across interruptions.
    return &RunSeed{IR: ir, Prior: done, Costs: costs, Journal: reopen(runDir, lock)}, nil
}
```

There is no upfront skip set: input-hashes depend on upstream payloads, so the
Scheduler asks at readiness time —

```go
// Lookup: called by the Scheduler when a step becomes ready.
func (s *RunSeed) Lookup(stepID string, inputs map[string]any, tmpl, image string) (Result, bool) {
    k := Key{stepID, mustHash(inputs, tmpl, image)}
    r, ok := s.Prior[k]
    if !ok || r.Result.Status != StatusOK {
        return Result{}, false // failed or absent => run it
    }
    return r.Result, true // skip: reuse payload for threading
}
```

Fresh (`--fresh`) is the degenerate seed: new run id, new journal, empty
`Prior` and `Costs`. Fresh takes the new run's lock exactly like resume —
`Begin` acquires it before creating the journal.

## Interactive re-entry

```go
func Interactive(ctx context.Context, runDir, stepID string, deps Deps) error
```

1. `Load` the journal; find the result record for `stepID` (must be failed —
   refuse otherwise, naming the step's actual state).
2. Re-resolve the step from the re-derived IR: image tag, template, inputs
   (upstream payloads come from the journal — a failed step's dependencies
   necessarily settled ok).
3. Rebuild the step's BindingSet via the security module — same network,
   remote, credential handles, and a freshly spawned single-key agent for the
   same identity — and run the container through infra's ContainerRunner with
   the entry program replaced by a login shell, `-it` wired to the operator's
   TTY (x/term for raw-mode handling), inputs exported as the step env, and
   the record's handoff directory mounted read-only at a well-known path.
4. On shell exit: BindingSet teardown hooks run unconditionally; nothing is
   appended to the journal; exit code is the shell's.
