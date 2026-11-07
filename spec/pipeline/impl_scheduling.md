# Implementation: Scheduling algorithm

Covers Scheduler.

## State (internal/pipeline/scheduler.go)

```go
type Scheduler struct {
    nodes   map[string]*nodeState // grows on generate splice
    edges   *edgeIndex            // out-edges + in-degree, both directions
    ready   *readyQueue           // min-heap ordered by node ID
    slots   chan struct{}         // cap = --max-parallel; box runs only
    events  chan event            // workers -> loop; loop owns all state
    conds   *ConditionEvaluator
    expand  *GenerateExpander
    journal failure.Journal
    meter   metering.Meter
    steps   StepRunner            // agent box wrapped in failure retry
    clock   clock                 // injectable for defer tests
}

type nodeState struct {
    node    *config.Node
    state   State                 // pending|running|deferred|terminal…
    result  *failure.Result
}

type event interface{ isEvent() }
// settled{id, result} | deferred{id, until} | expanded{id, splice, err}
```

All graph mutation happens on the loop goroutine; workers communicate only via
`events`. No mutex guards the DAG — the splice, the decrements, and the ready
queue are single-threaded by construction, and `go test -race` stays quiet.

The `steps` closure is the host-side `infra.ContainerSpec` assembly seam: it bridges
the security module's `Assembled` (verbatim argv fragment + `SecretsStdin`) and
the engine mounts/env into one run spec before handing it to
`infra.ContainerRunner`, so it is the single place that owns `ContainerSpec.Env`
assembly and therefore enforces the credentials pairing — a non-empty
`Assembled.SecretsStdin` is copied into `ContainerSpec.StdinSecrets` and
`ContainerSpec.Env[contract.EnvSecretsStdin]="1"` is set in the same step, never one
without the other (see spec/infra/impl_run_argv.md).

## Skills staging (image content, not a run-prep mount)

The config module's desugarer resolves each template's skills leg into
`config.ResolvedSkills` (see spec/config/impl_desugaring.md). Collapsing it into
the single `/faber/skills` tree is impure (it reads host files), and it is done
once per image, at `faber build`, by the image builder — never in the pure
desugarer and no longer per attempt: the tree is baked into the template's image
(`spec/infra/impl_nix_build.md`, "Engine inputs") and its bytes fold into the
image tag. Run-prep stages nothing for it, and the `ContainerSpec` carries no
`/faber/skills`, `/faber/hooks` or `/faber/bin` mount; the box still sees one
`<name>/SKILL.md` tree and symlinks `$HOME/<SkillsLink> -> /faber/skills` from
`FABER_SKILLS_LINK`, unchanged.

## Session capture (the run-prep seam owns the live bind)

Session transcripts are the harness's native session state — where it keeps
them is the invocation profile's `session_dir` (vendor dialect as data, see
spec/config/arch_schema_types.md); whether to keep them is per-run policy
(`--sessions`, journaled in the header and OR-able back in by
`resume --sessions`). When capture is on **and** the step template's resolved
profile carries a `session_dir`, `RunAttempt` creates
`<attemptDir>/sessions/` (after the pre-attempt scrub, so each attempt starts
empty) and passes it as `BoxSpec.SessionsDir`; the run-spec assembler then
mounts it read-write at `$HOME/<session_dir>` and sets `FABER_SESSIONS_DIR`
to that container path — mount and variable always together, the
`FABER_SECRETS_STDIN` pair-set discipline. Either gate absent ⇒ no mount, no
variable, byte-identical run specs.

Consequences, all by construction: each attempt's transcripts are isolated
and addressed by the existing `boxes/<step>/attempt-<n>/` layout (the scrub
clears only its *own* attempt dir, so earlier attempts' records persist); the
transcript is on the host the moment the harness writes it, so a container
that dies mid-step leaves the record behind; `/workspace` stays an anonymous
native volume — only append-scale transcript writes cross the bind. The one
place the scrub WOULD eat a transcript is the resume-reuse case: a resumed
run restarts attempt numbering, so the re-run of a failed step reuses the
failed attempt's dir — and that failed attempt's transcript is the record
that explains the failure. `preserveSessions` therefore moves a reused
attempt dir's non-empty `sessions/` aside — to the sibling
`<attemptDir>.sessions.<k>`, first free `k` — before the scrub, regardless of
the current toggle: whatever a prior execution captured is a record. Re-entry
keeps reading `attempt-<final>/sessions` (the latest failed execution's
transcript); the `.sessions.<k>` siblings are older preserved generations. The
assembler re-checks the profile pairing (a `SessionsDir` without a profile
`session_dir` is a refusal) and the container path stays inside `HOME` by
config validation, re-checked as defense in depth. The engine's job ends at
durable, step-addressed transcripts under the run directory; reading them is
user-side work.

## Session-resuming interactive re-entry

Re-entry (`Reentry.Reenter`) keeps its shape — same image, bindings, inputs,
handoff mount — but when the failed attempt saved a session
(`boxes/<step>/attempt-<final>/sessions/` exists and is non-empty), the
profile defines `resume_argv`, and the operator did not force `--shell`, the
saved session is **copied** (never bound — the archived record stays
immutable while the ephemeral session diverges freely) into the salted
interactive dir and that copy is mounted read-write at `$HOME/<session_dir>`.
The copy is faithful, not the skills stager's normalizing `copyTree`:
harnesses locate "the most recent session" via file mtimes or a
latest-pointer symlink, so `copySessionTree` preserves file modes and mtimes
and recreates symlinks verbatim (they resolve inside the operator's own
debug container);
the entry program becomes `resume_argv` with `HOME` pinned to the box home
(the raw entry replaces the sequencer, so no preamble exports it), landing
the operator inside the conversation that produced the failure. The salted
dir — copy included — is removed when the session exits, as today. Fallback
to the bare shell: `--shell`, no saved session, or no `resume_argv`.
Re-entry scope stays failed-only.

## The loop

```go
func (s *Scheduler) Run(ctx context.Context) error {
    s.seed() // in-degrees; indeg==0 -> ready
    for s.unsettled() > 0 {
        for s.ready.Len() > 0 {
            s.dispatch(ctx, s.ready.Pop()) // ID order = deterministic
        }
        s.step(ctx, <-s.events)
    }
    return s.runError() // nil unless some node failed
}
```

`dispatch` runs the cheap gates inline, in the fixed order:

1. **Condition**: `conds.Evaluate` false (or skipped-dep short-circuit) ⇒
   `settle(id, SkippedCondition)`.
2. **Journal**: `journal.Lookup(id, inputHash(id))` returning an `ok` record ⇒
   `settle(id, cached(rec))`. `inputHash` covers resolved input values,
   template identity, and image tag — the failure module's key contract. The image
   tag is recomputed here from the node's `ResolvedTemplate` via the `imageTagger`
   seam, which must carry the template's pin into its reconstructed `BuildDef`, so a
   pinned toolset's resume tag matches the one `faber build` produced (see
   `spec/infra/impl_nix_build.md`, "Run-time tag reconstruction").
3. **Selector**: resolve newest `ok` candidate; exhaustion condition true on
   the final iteration ⇒ settle `failed(loop-exhausted)`, else adopt payload.
4. Otherwise spawn a worker: acquire a slot, `meter.Estimate` ⇒ on
   `defer(until)` release the slot and emit `deferred`; on `reject` emit
   `settled(failed(budget))`; on `admit` run `steps.Run` (retry loop inside)
   and emit `settled` with the final record. A rate-limit failure carrying a
   reset time comes back from metering's defer floor as `deferred`, not
   `settled`, and consumes no retry.

`step` folds one event into the graph:

```go
case settled:
    s.record(ev)                       // journal append (skips too, hashless)
    for _, d := range s.edges.dependents(ev.id) {
        if s.edges.dec(d) == 0 { s.ready.Push(d) }
    }
    if ev.result.Failed() { s.propagate(ev.id) }
    if s.isGenerate(ev.id) && ev.result.OK() { /* settled by expanded */ }
case deferred:
    s.clock.AfterFunc(until, func() { s.ready.Push(ev.id) }) // re-admission
case expanded:
    s.splice(ev) // add nodes+edges, seed in-degrees, rewire dependents
```

Generate nodes dispatch to `expand.Expand` in a worker; the `expanded` event
carries the splice (or the contract error, settling the node failed). The node
settles `ok` only after the splice is applied, so no dependent can slip
between expansion and rewiring.

## The pause gate

The scheduler carries a `pause func() bool` probe (the executor wires it to
`failure.PauseRequested(runDir)`; tests fake it). The drain pass checks it
first and, once true, latches `paused` and stops granting slots and
dispatching ready nodes; the event loop keeps folding settlements (journal
appends, cost records, propagation all normal) and exits when `paused` and
nothing is outstanding while nodes remain undispatched. An idle loop (a
timed defer window with no worker outstanding) re-probes on a coarse tick
(`pausePollInterval`, ~1s, test-shrinkable via the scheduler's `pausePoll`),
so the pause never waits out the window. The executor then
appends the `paused` run-end record and — when no step failed or halted —
returns the typed `RunPaused` error (`ExitCode() 4`). A deferred node (timed
or zero-until) is not outstanding: its wake event may arrive after the loop
exits and is absorbed by the closed-loop send guard.

## Failure and halt propagation

```go
// state is StateSkippedDependency for a failed root, StateSkippedHalt for a
// halted one; the root's id is recorded as the ancestor either way.
func (s *Scheduler) propagate(root, state string) {
    q := s.edges.dependents(root)
    for len(q) > 0 {
        id := pop(&q)
        if s.nodes[id].settledOrRunning() { continue }
        s.settle(id, skip(state, root)) // ancestor recorded
        q = append(q, s.edges.dependents(id)...)
    }
}
```

BFS, idempotent, and cheap: nodes already settled (or mid-flight — their own
result will tell) are left alone; everything else downstream terminates
immediately with the root cause attached. Independent subgraphs never appear
in `dependents` and keep executing. A `halted` settlement additionally counts
into the scheduler's halt tally (with the halt reason), which the executor
folds into the run-end record and the exit-code decision.

## Bounds (pools and lanes)

```go
type pool struct{ max, used int }            // free() when max <= 0 or used < max
genPools map[string]*pool                     // generate node id -> its instance pool (GenSpec.MaxParallel > 0)
lanes    map[string]*pool                     // IR.Lanes, name -> bound
execNode.gen     string                       // the generate an instance node belongs to
execNode.waiting string                       // the hold reason last announced
func (s *scheduler) holdReason(n) string      // "" | "slot u of m" | "generate <id> u of m" | "lane <name> u of m"
func (s *scheduler) takePools(n) / releasePools(n)   // global slot + generate pool + lane, always together
```

The drain pass pops `slotQ` in id order while the global slot is free; a
node whose generate pool or lane is full is announced (`announceHold`, once
per distinct reason) and set aside; held nodes re-queue after the pass.
When the global slot is full the queue head is announced with `slot u of m`.
`applyExpansion` tags every spliced node with its generate id and creates
the pool when the spec declares a bound; the executor builds the lane pools
from `IR.Lanes`. Release happens in the `evSettled`/`evDeferred` folds
through `releasePools`, so a defer frees the lane for the next box exactly
as it frees the semaphore.

## The expect gate

```go
loops    map[string]config.LoopSpec // loop prefix id -> {Max, Command, Args}; entry IR + subs, instances rebased at expansion
checked  map[string]int             // loop -> highest iteration the command ran for
expected map[string]int             // loop -> last count
pauseDetail string                  // the cap stop's message (run-end detail, RunPaused.Detail)
func (s *scheduler) unexpectedIteration(id) (loop, iter, ok)  // outermost declared loop whose iteration id enters first
func (s *scheduler) startExpect(ctx, n, loop, iter)           // worker: expander.expectCount(spec, n.env) -> evExpected
func (s *scheduler) applyExpected(ev)                          // fold on the loop goroutine
```

`dispatch` evaluates the node's condition first, then asks
`unexpectedIteration`; a hit parks the node (`nsRunning`, `outstanding++`)
and runs the command host-side through the generate expander's runner
(`expectCount`: args resolved from the node's scope like data-source args,
stdout decoded as `{"count": N}`, N >= 0, else a contract error).
`applyExpected` records the boundary (`checked`), then: an error settles the
node failed with `sourceContractResult`; `count > Max` sets `paused` and
`pauseDetail` (the pause gate then stops every grant and dispatch, the
executor writes the detail into the run-end record and `RunPaused`), leaving
the node undispatched for resume; otherwise the count is emitted
(`loop-expected`, with the cap) and the node is pushed back to the ready
heap. `evExpected` is the worker-to-loop message, like `evExpanded`.

## Progress events and the per-attempt output log

```go
type EventKind string // run-started | step-started | step-settled | step-deferred | step-waiting |
                      // loop-expected | generate-expanded | pause-requested | run-ended
type Event struct {
    Kind EventKind; Time time.Time; Step string
    Template string; Attempt int                         // step-started
    Status, Reason string; Cached bool; Duration time.Duration // step-settled
    Until time.Time; Detail string                       // step-deferred / run-ended
    Items, Nodes int                                     // generate-expanded
    RunID, Workflow, RunStatus string; Failed, Halted int // run-started / run-ended
    Loops map[string]int                                 // loop caps: all on run-started, instances on generate-expanded
}
type EventSink interface{ Emit(Event) }                  // concurrency-safe

func (s *scheduler) emit(ev Event)   // nil sink → no-op; fills Time from the clock
```

Emission points are the existing single points of truth: `settleResult` and
`settleSkip` (one settled event each, before `finish`), the `evDeferred`
fold, `applyExpansion`, `pauseGate` (once, when the request latches), and the
executor after `endRun`. The attempt start is emitted by the box worker's
runner closure immediately before `RunAttempt`, right after it appends the
journal's `StartRecord{StepID, Attempt: base+attempt, Started}` — a failed
start append is logged, not fatal (the settlement's own append surfaces an
unappendable journal). A defer-converted dispatch consumes no attempt, so its
re-dispatch starts as attempt 1 again, exactly as its result records it.

The run-prep seam (`AgentBoxes.RunAttempt`) opens
`<attemptDir>/output.log` (`O_CREATE|O_TRUNC`, after the pre-attempt scrub)
and sets `ContainerSpec.Output` to it; the container runner tees the streamed
combined output into it beside the bounded tail and the debug forwarding.
The executor emits `run-started` (run id, workflow, every loop cap off the
IR via `loopBounds`) before scheduling; `applyExpansion` adds the spliced
instances' caps to `generate-expanded`. The renderers are the reporter's
(`progress.go`: `TextSink`, `JSONSink`; see `impl_conditions_reporting.md`
and `arch_run_reporter.md`, "Progress renderers"); the cmd wiring attaches
the one `RunOptions.Progress` names.

## Journal records for skips

Skip settlements append journal records with a null input hash (a skipped
node's inputs may be unresolvable — its producer failed). Null-hash records
are never resume hits; they exist so the report and journal replay see every
terminal state. All three skip flavors (condition, dependency, halt) share
the encoding: a failed-status record carrying the reserved skip reason, the
ancestor in the detail. Ok/failed/halted records carry the real hash and
full result.
