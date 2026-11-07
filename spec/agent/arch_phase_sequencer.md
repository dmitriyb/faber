# PhaseSequencer — the in-container entry program

## What it is

The single engine-owned process a box runs: `faber-box`, a statically linked
binary built from the same Go module as the host engine, baked into every
template image at `/faber/bin/faber-box` by the image builder (its bytes fold into
the image tag, so a faber upgrade means `faber build`) and set as the container
command by infra's runner. It is deliberately not configurable: every agent step, whatever its
template, executes the same fixed internal sequence. There is no in-container
DAG. Anything that needs ordering across a signing identity is a separate
host-side step by design; everything below is what happens *within* one
identity's turn.

## The fixed order

Phases 1–6 are deterministic engine code (environment setup), 7–9 are the
user-filled phases, 10 is the container-boundary phase. Each phase either
completes or the box fail-stops — no phase ever runs after a failed one.

0. **Privileged preamble (drop to the run user).** The container starts as root
   because the writable mounts arrive root-owned: a fresh `/workspace` volume
   and the tmpfs writables (`/faber/result`, `/faber/bundle`, `/tmp`, `HOME`)
   are created by the daemon owned by root. Before any other phase, `faber-box`
   `chown`s exactly those writable mounts to `FABER_RUN_UID:FABER_RUN_GID`
   (the uid:gid the box runs as inside the container; nothing host-owned is
   shared with it, so the value is a container-namespace id and nothing
   more), then drops privileges —
   gives the run user a passwd and a group entry (`/etc/passwd`,
   `/etc/group`, appended or created; a present uid or gid is left alone) —
   a toolset image carries no users, the uid is a run-time value, and
   OpenSSH refuses to act for a uid it cannot look up ("No user exists for
   uid"), which would fail the clone and the signing phase; the write needs
   a writable `/etc`, which every image has today, and a read-only rootfs
   would have to mount a tmpfs there or the preamble fail-stops naming it —
   and drops
   privileges: `setgroups` to the run group alone, `setgid`, `setuid` — so
   every phase below, and the untrusted agent in particular, runs non-root.
   No supplementary group is needed for the agent: the box's agent socket is
   its own, created inside the container by the relay (below) and chowned to
   the run user, so there is no host-owned socket to be admitted to. The
   `/run/secrets` tmpfs (present only in file mode, mounted root-owned by the
   binding's `--tmpfs` flag) is a **gated add to the chown set**: chowned only
   when it exists, so the dropped run user can write its `0600` secret files in
   phase 3. Absent file mode there is no such mount and the chown set is
   unchanged.
   File-mode secrets therefore presume the root-entry-then-drop path
   (or the box-lifecycle harness's writable `/run/secrets` override): the gated
   chown is what makes the root-owned tmpfs writable by the run user, so a
   genuinely non-root / no-drop box — where the preamble is a no-op and chowns
   nothing — must already own `/run/secrets` itself for phase 3 to write there.
   There is no sessions bind to account for: with session capture on the
   harness writes its transcripts under the `HOME` tmpfs as it always did, and
   the box ships them at the end (see the frame channel below).
   The store paths
   of the toolset remain root-owned and read-only throughout. If the box is
   already non-root (a gateless local invocation with no root to drop), the
   preamble is a no-op. This is the same root-entry-then-drop shape the network
   binding uses; it is the *only* moment `faber-box` holds privilege.

1. **Skills link.** When `FABER_SKILLS_LINK` is set, the box creates the
   agent-specific symlink `$HOME/<link> → /faber/skills` (`contract.ContainerSkillsDir`),
   `os.MkdirAll`-ing the link's parent under `$HOME` first. `HOME` is read from
   the **box environment** (`b.Environ`), not the process env: the preamble sets
   `HOME=/home/box` via `b.setEnv`, which mutates only `b.Environ` (the
   no-global-state policy), so on the production drop path the box `HOME` is that
   writable tmpfs even though the process `HOME` still reads `/root` — and the
   agent and hooks below resolve `HOME` from `b.Environ` too, so the link must
   land there. On the no-drop local path (a non-root or `RunUID==0` invocation,
   e.g. the box-lifecycle harness) `b.Environ`'s `HOME` is whatever the
   caller/harness put there, which is why that harness points the box `HOME` at a
   scratch dir rather than clobbering the real one. This is the one agent-specific
   translation in the box, and it is Go,
   not a shell command, because the image is shell-less. `link` is opaque config
   (a claude box passes `.claude/skills`); faber never hardcodes it. The mount at
   `/faber/skills` is read-only, so the link points a read-only tree into the
   place *this* agent looks for skill definitions. Absent `FABER_SKILLS_LINK` (no
   `skills` leg on the template) the phase is a no-op — current behavior. See the
   Skills leg subsection below.
2. **Env contract.** The box's inputs arrive as environment: `FABER_SKILL`,
   `FABER_IDENTITY`, `FABER_RESULT_DIR` (the `/faber/result` tmpfs),
   `FABER_BUNDLE_DIR`, `FABER_OUTPUT_SCHEMA`, the optional `FABER_SKILLS_LINK`,
   the optional `FABER_SECRETS_STDIN` (set to `1` when file-mode tokens ride
   stdin), and one `FABER_INPUT_<SLOT>` per bound input slot. Every required
   slot of the template must be present and non-empty; a violation aborts
   immediately with reason `env-contract`.
3. **Delegated secrets.** Two steps, the first gated. When
   `FABER_SECRETS_STDIN=1` (file mode delivered its tokens over stdin), the box
   reads **all of stdin** to EOF, JSON-decodes the single object
   `{"<name>":"<base64(token)>", ...}`, base64-decodes each value, and writes
   `/run/secrets/<name>` at `0600` into the container tmpfs. This runs at the
   start of phase 3 (phases 1 skills and 2 env run between the preamble drop and
   here); nothing earlier touches stdin
   and the headless agent never reads it, so faber closing stdin gives a clean
   EOF. Then — unchanged, and whether or not the stdin step ran — each regular
   file under `/run/secrets/` is exported into the *process* environment under
   its uppercased basename. Secrets never cross the docker boundary as `-e` env
   — that rule is the host-side binding's; this phase materializes the
   stdin-delivered handle and makes it reachable by hooks and agent. Only the
   secret's *origin* changed (container stdin, not a host bind mount); the
   `0600`-file-plus-env-export contract is identical.
4. **Host-key policy.** Pinned key material (`FABER_HOST_KEY`) is written to a
   known-hosts file with `StrictHostKeyChecking=yes` (fail closed); else an
   explicit TOFU opt-in (`FABER_HOST_KEY_TOFU=1`) selects `accept-new`
   (sandbox only); else the box aborts before any network use.
5. **Clone.** `repo` is the one reserved slot name the engine interprets.
   The HOST splices it: the security module's remote binding shape-checks the
   resolved repo value and exports the complete clone URL as
   `FABER_REMOTE_URL`; the sequencer clones that URL as-is — it never
   assembles a URL — into `/workspace/<repo>`, the working directory for
   every later phase. When absent (a gateless step), later
   phases run in a scratch directory and phases 5–6 are skipped. When the
   host set `SSH_AUTH_SOCK`, the phase first — gateless or not, so no hook
   runs before the relay has set up either — waits until that socket
   accepts a connection from the run user: the host attaches the relay
   beside the container a moment after it starts, and the relay gives the
   socket to the run user before serving. Bounded (30 s); past the bound the
   phase fails with the signing reason naming the missing relay, so a host
   that never attached is a recorded failure and not a hang.

   *Shared object cache (the large-repo seam).* A cross-container git worktree
   is rejected: a shared `.git` is mutable state shared between untrusted
   sandboxes, which breaks the step-as-lambda isolation and git's own
   single-writer assumption. Instead, when a read-only git object cache is bound
   as a pre-warmed `Volumes` entry (`FABER_GIT_CACHE` points at its mount), the
   clone adds `--reference-if-able <cache>`: each box keeps its own isolated
   `.git` and working tree but borrows objects (the bulk — history) from the
   shared cache via alternates, so N parallel boxes pay N× only for the working
   checkout, never N× for history. The cache is never written by the box.
6. **Signing config.** The public key is read from the box's agent socket
   (`ssh-add -L` over `SSH_AUTH_SOCK`, which the host's proxy serves through
   the relay); exactly one key must be listed — zero
   or several is an identity-binding violation and aborts. Then:
   `git config gpg.format ssh`, `user.signingkey key::<pub>`,
   `commit.gpgsign true`; committer name from `FABER_GIT_NAME` or the default
   `faber-<identity>`; committer email from `FABER_GIT_EMAIL`. Both are box
   CONTRACT env, injected per container by the host from the role registry
   (the role's `git_name`/`git_email`) — never inherited from any ambient
   environment. The email is
   **required** on a gated step — an empty email aborts the signing phase
   naming the variable. There is no synthetic email fallback: a made-up
   address signs fine and passes a key-based gate, but a forge that ties
   signature verification to a registered account email can never verify it —
   a silent misconfiguration this abort surfaces at the first run. The same
   key signs commits and authenticates SSH — one fingerprint, one role;
   enforcement of what that fingerprint may do belongs to the user's gate
   service, never to the box.
7. **Context hook.** First user-filled phase, under the PreludeHooks contract.
8. **Prelude hook.** Second user-filled phase, same contract. After both, the
   context bundle must exist in the bundle directory or the step aborts —
   the agent never starts on a missing bundle.
9. **Agent.** Delegated to AgentInvoker: one headless skill invocation, the
   only nondeterministic phase in the box. On an agent-skippable template
   (`FABER_AGENT_OPTIONAL=1`) whose prelude wrote `FABER_SKIP_AGENT=1` into
   `bundle.env`, the invocation is skipped as a logged no-op — the phase
   stays in the order, the postlude and result phases run unchanged, and the
   attempt record marks `agent_skipped` (see AgentInvoker).
10. **Postlude hook.** Third user-filled phase, same PreludeHooks contract,
   running AFTER the agent and BEFORE result extraction. Declared per
   template (`hooks.postlude`), optional — absent skips the phase. Its job
   is deterministic post-agent work: act on and validate the agent's
   artifacts (turning "the agent's last step silently didn't happen" into
   exact diagnostics — artifact absent vs malformed vs empty-but-legal),
   and host post-steps the agent should not even know exist. Fail-stop with
   phase name `postlude`.
11. **Result.** Delegated to ResultExtractor: extraction, schema validation,
   declared side-effect verification, and emission of the attempt record.

## The skills leg

A template names a `skill` (the leading `/<skill>` of the agent prompt), but the
skill *definition* — a claude `SKILL.md` tree, for instance — is not carried by
the hooks (those deliver the per-run context bundle). The skills leg fills that
gap with two engine-owned pieces and nothing agent-specific baked into faber:

- **A read-only tree in the image.** When the template declares a skills leg,
  the image builder bakes the resolved tree at the fixed neutral path
  `/faber/skills` (`contract.ContainerSkillsDir`) — a sibling of `/faber/hooks`,
  and like it a per-template, static, read-only capability: root-owned image
  content, deliberately *not* under the box-writable per-run `/faber/bundle`
  tmpfs. Its files' bytes fold into the image tag, so a skill edit means
  `faber build`.
- **A single symlink.** The host passes the config `link` to the box as
  `FABER_SKILLS_LINK`; the skills-link phase creates `$HOME/<link> → /faber/skills`.
  This is the whole agent-specific translation, and it is driven entirely by
  config: faber never learns `.claude`. A claude box discovers skills natively at
  `$HOME/.claude/skills/<name>/SKILL.md` (no `CLAUDE_CONFIG_DIR` needed: on the
  production drop path `$HOME` is the writable `/home/box` tmpfs the preamble set,
  and on a no-drop local run it is the ambient/overridden `HOME`), and claude
  follows the symlink into the read-only tree — empirically confirmed. A different
  agent just sets a different
  `link`; the mount path stays neutral.

Absent a `skills` leg, no mount and no `FABER_SKILLS_LINK` are emitted and the
symlink phase is a no-op, so hook-only and skill-less templates are unchanged.

## The frame channel

The box's standard output belongs to the engine. `faber-box` opens a frame
writer on it at start (the format and the reader are the contract package's,
`impl_hook_result_contracts.md`, "The frame channel") and nothing else in the
container writes there: the box's own log is on stderr, and the runner's
`Stream` sends a child's stdout (hooks, the agent) to the box's stderr beside
the child's own stderr. The container's stderr is therefore the whole human
channel, live in the host's `output.log` as before; the container's stdout is
frames and nothing else.

Two things travel on it. While the user phases run, a tail over
`$FABER_RESULT_DIR/hooks/events.jsonl` sends every complete new line as an
`event` frame, so the hooks' account of the attempt reaches the operator's
view as it is written. When `Main` returns — after the result phase, after a
fail-stop, after a halt — the binary ships the result directory: every file
under `/faber/result` as `result/<rel>`, `result.json` last; with
`FABER_SESSIONS_DIR` set, every entry under that directory as
`sessions/<rel>` with its mtime; then `end`. The host demultiplexes the
stream into the attempt directory, so the records land exactly where they
did when the directory was a bind, without the box ever touching a host
path. A container killed mid-step has sent its events and nothing else; the
host then has no record and synthesizes `box-vanished`, as it always did.

## The halt request

A user phase may ask the run to stop for an operator without failing: a hook
(context, prelude, postlude) or the agent's skill writes `halt.json`
(`{"reason": "<machine word>", "detail": "<human text>"}`) into
`$FABER_RESULT_DIR`. After each user-filled phase that exits 0, the sequencer
checks for the file; when present it stops the phase order there and emits a
`halted` attempt record (the failure module's third status) carrying the
halter's reason, detail, and the phase that requested it — later phases never
run: a prelude halt skips the agent and the postlude, an agent halt skips the
postlude, and result extraction is skipped in every case (a halting step
threads nothing, so its output contract is moot; dependents are skipped by
the host). A halting prelude is likewise exempt from the bundle
postcondition — no agent will run, so no prompt bundle is owed.

Deliberately not an exit code: exit status keeps meaning pass/fail (`set -e`
hooks would mis-handle a third meaning), so the halt is honored only from an
orderly exit — a phase that exits nonzero after writing `halt.json` is a
failure through the ordinary fail-stop funnel, and the halt file is ignored.
A `halt.json` that exists but does not parse, or carries no reason, fails the
step loudly (reason `halt-invalid`) rather than guessing. The engine never
interprets the halt reason; it is the user's vocabulary, surfaced verbatim in
the run report.

## Fail-stop and the handoff record

A failed phase converges on one path: the sequencer writes a structured
handoff record (`handoff.json`: phase, reason, hook exit code, stderr tail,
secret-free inputs, workdir) plus a snapshot of the bundle directory into
`FABER_RESULT_DIR`, writes the failed attempt record (`result.json`, error
carrying the handoff pointer), ships the directory over the frame channel
and exits nonzero. The handoff is shipped with everything else precisely so
that container removal cannot lose it; the interactive recovery mode
reconstructs the box from it.

## The three other entries

The same binary has three small entries, none of which runs a phase. Two
belong to the interactive recovery mode. `faber-box hold` waits for a termination
signal and exits: it is the entry of a reconstructed box that the operator
enters through `docker exec`, so the container exists without a shell of its
own. `faber-box receive` reads frames from its standard input and writes
them into the container: `handoff/<rel>` under `/faber/handoff` (a tmpfs of
the reconstructed box) and `sessions/<rel>` under `FABER_SESSIONS_DIR`,
refusing any other first segment and any link outside `sessions/`, and
resolving every write inside the tree it belongs to, then does for the run
user what a held box's missing preamble would have: its passwd and group
entry, and the chown of both trees and of the writable mounts left
root-owned (the workspace, `/tmp`, `HOME`, the path down to the sessions
directory). It runs as root through `docker exec` before the operator's own
exec drops to the run user.

`faber-box relay` is the box's end of the identity binding: the host starts
it through `docker exec -i` the moment the container runs (a sequenced box
and a held box alike). It listens on `SSH_AUTH_SOCK` (`/faber/agent/agent.sock`,
a tmpfs the binding mounts), chowns the directory and the socket to
`FABER_RUN_UID:FABER_RUN_GID` from the container's own environment, drops to
that user, and pumps: for every client connection it reads one agent message
(4-byte big-endian length, at most 256 KiB, then the body), writes it to its
stdout, reads one reply from its stdin, and writes it to the client — one
exchange at a time across all connections, so the single stream to the host
stays request–reply aligned for the host's agent server. A client sending a
malformed length is disconnected alone; EOF on stdin ends the relay; the
exec ends with the container. Nothing but length-checked bytes crosses it,
so no key material enters the box through it either.

## What it never does

The sequencer holds no resolver and fetches no secret (the host delegated
handles before the container existed); it never pushes (that is the agent's
work, and the gateway's to accept); it applies no policy (verdicts, role
rules, and content checks are the user's gate service's); and it never
retries (a step is atomic — the host's failure policy re-runs the whole box).

Requirements implemented: Fixed box phase order.
