package config

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

const runUsageText = "usage: faber run <workflow> [--param k=v ...] [flags]"

func newRunCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <workflow>",
		Short: "Execute a workflow: faber run <workflow> --param k=v ...",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunE(cmd, args, deps)
		},
	}
	addCommonFlags(cmd)
	cmd.Flags().StringArray("param", nil, "workflow param binding k=v (repeatable)")
	cmd.Flags().StringArray("budget", nil, "budget bound unit=n (repeatable)")
	cmd.Flags().Int("max-parallel", 0, "maximum concurrently running steps (0 = unlimited)")
	cmd.Flags().String("metering", "", "path to run-time metering config")
	cmd.Flags().String("report-json", "", "write the machine-readable run report to this path (- = stdout)")
	return cmd
}

func runRunE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) == 0 {
		return usageErr(errors.New(runUsageText))
	}
	workflow := args[0]

	common := readCommonFlags(cmd)
	paramFlags, _ := cmd.Flags().GetStringArray("param")
	budgetFlags, _ := cmd.Flags().GetStringArray("budget")
	maxParallel, _ := cmd.Flags().GetInt("max-parallel")
	metering, _ := cmd.Flags().GetString("metering")
	reportJSON, _ := cmd.Flags().GetString("report-json")

	logger, err := common.logger(cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}
	supplied, err := parsePairs(paramFlags, "--param")
	if err != nil {
		return usageErr(err)
	}
	budgets, err := parseBudgets(budgetFlags)
	if err != nil {
		return usageErr(err)
	}

	cfg, ir, targets, params, err := runEntry(common.config, workflow, supplied)
	if err != nil {
		return err
	}
	if deps.Executor == nil {
		return errors.New("faber run: execution requires the pipeline module, which is not wired into this binary yet (validation passed)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := RunOptions{
		MaxParallel: maxParallel, Budgets: budgets, MeteringPath: metering,
		ReportJSON: reportJSON,
		ConfigPath: common.config, Workflow: workflow, Supplied: supplied,
		Targets: targets, Config: cfg,
	}
	return deps.Executor.Execute(ctx, ir, params, opts, logger.With("component", "pipeline"))
}
