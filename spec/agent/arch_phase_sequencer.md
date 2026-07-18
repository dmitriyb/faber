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

Phases 1–6 are deterministic engine code (environment setup), 7–9 are the
user-filled phases, 10 is the container-boundary phase. Each phase either
completes or the box fail-stops — no phase ever runs after a failed one.

0. **Privileged preamble (drop to the run user).** The container starts as root
   because the writable mounts arrive root-owned: a fresh `/workspace` volume
   and the tmpfs writables (`/faber/bundle`, `/tmp`, `HOME`) are created by the
   daemon owned by root. Before any other phase, `faber-box` `chown`s exactly
   those writable mounts to `FABER_RUN_UID:FABER_RUN_GID` (the host user's
   uid:gid, so result-bind files stay host-owned), then drops privileges —
   `setgroups` to the single run group, `setgid`, `setuid` — so every phase
   below, and the untrusted agent in particular, runs non-root. The
   `/run/secrets` tmpfs (present only in file mode, mounted root-owned by the
   binding's `--tmpfs` flag) is a **gated add to the chown set**: chowned only
   when it exists, so the dropped run user can write its `0600` secret files in
   phase 3. Absent file mode there is no such mount and the chown set is
   unchanged. File-mode secrets therefore presume the root-entry-then-drop path
   (or the box-lifecycle harness's writable `/run/secrets` override): the gated
   chown is what makes the root-owned tmpfs writable by the run user, so a
   genuinely non-root / no-drop box — where the preamble is a no-op and chowns
   nothing — must already own `/run/secrets` itself for phase 3 to write there.
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
   `FABER_IDENTITY`, `FABER_RESULT_DIR` (a host-mounted directory),
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
5. **Clone.** `repo` is the one reserved slot name the engine interprets: when
   bound, the sequencer clones `<FABER_REMOTE_URL>/<repo>.git` — the gateway,
   the box's only reachable remote — into `/workspace/<repo>`, the working
   directory for every later phase. When absent (a gateless step), later
   phases run in a scratch directory and phases 5–6 are skipped.

   *Shared object cache (the large-repo seam).* A cross-container git worktree
   is rejected: a shared `.git` is mutable state shared between untrusted
   sandboxes, which breaks the step-as-lambda isolation and git's own
   single-writer assumption. Instead, when a read-only git object cache is bound
   as a pre-warmed `Volumes` entry (`FABER_GIT_CACHE` points at its mount), the
   clone adds `--reference-if-able <cache>`: each box keeps its own isolated
   `.git` and working tree but borrows objects (the bulk — history) from the
   shared cache via alternates, so N parallel boxes pay N× only for the working
   checkout, never N× for history. The cache is never written by the box.
6. **Signing config.** The public key is read from the forwarded agent socket
   (`ssh-add -L` over `SSH_AUTH_SOCK`); exactly one key must be listed — zero
   or several is an identity-binding violation and aborts. Then:
   `git config gpg.format ssh`, `user.signingkey key::<pub>`,
   `commit.gpgsign true`; committer name/email from `FABER_GIT_NAME`/
   `FABER_GIT_EMAIL` or the defaults `faber-<identity>` /
   `faber-<identity>@box.invalid`. The same key signs commits and
   authenticates SSH — one fingerprint, one role; enforcement of what that
   fingerprint may do belongs to the user's gate service, never to the box.
7. **Context hook.** First user-filled phase, under the PreludeHooks contract.
8. **Prelude hook.** Second user-filled phase, same contract. After both, the
   context bundle must exist in the bundle directory or the step aborts —
   the agent never starts on a missing bundle.
9. **Agent.** Delegated to AgentInvoker: one headless skill invocation, the
   only nondeterministic phase in the box.
10. **Result.** Delegated to ResultExtractor: extraction, schema validation,
   declared side-effect verification, and emission of the attempt record.

## The skills leg

A template names a `skill` (the leading `/<skill>` of the agent prompt), but the
skill *definition* — a claude `SKILL.md` tree, for instance — is neither baked
into the image (the image is a pure function of the toolset) nor carried by the
hooks (those deliver the per-run context bundle). The skills leg fills that gap
with two engine-owned pieces and nothing agent-specific baked into faber:

- **A read-only mount.** When the template declares `skills: {dir, link}`, faber
  bind-mounts the host `dir` read-only at the fixed neutral path
  `/faber/skills` (`contract.ContainerSkillsDir`) — a sibling of `/faber/hooks`,
  and like it a per-template, static, read-only capability, deliberately *not*
  nested under the box-writable per-run `/faber/bundle` tmpfs.
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
