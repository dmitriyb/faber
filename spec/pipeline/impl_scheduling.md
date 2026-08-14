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

The `steps` closure is the host-side `infra.RunSpec` assembly seam: it bridges
the security module's `Assembled` (verbatim argv fragment + `SecretsStdin`) and
the engine mounts/env into one run spec before handing it to
`infra.ContainerRunner`, so it is the single place that owns `RunSpec.Env`
assembly and therefore enforces the credentials pairing — a non-empty
`Assembled.SecretsStdin` is copied into `RunSpec.StdinSecrets` and
`RunSpec.Env[contract.EnvSecretsStdin]="1"` is set in the same step, never one
without the other (see spec/infra/impl_run_argv.md).

## Skills staging (the run-prep seam owns the staged copy)

The config module's desugarer resolves each template's skills leg into
`config.ResolvedSkills` (see spec/config/impl_desugaring.md): a named-skills
template carries `Sources` — an ordered, name-deduped `[(name, dir)…]` of *single*
skill trees — while an inline `{dir,link}` template carries `Root`, a pre-composed
skills-root of `<name>/SKILL.md` subtrees. `ResolvedSkills` is pure data; it is NOT
a mount. Collapsing it to the single `/faber/skills` bind is impure (it reads and
writes host files), so it is done **here**, in the `steps` run-prep seam, exactly
once per box attempt — never in the pure desugarer.

`stageSkills(rs *config.ResolvedSkills, attemptDir string) (hostPath string)`:

- **No skills leg** (`rs == nil`): no `/faber/skills` mount is added — today's
  no-skills behavior, unchanged.
- **Inline `Root`**: return `rs.Root` directly. No staging, no copy — the
  root already has the `<name>/SKILL.md` shape `/faber/skills` expects, so it is
  bind-mounted verbatim. Byte-identical to the single-dir mount faber emits today.
- **Named `Sources`**: build a fresh per-attempt staging directory
  `<attemptDir>/skills` (clearing any leftover tree first, so a crashed re-entry's
  reused session dir cannot fail restaging) and, for each `(name, dir)` in order,
  **copy** the source tree `dir` into `<stage>/<name>` as real files. Return
  `<stage>`. The staged tree must NOT be a symlink farm: `<stage>` is bind-mounted
  read-only into the container, and a symlink's target is a *host* path that is
  not mounted — it would dangle inside the box and `/faber/skills/<name>/SKILL.md`
  would silently vanish, so the source files must be copied through for real.
  Because the mount is `:ro` the box preamble cannot chown it and the non-root run
  user must still read it, so every staged node is world-readable: directories
  `0o755`, files `0o644`. Duplicate names cannot occur (the slice is deduped
  upstream); the copy is deterministic in declared order. The per-file copy is
  **streamed** (`os.Open` + `io.Copy`, not a whole-file `ReadFile`/`WriteFile`) so
  a large skill file never spikes host memory; no size ceiling is imposed because
  the tree is operator-authored. Before joining `name` onto `<stage>`, staging
  re-validates it as a **safe single path segment** (no separator, `..`, absolute
  root, or leading `.` / `~`) and errors out otherwise — belt-and-suspenders with
  config.Validate's name discipline (spec/config/arch_loader.md), so even a
  bypassed validation can never write outside the per-attempt tree.

Whatever `stageSkills` returns is set as the **single** `Mount{Host: hostPath,
Container: contract.ContainerSkillsDir /* /faber/skills */, ReadOnly: true}` on the
`RunSpec` — one mount regardless of how many sources fed it. infra's argv builder
therefore still emits exactly one read-only `/faber/skills` bind and is untouched
(spec/infra/impl_run_argv.md); the agent box still sees one `<name>/SKILL.md` tree
and symlinks `$HOME/<SkillsLink> -> /faber/skills` from `FABER_SKILLS_LINK`,
unchanged. The staging tree is per-attempt and read-only to the container; it is
disposable and lives under the attempt's scratch dir. This is the settled option
(a) from spec/config/arch_desugarer.md — a single staged mount that preserves the
mount contract; the rejected option (b) of one `/faber/skills/<name>` mount per
source does not exist anywhere in the design.

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

## Journal records for skips

Skip settlements append journal records with a null input hash (a skipped
node's inputs may be unresolvable — its producer failed). Null-hash records
are never resume hits; they exist so the report and journal replay see every
terminal state. All three skip flavors (condition, dependency, halt) share
the encoding: a failed-status record carrying the reserved skip reason, the
ancestor in the detail. Ok/failed/halted records carry the real hash and
full result.
