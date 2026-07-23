package config

import (
	"errors"

	"github.com/spf13/cobra"
)

func newAddKeyCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-key",
		Short: "Register a role→fingerprint in the global registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddKeyE(cmd, deps)
		},
	}
	addLogFlags(cmd)
	cmd.Flags().String("role", "", "role name (a bare identifier)")
	cmd.Flags().String("fingerprint", "", "key fingerprint (SHA256:…)")
	cmd.Flags().String("comment", "", "optional human label")
	cmd.Flags().Bool("force", false, "re-point an existing role at a different fingerprint")
	return cmd
}

// runAddKeyE is thin dispatch over the security RoleRegistry: read flags,
// call the injected controller, and let its error (plain, or
// *RegistryUsageError for exit 2) propagate as-is. It touches only the
// registry file and never key material — just a fingerprint string and an
// optional label.
func runAddKeyE(cmd *cobra.Command, deps Deps) error {
	role, _ := cmd.Flags().GetString("role")
	fingerprint, _ := cmd.Flags().GetString("fingerprint")
	comment, _ := cmd.Flags().GetString("comment")
	force, _ := cmd.Flags().GetBool("force")
	if role == "" || fingerprint == "" {
		return usageErr(errors.New("usage: faber add-key --role <name> --fingerprint SHA256:… [--comment c] [--force]"))
	}
	if deps.Registry == nil {
		return errors.New("faber add-key: registry management requires the security module, which is not wired into this binary yet")
	}
	return deps.Registry.AddKey(role, fingerprint, comment, force)
}
