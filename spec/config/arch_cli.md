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
| `faber run <workflow> [--param k=v ...] [--config path] [--max-parallel n] [--budget u=n] [--metering path]` | validate pipeline -> executor with journal, meter, bindings | run settled with every step ok or skipped-by-condition |
| `faber resume <run-id> [--fresh] [--interactive <step-id>]` | journal load -> recovery mode dispatch (failure module) | as `run` |

Flags shared by all: `--config` (default `orchestrator.yaml`), `--log-level`
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
