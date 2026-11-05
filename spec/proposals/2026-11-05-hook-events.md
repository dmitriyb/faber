# Change Proposal: Hook events — a per-attempt events file the view reads, and hook files checked at run start

## Context

Every tool a workflow touches — the tracker, the gate, the forge, the spec
tool — is called from inside a box by the user's hooks, and faber knows none
of them by design. That is also why the live view can say nothing about
what a step is doing between its start and its result: `output.log` is the
box's whole stdout, and the typed output arrives only at the end. The
operator's real questions during a run — which PR is this cycle on, did the
push land, are the checks green, why did the hook halt and what should I do
about it — are answered today by the hooks themselves, in a private
convention faber never reads: every hook in the operator's configuration
appends one JSON object per event to `$FABER_RESULT_DIR/hooks/events.jsonl`
(`{ts, hook, event, k=v…}` — `decision pr=17 result=landable mss=clean`,
`push_verify ref=… result=mismatch`, `halt reason=approval-not-boolean pr=17`).

The seam is already there. The result directory is the one host-visible,
per-attempt, hook-writable place the box contract defines, and it already
carries three hook-authored files (`output.json`, `halt.json`, `usage.json`).
Naming a fourth, with a line shape, makes the convention a contract the
view can read and any hook can honour, without faber learning a single
domain word: the view shows events verbatim and understands exactly two
things, that an event named `halt` stops the step, and that a field named
`result` is good when it says `ok` and bad when it says `failed`.

A second, smaller gap surfaced in the same review. A template's hook path
is never checked before a box launches: `faber validate` deliberately does
not check file existence (the config may be authored on another machine —
`spec/config/arch_loader.md`, "What Validate deliberately does not check"),
and the run preflight checks images and the daemon only. A missing hook
file is first noticed by `docker run -v`, which does not refuse a missing
host path; the step then fails mid-run at the hook phase with a reason that
does not name the cause. The loader spec already says where the
check belongs: "run-machine concerns, checked at run start". The live view's
Home preflight stats the same paths today, with its own predicate.

## Proposed change

- **The events file (agent module, contract).** `HooksEventsFile =
  "hooks/events.jsonl"` under the result directory, a sibling of `output.json`
  and `halt.json`. Hooks write it; the box never reads it, never creates it,
  and never fails a step over it — the result directory exists, that is the
  box's whole guarantee. One JSON object per line: `ts` (RFC 3339), `hook`
  (the writer's own name — the script's, since a script does not know which
  slot mounted it), `event` (a non-empty word), and any further fields as
  strings, opaque to faber and shown as `key=value`. A line that is not a
  JSON object, or lacks one of the three, is malformed: the reader returns
  it with its line number and keeps going, it never drops it. The reader
  (`contract.ReadEvents`) bounds each line and the file like every
  container-boundary record and keeps the newest lines when the bound is
  crossed. The reserved vocabulary is deliberately tiny: an event named
  `halt` is the hook's own account of a halt (`reason`, and `next` — the one
  sentence an operator should act on; absent, `halt.json`'s detail stands
  as today), and a field named `result` with the value `ok` or `failed` is
  the status the view colours. Everything else is the hook's business.
  The on_failure hook, which runs host-side, gets the same seam: the hook
  runner exports `FABER_RESULT_DIR` pointing at the failed attempt's result
  directory, so all four slots write one file per attempt. Additive on
  both sides (an old box ignores an unread file, an old hook writes none);
  the contract version does not move.
- **Hook files checked at run start (pipeline module).** The run preflight
  that today proves images and the daemon also proves every hook file a
  reachable template declares, in all four slots: it exists, is a regular
  file, is executable, and starts with `#!`. One aggregated refusal before
  any run state exists, naming the template, the slot and the path as the
  YAML spells it; the fix is in the message. `faber validate` stays
  machine-independent, as its spec says. The predicate is one exported
  function in the config module (`config.CheckHookFile`), and the live
  view's Home preflight and STEP hooks block call that same function, so
  the view and the run can never disagree about a hook.
- **The view reads the events (tui module).** STEP gains a `hooks` block
  under the attempt's facts: one line per event, oldest first, `hook`,
  `event`, then the fields as `key=value`; a `halt` event in the FAIL
  colour with `next` beside it; `result=ok` green, `result=failed` red; a
  malformed line as `malformed line N`. The now strip's `failed at …` gains
  the halt's `next` when the attempt's events carry one. No new data
  source: the file is read beside `output.log`, per attempt, on the same
  tick. The hooks block folds like the preview's blocks and opens itself
  on a `halt` or a `result=failed`.
- **Spec and docs.** The agent module's hook contract leaf gains the events
  file (shape, reserved words, "hooks write, the box never reads"); a
  requirement "Hook events contract" under the prelude-hooks component.
  The pipeline scheduler's run preflight section gains the hook file
  checks; the config loader's "deliberately does not check" list points at
  the preflight as the place that does. The tui screens leaf gains the
  hooks block, the tests leaf a scenario. `docs/configuration.md` gets a
  hooks paragraph with a copy-paste `_emit` shell helper (the operator's
  own, generalised); `docs/commands.md`'s STEP line names the block.
- **Out of scope.** Running hooks outside a run (the sandboxed `faber
  check`, its own proposal); any event vocabulary beyond `halt` and
  `result`; a cycle-row summary drawn from a named output — a name like
  `pr` is a domain word and stays in the hook's events; reading events
  from the progress stream or the report (the file is the view's, the
  journal stays as it is).

## Impact expectation

- **agent**: `contract.HooksEventsFile`, the `Event`/`Malformed` types and
  `ReadEvents` with bounds; unit tests over the operator's real emit shapes;
  the hook contract leaf and a requirement.
- **failure**: `HookInvocation.ResultDir`; `ExecHookRunner` exports
  `FABER_RESULT_DIR`; the step spec carries what the policy needs to name
  an attempt's result directory.
- **pipeline**: the preflight's hook file checks over `collectTemplates`,
  both call sites of the on_failure runner fill the result directory; the
  scheduler leaf's preflight section.
- **config**: `CheckHookFile` (the one predicate); the loader leaf's
  pointer. No validate behaviour change.
- **tui**: the per-attempt events read beside the output tail; the STEP
  hooks block; `next` on the now strip; Home's slot check and the preview's
  hooks block over `CheckHookFile`; goldens re-rendered with a seeded
  events file on the failed run; a scenario.
- **docs**: configuration (the hooks paragraph and helper), commands (the
  STEP line).
