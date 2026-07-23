package config

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// BuildInfo carries the version/commit/date the release pipeline stamps into
// cmd/faber's package-main vars via -ldflags. A locally built binary passes a
// zero-valued BuildInfo, and printVersion substitutes dev/none/unknown so the
// version command always prints something, never an empty field.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// newVersionCmd is the `faber version` subcommand; `faber --version`/`-v` on
// the root command print identically via the same printVersion body.
func newVersionCmd(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		RunE: func(cmd *cobra.Command, args []string) error {
			return printVersion(cmd.OutOrStdout(), deps.BuildInfo)
		},
	}
}

func printVersion(stdout io.Writer, info BuildInfo) error {
	_, err := fmt.Fprintf(stdout, "faber %s (commit %s, built %s)\n",
		orDefault(info.Version, "dev"), orDefault(info.Commit, "none"), orDefault(info.Date, "unknown"))
	return err
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
