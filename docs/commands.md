# Commands

| Command | Purpose |
|---|---|
| `faber validate` | Load, desugar, wiring-check every workflow; prove package resolution; `--emit-ir` prints the canonical IR; `--workflow name` narrows |
| `faber build` | Build template images via Nix `dockerTools.buildLayeredImage`; `--template` narrows |
| `faber run <workflow>` | Execute with `--param k=v`, `--budget unit=n`, `--max-parallel n`, `--metering path`, `--report-json path\|-` |
| `faber resume <run-id>` | Re-enter a journaled run; `--fresh` ignores the journal, `--interactive <step>` reopens the failed box with a shell |
| `faber upgrade-check` | Read-only pre-upgrade guard: refuses while live or unfinished runs exist (`--force` acknowledges) |
| `faber upgrade` | Update faber and faber-box to a newer signed release via the embedded `install.sh`; runs `upgrade-check` first, then self-replaces both binaries: `--check`/`--dry-run`, `--version vX.Y.Z`, `--rollback`, `--force` |
| `faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--force]` | Register a role→fingerprint in the global identity registry |
| `faber list-keys` | Print the global role→fingerprint registry |
| `faber version` / `--version` / `-v` | Print version, commit, and build date |

Common flags: `--config` (default `orchestrator.yaml`), `--log-level` (`debug`/`info`/`warn`/`error`), `--log-format` (`auto`/`json`/`text`; JSON when not a TTY).
`upgrade-check`/`upgrade`/`add-key`/`list-keys`/`version` touch no `orchestrator.yaml` and take no `--config`.
Exit codes: 0 ok, 1 validation/run failure, 2 usage.
`--help`/`-h`/`help` print usage and exit 0 at every level: `faber --help`, `faber <command> --help`, `faber help <command>`.

## Environment

| Variable | Purpose |
|---|---|
| `FABER_STATE_DIR` | Journals + image manifest directory (default `.faber`) |
| `FABER_BOX_BIN` | Path to the `faber-box` sequencer binary (default: next to the `faber` executable) |
| `FABER_GIT_NAME` / `FABER_GIT_EMAIL` | Box committer identity |

`FABER_*` names (plus `SSH_AUTH_SOCK` and reserved mount paths) are engine- and security-owned: a template's own `env`/volumes are screened at validate or spec-build time, so a config can never accidentally override them.

## `faber run` / `faber resume`

Both share one validate-then-execute pipeline: the target workflow and everything reachable from it (via `use:` reuse or `generate:` fan-out) are desugared and wiring-checked in the same process before any container runs — there is no code path that executes an IR that did not just pass full validation.

`resume` additionally guards on three independent schema stamps before touching the journal: the journal's own format version, the IR schema version, and the IR hash itself (a changed config re-derives a different hash and resume refuses, naming the drift rather than guessing).
`--fresh` restarts under a new run id, ignoring all three.

## `faber add-key` / `list-keys`

Prefer `add-key` over hand-editing the registry file directly: it validates the fingerprint, load-modifies-writes atomically (temp file + rename), and reports a clear usage error (exit 2) for a malformed `--role`/`--fingerprint` versus an operational error (exit 1) for everything else.
See `spec/security/arch_role_registry.md` for the registry format and the fingerprint→role mapping it backs.
