# Change Proposal: `faber check` — a workflow's hooks run against a throwaway remote, no network, no secrets, no agent

## Context

The hook events contract gave the operator a view of what hooks do during a
run. It cannot say whether a hook will work before a run: a hook is an
opaque script whose contract is its environment going in and three files
coming out (`CONTEXT.md`, `output.json`, `halt.json`), and nothing short of
running it can check that. Running it for real means a real remote, real
credentials and a real agent — side effects the operator does not want from
a check, and a reason the hooks are tested today by a full run against a
sandbox repository.

Faber owns everything a hook can touch from inside a box: the remote (one
pinned URL), the network (one named docker network or none), the identity
(one key in an ephemeral agent), the credentials (handles the broker
mints), and the agent phase. Each is a binding or an env variable the host
sets. Substituting all of them at once gives a run that executes the
workflow's hooks for real, in their real images, with the real phase order,
and can reach nothing: a bare repository under a temporary directory, bind
mounted, as the only remote (the box already clones and pushes to a
local-path remote — "non-ssh remotes need no policy"); `--network none`; a
key generated for the check and discarded with it; no credential broker; the
agent phase a logged no-op. A hook that calls the gate, the forge or an API
fails at that call, and the report names the hook, the step and the phase —
which is exactly the answer "does this hook need something I have not given
it" deserves.

## Proposed change

- **Command (config module).** `faber check <workflow> --param k=v… [--keep]`
  with the common and progress flags. The same entry as `run` (`runEntry`,
  `CheckRunParams`), then `Executor.Execute` with `RunOptions.Check = true`.
  The journal lives under a temporary directory the wiring creates for the
  check and removes afterwards; `--keep` keeps it and prints its path, so a
  failed hook's handoff and events can be read. The run's exit codes apply:
  0 settled with nothing failed, 1 a step failed (a hook, a bundle, an
  output contract), 3 halted, 4 paused, 2 usage. `check` is a run, not a
  view: it works off a terminal and with `--progress json`.
- **The sandbox (security module, wired in cmd).** A check binding set,
  `NewCheckBindingSet(remoteDir, keyPath, identity, logger)`, composed in the
  fixed order with substitutes for three members and the fourth absent:
  - network: `--network none`, whatever the config's network section says.
    The companion services go with it, by intent.
  - remote: for a repo-bound step, a bare repository at
    `<remoteDir>/<repo>.git` cloned on first use from the working directory's
    repository (`git clone --bare`); bind mounted read-write at
    `/faber/remote` with `FABER_REMOTE_URL=/faber/remote/<repo>.git`. No
    host-key material: the box's host-key policy only applies to ssh
    remotes. A repo-bound step in a working directory that is not a git
    repository fails the step before launch, saying so. Pushes land in the
    bare repository; nothing else ever sees them.
  - identity: the real identity binding, with every role resolved to one
    key generated for the check (`ssh-keygen -t ed25519`, no passphrase,
    under the temporary directory) — path-form, so no registry and no
    locator, and the operator's own keys are never touched. Commits are
    signed with it through the same proxy and queue as a run.
  - credentials: no broker. No resolver call, no handle, no secret.
  Every role's committer identity is the explicit check identity
  (`faber-check`, `faber-check@localhost`): not a fallback — there is no
  forge to verify against — and recorded as such in the spec.
- **The box (agent module).** One new env variable, `FABER_CHECK=1`
  (`contract.EnvCheck`), set by the host for every box of a check run and
  tolerated absent, so the contract version does not move. With it: the
  agent phase is a logged no-op regardless of `agent_optional`; the result
  phase, when no hook wrote `output.json`, fills the declared outputs with
  placeholders — the type's zero value, an enum's first value — so the step
  settles ok, downstream steps bind, and their hooks run too; the record
  carries `check: true` beside `agent_skipped`. A hook that did write
  `output.json` is held to the schema as always. Side-effect verification
  (`BRANCH` against the remote) still runs, against the bare repository.
  Hooks may read `FABER_CHECK` to skip what they know cannot work; nothing
  requires them to.
- **The run (pipeline module).** `AgentBoxes.Check` carries the mode to
  `BoxSpec`, which emits the variable; nothing else in the executor changes.
  The preflight is the real one: images must be built, hook files must be
  there.
- **The report.** After the ordinary run report, one line per step that
  ran: `implement   ok   hooks context prelude   events 3` from the
  template's declared slots and the attempt's events file, or
  `wait-checks   FAILED   prelude   hook-failed: hook prelude exited 1`
  from the handoff's phase and reason. The same lines are in the JSON report
  under `check`.
- **Spec and docs.** security: a requirement "Check sandbox" under the
  BindingSet component; `arch_binding_set.md` and `impl_bindings.md` gain
  the check composition and the three substitutes' argv. config: the
  subcommand in `arch_cli.md` and `impl_cli.md`, the CLI requirement's
  description. agent: `EnvCheck` and the box's two behaviours in
  `impl_phase_sequencing.md` and `impl_hook_result_contracts.md`, a
  requirement "Check run" under PhaseSequencer and ResultExtractor.
  `docs/commands.md` (the row), `docs/configuration.md` (the hook events
  section: what check gives a hook and what it withholds; `FABER_CHECK`).
- **Out of scope.** Faking the gate or any companion service; a check that
  reaches the network; running a check from the live view; `--interactive`
  into a check's box; keeping check journals in the run store.

## Impact expectation

- **config**: `cmd_check.go`, `RunOptions.Check` and `Keep`; a CLI test
  that the command reaches the executor with the flag.
- **security**: `check.go` — the no-network, check-remote and
  check-identity bindings and `NewCheckBindingSet`; unit tests over the
  argv with a fake agent controller and a temp repository.
- **agent**: `contract.EnvCheck`, the result's `Check` field,
  `contract.PlaceholderPayload(schema)`; box tests: agent skipped under
  check, placeholders pass the schema, a hook-written output still
  validated.
- **pipeline**: `AgentBoxes.Check` into `BoxSpec`; the check lines in the
  report and JSON.
- **cmd**: the wiring — temporary directory, key generation, the check
  binding set, the check committer identities, the store under the
  temporary directory, `--keep`.
- **tests on a real machine**: `faber check brief` on `examples/quickstart`
  in the realinfra suite (docker is needed; this environment has none, so
  the acceptance run is the host's), and `faber check bead --param bead=…`
  against the operator's workflows, expecting the implement hooks to pass
  and the first gate call to fail by name.
