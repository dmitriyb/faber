# Implementation: CLI dispatch and logging

Covers CLI.

## Dispatch (main.go + internal/config/cli.go)

stdlib `flag` with one FlagSet per subcommand:

```go
func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
    if len(args) == 0 { usage(stderr); return 2 }
    cmd, rest := args[0], args[1:]
    switch cmd {
    case "validate": return cmdValidate(rest, stderr)
    case "build":    return cmdBuild(rest, stderr)
    case "run":      return cmdRun(rest, stderr)
    case "resume":   return cmdResume(rest, stderr)
    case "add-key":  return cmdAddKey(rest, stderr)
    case "list-keys": return cmdListKeys(rest, stderr)
    default: usage(stderr); return 2
    }
}
```

`run(args, stderr) int` (not `main` directly) so the whole CLI is testable
in-process. Common flags are registered by a shared helper on each FlagSet:
`--config`, `--log-level`, `--log-format`.

### cmdValidate

```
Load -> Validate -> for each workflow (or --workflow): Desugar -> CheckWiring
     -> infra.ProvePackages(cfg)          # nix eval, one call per template
all errors aggregated; if --emit-ir: canonical IR bytes to stdout (only when valid)
exit 0/1
```

### cmdRun

```
cmdValidate pipeline (in-process, no --emit-ir)
CheckRunParams(workflow, --param pairs)
construct: journal (fresh run id), meter (from --budget), bindings, runner
pipeline.Execute(ctx, ir, params, deps...)
print report; exit by run status
```

`--param k=v` repeats; `--budget unit=n` repeats per unit; `--metering path`
names the run-time metering config (endpoint tiers) — measurement is run
policy, so it lives beside the run, not in orchestrator.yaml. SIGINT/SIGTERM cancel
the context; the executor owns graceful shutdown (failure module's deferred
cancellation item tracks the richer semantics).

### cmdResume

```
journal := failure.OpenJournal(runID)
mode := resume | fresh (--fresh) | interactive (--interactive <step-id>)
re-derive config path + workflow + params from the journal header
re-run cmdRun pipeline with the journal attached
```

The journal header records config hash, workflow, params, IR hash, and the
schema stamps (journal format, IR version). Resume's guards run in order,
each refusing with a clear message unless `--fresh` is given: a journal
format this binary does not speak (no auto-migration), an IR version the
current engine does not emit (an engine upgrade — the message names the
engine, never the operator's config), and finally a different IR hash
(config drift). Silently mixing journals across configs — or across engine
schemas — is the mistake this must prevent. The executor's resume path
re-checks the failure-module versions of the same guards plus the recorded
image tags; the CLI guards exist to refuse before the full config pipeline
re-runs.

### cmdUpgradeCheck

```
runs := deps.Audit.AuditRuns()   // read-only, format-tolerant kind probe
blocking := live (lock held) ∪ unfinished (no run-end marker)
exit 1 listing blocking runs; --force acknowledges and exits 0
```

The read-only pre-upgrade guard ("faber is not upgraded mid-run"): it never
mutates a journal and never updates faber — the binary swap is external. A
pre-versioning journal (no format stamp) reports as unfinished-unknown and
blocks; across a schema bump the in-flight runs are finished on the old
binary or restarted `--fresh`.

### cmdAddKey / cmdListKeys

```
cmdAddKey:
  flags: --role, --fingerprint, --comment, --force  (no --config)
  --role and --fingerprint required (missing ⇒ usage error, exit 2)
  reg  := security.LoadRegistry(security.RegistryPath())
  reg, changed, err := security.AddKey(reg, role, fingerprint, comment, force)
      err is a validation error (bad fingerprint/role) ⇒ exit 2
      err is a refusal (role re-point without --force) ⇒ exit 1
  if changed { security.SaveRegistry(path, reg) }   # atomic, 0600; dir lazily 0700
  exit 0

cmdListKeys:
  flags: (none but the log flags; no --config)
  reg := security.LoadRegistry(security.RegistryPath())   # missing file ⇒ empty
  print role/fingerprint/comment, sorted by role, to stdout
  exit 0
```

Both are pure dispatch: parse flags, call the security-module entry points,
map the returned error kind to an exit code. They read and mutate only the
registry file, never `orchestrator.yaml`, and never touch key material —
the CLI only ever handles a fingerprint string and an optional label. The
store, validation, atomic write, and idempotency semantics are the security
module's (`spec/security/impl_role_registry.md`); the CLI adds nothing but
flag wiring and exit-code mapping.

## Logging (internal/config/logging.go)

Carried over from conductor nearly verbatim, it was right:

```go
func InitLogging(level, format string, stderr *os.File) *slog.Logger {
    lvl := parseLevel(level) // debug|info|warn|error
    var h slog.Handler
    useText := format == "text" || (format == "auto" && term.IsTerminal(int(stderr.Fd())))
    if useText { h = slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: lvl}) }
    else       { h = slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}) }
    return slog.New(h)
}
```

Modules receive `logger.With("component", "pipeline")` etc. Run-scoped loggers
add `run_id` and step-scoped ones `step`; the journal is the source of record,
logs are for humans and debugging.

## Secret hygiene at the CLI boundary

Values from `--param` are ordinary data. Credential material never transits the
CLI: resolvers are invoked by the security module at run time, return opaque
`security.Secret` values whose `String()`/`Format()` render `[redacted]`, and are
passed by handle thereafter. Nothing in the CLI layer accepts or prints one.
