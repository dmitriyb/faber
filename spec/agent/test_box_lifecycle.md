# Test section: Box lifecycle tests

Integration scenarios spanning PhaseSequencer, PreludeHooks, AgentInvoker, and
ResultExtractor — one box from env contract to attempt record. (Unit tests
with a fake CmdRunner live beside the code; these run the real `faber-box`
binary as a plain process, no docker: scratch directories stand in for the
mounts, the env contract is set directly, and the binary's stdout is captured
and demultiplexed by the harness with the contract reader into a scratch
attempt directory — the records every scenario reads are the shipped ones,
never the binary's own result directory.)

## Fixtures

- A stub agent CLI first on `PATH`: records its full argv and environment to
  a file, then writes a configurable `output.json` (or nothing, or garbage)
  and exits with a configurable code.
- Hook scripts: a happy pair (context writes `CONTEXT.md`, prelude appends
  `bundle.env` with `BRANCH=t-1` and touches a marker), a failing prelude
  (exit 3 with stderr), a bundle-less pair (exit 0, write nothing), and a
  postlude (touches a marker after asserting the agent's artifact exists; a
  failing variant exits 5 with stderr).
- A local bare git repository standing in for the gateway (path remote, so no
  ssh transport); host-key scenarios use a fake `ssh://` URL and never reach
  the network — the policy decision precedes any connection.
- A throwaway `ssh-agent` loaded with one ephemeral ed25519 key, pointed
  at by `SSH_AUTH_SOCK` directly: the lifecycle runs the sequencer against a
  socket that already exists, as the relay provides in a real box.

## Scenarios

1. **Happy path, fixed order.** Full run with the happy hooks and a stub
   emitting a valid payload: phase order is exactly
   skills/env/secrets/hostkey/clone/signing/context/prelude/agent/postlude/result
   (postlude, like skills, always appears in the order and is a no-op when
   its hook is undeclared)
   (asserted from the structured log; `skills` is a no-op when the fixture
   declares no skills leg); the stub's recorded prompt is `/<skill>` + blank
   line + the exact `CONTEXT.md` bytes; `result.json` is status ok with the
   validated payload and `attempt` echoing `FABER_ATTEMPT`.
2. **Prelude failure aborts before the agent.** Failing prelude: the agent
   stub was never executed (no argv recording exists); `handoff.json` carries
   `{phase: prelude, exit_code: 3}` and the stderr tail; `result.json` is
   failed with `error.handoff` pointing at it; process exit is nonzero.
3. **Bundle-less prelude.** Hooks exit 0 but no `CONTEXT.md`: same abort shape
   with reason `bundle-missing`; the agent never starts.
4. **Hook-less template.** No hooks declared: a synthesized `CONTEXT.md`
   enumerating the typed inputs reaches the agent; the run completes.
5. **Signing derived from the agent.** After the run, the clone's
   git config shows `gpg.format ssh`, `commit.gpgsign true`, and
   `user.signingkey key::<pub>` matching `ssh-add -L` of the fixture agent.
   With *two* keys loaded, the box aborts at the signing phase naming the
   count — the one-key-per-box invariant is checked, not assumed. With
   no committer email in the contract env, the box aborts at the signing
   phase pointing at the `add-key --git-email` remedy — a gated step never
   invents a committer email.
6. **Postlude runs after the agent, before extraction.** With a postlude
   hook declared: it observes the agent's artifacts (the stub's output file
   exists when it runs — ordering proven, not assumed) and its own marker
   lands before the record is read. A failing postlude aborts with
   `handoff.json` carrying `{phase: postlude}` and the stderr tail — the
   agent already ran, and the record is the failed-attempt shape. A template
   WITHOUT a postlude behaves identically to before the phase existed, except
   the phase-start log line and the timing-map entry the static order adds.
7. **Host-key policy.** An `ssh://` remote with neither pinned key nor TOFU
   aborts at the hostkey phase before clone; with `FABER_HOST_KEY` set, the
   known-hosts file contains the pinned line and `GIT_SSH_COMMAND` says
   `StrictHostKeyChecking=yes`.
8. **Secrets from stdin, reaching hooks, never the handoff.** With
   `FABER_SECRETS_STDIN=1` and stdin fed the single JSON object
   `{"service_token":"<base64(value)>"}`, the secrets phase writes
   `<secrets-dir>/service_token` (the scratch stand-in for `/run/secrets`) at
   mode `0600` with the decoded bytes, then exports it: the hook's dumped
   environment contains `SERVICE_TOKEN=<value>`. `handoff.json` from a forced
   later failure contains the `FABER_INPUT_*` map and no trace of the value,
   and the raw token appears in no log line. A malformed stdin payload (not a
   JSON object, or a non-base64 value) aborts at the secrets phase with reason
   `secrets` before any hook runs. Unset `FABER_SECRETS_STDIN` with a
   pre-placed file in the secrets dir still exports it (the origin-agnostic
   second step), and stdin is left unread.
9. **Fallback record.** Agent exits 0 writing no `output.json`: with an
   all-optional output schema, `result.json` is ok, empty payload,
   `fallback: true`; with a required field, it is failed with reason
   `missing-output` — same fixture, schema flipped.
10. **Schema violations collected.** A payload with a wrong-typed field *and*
   an out-of-enum value fails with reason `output-schema` listing both; an
   extra undeclared field alone does not fail but is marked unthreaded.
11. **Unfavorable is not failure.** `{"verdict": "changes"}` against the
    reference review schema yields status ok — conditions, not failure
    semantics, react to the verdict.
12. **Declared side-effect verified.** Prelude declares `BRANCH=t-1`: when the
    stub agent actually pushes the branch to the bare repo, the record is ok;
    when it does not, the record is failed with `side-effect-unverified`
    despite a schema-valid payload.
13. **Agent crash.** Stub exits 17: handoff `{phase: agent, exit_code: 17}`,
    failed record, no extraction of the stale `output.json`.
14. **Host boundary.** `ExtractResult` over the scenario-1 directory returns
    the same record; over a truncated `result.json` returns the synthesized
    `box-vanished` failure; over a record whose payload was hand-edited to
    break the schema, a failed record — the host never threads it.

15. **Halt request settles the step halted.** A prelude that writes
    `halt.json` (`{"reason":"needs-triage","detail":"…"}`) and exits 0: the
    agent stub and the postlude never ran, `result.json` is
    `{status: halted, halt: {reason: needs-triage, phase: prelude}}`, and the
    box exits 0 (the record, not the exit code, is authoritative). The same
    file written by the agent stub halts after the agent phase — the postlude
    marker is absent. A malformed `halt.json` (not JSON, or no reason) fails
    the step with reason `halt-invalid`; a prelude that writes `halt.json`
    but exits 3 fails with the ordinary `hook-failed` shape — the halt file
    is ignored on a failing phase. `ExtractResult` over the halted directory
    returns the halted record with its payload nil; over a hand-edited
    `{status: halted}` with no halt body it returns a `halt-invalid` failure.
16. **Prelude skips the agent on an opted-in template.** With
    `FABER_AGENT_OPTIONAL=1` and a prelude writing `FABER_SKIP_AGENT=1` into
    `bundle.env` plus `output.json` satisfying the schema: the agent stub
    never ran (no argv recording), the postlude ran, `result.json` is ok with
    `agent_skipped: true`, and no child environment ever carried
    `FABER_SKIP_AGENT`. Without the opt-in env, the same prelude gets a
    logged "ignoring" warning, the agent runs, and the record carries no
    skip marker — the signal is inert, and still never exported. A skip
    value other than `1` fails the prelude phase `bundle-malformed`. A skip
    with required outputs unsatisfied fails `missing-output` with the detail
    naming the skipped agent. `ExtractResult` preserves the marker across
    the host boundary.
17. **Skills leg links a read-only tree, under the box `HOME` not the process
    `HOME`.** The phase resolves `HOME` from the box environment (`b.Environ`),
    never `os.Getenv` — the preamble sets `HOME=/home/box` only in `b.Environ`,
    so on the drop path the process `HOME` diverges. The test makes them diverge
    deliberately: it puts a scratch `HOME` in the box env, points the process
    `HOME` (via `t.Setenv`) at a **different** scratch dir, and asserts the
    symlink lands under the **box** `HOME` — with the pre-fix `os.Getenv("HOME")`
    code it would land under the process `HOME` and the test fails. With
    `FABER_SKILLS_LINK=.claude/skills`, the skills phase creates
    `$HOME/.claude/skills` (under the box `HOME`, parent `.claude` dir created) as
    a symlink to the fixed read-only `/faber/skills` tree (image content), and nothing appears
    under the process `HOME`; unset, no `$HOME/.claude` is created and the phase
    is a no-op. The `link` value is honored verbatim — nothing asserts `.claude`
    beyond the fixture's own config.
18. **Invocation profile expansion.** A byte-for-byte table test pins the
    default profile's prompt and argv to the historical output — prompt
    `/skill\n\nbody[\n\nADDITIONAL INSTRUCTION: …]`, argv `agent-cli -p
    <prompt> --permission-mode bypassPermissions [--model …] [--effort …]
    [--max-budget-usd …]` — so the expander can never drift the default
    dialect. A non-default profile (subcommand, positional prompt via
    `prompt_flag: ""`, `skill_mode: flag`, empty `fixed_flags`, an empty
    `effort_flag` dropping the pair) expands to exactly the profile's shape:
    the skill rides its flag and appears nowhere in the prompt, the effort
    pair is absent though `FABER_EFFORT` is set. Prompt expansion is
    injection-proof: a `{skill}`/`{extra}` literal inside the bundle body
    survives verbatim, never re-expanded. End to end through the sequencer:
    with `FABER_INVOKE_PROFILE` set to a non-default profile the stub's
    recorded argv matches that dialect; with the variable absent the recorded
    argv takes the default dialect's shape — the byte-identity itself is what
    the unit-level table test above pins, on the same code path; with malformed JSON
    (or a profile violating the template rules) the env phase fails the step
    before any phase below runs.

19. **The stdout is frames and nothing else.** On the happy path the
    captured stdout parses as frames end to end with no foreign bytes: the
    hooks' and the stub agent's stdout (both print a marker line) appear in
    the captured stderr, not in stdout; the frames are `result/output.json`,
    `result/result.json` (last among `result/`), then `end`; the demuxed
    `result.json` equals the one the binary wrote into its result directory.
    The same holds for a failing prelude (scenario 2: `result/handoff.json`,
    `result/handoff/bundle/CONTEXT.md`, `result/result.json`, `end`) and for
    a halt (scenario 15). A frame name is never absolute and never carries
    `..` — asserted over every captured name.
20. **Events are streamed while the phases run.** A prelude writes two
    lines into `$FABER_RESULT_DIR/hooks/events.jsonl` and then waits for a
    release file; the harness reads the box's stdout as it streams and
    writes the release only once both lines have arrived as `event` frames
    — so the frames provably left the box while the hook was still running
    (a tail that flushed only at the end would deadlock and fail). The final
    `result/hooks/events.jsonl` file frame carries the lines. A line the
    hook writes without a trailing newline is not sent as an event.
21. **Sessions ship at the end.** With `FABER_SESSIONS_DIR` naming a
    scratch directory holding `a.jsonl` with an old mtime and a `latest`
    symlink to it: the frames carry `sessions/a.jsonl`
    with that mtime and `sessions/latest` as a `link` whose body is the
    target; the demuxed copy has the same mtime and link. Unset, no
    `sessions/` frame is sent.
22. **Receive writes under two roots and chowns.** `faber-box receive`
    fed `handoff/handoff.json`, `handoff/bundle/CONTEXT.md` and
    `sessions/a.jsonl` (with `FABER_SESSIONS_DIR` set) writes them under the
    scratch stand-ins for `/faber/handoff` and the sessions dir with the
    frames' mtimes; fed a name under any other root, or `sessions/` without
    the variable, it exits nonzero naming the frame and writes nothing for
    it; so does a `link` under `handoff/` (links are received under
    `sessions/` only); a stream error (foreign bytes) exits nonzero. Every
    write goes through an `os.Root` of its tree, as the host's demuxer does,
    so a link planted by an earlier frame cannot redirect a later one. (The
    chown — of both trees, of HOME, /workspace and /tmp, and of the path
    from HOME down to the sessions directory, since a held box ran no
    preamble — is asserted in the realinfra hold-and-receive test, where the
    receive runs as root.)
23. **The relay pumps one exchange at a time.** `Relay` on a scratch socket
    (non-root: no chown, no drop) against a strict in-process host over real
    pipes — a gate in front of `ServeAgent` over a keyring that, after
    reading each request off the relay's stdout, peeks for bytes already
    waiting before it has replied — serves four concurrent
    `agent.NewClient`s, each signing its own distinct data ten times: every
    signature verifies against the data its client sent, and the gate never
    finds a second request waiting (with the relay's exchange mutex removed
    it does, and the test fails). A further client that writes a 4-byte length above the bound is
    disconnected while the others keep working; closing the host's side
    ends the relay with no error. With a client connected and idle, closing
    the host's side still ends the relay promptly and the idle client reads
    EOF.
24. **The clone waits for the relay.** With `SSH_AUTH_SOCK` naming a socket
    that starts accepting a moment after the box starts, the clone phase
    proceeds once it does (the fake runner records the clone after the
    listener's creation); with a path that never accepts and the bound
    lowered, the phase fails with reason `signing` naming the relay, before
    any git command runs — on a gateless step too. Without `SSH_AUTH_SOCK`
    nothing is waited for.
25. **The run user exists.** `ensureRunUser` against a scratch `etc`
    creates `passwd` and `group` with the run user's entries (home
    `/home/box`), appends nothing on a second call, appends after an image's
    own entries on a line of their own, and leaves a uid or gid already
    present alone. (That OpenSSH is satisfied by it is the realinfra relay
    round trip's business: `ssh-add -L` as the run user in a held box after
    `receive`.)
26. **Named volumes are chowned.** `FABER_VOLUMES=/cache:/models` parses to
    the two mount points, an empty or absent variable to none; the
    preamble's chown list (asserted on the pure list the preamble hands to
    chown) carries them after the engine mounts.
27. **seed-remote fills the volume once.** Against a scratch remote dir
    and a real local git repository bundled with `git bundle create -
    --all`: `SeedRemote` leaves `<repo>.git` holding the bundle's commits
    and no bundle file behind; a second call with another bundle leaves
    the first repository untouched and exits 0; an empty stdin, a bad repo
    path (`../x`, `/abs`, `a//b`, `a/./b`, empty) and a clone that fails (a
    corrupt bundle) are errors and leave no `<repo>.git`; `acme/widgets`
    lands at `acme/widgets.git` with nothing left beside it. `installBare`
    on its own: a clone renamed onto an occupied repository is kept out and
    reports not-first with no error, onto a fresh target lands and reports
    first, from a missing source errors.

## Edge cases

- Empty (zero-byte) `CONTEXT.md` counts as missing.
- A malformed `bundle.env` line fails the prelude phase, not the agent phase.
- No `repo` input: clone and signing phases are skipped, hooks run in a
  scratch cwd, and a `BRANCH` declaration without a repo is a contract error.
- `FABER_MODEL`/`FABER_EFFORT`/`FABER_MAX_BUDGET` unset: the stub's recorded
  argv contains none of the flags; set: all appear with the exact values.
