# PreludeHooks — the deterministic hook contract

(One contract, three user slots: `context` and `prelude` before the agent,
`postlude` after it — everything below applies to all three unless a slot is
named explicitly.)

## What it is

`context`, `prelude`, and `postlude` are opaque user executables declared per
template (`hooks.context`, `hooks.prelude`, `hooks.postlude`), bind-mounted
read-only at `/faber/hooks/context`, `/faber/hooks/prelude`, and
`/faber/hooks/postlude` as part of the box run contract. The pre-agent pair
runs *before* the agent, the postlude *after* it (and before result
extraction); none contains an agent, and none holds credentials beyond the
handles already delegated to the box. Faber never reads their contents — it
enforces only their environment, their exit codes, and (for the pre-agent
pair) their postcondition: the context bundle.

## Inputs: the environment

A hook inherits the box environment as assembled by the setup phases
(env-contract through signing):

- `FABER_INPUT_<SLOT>` for every bound input slot, values stringified per the
  slot's declared type — this is how the step's typed inputs reach user code.
- Engine variables: `FABER_SKILL`, `FABER_IDENTITY`, `FABER_BUNDLE_DIR`,
  `FABER_RESULT_DIR`, `FABER_REMOTE_URL`.
- Delegated handles: `SSH_AUTH_SOCK` (the single-role signing key), the proxy
  environment, and any `/run/secrets/*` exports.
- Working directory: the cloned workspace when `repo` is bound, a scratch
  directory otherwise.

Hooks receive nothing else; in particular there is no argv protocol — the
environment is the whole interface, so the same hook works unchanged whether
invoked by faber or by hand inside an interactive recovery shell.

## Output: the context bundle

`$FABER_BUNDLE_DIR` is a per-container tmpfs (RAM), writable by the run user and
discarded on exit — the bundle is regenerated every run, never persisted. By the
time the prelude exits, it must contain:

- `CONTEXT.md` — the prompt body handed verbatim to the agent. Mandatory.
- `bundle.env` — optional machine-readable sidecar: line-oriented
  `KEY=VALUE`, no quoting or expansion. Values here are opaque to the engine
  with one convention: a `BRANCH=<name>` entry is a *declared side-effect* —
  a postcondition the ResultExtractor verifies against the gateway.
- Any further files (resolved document lists, per-item context directories),
  referenced from `CONTEXT.md` by path.

The bundle is read once, after the prelude: `bundle.env` values (including
the `BRANCH` side-effect declaration) are captured then, so a postlude's
writes into the bundle are snapshotted on failure but never interpreted —
declare side-effects from the prelude only.

The split of labor is convention, not enforcement: `context` gathers and
derives (read-only — resolve the work item, collect the authoritative
documents), `prelude` acts (create the branch, claim the item with a signed
setup commit). The engine enforces only the order and the postcondition.
Templates that declare neither hook still get a uniform agent phase: the
sequencer synthesizes a minimal `CONTEXT.md` enumerating the step's typed
inputs, so a hook-less template (the reference `merge`) is valid.

## Exit semantics

Each hook must exit 0. A nonzero exit — or, for the pre-agent pair, a
missing/empty `CONTEXT.md` after both succeeded — aborts the step through the
fail-stop path: a handoff record naming the phase (`context`, `prelude`, or
`postlude`), the exit code, and a stderr tail, plus a failed attempt record.
A pre-agent failure means the agent never starts; a postlude failure comes
after the agent ran, and its artifacts survive in the mounted result
directory for diagnosis. There is no
partial credit and no retry inside the box; the host's failure policy decides
whether the whole step runs again (with `on_failure` cleanup between
attempts, which is what lets a claiming prelude be re-run safely).

## Determinism expectations

Hooks are expected to be deterministic functions of the step's inputs and the
repo state: the proven pattern is a preflight (clean tree, exactly one key in
the agent, fetch, idempotent domain-store setup) followed by pure derivation.
Faber cannot verify determinism — it verifies the contract — but the
architecture depends on it: everything before the agent must be reproducible
so that a re-run attempt and an interactive reconstruction see the same box.

## Deferred seam: generic context resolvers

First pass, the context hook is a single opaque script, which means rich
context gathering (resolve a work item to its authoritative documents) is
buried in user shell. Reserved: a declared `resolve_context` command contract
mirroring the generate data-source contract — an opaque user command with a
typed JSON output the engine can thread — so context resolution becomes
declarative without faber ever learning the user's tracker or spec tooling.
Nothing in the first-pass bundle convention blocks this: a resolver would
simply become a structured producer of the same bundle.

Requirements implemented: Deterministic prelude contract; Deferred: generic
context resolvers.
