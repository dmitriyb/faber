# Commands

| Command | Purpose |
|---|---|
| `faber validate` | Load, desugar, wiring-check every workflow; prove package resolution; `--emit-ir` prints the canonical IR; `--workflow name` narrows |
| `faber build` | Build template images via Nix `dockerTools.buildLayeredImage`; `--template` narrows |
| `faber run <workflow>` | Execute with `--name n` (a human name for the run, stored in the journal header), `--param k=v`, `--budget unit=n`, `--max-parallel n`, `--metering path`, `--sessions` (capture each attempt's harness session transcripts under the run dir), `--report-json path\|-` |
| `faber resume <run-id\|name>` | Re-enter a journaled run; `--fresh` ignores the journal, `--sessions` turns capture on for the remainder, `--interactive <step>` reopens the failed box inside its saved session (when one exists and the profile defines `resume_argv`) or a shell, `--shell` forces the shell |
| `faber runs` | List journaled runs: id, name, workflow, state (`live`/`paused`/`settled`/`aborted`/`incomplete`), started; `--json` for the same rows machine-readably |
| `faber runs pause <run-id\|name>` | Ask a live run to pause: in-flight steps finish and journal, nothing new dispatches, the run ends `paused` (exit 4) and resumes later with `faber resume` |
| `faber runs prune` | Delete finished, non-live run directories; paused runs are kept (resumable state) unless `--all`, which also removes paused and incomplete non-live runs; live runs are never touched |
| `faber upgrade` | Forward-only update of faber and faber-box to the latest signed release via the embedded `install.sh`; self-replaces both binaries as a unit. Refuses a latest older than installed (rollback anomaly, non-overridable) and refuses while live/unfinished runs exist. `--check`/`--dry-run` (report only; warns about — never blocks on — active runs), `--version vX.Y.Z` (install an exact release, any direction), `--rollback`, `--force` (proceed despite active runs) |
| `faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--git-name n] [--git-email e] [--force]` | Register a role→fingerprint (plus the role's committer identity) in the global identity registry |
| `faber list-keys` | Print the global role→fingerprint registry |
| `faber version` / `--version` / `-v` | Print version, commit, and build date |

Common flags: `--config` (default `orchestrator.yaml`), `--log-level` (`debug`/`info`/`warn`/`error`), `--log-format` (`auto`/`json`/`text`; JSON when not a TTY).
`runs`/`upgrade`/`add-key`/`list-keys`/`version` touch no `orchestrator.yaml` and take no `--config` (the run store lives under `host.json`'s `state_dir`).
Exit codes: 0 ok, 1 validation/run failure, 2 usage, 3 halted (`run`/`resume`: no step failed but at least one settled halted — the run stopped for an operator and is resumable), 4 paused (`run`/`resume`: the run ended in a cooperative pause with nothing failed or halted; failure outranks halt outranks pause).
Run references (`resume`, `runs pause`) accept a run id or a `--name`-given name; an id always wins over an equal name, and an ambiguous name errors naming the matching ids.
`--help`/`-h`/`help` print usage and exit 0 at every level: `faber --help`, `faber <command> --help`, `faber help <command>`.

## Host configuration

Faber reads **no configuration from its process environment**. Host-side
inputs live in two explicit files under faber's config home
(`$XDG_CONFIG_HOME/faber/` or `~/.config/faber/`), so what a run will use is
auditable before it runs; the effective host config is also logged at run
start.

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
