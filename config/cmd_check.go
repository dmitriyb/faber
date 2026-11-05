package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

const checkUsageText = "usage: faber check <workflow> [--param k=v ...] [--keep] [flags]"

// newCheckCmd is faber check: the workflow's hooks run for real, in their
// images and phase order, against a sandbox — a throwaway remote, no
// network, a key generated for the check, no credentials, no agent. It is
// a run, not a view: the same entry as run, the same exit codes, a journal
// under a temporary directory.
func newCheckCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check <workflow>",
		Short: "Run a workflow's hooks in a sandbox: faber check <workflow> --param k=v ...",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckE(cmd, args, deps)
		},
	}
	addCommonFlags(cmd)
	addProgressFlags(cmd)
	cmd.Flags().StringArray("param", nil, "workflow param binding k=v (repeatable)")
	cmd.Flags().Int("max-parallel", 0, "maximum concurrently running steps (0 = unlimited)")
	cmd.Flags().String("report-json", "", "write the machine-readable run report to this path (- = stdout)")
	cmd.Flags().Bool("keep", false, "keep the check's temporary directory (journal, attempt records, events) and print its path")
	return cmd
}

func runCheckE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) == 0 {
		return usageErr(errors.New(checkUsageText))
	}
	workflow := args[0]

	common := readCommonFlags(cmd)
	paramFlags, _ := cmd.Flags().GetStringArray("param")
	maxParallel, _ := cmd.Flags().GetInt("max-parallel")
	reportJSON, _ := cmd.Flags().GetString("report-json")
	keep, _ := cmd.Flags().GetBool("keep")

	logger, err := common.logger(cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}
	progress, color, err := readProgressFlags(cmd, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	supplied, err := parsePairs(paramFlags, "--param")
	if err != nil {
		return usageErr(err)
	}
	configPath, err := filepath.Abs(common.config)
	if err != nil {
		return fmt.Errorf("faber check: resolve --config: %w", err)
	}
	cfg, ir, targets, params, err := runEntry(configPath, workflow, supplied)
	if err != nil {
		return err
	}
	if deps.Executor == nil {
		return errors.New("faber check: execution requires the pipeline module, which is not wired into this binary yet (validation passed)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := RunOptions{
		Check: true, Keep: keep,
		MaxParallel: maxParallel,
		ReportJSON:  reportJSON, Progress: progress, Color: color,
		ConfigPath: configPath, Workflow: workflow, Supplied: supplied,
		Targets: targets, Config: cfg,
	}
	return deps.Executor.Execute(ctx, ir, params, opts, logger.With("component", "pipeline"))
}
