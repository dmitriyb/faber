# Change Proposal: outputs over a framed stdout channel — the result directory becomes a tmpfs the box ships out at the end, sessions at step end, re-entry without mounts, contract v2

## Context

After the baked-inputs change the box still shares two host paths it must
write: the result directory (`-v <attempt>/result:/faber/result`, where
`result.json`, `halt.json`, `output.json`, `usage.json`, `handoff.json`, the
bundle snapshot and `hooks/events.jsonl` land) and, with `--sessions`, the
harness's transcript directory (`-v <attempt>/sessions:$HOME/<session_dir>`).
Interactive re-entry adds two more: the failed attempt's handoff directory
read-only at `/faber/handoff` and a copy of the saved transcript at
`$HOME/<session_dir>`. On a docker host with userns-remap the box's uid is a
different host uid (container 1000 is host 101000), so every one of these binds
is unwritable, or unreadable unless world-readable, and the step loses its
record. The host already reads one channel from every container that always
works: the `docker run` client's stdout, which `output.log` is written from.

Settled with the operator: the records travel over that channel, framed, and
`output.log` stays live; `--sessions` transcripts travel the same way at step
end (a container killed mid-step loses its transcript, documented); `docker cp`
is not used; re-entry receives the handoff and the transcript into the
container instead of mounting them; the contract version bumps. The role
agent's socket and the template volumes are the next two proposals.

## Proposed change

- **The frame (agent/contract).** One binary record shape:
  `\x00faber\x00<kind>\x00<name>\x00<mtime>\x00<len>\n` followed by exactly
  `len` bytes. `kind` is one of `event`, `file`, `link`, `end`; `name` is a
  relative slash path with no empty, `.` or `..` segment (the handoff
  pointer's containment rule); `mtime` is unix nanoseconds, `0` where it has
  no meaning; `len` is decimal, at most `MaxRecordBytes` (64 MiB). The header
  starts with a NUL byte so no text line can be mistaken for one. A `Writer`
  and a `Reader` live in the contract package beside the record types; both
  halves link them. The reader hands bytes that arrive outside a frame to a
  foreign-bytes sink instead of failing: the stream stays usable when
  something other than faber-box writes to the container's stdout.
- **The box's stdout is the frame channel, its stderr is the human channel
  (agent/box).** faber-box opens a frame writer on fd 1 at start and nothing
  else in the container writes there: its own slog stays on stderr, and the
  runner's `Stream` sends child stdout (hooks, the agent) to the box's stderr
  beside child stderr, so `output.log` reads as today. `/faber/result` becomes
  a tmpfs the preamble chowns (one more entry in the writable set, replacing
  the sessions components walk, which goes with the bind). During the user
  phases a goroutine tails `$FABER_RESULT_DIR/hooks/events.jsonl` and sends
  every complete new line as an `event` frame, so the live view's events stay
  live. On every exit path of `Main`, fail-stop included, the binary ships the
  result directory: every regular file under it as a `file` frame (symlinks as
  `link`, target in the body), `result.json` last; with `FABER_SESSIONS_DIR`
  set, every entry under `$HOME/<session_dir>` as `sessions/<rel>` with its
  mtime, since harnesses find the latest session by mtime or a pointer link;
  then `end`. A file over the frame bound is skipped with a log line, never
  truncated. The in-box reads of `output.json`, `halt.json` and the halt check
  are unchanged: they read the tmpfs.
- **The host splits the two streams (infra).** `ContainerSpec` gains
  `Stdout io.Writer` for the container's stdout; `Output` keeps receiving the
  container's stderr plus docker's own messages, which is what `output.log`
  and the bounded tail hold. `cliRunner.runStreaming` stops merging the two.
  `DockerClient` gains `Exec(ctx, name, argv, stdin, stdout, stderr) (int,
  error)` and `ExecTerminal(ctx, name, argv, env, user) error` (the operator's
  terminal attached), the primitives re-entry needs below and the relay
  proposal reuses; `docker exec` joins the structured-query adapters with the
  same typed error contract. Nothing else in the argv changes: the result bind
  is replaced by `--tmpfs /faber/result`, the sessions bind disappears, `-i`
  stays the secrets payload's.
- **The demuxer (pipeline).** `RunAttempt` points `Stdout` at a demuxer rooted
  in the attempt directory: `event` frames append to
  `<result>/hooks/events.jsonl` as they arrive; `file` and `link` frames land
  under `<result>/` or `<attempt>/sessions/` (the two roots the name may
  start with; anything else is refused and logged), written to a temp name and
  renamed into place so a truncated frame is an absent file, with the frame's
  mtime applied; foreign bytes are appended to `output.log` with a warning.
  Per-frame and per-attempt byte bounds (`MaxRecordBytes`, and a 1 GiB
  `MaxAttemptStreamBytes` for the whole attempt) close the demuxer on
  violation: later frames are discarded, the violation is kept, and because
  `result.json` is shipped last the attempt extracts as `box-vanished` with
  the violation in its detail. `ExtractResult`, `readUsage`, the on_failure
  hook's `FABER_RESULT_DIR`, `preserveSessions`, `AttemptEvents` and the live
  view read the same host paths as today; only the writer changed. The stdin
  secrets payload is unchanged.
- **Re-entry without mounts (pipeline, infra, cmd).** `Reentry` drops the
  handoff bind, the session copy and `copySessionTree`. It builds a frame
  stream from the host: the handoff directory as `handoff/<rel>` and, when the
  profile resumes sessions, the saved transcript as `sessions/<rel>` (files
  and links, mtimes kept). The interactive runner starts the reconstructed
  box detached with the entry `faber-box hold` (sleeps until killed; the
  image has no shell to rely on), feeds the stream to `docker exec -i <name>
  faber-box receive`, which writes `handoff/` under the `/faber/handoff` tmpfs
  and `sessions/` under `$HOME/<session_dir>`, chowns both to the run user and
  refuses any name outside the two roots; then attaches the operator's
  terminal with `docker exec -it -u <uid>:<gid> -e HOME=/home/box -w
  /workspace <name> <shell | resume_argv>`, and kills the container when the
  terminal exits. `InteractiveRunner` becomes a session: the spec, the input
  stream and the entry. The salted interactive directory on the host holds
  only the binding scratch now.
- **Contract (agent/contract, config).** `ContractVersion = 2`: a v1 image
  writes files into a mount the host no longer provides, so the host refuses a
  v1 record and the box refuses a v1 host, both with "run faber build".
  `FABER_RUN_UID/GID` keep their values; their comment stops claiming
  host-owned files, since nothing host-owned is shared any more. The
  `agent_socket_group` host field and the forwarded socket are untouched here.
- **Out of scope.** The role agent's socket (next proposal), template
  volumes and the check remote (the one after), a live transcript channel.

## Impact expectation

- **agent**: requirements "Structured result extraction" (the record reaches
  the host over the box's stdout, not a mount), "Fixed box phase order" (the
  tmpfs result dir in the preamble's chown set, the events tail, the shipping
  step after the result phase), "Hook events contract" (lines streamed as
  written); components PhaseSequencer, ResultExtractor; leaves
  `arch_result_extractor.md`, `impl_hook_result_contracts.md` (the frame),
  `arch_phase_sequencer.md`, `impl_phase_sequencing.md`,
  `flow_box_execution.md`, `test_box_lifecycle.md` (the lifecycle fixture
  reads the records from the captured stdout frames).
- **infra**: requirements "Container run primitive" (two streams, the tmpfs
  result dir, no sessions bind) and "Typed subprocess interfaces" (`docker
  exec`); components ContainerRunner, ExecAdapters; leaves
  `arch_container_runner.md`, `impl_run_argv.md`, `arch_exec_adapters.md`,
  `impl_exec_adapters.md`, `test_infra.md`.
- **pipeline**: component Scheduler; `impl_scheduling.md` (the demuxer,
  session capture at step end, re-entry as a session), `arch_scheduler.md`,
  `test_pipeline_execution.md`; the realinfra round trip runs over the real
  channel.
- **failure**: requirement "Recovery modes", component RecoveryModes;
  `arch_recovery_modes.md` (interactive: receive, not mount),
  `impl_retry_recovery.md`, `flow_failure_resume.md`,
  `test_failure_semantics.md`.
- **tui**: `impl_views.md` note that the paths the view reads are unchanged
  and the transcripts arrive at step end.
- **docs**: `commands.md` (`--sessions` at step end, the caveat), `deploy.md`,
  `configuration.md` (events streamed as written).
- **cmd**: the terminal runner becomes the hold, receive, attach, kill
  choreography over infra's exec primitives.
