# Commands

| Command | Purpose |
|---|---|
| `faber validate` | Load, desugar, wiring-check every workflow; prove package resolution; `--emit-ir` prints the canonical IR; `--workflow name` narrows |
| `faber build` | Build template images via Nix `dockerTools.buildLayeredImage`; `--template` narrows |
| `faber run <workflow>` | Execute with `--param k=v`, `--budget unit=n`, `--max-parallel n`, `--metering path`, `--report-json path\|-` |
| `faber resume <run-id>` | Re-enter a journaled run; `--fresh` ignores the journal, `--interactive <step>` reopens the failed box with a shell |
| `faber upgrade` | Forward-only update of faber and faber-box to the latest signed release via the embedded `install.sh`; self-replaces both binaries as a unit. Refuses a latest older than installed (rollback anomaly, non-overridable) and refuses while live/unfinished runs exist. `--check`/`--dry-run` (report only; warns about — never blocks on — active runs), `--version vX.Y.Z` (install an exact release, any direction), `--rollback`, `--force` (proceed despite active runs) |
| `faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--git-name n] [--git-email e] [--force]` | Register a role→fingerprint (plus the role's committer identity) in the global identity registry |
| `faber list-keys` | Print the global role→fingerprint registry |
| `faber version` / `--version` / `-v` | Print version, commit, and build date |

Common flags: `--config` (default `orchestrator.yaml`), `--log-level` (`debug`/`info`/`warn`/`error`), `--log-format` (`auto`/`json`/`text`; JSON when not a TTY).
`upgrade`/`add-key`/`list-keys`/`version` touch no `orchestrator.yaml` and take no `--config`.
Exit codes: 0 ok, 1 validation/run failure, 2 usage.
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
- `host.json` — per-machine knobs, strict-decoded (unknown keys refused,
  malformed file refuses the invocation):

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

## `faber add-key` / `list-keys`

Prefer `add-key` over hand-editing the registry file directly: it validates the fingerprint (and the git identity — single printable lines, `local@domain` email), load-modifies-writes atomically (temp file + rename), and reports a clear usage error (exit 2) for a malformed flag value versus an operational error (exit 1) for everything else. `--git-email` is what gated boxes commit as; register an address the forge ties to the key's account, or the role's gated steps abort at their signing phase.
See `spec/security/arch_role_registry.md` for the registry format and the fingerprint→role mapping it backs.
