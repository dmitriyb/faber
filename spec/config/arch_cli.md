# CLI — entry points, dispatch, logging

## What it is

The `faber` binary's user surface: flag parsing, subcommand dispatch, exit codes,
and structured logging initialization. Thin by design — every subcommand is a
composition of module entry points, and nothing below the CLI reads flags or
writes to the terminal directly.

## Subcommands

| Command | Composition | Exit 0 means |
|---------|-------------|--------------|
| `faber validate [--config path] [--emit-ir] [--workflow name]` | Load -> Validate -> Desugar -> WiringChecker -> infra package-resolution proof | the named (or every) workflow would start |
| `faber build [--config path] [--template name]` | Load -> Validate -> ImageBuilder per template | images built and tagged |
| `faber run <workflow> [--param k=v ...] [--config path] [--max-parallel n] [--budget u=n] [--metering path] [--report-json path\|-]` | validate pipeline -> executor with journal, meter, bindings | run settled with every step ok or skipped-by-condition |
| `faber resume <run-id> [--fresh] [--interactive <step-id>] [--report-json path\|-]` | journal load -> version/drift guards -> recovery mode dispatch (failure module) | as `run` |
| `faber upgrade-check [--force]` | run audit (failure module) -> refuse while live/unfinished runs exist | safe to swap the faber binary |
| `faber add-key --role <name> --fingerprint SHA256:… [--comment <c>] [--force]` | security.RoleRegistry load -> AddKey -> atomic save | the role points at the fingerprint (upsert or verified no-op) |
| `faber list-keys` | security.RoleRegistry load -> print | the registry was read and printed |

`add-key` and `list-keys` manage the global `role → fingerprint` registry
(`roles.json` under faber's config home) the identity binding resolves
against; they take **none** of the shared config flags below (they touch no
`orchestrator.yaml`), only `--log-level`/`--log-format`. faber never writes
key material — only the fingerprint plus an optional label. The subcommands
are thin dispatch; the store, validation, atomic write, idempotency, and
`--force` refusal live in the security module (`spec/security/arch_role_registry.md`,
`impl_role_registry.md`). A malformed `--fingerprint` or `--role`, or a
missing required flag, is a usage error (exit 2); a refusal to re-point an
existing role without `--force`, or an IO error, is exit 1.

`run`/`resume` answer `-h`/`--help` with their usage and flag defaults on
stdout, exit 0 — never the exit-2 usage error. `--report-json` writes the
machine-readable run report to a path (`-` = stdout, after the human report).

Flags shared by validate/build/run/resume: `--config` (default `orchestrator.yaml`), `--log-level`
(default `info`), `--log-format` (auto/json/text). `--config` names the **root
project file**; `Load` transitively pulls its `include:` closure and merges the
component libraries before validation (see `arch_loader.md`). A single-file config
with no `include:` behaves exactly as before, so the default and every existing
invocation are unchanged.

Exit codes: 0 success; 1 validation or run failure (details already reported);
2 usage error. `validate` reports *all* errors before exiting — the multi-error
discipline of the Loader and WiringChecker surfaces here as one combined,
field-path-sorted listing.

## Logging

`slog` initialized once at startup: JSON handler when stderr is not a TTY, text
handler when it is (overridable via `--log-format`). Each module receives a child
logger via `logger.With("component", name)` — no global logger, no
`slog.SetDefault` (conductor's D3, carried over). Secrets never reach the logger:
resolver outputs and credential material are typed so they cannot be passed to
logging calls (dedicated opaque types with redacted `String()`).

## Design rationale

- **stdlib `flag` with manual subcommand dispatch** — the surface is four
  subcommands; cobra would be the largest dependency in the binary for no
  structural gain (and the stdlib-first constraint says no).
- **`run` embeds `validate`.** There is no code path that executes an IR that did
  not just pass the full validation pipeline in the same process. `--emit-ir`
  exists so the validated artifact is inspectable and diffable, not so it can be
  fed back in.
- **No hidden state.** The CLI passes `*Config`, the IR, and constructed module
  values down explicitly; nothing reads globals. This is what keeps every module
  testable without a CLI harness.

Requirement implemented: CLI commands.
