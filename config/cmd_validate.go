package config

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
)

func newValidateCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Load, desugar, and check every workflow",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidateE(cmd, deps)
		},
	}
	addCommonFlags(cmd)
	cmd.Flags().Bool("emit-ir", false, "print the canonical IR to stdout")
	cmd.Flags().String("workflow", "", "validate only this workflow")
	return cmd
}

func runValidateE(cmd *cobra.Command, deps Deps) error {
	common := readCommonFlags(cmd)
	emitIR, _ := cmd.Flags().GetBool("emit-ir")
	workflow, _ := cmd.Flags().GetString("workflow")

	logger, err := common.logger(cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}
	logger = logger.With("component", "cli")

	cfg, viols, err := Load(common.config)
	if err != nil {
		return err
	}
	if err := Validate(cfg, viols); err != nil {
		return err
	}
	names, err := workflowNames(cfg, workflow)
	if err != nil {
		return err
	}

	// Desugar + wiring + package-proof errors are one wave, reported together.
	var errs []error
	irs := map[string]*IR{}
	for _, name := range names {
		ir, derr := Desugar(cfg, name)
		if derr != nil {
			errs = append(errs, derr)
			continue
		}
		if werr := CheckWiring(ir, cfg); werr != nil {
			errs = append(errs, werr)
			continue
		}
		irs[name] = ir
	}
	if deps.Prover != nil {
		if perr := deps.Prover.ProvePackages(context.Background(), cfg, logger.With("component", "infra")); perr != nil {
			errs = append(errs, perr)
		}
	} else {
		logger.Debug("package resolution proof skipped", "reason", "infra module not wired")
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if emitIR {
		stdout := cmd.OutOrStdout()
		for _, name := range names {
			b, err := EncodeIR(irs[name])
			if err != nil {
				return err
			}
			if _, err := stdout.Write(b); err != nil {
				return err
			}
		}
	}
	return nil
}
