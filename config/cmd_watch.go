package config

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newWatchCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch [<run-id|name>]",
		Short: "Open the live view: faber watch [<run-id|name>]",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatchE(cmd, args, deps)
		},
	}
	cmd.Flags().String("config", "orchestrator.yaml", "path to orchestrator.yaml")
	cmd.Flags().String("color", "auto", "colour the view: auto|always|never (auto: a terminal that is not dumb, NO_COLOR unset)")
	return cmd
}

// runWatchE opens the TUI on the config in the current directory and, when
// a run reference is given, on that run. It needs a terminal on both ends:
// the view reads keys and redraws the screen.
func runWatchE(cmd *cobra.Command, args []string, deps Deps) error {
	if deps.UI == nil {
		return errors.New("faber watch: the live view requires the tui module, which is not wired into this binary")
	}
	if !isTerminal(cmd.OutOrStdout()) || !isTerminal(os.Stdin) {
		return usageErr(errors.New("faber watch: needs a terminal on stdin and stdout (pipe-friendly surfaces are `faber runs` and `--progress json`)"))
	}
	configPath, _ := cmd.Flags().GetString("config")
	if abs, err := filepath.Abs(configPath); err == nil {
		configPath = abs
	}
	colorMode, _ := cmd.Flags().GetString("color")
	color, err := ColorEnabled(colorMode, cmd.ErrOrStderr())
	if err != nil {
		return usageErr(err)
	}
	run := ""
	if len(args) > 0 {
		run = args[0]
		if deps.Runs != nil {
			resolved, err := deps.Runs.ResolveRunRef(run)
			if err != nil {
				return err
			}
			run = resolved
		}
	}
	return deps.UI.Run(cmd.Context(), UIOptions{
		ConfigPath: configPath, Run: run, Color: color,
		Stdin: os.Stdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
	})
}
