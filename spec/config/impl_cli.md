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

The journal header records config hash, workflow, params, and IR hash; resume
refuses (with a clear message) if the current config desugars to a different IR
hash unless `--fresh` is given — silently mixing journals across configs is the
one mistake this must prevent.

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
