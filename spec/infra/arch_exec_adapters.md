# ExecAdapters — typed CLI actuation

## What it is

The only place `os/exec` appears in faber. Every external tool the engine
actuates — `docker`, `git`, `nix`, and opaque user commands (the credential
resolver, data-source commands, `on_failure` hooks) — is reached through one of
four small Go interfaces: `DockerClient`, `GitClient`, `NixClient`, and
`CommandRunner`. The value of the engine is the DAG/validation/threading logic,
not the subprocess calls; keeping the subprocess calls behind these seams is
what lets every other module run against fakes.

## The four adapters

| Adapter | Verbs | Structured mode |
|---------|-------|-----------------|
| `DockerClient` | image-exists, load (tarball), network-exists, container-run (pre-assembled argv), kill (by name) | `docker ... --format '{{json .}}'`, `docker load` tag line parsed once |
| `GitClient` | ls-remote (ref lookup on a URL), rev-parse | plumbing output only — fixed-format, machine-stable |
| `NixClient` | eval (an expression, `--json`), build (an expression file, `--json` → out paths) | `nix eval --json`, `nix build --json` |
| `CommandRunner` | run (path + args + stdin + env + dir) → stdout/stderr/exit | none — output bytes are typed by the caller |

Rules that hold across all four:

- **Structured output or bust.** Where the tool has a JSON mode, the adapter
  uses it and unmarshals into a small Go struct. Free-text stdout is never
  regex-scraped; the two exceptions (git plumbing, the `docker load` result
  line) have byte-stable formats and are parsed in exactly one function each.
- **Context everywhere.** Every verb takes a `context.Context` as its first
  parameter and is built on `exec.CommandContext`; cancellation kills the
  process (with a bounded wait for cleanup) and returns the context error.
- **Explicit argv.** Never `sh -c`; every invocation is an explicit argument
  list. User commands (`CommandRunner`) run the user's file directly with
  declared args — faber does no shell interpolation on their behalf.
- **Wrapped errors.** A failed invocation returns a typed exec error carrying
  the command, its argv, the exit code, and a bounded stderr tail — enough to
  debug without re-running. `errors.As` recovers the exit code upstream.

## CommandRunner is the opaque-command seam

Resolver invocations (`get_token(service)`), generate data-source commands, and
host-side cleanup hooks all flow through `CommandRunner`. It is deliberately
dumber than the other three: it returns raw stdout bytes and lets the caller
type them (the security module wraps resolver output as an opaque secret; the
pipeline module parses the `{"items": [...]}` contract). Because resolver
output may be a credential, `CommandRunner` never includes stdout in an error
or a log line — only stderr and the exit code.

## Fakeability

Each interface is consumed by exactly the components that need it (ImageBuilder
takes a `NixClient` and a `DockerClient`; ContainerRunner takes a
`DockerClient`; security and pipeline take a `CommandRunner`), so tests hand in
recording fakes and assert on the exact argv the component produced. The real
implementations are thin — argv construction, one exec, one parse — so the
untested surface is minimal and the integration suite only has to prove the
parse functions against real tool output.

Requirement implemented: Typed subprocess interfaces.
