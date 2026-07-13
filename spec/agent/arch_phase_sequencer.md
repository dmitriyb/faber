# PhaseSequencer — the in-container entry program

## What it is

The single engine-owned process a box runs: `faber-box`, a statically linked
binary built from the same Go module as the host engine, bind-mounted read-only
into every container and set as the container command by infra's runner. It is
deliberately not image content — the image stays a pure function of the toolset —
and it is deliberately not configurable: every agent step, whatever its
template, executes the same fixed internal sequence. There is no in-container
DAG. Anything that needs ordering across a signing identity is a separate
host-side step by design; everything below is what happens *within* one
identity's turn.

## The fixed order

Phases 1–5 are deterministic engine code (environment setup), 6–8 are the
user-filled phases, 9 is the container-boundary phase. Each phase either
completes or the box fail-stops — no phase ever runs after a failed one.

0. **Privileged preamble (drop to the run user).** The container starts as root
   because the writable mounts arrive root-owned: a fresh `/workspace` volume
   and the tmpfs writables (`/faber/bundle`, `/tmp`, `HOME`) are created by the
   daemon owned by root. Before any other phase, `faber-box` `chown`s exactly
   those writable mounts to `FABER_RUN_UID:FABER_RUN_GID` (the host user's
   uid:gid, so result-bind files stay host-owned), then drops privileges —
   `setgroups` to the single run group, `setgid`, `setuid` — so every phase
   below, and the untrusted agent in particular, runs non-root. The store paths
   of the toolset remain root-owned and read-only throughout. If the box is
   already non-root (a gateless local invocation with no root to drop), the
   preamble is a no-op. This is the same root-entry-then-drop shape the network
   binding uses; it is the *only* moment `faber-box` holds privilege.

1. **Env contract.** The box's inputs arrive as environment: `FABER_SKILL`,
   `FABER_IDENTITY`, `FABER_RESULT_DIR` (a host-mounted directory),
   `FABER_BUNDLE_DIR`, `FABER_OUTPUT_SCHEMA`, and one `FABER_INPUT_<SLOT>` per
   bound input slot. Every required slot of the template must be present and
   non-empty; a violation aborts immediately with reason `env-contract`.
2. **Delegated secrets.** Each file under `/run/secrets/` (the credential
   binding's degraded file mode) is exported into the *process* environment
   under its uppercased basename. Secrets never cross the docker boundary as
   `-e` env — that rule is the host-side binding's; this phase is the in-box
   convenience that makes the mounted handle reachable by hooks and agent.
3. **Host-key policy.** Pinned key material (`FABER_HOST_KEY`) is written to a
   known-hosts file with `StrictHostKeyChecking=yes` (fail closed); else an
   explicit TOFU opt-in (`FABER_HOST_KEY_TOFU=1`) selects `accept-new`
   (sandbox only); else the box aborts before any network use.
4. **Clone.** `repo` is the one reserved slot name the engine interprets: when
   bound, the sequencer clones `<FABER_REMOTE_URL>/<repo>.git` — the gateway,
   the box's only reachable remote — into `/workspace/<repo>`, the working
   directory for every later phase. When absent (a gateless step), later
   phases run in a scratch directory and phases 4–5 are skipped.

   *Shared object cache (the large-repo seam).* A cross-container git worktree
   is rejected: a shared `.git` is mutable state shared between untrusted
   sandboxes, which breaks the step-as-lambda isolation and git's own
   single-writer assumption. Instead, when a read-only git object cache is bound
   as a pre-warmed `Volumes` entry (`FABER_GIT_CACHE` points at its mount), the
   clone adds `--reference-if-able <cache>`: each box keeps its own isolated
   `.git` and working tree but borrows objects (the bulk — history) from the
   shared cache via alternates, so N parallel boxes pay N× only for the working
   checkout, never N× for history. The cache is never written by the box.
5. **Signing config.** The public key is read from the forwarded agent socket
   (`ssh-add -L` over `SSH_AUTH_SOCK`); exactly one key must be listed — zero
   or several is an identity-binding violation and aborts. Then:
   `git config gpg.format ssh`, `user.signingkey key::<pub>`,
   `commit.gpgsign true`; committer name/email from `FABER_GIT_NAME`/
   `FABER_GIT_EMAIL` or the defaults `faber-<identity>` /
   `faber-<identity>@box.invalid`. The same key signs commits and
   authenticates SSH — one fingerprint, one role; enforcement of what that
   fingerprint may do belongs to the user's gate service, never to the box.
6. **Context hook.** First user-filled phase, under the PreludeHooks contract.
7. **Prelude hook.** Second user-filled phase, same contract. After both, the
   context bundle must exist in the bundle directory or the step aborts —
   the agent never starts on a missing bundle.
8. **Agent.** Delegated to AgentInvoker: one headless skill invocation, the
   only nondeterministic phase in the box.
9. **Result.** Delegated to ResultExtractor: extraction, schema validation,
   declared side-effect verification, and emission of the attempt record.

## Fail-stop and the handoff record

A failed phase converges on one path: the sequencer writes a structured
handoff record (`handoff.json`: phase, reason, hook exit code, stderr tail,
secret-free inputs, workdir) plus a snapshot of the bundle directory into
`FABER_RESULT_DIR`, writes the failed attempt record (`result.json`, error
carrying the handoff pointer), and exits nonzero. The handoff lands in the
*mounted* result directory precisely so that container removal cannot lose it;
the interactive recovery mode reconstructs the box from it.

## What it never does

The sequencer holds no resolver and fetches no secret (the host delegated
handles before the container existed); it never pushes (that is the agent's
work, and the gateway's to accept); it applies no policy (verdicts, role
rules, and content checks are the user's gate service's); and it never
retries (a step is atomic — the host's failure policy re-runs the whole box).

Requirements implemented: Fixed box phase order.
