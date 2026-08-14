package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

const resumeUsageText = "usage: faber resume <run-id|name> [--fresh] [--sessions] [--interactive <step-id>] [--shell] [flags]"

func newResumeCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <run-id|name>",
		Short: "Re-enter a journaled run: faber resume <run-id|name>",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResumeE(cmd, args, deps)
		},
	}
	addCommonFlags(cmd)
	cmd.Flags().Bool("fresh", false, "restart from the journal's config without reusing step results")
	cmd.Flags().Bool("sessions", false, "capture harness session transcripts for the remainder of the run (a run started with --sessions keeps capturing regardless)")
	cmd.Flags().String("interactive", "", "re-enter interactively at this step id")
	cmd.Flags().Bool("shell", false, "with --interactive: force the bare shell even when a saved session and a profile resume_argv exist")
	cmd.Flags().String("report-json", "", "write the machine-readable run report to this path (- = stdout)")
	return cmd
}

// cmdResume: resuming is a read-only guard sequence (journal format, IR
// schema, IR hash) before the shared runEntry pipeline re-validates the
// journaled config and re-enters the executor. --fresh escapes every guard.
func runResumeE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) == 0 {
		return usageErr(errors.New(resumeUsageText))
	}
	// The positional is a run reference: an id, or a --name-given header
	// name (an id always wins over an equal name). With no RunController
	// wired the reference is used as an id unchanged.
	runID := args[0]
	if deps.Runs != nil {
		resolved, err := deps.Runs.ResolveRunRef(runID)
		if err != nil {
			return err
		}
		runID = resolved
	}

	common := readCommonFlags(cmd)
	fresh, _ := cmd.Flags().GetBool("fresh")
	sessions, _ := cmd.Flags().GetBool("sessions")
	interactive, _ := cmd.Flags().GetString("interactive")
	shell, _ := cmd.Flags().GetBool("shell")
	reportJSON, _ := cmd.Flags().GetString("report-json")
	if shell && interactive == "" {
		return usageErr(errors.New("faber resume: --shell is meaningful only with --interactive <step-id>"))
	}

	logger, err := common.logger(cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}
	if deps.Journal == nil {
		return errors.New("faber resume: journaled runs require the failure module, which is not wired into this binary yet")
	}
	header, err := deps.Journal.LoadHeader(runID)
	if err != nil {
		return err
	}
	if supported := deps.Journal.SupportedFormat(); header.Format != supported && !fresh {
		return fmt.Errorf("faber resume: run %s was journaled under schema v%d; this faber speaks v%d and does not auto-migrate — finish the run on the faber that wrote it, or pass --fresh to start over",
			runID, header.Format, supported)
	}
	if header.IRVersion != 0 && header.IRVersion != IRVersion && !fresh {
		// Checked before the config pipeline re-runs: the IR schema itself
		// moved — an engine upgrade, not config drift — and a config-shaped
		// error from re-validation must not preempt the message that names
		// the engine rather than the operator's config.
		return fmt.Errorf("faber resume: run %s was journaled under IR schema v%d; this faber emits v%d and does not auto-migrate — finish the run on the faber that wrote it, or pass --fresh to start over",
			runID, header.IRVersion, IRVersion)
	}

	// Re-derive config path, workflow, and params from the journal header.
	cfg, ir, targets, params, err := runEntry(header.ConfigPath, header.Workflow, header.Params)
	if err != nil {
		return err
	}
	hash, err := HashIR(ir)
	if err != nil {
		return err
	}
	if hash != header.IRHash && !fresh {
		return fmt.Errorf("faber resume: run %s was journaled against a different IR (config has changed since the run; journal %s, current %s) — fix the config back or pass --fresh to restart",
			runID, header.IRHash, hash)
	}
	if deps.Executor == nil {
		return errors.New("faber resume: execution requires the pipeline module, which is not wired into this binary yet (resume guard passed)")
	}
	mode := "resume"
	if fresh {
		mode = "fresh"
	}
	if interactive != "" {
		mode = "interactive"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := RunOptions{
		RunID: runID, Mode: mode, InteractiveStep: interactive, InteractiveShell: shell,
		// The flag ORed with the journaled header: a capturing run keeps
		// capturing across resume AND --fresh (the fresh path writes a new
		// header from opts, and losing the flag on restart would silently
		// stop the transcripts), exactly like Name inheritance below.
		Sessions: sessions || header.Sessions,
		// Only the fresh path reads Name (it writes a brand-new header);
		// inheriting it keeps a named run named across --fresh restarts.
		Name:       header.Name,
		ReportJSON: reportJSON,
		ConfigPath: header.ConfigPath, Workflow: header.Workflow, Supplied: header.Params,
		Targets: targets, Config: cfg,
	}
	return deps.Executor.Execute(ctx, ir, params, opts, logger.With("component", "pipeline"))
}
