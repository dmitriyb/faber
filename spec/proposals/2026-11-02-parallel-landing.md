# Change Proposal: Parallel landing — generate bound, lanes, serialized signing

## Context

The executor already runs independent nodes in parallel: Kahn readiness,
one goroutine per box, `--max-parallel` as the only bound. The user's epic
workflow is sequential by construction — a pull-loop whose every iteration
orders after the previous one — and is moving to one `generate` over the
epic's task graph with `deps`, so that a task starts the instant its
dependencies land. That change is configuration. What stops it from working
is faber's, in two places found in the user's own workflow comments and in
the identity binding:

1. **One hardware token, N agents.** The identity binding spawns a fresh
   `ssh-agent` per step attempt and loads the role's key into it — role
   isolation by construction. With no-touch SK keys the token itself is the
   serialization point: when two boxes ask it to sign at the same instant
   (both cloning, both committing), the second gets `agent refused
   operation` and dies before its first hook. The workflow's `depends_on`
   on `ci-review` exists only to keep two boxes from ever launching
   together. Any fan-out reproduces the failure.
2. **One main, N landings.** N sibling PRs each try to land; the first
   advances main and the other N-1 go `behind`, each costing a `ci-fix`
   iteration of a land loop capped at 2. With enough siblings a PR goes
   behind more than twice and the loop exhausts on a healthy PR.

A third, smaller gap: `--max-parallel` bounds the whole run. A generate
over 20 ready tasks has no way to say "at most 4 of these at once" without
also bounding everything else.

All three are mechanism: a bound on a fan-out, a bound on a named resource,
a queue in front of a signing device. None teaches faber a domain word.
Portitor needs no change for the in-run case, which is the only case the
user runs.

## Proposed change

- **Generate bound.** `generate:` gains `max_parallel: N` (optional, N >= 1):
  at most N of that generate's instances hold a box slot at once, on top of
  the run-wide `--max-parallel`. The bound travels in the IR's generate
  spec; the scheduler keeps one pool per generate node beside the global
  pool and grants both, in the same deterministic id order, before a launch.
- **Lanes.** A top-level `lanes:` section declares named bounds,
  `lanes: {landing: 1}` — config is data, the bound lives in the file, not
  in a script. A template opts into a lane with `lane: landing`; every box
  of that template then takes one lane slot before launch and releases it at
  settlement or defer. Validation refuses a `lane` that names no declared
  lane and a declared max below 1. A lane is a bound on a user-named
  resource; faber never learns what "landing" means. With `lanes: {landing:
  1}` on the merge template, merges within a run are strictly sequential in
  dependency order while implement, review and fix fan out, so each PR goes
  `behind` at most once.
- **Why-waiting on the stream.** A node that is ready but held — by the
  global pool, a generate pool, a lane, or a defer — is one more thing the
  progress stream can say. The scheduler emits `step-waiting` with the
  reason (`slot 3 of 3`, `lane landing`, `generate tasks 4 of 4`) when a
  ready node is first held, so the renderer and a later live view can show
  `ready  waiting for lane landing` instead of silence.
- **Serialized signing (sign queue).** The identity binding keeps one
  long-lived `ssh-agent` per role per run, spawned and loaded exactly as
  today (`ssh-agent -D -a`, `ssh-add`), holding exactly one key — role
  isolation stays by construction. Per step attempt it serves a private Unix
  socket with `golang.org/x/crypto/ssh/agent`'s `ServeAgent`, backed by a
  proxy agent that forwards to the role's agent through `NewClient`: `List`
  answers only the role's fingerprint, `Sign` passes through one
  process-wide mutex — the queue — and `Add`, `Remove`, `Lock`, `Unlock`
  and extensions are refused. The box still receives only `SSH_AUTH_SOCK`
  to its private socket; nothing inside the container changes, no second
  channel exists, and no key material ever enters faber's process (the agent
  protocol carries challenges and signatures). The per-attempt teardown
  closes the socket; the run's end stops the role agents. With no-touch SK
  keys the mutex is the whole fix: every signature on the machine passes it
  one at a time, and the token never sees two requests at once. `x/crypto`
  joins the dependency list, in the same family as `x/term`.
- **Out of scope.** A merge queue on the gate (needed only for two runs
  against one main, which the user does not do); touch-required keys (a
  queue makes the touches orderly but cannot remove them); the user's
  workflow rewrite to generate with deps, which is the acceptance input for
  all of the above.

## Impact expectation

- **config**: `GenerateDef.MaxParallel`, top-level `Lanes`, `TemplateDef.Lane`;
  desugar carries the bound into `GenSpec` and the lane name into the
  resolved template; validation of both; docs.
- **pipeline**: per-generate pool and lane map beside the global semaphore
  in the scheduler, granted in `drain` before `launch` and released on
  settle/defer; the `step-waiting` event and its rendering; the deferred
  concurrency-control requirement narrowed.
- **security**: the role-scoped agent lifecycle, the per-attempt proxy
  socket and the sign mutex in the identity binding; `go.mod` gains
  `golang.org/x/crypto`.

Delivered as two PRs under this one proposal: config+pipeline (bound,
lanes, waiting) first, security (sign queue) second, so a token-side
regression bisects alone.
