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
the security module's `Assembled` (verbatim argv fragment + `SecretsStdin` +
`Attach`) and
the engine mounts/env into one run spec before handing it to
`infra.ContainerRunner`, attaches the attempt's two writers (`Output` for the
human log, `Stdout` for the demuxer) and copies `Assembled.Attach` into
`ContainerSpec.Attach` (the identity relay the runner starts beside the
container), so it is the single place that owns `ContainerSpec.Env`
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

## The demuxer (the run-prep seam owns the box's stdout)

The box's stdout is the frame stream the agent module defines
(spec/agent/impl_hook_result_contracts.md, "The frame channel"). `RunAttempt`
points `ContainerSpec.Stdout` at a demuxer rooted in the attempt directory
and runs `contract.ReadFrames` over it as the container streams:

```go
type demuxer struct {
    attemptDir string      // <run>/boxes/<step>/attempt-<n>
    log        io.Writer   // output.log, for foreign bytes
    total      int64       // bytes accepted so far, against MaxAttemptStreamBytes
    err        error       // the first violation; set, the demuxer discards
}
const MaxAttemptStreamBytes = 1 << 30
func (d *demuxer) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error
func (d *demuxer) Foreign(p []byte) error
func (d *demuxer) Close() error   // ends the stream, waits for the reader; the first violation or stream error
func (d *demuxer) Ended() bool    // the end frame arrived
```

Every write goes through an `os.Root` of `<attempt>/result` or
`<attempt>/sessions`, opened on first use: the reader proves a name
lexically contained, the root keeps it contained through whatever the box
planted earlier (a link frame followed by a file, an event, or a link
through it resolves outside and is refused in the kernel).

- `event` frames append the line to `<attemptDir>/result/hooks/events.jsonl`
  (the `hooks/` dir created on first use), so the file the live view and the
  on_failure hook read grows as the hooks write.
- `file` and `link` frames land under the attempt directory. The name's
  first segment must be `result` or `sessions`; any other root is a
  violation, and so is a `link` under `result/` — the extractor and the
  view read the records there, and must never read one through a link the
  box planted (a harness's latest-pointer under `sessions/` is a real link
  and is kept). A file is written to a temporary name beside its target and
  renamed into place, with the frame's mtime applied, so a stream that
  breaks mid-frame leaves no partial file; a link replaces whatever was at
  its name.
- Foreign bytes go to `output.log` with one warning per attempt: a toolset
  binary that writes to the container's stdout costs the attempt nothing but
  noise.
- A frame over `MaxRecordBytes`, an attempt over `MaxAttemptStreamBytes`, an
  invalid name or an unknown kind is the demuxer's first violation: it is
  kept, every later frame is drained and discarded, and after the container
  exits `RunAttempt` logs it with the step and folds it into the record the
  same way an actuation error is folded when the record is missing — the box
  ships `result.json` after the other result files, so a violation among
  them leaves the extractor no record and it synthesizes `box-vanished`,
  whose detail names the violation. A violation after the record (the
  sessions are shipped last) leaves the record standing; the log line is its
  trace. The stream of a re-entry is the host's own and fails the session
  when a file cannot be sent (`handoffStream` reports it after the runner
  returns).

Nothing downstream changed: `ExtractResult`, `readUsage`, `AttemptEvents`,
the on_failure hook's `FABER_RESULT_DIR` and the live view read the attempt
directory exactly as they did when the result dir was a bind. The pairing
discipline of the secrets payload is untouched.

## Session capture (shipped at step end)

Session transcripts are the harness's native session state — where it keeps
them is the invocation profile's `session_dir` (vendor dialect as data, see
spec/config/arch_schema_types.md); whether to keep them is per-run policy
(`--sessions`, journaled in the header and OR-able back in by
`resume --sessions`). When capture is on **and** the step template's resolved
profile carries a `session_dir`, `RunAttempt` creates
`<attemptDir>/sessions/` (after the pre-attempt scrub, so each attempt starts
empty) and sets `BoxSpec.Sessions`; the run-spec assembler then
sets `FABER_SESSIONS_DIR` to `$HOME/<session_dir>` and mounts nothing — the
harness writes into the `HOME` tmpfs as it always did, and the box ships the
directory as `sessions/<rel>` frames when the step ends, which the demuxer
writes under `<attemptDir>/sessions/` with the frames' mtimes and links.
Either gate absent ⇒ no variable, nothing shipped, byte-identical run specs.

Consequences, all by construction: each attempt's transcripts are isolated
and addressed by the existing `boxes/<step>/attempt-<n>/` layout (the scrub
clears only its *own* attempt dir, so earlier attempts' records persist); the
transcript reaches the host when the step ends, so a container killed
mid-step loses it (the trade for sharing no host path with the box — the
events and `output.log` it streamed live are what remains); `/workspace`
stays an anonymous native volume. The one
place the scrub WOULD eat a transcript is the resume-reuse case: a resumed
run restarts attempt numbering, so the re-run of a failed step reuses the
failed attempt's dir — and that failed attempt's transcript is the record
that explains the failure. `preserveSessions` therefore moves a reused
attempt dir's non-empty `sessions/` aside — to the sibling
`<attemptDir>.sessions.<k>`, first free `k` — before the scrub, regardless of
the current toggle: whatever a prior execution captured is a record. Re-entry
keeps reading `attempt-<final>/sessions` (the latest failed execution's
transcript); the `.sessions.<k>` siblings are older preserved generations. The
assembler re-checks the profile pairing (`Sessions` without a profile
`session_dir` is a refusal) and the container path stays inside `HOME` by
config validation, re-checked as defense in depth. The engine's job ends at
durable, step-addressed transcripts under the run directory; reading them is
user-side work.

## Interactive re-entry as a session

Re-entry (`Reentry.Reenter`) keeps its shape — same image, bindings, inputs
— but mounts nothing from the host. It builds a frame stream
(`handoffStream`, over `contract.FrameWriter.Tree`: regular files as `file`
with their mtimes, symlinks as `link` under `sessions/` only, under a given
first segment) from the
failed attempt's handoff directory as
`handoff/<rel>` and, when the failed attempt saved a session
(`boxes/<step>/attempt-<final>/sessions/` exists and is non-empty), the
profile defines `resume_argv`, and the operator did not force `--shell`, from
that directory as `sessions/<rel>`. The stream is what the operator's box
receives: the archived record stays untouched on the host while the
ephemeral session diverges freely inside the container. The spec carries a
`--tmpfs /faber/handoff` beside the engine tmpfs, `FABER_SESSIONS_DIR` when
a session rides along, the bindings' `Attach` (the held box gets its agent
the same way a sequenced one does), and the entry `faber-box hold`; the session is
`{Entry: resume_argv | shell, Env: {HOME: /home/box}, User: <uid>:<gid> from
the spec's FABER_RUN_UID/GID, Workdir: /workspace}` — `HOME` pinned because
the exec, not the sequencer, is the operator's entry, and no preamble exports
it. The seam is `InteractiveRunner.RunInteractive(ctx, InteractiveSession{Spec,
Input, Session})`; the wiring hands it to infra's `ContainerRunner.Enter`
(hold, receive, attach, kill). The salted interactive dir holds only the
binding scratch and is removed when the session exits. Fallback to the bare
shell: `--shell`, no saved session, or no `resume_argv`. Re-entry scope
stays failed-only.

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
and sets `ContainerSpec.Output` to it; the container runner tees the
container's stderr (the human channel: the box's log, hook and agent output,
docker's own messages) into it beside the bounded tail and the debug
forwarding. `ContainerSpec.Stdout` is the demuxer above.
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
