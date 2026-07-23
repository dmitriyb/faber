# Implementation: CLI dispatch and logging

Covers CLI.

## Dispatch (config/cli.go + one file per subcommand)

cobra root command with one `newXxxCmd(deps Deps) *cobra.Command` per
subcommand file, wired via `AddCommand`:

```go
// config/cli.go
func NewRootCmd(deps Deps) *cobra.Command {
    var showVersion bool
    cmd := &cobra.Command{
        Use: "faber", SilenceUsage: true, SilenceErrors: true,
        Args: cobra.ArbitraryArgs, // root handles the zero-args / unknown-command cases itself
        RunE: func(cmd *cobra.Command, args []string) error {
            if showVersion {
                return printVersion(cmd.OutOrStdout(), deps.BuildInfo)
            }
            if len(args) == 0 {
                return usageErr(errors.New(rootUsageText))
            }
            return usageErr(fmt.Errorf("faber: unknown command %q", args[0]))
        },
    }
    cmd.Flags().BoolVarP(&showVersion, "version", "v", false, "...")
    cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return usageErr(err) })
    cmd.CompletionOptions.DisableDefaultCmd = true
    cmd.AddCommand(
        newVersionCmd(deps), newValidateCmd(deps), newBuildCmd(deps),
        newRunCmd(deps), newResumeCmd(deps), newUpgradeCheckCmd(deps),
        newAddKeyCmd(deps), newListKeysCmd(deps),
    )
    return cmd
}

func RunWithDeps(args []string, stdout, stderr io.Writer, deps Deps) int {
    root := NewRootCmd(deps)
    root.SetOut(stdout)
    root.SetErr(stderr)
    root.SetArgs(args)
    err := root.Execute()
    if err == nil {
        return 0
    }
    fmt.Fprintln(stderr, err)
    var ec interface{ ExitCode() int }
    if errors.As(err, &ec) {
        return ec.ExitCode()
    }
    return 1
}
```

`func main() { os.Exit(config.RunWithDeps(os.Args[1:], os.Stdout, os.Stderr, wireDeps(...))) }`
in `cmd/faber/main.go` is the only caller outside tests; `wireDeps` (integration
wiring, `cmd/faber/wire.go`) fills the seams `NewRootCmd`'s closures need.

### `root.Args = cobra.ArbitraryArgs`

Without this, cobra's own `Find()` throws a plain (non-`ExitCode()`) "unknown
command" error before `RunE` ever runs, which would map to exit 1 under the
generic fallback instead of the required exit 2. Setting `Args` to accept
anything routes both the zero-args and unknown-command cases through root's
own `RunE`, where they're wrapped in `usageErr` like every other exit-2 case.

### `cliError` / `usageErr`

```go
type cliError struct {
    code int
    err  error
}
func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }
func (e *cliError) ExitCode() int { return e.code }
func usageErr(err error) error { return &cliError{code: 2, err: err} }
```

A `runXxxE` returns a plain `error` for exit-1 cases (validation/run
failures, unwired module seams — the pre-existing message text, unchanged) and
`usageErr(...)` for exit-2 cases (missing positional arg, malformed
`--param`/`--budget`, missing `--role`/`--fingerprint`, unknown
`--log-level`/`--log-format`). `RegistryUsageError` (security seam,
`add-key`) implements `ExitCode() int { return 2 }` directly rather than
being wrapped, so `runAddKeyE` just returns `deps.Registry.AddKey(...)`'s
error untouched and the generic `errors.As` in `RunWithDeps` still routes it
to exit 2.

### `newXxxCmd(deps Deps)` / `runXxxE(cmd, ..., deps)` per subcommand

```go
// config/cmd_validate.go
func newValidateCmd(deps Deps) *cobra.Command {
    cmd := &cobra.Command{
        Use: "validate", Args: cobra.NoArgs,
        RunE: func(cmd *cobra.Command, args []string) error { return runValidateE(cmd, deps) },
    }
    addCommonFlags(cmd)
    cmd.Flags().Bool("emit-ir", false, "...")
    cmd.Flags().String("workflow", "", "...")
    return cmd
}
```

Flags are read back inside `runXxxE` via `cmd.Flags().GetString(...)` /
`GetBool(...)` / `GetStringArray(...)` / `GetInt(...)` — no package-level or
closure-captured flag variables, so the whole tree is rebuilt fresh (and
race-free across parallel tests) on every `RunWithDeps` call, the same way a
fresh `flag.FlagSet` used to be built per call.

`addCommonFlags(cmd)` registers `--config`/`--log-level`/`--log-format`;
`addLogFlags(cmd)` registers only the latter two, for `upgrade-check`,
`upgrade`, `add-key`, `list-keys` (no `--config` — see `arch_cli.md`). `readCommonFlags(cmd)`
reads all three back into a `commonFlags` struct with a `.logger(stderr)`
convenience method (`InitLogging(logLevel, logFormat, stderr)`).

Repeatable flags (`--param`, `--budget` on `run`) use pflag's
`StringArray` (`cmd.Flags().StringArray("param", nil, ...)` /
`GetStringArray("param")`) — each occurrence is kept as given, never
comma-split, matching the old hand-rolled `stringList` `flag.Value`
exactly. `run`/`resume`'s positional workflow/run-id argument is read as
`args[0]` inside `runRunE`/`runResumeE`; cobra performs no positional-count
validation for these (`Args` is left unset on the leaf commands, which is a
no-op for a command with no subcommands of its own — see `arch_cli.md`'s
`cobra.ArbitraryArgs` note, which only matters at the root), so a missing
positional is checked by hand and wrapped in `usageErr` with the same literal
usage strings the old `flag`-based version printed
(`"usage: faber run <workflow> [--param k=v ...] [flags]"` etc.). `-h`/`--help`
before, after, or interspersed with the positional (`faber run task -h`) are
all handled by cobra's own flag scanning before `RunE` ever runs — no
hand-written `wantsHelp` check is needed anymore.

### runValidateE

```
Load -> Validate -> for each workflow (or --workflow): Desugar -> CheckWiring
     -> infra.ProvePackages(cfg)          # nix eval, one call per template
all errors aggregated; if --emit-ir: canonical IR bytes to cmd.OutOrStdout() (only when valid)
```

### runRunE (config/cmd_run.go)

```
runEntry(configPath, workflow, params)   # shared with resume, see below
construct: journal (fresh run id), meter (from --budget), bindings, runner
deps.Executor.Execute(ctx, ir, params, opts, logger)
```

`--param k=v` repeats; `--budget unit=n` repeats per unit; `--metering path`
names the run-time metering config (endpoint tiers) — measurement is run
policy, so it lives beside the run, not in orchestrator.yaml. SIGINT/SIGTERM cancel
the context (`signal.NotifyContext`); the executor owns graceful shutdown (failure
module's deferred cancellation item tracks the richer semantics).

### runResumeE (config/cmd_resume.go)

```
header := deps.Journal.LoadHeader(runID)
mode := resume | fresh (--fresh) | interactive (--interactive <step-id>)
re-derive config path + workflow + params from the journal header
runEntry(header.ConfigPath, header.Workflow, header.Params)
deps.Executor.Execute(...)
```

`runEntry(configPath, workflow, supplied) (*Config, *IR, map[string]*IR, Params, error)`
(`config/cli.go`) is the shared validate-then-check-params pipeline both
`runRunE` and `runResumeE` call — full `Load`/`Validate`/`Desugar`/`CheckWiring`
over the entry workflow and everything reachable from it, then
`CheckRunParams`. It returns an `error` (no exit code — always an exit-1 case
from the caller's perspective) instead of printing and returning an int, the
one behavioral difference from its pre-cobra shape.

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

### runUpgradeCheckE (config/cmd_upgradecheck.go)

```
total, blocking := auditGate(deps)   // read-only, format-tolerant kind probe
   blocking := live (lock held) ∪ unfinished (no run-end marker)
print the report to cmd.OutOrStdout() either way;
return an error (exit 1) listing blocking runs, unless --force (prints and returns nil, exit 0)
```

The read-only pre-upgrade guard ("faber is not upgraded mid-run"): it never
mutates a journal and never updates faber — the binary swap is external. A
pre-versioning journal (no format stamp) reports as unfinished-unknown and
blocks; across a schema bump the in-flight runs are finished on the old
binary or restarted `--fresh`. The blocking-runs report itself always prints
to stdout (both the "safe to swap" and the "blocks an upgrade" cases) — only
the final refusal (no `--force`) becomes the returned `error`, so `RunWithDeps`
prints it to stderr exactly once rather than the command printing it directly.
The kind-probe body is factored into `auditGate(deps) (total, blocking, err)`
so `runUpgradeE` reuses the identical guard rather than re-deriving it.

### runUpgradeE (config/cmd_upgrade.go)

```
flags: --check/--dry-run (aliases), --version vX.Y.Z, --rollback, --force  (no --config)
if deps.Installer == nil ⇒ error (exit 1): the installer is not wired
total, blocking := auditGate(deps)                 // the SAME guard as upgrade-check
  if blocking and not --force ⇒ error (exit 1), installer never invoked
  if blocking and --force ⇒ print acknowledgement, proceed
faberPath := os.Executable() then EvalSymlinks     // replace the real binary, not an alias
boxPath   := EvalSymlinks(deps.BoxBinary)           // "" ⇒ hard error, not a partial upgrade
plan := UpgradePlan{faberPath, boxPath, --version, BuildInfo.Version (or "dev"), dryRun, rollback, force}
return deps.Installer.Upgrade(ctx, plan, stdout, stderr)   // runs the embedded install.sh, synchronously
```

`upgrade` is thin dispatch, like `add-key`: it owns the guard (in Go, because
it reads faber's run state) and the two path resolutions, then hands off to the
`Installer` seam. `UpgradePlan.args()` renders the plan as the flags the
embedded script parses (`--upgrade`/`--rollback`/`--check`, `--target`/
`--box-target`, `--current`, `--force`) — the operator-facing contract is
self-documenting flags, not env; the mode flags are mutually exclusive (upgrade
vs rollback) and `--force` is orthogonal. Only the release pin (`VERSION`, via
`UpgradePlan.scriptEnv()`) and the test-only origin bases stay env;
`--current` is omitted for a `dev`/unstamped build (it cannot be ordered
against a release tag). The real `Installer`
(`EmbeddedInstaller`, wired in `cmd/faber/wire.go`) writes the `//go:embed`-ed
`install.sh` to a private temp file and execs `sh` synchronously; the embedded
copy (`config/install.sh`) is kept byte-identical to the released repo-root
`install.sh` by `go generate ./config` and a build-failing identity test — that
identity is the whole trust argument, since the script is run from the signed
binary rather than fetched (`spec/delivery/arch_release.md`). `boxPath` uses the
same `FABER_BOX_BIN`-or-next-to-faber convention `cmd/faber/wire.go` bind-mounts
faber-box with (`boxBinary()`), injected as `Deps.BoxBinary`, so the config
package never learns the convention twice.

### runAddKeyE / runListKeysE (config/cmd_addkey.go, config/cmd_listkeys.go)

```
runAddKeyE:
  flags: --role, --fingerprint, --comment, --force  (no --config)
  --role and --fingerprint required (missing ⇒ usageErr, exit 2)
  return deps.Registry.AddKey(role, fingerprint, comment, force)
      *RegistryUsageError (bad fingerprint/role) ⇒ exit 2 (via its own ExitCode() method)
      any other error (refusal to re-point without --force, IO) ⇒ exit 1

runListKeysE:
  flags: (none but the log flags; no --config)
  return deps.Registry.ListKeys(cmd.OutOrStdout(), cmd.ErrOrStderr())
```

Both are pure dispatch: read flags, call the security-module entry points via
the injected `Registry` seam, return the error untouched. They read and
mutate only the registry file, never `orchestrator.yaml`, and never touch key
material — the CLI only ever handles a fingerprint string and an optional
label. The store, validation, atomic write, and idempotency semantics are the
security module's (`spec/security/impl_role_registry.md`); the CLI adds
nothing but flag wiring and exit-code mapping.

## Logging (config/logging.go)

Unchanged by the cobra migration, carried over from conductor nearly verbatim:

```go
func InitLogging(level, format string, stderr io.Writer) (*slog.Logger, error) {
    lvl := parseLevel(level) // debug|info|warn|error
    var h slog.Handler
    useText := format == "text" || (format == "auto" && term.IsTerminal(int(stderr.Fd())))
    if useText { h = slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: lvl}) }
    else       { h = slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}) }
    return slog.New(h), nil
}
```

Modules receive `logger.With("component", "pipeline")` etc. Run-scoped loggers
add `run_id` and step-scoped ones `step`; the journal is the source of record,
logs are for humans and debugging. An unknown `--log-level`/`--log-format`
returns an error from `InitLogging`, wrapped `usageErr` at each call site
(exit 2) — unchanged from the pre-cobra behavior.

## Secret hygiene at the CLI boundary

Values from `--param` are ordinary data. Credential material never transits the
CLI: resolvers are invoked by the security module at run time, return opaque
`security.Secret` values whose `String()`/`Format()` render `[redacted]`, and are
passed by handle thereafter. Nothing in the CLI layer accepts or prints one.
