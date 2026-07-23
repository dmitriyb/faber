package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newBuildCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build template images",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuildE(cmd, deps)
		},
	}
	addCommonFlags(cmd)
	cmd.Flags().String("template", "", "build only this template")
	return cmd
}

func runBuildE(cmd *cobra.Command, deps Deps) error {
	common := readCommonFlags(cmd)
	template, _ := cmd.Flags().GetString("template")

	logger, err := common.logger(cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}

	cfg, viols, err := Load(common.config)
	if err != nil {
		return err
	}
	if err := Validate(cfg, viols); err != nil {
		return err
	}
	names := sortedKeys(cfg.Templates)
	if template != "" {
		if _, ok := cfg.Templates[template]; !ok {
			return fmt.Errorf("faber build: unknown template %q", template)
		}
		names = []string{template}
	}
	if deps.Builder == nil {
		return errors.New("faber build: image builds require the infra module, which is not wired into this binary yet")
	}
	blog := logger.With("component", "infra")
	for _, name := range names {
		if err := deps.Builder.BuildImage(context.Background(), cfg, name, blog); err != nil {
			return err
		}
	}
	return nil
}
