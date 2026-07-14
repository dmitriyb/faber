# Test section: Box lifecycle tests

Integration scenarios spanning PhaseSequencer, PreludeHooks, AgentInvoker, and
ResultExtractor — one box from env contract to attempt record. (Unit tests
with a fake CmdRunner live beside the code; these run the real `faber-box`
binary as a plain process, no docker: scratch directories stand in for the
mounts, and the env contract is set directly.)

## Fixtures

- A stub agent CLI first on `PATH`: records its full argv and environment to
  a file, then writes a configurable `output.json` (or nothing, or garbage)
  and exits with a configurable code.
- Hook scripts: a happy pair (context writes `CONTEXT.md`, prelude appends
  `bundle.env` with `BRANCH=t-1` and touches a marker), a failing prelude
  (exit 3 with stderr), and a bundle-less pair (exit 0, write nothing).
- A local bare git repository standing in for the gateway (path remote, so no
  ssh transport); host-key scenarios use a fake `ssh://` URL and never reach
  the network — the policy decision precedes any connection.
- A throwaway `ssh-agent` loaded with one ephemeral ed25519 key.

## Scenarios

1. **Happy path, fixed order.** Full run with the happy hooks and a stub
   emitting a valid payload: phase order is exactly
   skills/env/secrets/hostkey/clone/signing/context/prelude/agent/result
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
5. **Signing derived from the forwarded agent.** After the run, the clone's
   git config shows `gpg.format ssh`, `commit.gpgsign true`, and
   `user.signingkey key::<pub>` matching `ssh-add -L` of the fixture agent.
   With *two* keys loaded, the box aborts at the signing phase naming the
   count — the one-key-per-box invariant is checked, not assumed.
6. **Host-key policy.** An `ssh://` remote with neither pinned key nor TOFU
   aborts at the hostkey phase before clone; with `FABER_HOST_KEY` set, the
   known-hosts file contains the pinned line and `GIT_SSH_COMMAND` says
   `StrictHostKeyChecking=yes`.
7. **Secrets from stdin, reaching hooks, never the handoff.** With
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
8. **Fallback record.** Agent exits 0 writing no `output.json`: with an
   all-optional output schema, `result.json` is ok, empty payload,
   `fallback: true`; with a required field, it is failed with reason
   `missing-output` — same fixture, schema flipped.
9. **Schema violations collected.** A payload with a wrong-typed field *and*
   an out-of-enum value fails with reason `output-schema` listing both; an
   extra undeclared field alone does not fail but is marked unthreaded.
10. **Unfavorable is not failure.** `{"verdict": "changes"}` against the
    reference review schema yields status ok — conditions, not failure
    semantics, react to the verdict.
11. **Declared side-effect verified.** Prelude declares `BRANCH=t-1`: when the
    stub agent actually pushes the branch to the bare repo, the record is ok;
    when it does not, the record is failed with `side-effect-unverified`
    despite a schema-valid payload.
12. **Agent crash.** Stub exits 17: handoff `{phase: agent, exit_code: 17}`,
    failed record, no extraction of the stale `output.json`.
13. **Host boundary.** `ExtractResult` over the scenario-1 directory returns
    the same record; over a truncated `result.json` returns the synthesized
    `box-vanished` failure; over a record whose payload was hand-edited to
    break the schema, a failed record — the host never threads it.

14. **Skills leg links a read-only tree, under the box `HOME` not the process
    `HOME`.** The phase resolves `HOME` from the box environment (`b.Environ`),
    never `os.Getenv` — the preamble sets `HOME=/home/box` only in `b.Environ`,
    so on the drop path the process `HOME` diverges. The test makes them diverge
    deliberately: it puts a scratch `HOME` in the box env, points the process
    `HOME` (via `t.Setenv`) at a **different** scratch dir, and asserts the
    symlink lands under the **box** `HOME` — with the pre-fix `os.Getenv("HOME")`
    code it would land under the process `HOME` and the test fails. With
    `FABER_SKILLS_LINK=.claude/skills`, the skills phase creates
    `$HOME/.claude/skills` (under the box `HOME`, parent `.claude` dir created) as
    a symlink to the fixed read-only `/faber/skills` mount, and nothing appears
    under the process `HOME`; unset, no `$HOME/.claude` is created and the phase
    is a no-op. The `link` value is honored verbatim — nothing asserts `.claude`
    beyond the fixture's own config.

## Edge cases

- Empty (zero-byte) `CONTEXT.md` counts as missing.
- A malformed `bundle.env` line fails the prelude phase, not the agent phase.
- No `repo` input: clone and signing phases are skipped, hooks run in a
  scratch cwd, and a `BRANCH` declaration without a repo is a contract error.
- `FABER_EFFORT`/`FABER_MAX_BUDGET` unset: the stub's recorded argv contains
  neither flag; set: both appear with the exact values.
