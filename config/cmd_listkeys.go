package config

import (
	"errors"

	"github.com/spf13/cobra"
)

func newListKeysCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-keys",
		Short: "Print the global role→fingerprint registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runListKeysE(cmd, deps)
		},
	}
	addLogFlags(cmd)
	return cmd
}

// runListKeysE prints the registry, sorted by role. A missing registry reads
// as empty (a one-line note to stderr, from the Registry implementation),
// never an error.
func runListKeysE(cmd *cobra.Command, deps Deps) error {
	if deps.Registry == nil {
		return errors.New("faber list-keys: registry management requires the security module, which is not wired into this binary yet")
	}
	return deps.Registry.ListKeys(cmd.OutOrStdout(), cmd.ErrOrStderr())
}
