package failure

import (
	"context"
	"log/slog"
	"time"

	"github.com/dmitriyb/faber/config"
)

// Terminal step states — the vocabulary FailurePolicy defines and the
// scheduler applies. A failed step halts its dependency chain (fail-stop);
// there is no continue-on-error mode, because the type system promised
// dependents a schema-valid payload.
const (
	StateOK      = string(StatusOK)
	StateFailed  = string(StatusFailed)
	StateSkipped = "skipped (dependency failed)"
)

// StepSpec is what the policy needs to know about one step: identity, journal
// key, resolved inputs (what a cleanup can act on), and its declared failure
// policy. Retry is the number of allowed re-runs after the first failure;
// OnFailure is the opaque cleanup script path ("" = none).
type StepSpec struct {
	ID        string
	InputHash string
	Inputs    map[string]any
	Retry     int
	OnFailure string
}

// StepRunner launches one whole attempt of the step — a fresh container from
// the same image with the same resolved inputs — and returns its result
// record. A step is atomic: faber never resumes into an agent's chain of
// thought. A non-nil error means the attempt never launched.
type StepRunner func(ctx context.Context, attempt int) (Result, error)

// Policy orchestrates the attempt loop: run, cleanup between attempts (and
// once after a final failure), and attempt accounting. The final record is
// returned to the caller, which journals and propagates it; intermediate
// failures are not journaled individually — they travel as the final
// record's attempt history.
type Policy struct {
	Journal *Journal
	Hooks   HookRunner
	Log     *slog.Logger
}

// RunStep drives one step to settlement: up to 1+spec.Retry attempts, with
// on_failure cleanup between attempts (the clean-slate guarantee) and once
// after the final failure. Logging carries step ids, attempts, and reasons —
// never input values or payloads. First pass has no per-step timeout and no
// mid-attempt cancellation (abort is process-level); ctx is threaded to the
// runner and hooks so a dying process takes its subprocesses with it.
func (p *Policy) RunStep(ctx context.Context, spec StepSpec, run StepRunner) Result {
	log := p.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxAttempts := 1 + spec.Retry
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var history []AttemptInfo
	for attempt := 1; ; attempt++ {
		// A cancelled run must not consume the remaining retry budget on
		// attempts that cannot succeed: each one would fully stage and launch
		// (skills tree, bindings, docker kill) against a dead context. The
		// canceled record keeps the real attempts' history; the step re-runs
		// intact on resume.
		if cerr := context.Cause(ctx); cerr != nil {
			now := time.Now()
			return Result{
				Status:   StatusFailed,
				Error:    &ErrorRecord{Reason: ReasonCanceled, Detail: cerr.Error()},
				Timing:   Timing{Started: now, Finished: now},
				Attempt:  attempt,
				Attempts: history,
			}
		}
		started := time.Now()
		res, err := run(ctx, attempt)
		if err != nil {
			// A launch-level error is still a record, not an absence.
			res = Result{
				Status: StatusFailed,
				Error:  &ErrorRecord{Reason: ReasonLaunch, Detail: err.Error()},
			}
		}
		if res.Timing.Started.IsZero() {
			res.Timing = Timing{Started: started, Finished: time.Now()}
		}
		res.Attempt = attempt
		res.Attempts = history
		if verr := res.Validate(); verr != nil {
			// The runner broke the result contract; the step did not produce
			// what its contract promises.
			res = Result{
				Status:   StatusFailed,
				Error:    &ErrorRecord{Reason: ReasonResultSchema, Detail: verr.Error()},
				Timing:   res.Timing,
				Attempt:  attempt,
				Attempts: history,
			}
		}
		if res.Status == StatusOK {
			return res
		}
		log.Info("step attempt failed",
			"step", spec.ID, "attempt", attempt, "of", maxAttempts, "reason", res.Error.Reason)
		if attempt == maxAttempts {
			// Terminal cleanup: release what the last attempt claimed before
			// the final record is returned for journaling.
			p.cleanup(ctx, spec, res, log)
			return res
		}
		history = append(history, AttemptInfo{Attempt: attempt, Timing: res.Timing, Error: res.Error})
		// Between-attempt cleanup is what guarantees the next attempt starts
		// from a clean slate.
		p.cleanup(ctx, spec, res, log)
	}
}

// CleanupGrace bounds an on_failure hook run on an ALREADY-aborted run's
// detached context: cleanup must still run (an abort is exactly when external
// side-effects need releasing) but must not stall shutdown forever. A healthy
// run's cleanup keeps the live context unbounded — this cap applies only when
// the run context is already cancelled.
const CleanupGrace = 2 * time.Minute

// CleanupContext returns the context an on_failure hook runs under. On a
// healthy run it is the live context (cleanup is bounded only by the hook
// itself, as before). When the run is already aborted it is a
// cancellation-detached, time-bounded context, so the terminal cleanup still
// runs — an aborted run is the one moment releasing side-effects matters most
// — without stalling shutdown. Returns a no-op cancel when the live context
// is used; callers defer it unconditionally.
func CleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), CleanupGrace)
}

// cleanup runs the declared on_failure hook and journals its outcome. A
// cleanup that itself fails is reported — a cleanup record beside the failure
// — but the step's original failure record stands untouched. See
// CleanupContext for the abort-safety discipline.
func (p *Policy) cleanup(ctx context.Context, spec StepSpec, res Result, log *slog.Logger) {
	if spec.OnFailure == "" {
		return
	}
	hctx, cancel := CleanupContext(ctx)
	defer cancel()
	var err error
	if p.Hooks == nil {
		err = errNoHookRunner
	} else {
		err = p.Hooks.RunOnFailure(hctx, HookInvocation{
			Script:  spec.OnFailure,
			StepID:  spec.ID,
			Attempt: res.Attempt,
			Inputs:  spec.Inputs,
			Error:   *res.Error,
		})
	}
	rec := CleanupRecord{
		StepID:    spec.ID,
		InputHash: spec.InputHash,
		Attempt:   res.Attempt,
		OK:        err == nil,
	}
	if err != nil {
		rec.Detail = err.Error()
		log.Warn("on_failure cleanup failed", "step", spec.ID, "attempt", res.Attempt)
	}
	if p.Journal != nil {
		if aerr := p.Journal.AppendCleanup(rec); aerr != nil {
			log.Error("journal cleanup record", "step", spec.ID, "err", aerr)
		}
	}
}

var errNoHookRunner = errNoHooks{}

type errNoHooks struct{}

func (errNoHooks) Error() string {
	return "failure: step declares on_failure but no hook runner is wired"
}

// SkipSet is the fail-stop rule as data: given a graph and the ids of failed
// nodes, it returns every transitive dependent — the nodes the scheduler
// marks StateSkipped and never launches. Dependency follows the IR's edges
// (data and ordering alike) plus condition reads (a step whose `when` reads a
// failed step's result cannot evaluate). Independent branches are untouched.
// The set is computed per graph level; sub-workflow graphs are the
// scheduler's to recurse.
func SkipSet(ir *config.IR, failed ...string) map[string]bool {
	succ := map[string][]string{}
	for _, e := range ir.Edges {
		succ[e.From] = append(succ[e.From], e.To)
	}
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if n.When == nil {
			continue
		}
		for _, dep := range n.When.Deps {
			succ[dep] = append(succ[dep], n.ID)
		}
	}
	skip := map[string]bool{}
	queue := append([]string(nil), failed...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range succ[id] {
			if skip[next] {
				continue
			}
			skip[next] = true
			queue = append(queue, next)
		}
	}
	for _, id := range failed {
		delete(skip, id) // a failed node is failed, not skipped
	}
	return skip
}
