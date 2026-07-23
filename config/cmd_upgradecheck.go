package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newUpgradeCheckCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade-check",
		Short: "Read-only pre-upgrade guard: refuses while live or unfinished runs exist",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgradeCheckE(cmd, deps)
		},
	}
	addLogFlags(cmd)
	cmd.Flags().Bool("force", false, "acknowledge live/unfinished runs and exit 0 anyway")
	return cmd
}

// runUpgradeCheckE is the read-only pre-upgrade guard: it enumerates
// journaled runs and refuses (exit 1) while any is live (its lock is held) or
// unfinished (no run-end marker), listing them — encoding the rule "faber is
// not upgraded mid-run". It never modifies anything and never updates faber
// (the binary swap is external); --force acknowledges the listed runs and
// exits 0 so a deliberate upgrade can proceed. In-flight runs across a
// schema bump are finished on the old binary or restarted with --fresh.
func runUpgradeCheckE(cmd *cobra.Command, deps Deps) error {
	force, _ := cmd.Flags().GetBool("force")
	total, blocking, err := auditGate(deps)
	if err != nil {
		return err
	}
	stdout := cmd.OutOrStdout()
	if len(blocking) == 0 {
		fmt.Fprintf(stdout, "upgrade-check: %d journaled run(s), none live or unfinished — safe to swap the faber binary\n", total)
		return nil
	}
	fmt.Fprintf(stdout, "upgrade-check: %d of %d journaled run(s) block an upgrade:\n%s\n",
		len(blocking), total, strings.Join(blocking, "\n"))
	if force {
		fmt.Fprintln(stdout, "--force: proceeding anyway; the listed runs must be finished on the old binary or restarted with --fresh after the swap")
		return nil
	}
	return errors.New("faber upgrade-check: refusing — faber is not upgraded mid-run; finish or resume the listed runs first, or pass --force to acknowledge")
}

// auditGate is the shared read-only pre-upgrade guard: it enumerates
// journaled runs and returns the human-readable lines for those that block an
// upgrade (live, or unfinished with no run-end marker). total is the count of
// all journaled runs. Both `faber upgrade-check` and `faber upgrade` run it
// before any binary swap, encoding the rule "faber is not upgraded mid-run".
func auditGate(deps Deps) (total int, blocking []string, err error) {
	if deps.Audit == nil {
		return 0, nil, errors.New("faber upgrade-check: run auditing requires the failure module, which is not wired into this binary yet")
	}
	runs, err := deps.Audit.AuditRuns()
	if err != nil {
		return 0, nil, err
	}
	for _, r := range runs {
		switch {
		case r.Live:
			blocking = append(blocking, fmt.Sprintf("  %s  live (another faber process holds its lock)", r.RunID))
		case !r.Complete && r.Format == 0:
			blocking = append(blocking, fmt.Sprintf("  %s  unfinished (pre-versioning journal; completeness unknown)", r.RunID))
		case !r.Complete:
			blocking = append(blocking, fmt.Sprintf("  %s  unfinished (no run-end marker; interrupted or crashed)", r.RunID))
		}
	}
	return len(runs), blocking, nil
}
