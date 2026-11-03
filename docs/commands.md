# Commands

| Command | Purpose |
|---|---|
| `faber validate` | Load, desugar, wiring-check every workflow; prove package resolution; `--emit-ir` prints the canonical IR; `--workflow name` narrows |
| `faber build` | Build template images via Nix `dockerTools.buildLayeredImage`; `--template` narrows |
| `faber run <workflow>` | Execute with `--name n` (a human name for the run, stored in the journal header), `--param k=v`, `--budget unit=n`, `--max-parallel n`, `--metering path`, `--sessions` (capture each attempt's harness session transcripts under the run dir), `--report-json path\|-`, `--progress auto\|text\|json\|none`, `--color auto\|always\|never` |
| `faber resume <run-id\|name>` | Re-enter a journaled run; `--fresh` ignores the journal, `--sessions` turns capture on for the remainder, `--interactive <step>` reopens the failed box inside its saved session (when one exists and the profile defines `resume_argv`) or a shell, `--shell` forces the shell. A raised loop `max` is accepted without `--fresh` (see Configuration, loop `expect`) |
| `faber watch [<run-id\|name>]` | Open the live view on this config's runs (and on a run when given): Home lists workflows, image states and runs; a run shows its steps as a loop-aware tree, the workflow as written, or a timeline, with the selected step's facts and its box output. A bare `faber` at a terminal opens it too. Needs a terminal on stdin and stdout; `--color auto\|always\|never` |
| `faber runs` | List journaled runs: id, name, workflow, state (`live`/`paused`/`settled`/`aborted`/`incomplete`), started; `--json` for the same rows machine-readably |
| `faber runs pause <run-id\|name>` | Ask a live run to pause: in-flight steps finish and journal, nothing new dispatches, the run ends `paused` (exit 4) and resumes later with `faber resume` |
| `faber runs prune` | Delete finished, non-live run directories; paused runs are kept (resumable state) unless `--all`, which also removes paused and incomplete non-live runs; live runs are never touched |
| `faber upgrade` | Forward-only update of faber and faber-box to the latest signed release via the embedded `install.sh`; self-replaces both binaries as a unit. Refuses a latest older than installed (rollback anomaly, non-overridable) and refuses while live/unfinished runs exist. `--check`/`--dry-run` (report only; warns about — never blocks on — active runs), `--version vX.Y.Z` (install an exact release, any direction), `--rollback`, `--force` (proceed despite active runs) |
| `faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--git-name n] [--git-email e] [--force]` | Register a role→fingerprint (plus the role's committer identity) in the global identity registry |
| `faber list-keys` | Print the global role→fingerprint registry |
| `faber version` / `--version` / `-v` | Print version, commit, and build date |

Common flags: `--config` (default `orchestrator.yaml`), `--log-level` (`debug`/`info`/`warn`/`error`; default `warn` on a terminal, `info` otherwise), `--log-format` (`auto`/`json`/`text`; JSON when not a TTY).
While a run executes, stderr carries the progress stream, separate from the diagnostic log. On a terminal it is one ASCII line per event, the step named as a breadcrumb with iteration and cap:

```
19:04:08  start        cycles 1/45 > implement
19:07:08  ok           cycles 1/45 > implement  2m53s
19:11:08  not needed   cycles 1/45 > review-cycle 1/5 > fix
19:11:08  ok           cycles 1/45 > review-cycle  settled after 1 of 5
19:14:08  FAIL         cycles 1/45 > land 1/2 > merge  1m0s  agent-failed
19:14:08  not reached  cycles @2..@3  after cycles 1/45 > land 1/2 > merge
19:14:08  end          run 20260902-170408-d0feb66b: settled, 1 failed
```

Lower-case words are routine, upper-case (`FAIL`, `HALT`, `WAIT`, `PAUSE`) need attention. Iterations a loop never needed are not listed; steps past a failure collapse into one `not reached` range per loop. Colour, when the terminal allows it, touches the status word only and uses the terminal theme's own 16 colours (`--color never` or `NO_COLOR` turns it off). Off a terminal, or with `--progress json`, the same events are JSON lines; `--progress none` silences the stream. Each box attempt's combined container output is kept at `<run>/boxes/<step>/attempt-<n>/output.log`, and the journal records every attempt's start, so a step is visible from the moment it launches.

The post-run report on stdout uses the same vocabulary and folds loops the same way: `ok  review-cycle  2 of up to 3  -> review@2` with the executed iterations nested, `not reached  merge  after review-cycle`, and a footer of `ran N ok, M failed; not run X not needed, Y not reached`. `--report-json` keeps every node with the scheduler's own state names, plus an additive `loops` rollup.
`runs`/`upgrade`/`add-key`/`list-keys`/`version` touch no `orchestrator.yaml` and take no `--config` (the run store lives under `host.json`'s `state_dir`).
Exit codes: 0 ok, 1 validation/run failure, 2 usage, 3 halted (`run`/`resume`: no step failed but at least one settled halted — the run stopped for an operator and is resumable), 4 paused (`run`/`resume`: the run ended in a cooperative pause with nothing failed or halted — an operator's `runs pause`, or a loop whose `expect` count exceeded its cap, named in the message; failure outranks halt outranks pause).
Run references (`resume`, `runs pause`) accept a run id or a `--name`-given name; an id always wins over an equal name, and an ambiguous name errors naming the matching ids.
`--help`/`-h`/`help` print usage and exit 0 at every level: `faber --help`, `faber <command> --help`, `faber help <command>`.

## The live view

`faber watch` (or bare `faber` at a terminal) shows nothing the CLI cannot show and does nothing the CLI cannot do; every action names its command. Home: the project pane (workflows and params, then the preflight as counts with a row per problem: the validate verdict, images with the tag hash a missing one was looked up under, the roles templates use and whether a key resolves for each — `faber add-key` names the fix — the templates, listed only by a hook slot whose script is missing, the credential resolver; nothing is run to find out) beside the runs table (live runs pinned at the top, the history newest first beneath, `/` to filter by id, name, workflow or state), with the selected run's `now` / `last` lines; `tab` moves between the panes, and `enter` on a workflow opens its preview — the flow and the folded structure drawn from the IR before any run exists, with a right pane of folded blocks: PARAMS, STRUCTURE, NEEDS (this workflow's images, roles, templates and services and whether each is there) and STEP (template, identity, guard, retry, all four hooks, inputs and outputs, the agent line, history); `tab` or the arrows move the cursor between the flow and the pane, `enter` or right opens a block and left closes it, as on the tree; `esc` backs out one step before it leaves a screen, `q` always leaves; `p` and `R` pause and resume the run on screen as well as the one under Home's cursor. Run: a `now` strip (the running step, since when, and `usually 4m10s` from the history, `slow` past twice; STEP shows the full p50/p90/p99 with the scope it covers — all runs of this workflow in the store, or this run alone, `h` switches and the line says which — and the workflow preview shows p50/p90 per step from all runs), then `t` tree (finished iterations fold to one line with their composition; failed, running or opened ones expand), `w` workflow (the flow as written: steps with their `if` guards, loops as frames with cap and exit, generates with their instances; the current iteration carries the run's state), `l` timeline (one level at a time: bars coloured by status on the level's own axis, enter or right drills into an iteration, left or esc comes up, the full path above, `+`/`-` zoom), beside STEP (reason, attempts, handoff, the re-enter command) and OUTPUT (the attempt's `output.log` tail). `n` jumps to the next problem, `/` filters, `f` follows the newest active step. `r` starts a workflow and `R` resumes a run as detached `faber run`/`faber resume` children (logged under the state directory's `logs/`), `p` writes the pause marker; `q` on a run prints the selected step's re-enter command on stdout. Colour uses the terminal's own 16 colours only; `NO_COLOR` or `--color never` turns it off.

## Host configuration

Faber reads **no configuration from its process environment**. Host-side
inputs live in two explicit files under faber's config home
(`$XDG_CONFIG_HOME/faber/` or `~/.config/faber/`), so what a run will use is
auditable before it runs; the effective host config is also logged at run
start (debug level).

- `roles.json` — the role registry (`faber add-key` / `faber list-keys`):
  each role's key fingerprint plus its git committer identity
  (`--git-name` / `--git-email`). The email is required before a role can run
  gated steps — a box refuses to invent one — and must be an address the
  forge can tie to the key's account.
- `host.json` — per-machine knobs, strict-decoded (unknown or
  differently-cased keys refused; a malformed file refuses the whole
  invocation with exit 1, `version`/`--help` included — faber never runs
  with half-read host state):

| Field | Purpose |
|---|---|
| `box_bin` | Absolute path of the `faber-box` sequencer to bind-mount (default: next to the `faber` executable) |
| `agent_socket_group` | Extra group (`--group-add`) so the box's non-root user can reach the forwarded agent socket; needed on macOS docker VMs (typically `"0"`), leave unset on Linux. Use a numeric gid — a group name must exist inside the box image |
| `state_dir` | Journals + image manifest directory (default `.faber`) |

`FABER_*` names (plus `SSH_AUTH_SOCK` and reserved mount paths) remain engine-
and security-owned as the BOX contract — faber injects them per container; a
template's own `env`/volumes are screened at validate or spec-build time, so a
config can never override them.

## `faber run` / `faber resume`

Both share one validate-then-execute pipeline: the target workflow and everything reachable from it (via `use:` reuse or `generate:` fan-out) are desugared and wiring-checked in the same process before any container runs — there is no code path that executes an IR that did not just pass full validation.

`resume` additionally guards on three independent schema stamps before touching the journal: the journal's own format version, the IR schema version, and the IR hash itself (a changed config re-derives a different hash and resume refuses, naming the drift rather than guessing).
`--fresh` restarts under a new run id, ignoring all three.

`--sessions` captures each attempt's harness session transcripts (the profile's `session_dir`) into `<run>/boxes/<step>/attempt-<n>/sessions/` — live host binds, so a crashed container still leaves its record, and a resumed re-run preserves the failed execution's transcript at a `.sessions.<k>` sibling before reusing the attempt dir. Off by default; recorded in the journal header so a plain resume (and, like `--name`, a `--fresh` restart) inherits it, and `resume --sessions` widens a run that started without it (per resuming invocation). Reading the transcripts — token accounting, prompt archaeology — is user-side work over the run directory; faber's job ends at durable, step-addressed records. Two caveats: a transcript contains whatever the agent read and echoed — including any secret value a hook or service exposed in-box — so treat the run directory with the same care as the box; and the bind captures everything under `session_dir`, so point it at the narrow transcript subpath, not the harness's whole state directory.

## `faber add-key` / `list-keys`

Prefer `add-key` over hand-editing the registry file directly: it validates the fingerprint (and the git identity — single printable lines, `local@domain` email), load-modifies-writes atomically (temp file + rename), and reports a clear usage error (exit 2) for a malformed flag value versus an operational error (exit 1) for everything else. `--git-email` is what gated boxes commit as; register an address the forge ties to the key's account, or the role's gated steps abort at their signing phase.
See `spec/security/arch_role_registry.md` for the registry format and the fingerprint→role mapping it backs.
