package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const runsUsageText = `usage: faber runs [--json]
       faber runs pause <run-id|name>
       faber runs prune [--all]`

// newRunsCmd is the run-store administration group: list (the group's own
// RunE), pause, and prune. All three are thin dispatch over the failure
// module's RunAuditor/RunController seams; they touch no orchestrator.yaml
// and take no --config (the run store's location is host state).
func newRunsCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List journaled runs; pause a live run; prune finished run directories",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunsListE(cmd, args, deps)
		},
	}
	addLogFlags(cmd)
	cmd.Flags().Bool("json", false, "emit the listing as JSON")

	pause := &cobra.Command{
		Use:   "pause <run-id|name>",
		Short: "Ask a live run to pause: in-flight steps finish and settle, nothing new dispatches",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunsPauseE(cmd, args, deps)
		},
	}
	addLogFlags(pause)

	prune := &cobra.Command{
		Use:   "prune",
		Short: "Delete finished, non-live run directories (paused runs kept without --all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunsPruneE(cmd, args, deps)
		},
	}
	addLogFlags(prune)
	prune.Flags().Bool("all", false, "also remove paused and incomplete non-live runs (never live ones)")

	cmd.AddCommand(pause, prune)
	return cmd
}

// runRow is one listing line, shared by the text and JSON renderings.
type runRow struct {
	RunID    string `json:"run_id"`
	Name     string `json:"name,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	State    string `json:"state"`
	Started  string `json:"started,omitempty"`
}

// runState derives the listing state from the audit facts alone: live wins
// (the lock is held right now), an absent run-end marker means the execution
// never finished (interrupted or crashed), and otherwise the last run-end's
// status speaks for itself — the config module never interprets the status
// vocabulary. The settled fallback covers a probeable run-end whose status
// field is unprobeable (a hand-edited journal): completeness is still the
// audited fact.
func runState(a RunAudit) string {
	switch {
	case a.Live:
		return "live"
	case !a.Complete:
		return "incomplete"
	case a.EndStatus != "":
		return a.EndStatus
	default:
		return "settled"
	}
}

func runRunsListE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) > 0 {
		return usageErr(fmt.Errorf("faber runs: unknown subcommand %q\n%s", args[0], runsUsageText))
	}
	asJSON, _ := cmd.Flags().GetBool("json")
	if deps.Audit == nil {
		return errors.New("faber runs: run auditing requires the failure module, which is not wired into this binary yet")
	}
	audits, err := deps.Audit.AuditRuns()
	if err != nil {
		return err
	}
	rows := make([]runRow, 0, len(audits))
	for _, a := range audits {
		row := runRow{RunID: a.RunID, Name: a.Name, Workflow: a.Workflow, State: runState(a)}
		if !a.Started.IsZero() {
			row.Started = a.Started.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	stdout := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("faber runs: encode listing: %w", err)
		}
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no journaled runs")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tNAME\tWORKFLOW\tSTATE\tSTARTED")
	for _, r := range rows {
		// Run id (a directory name) and the header fields are journal-derived
		// text; strip terminal controls before they reach the operator's
		// screen. The JSON path needs none of this (encoding escapes them).
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			stripControls(r.RunID), stripControls(r.Name), stripControls(r.Workflow), r.State, r.Started)
	}
	return w.Flush()
}

// stripControls replaces C0/C1 control runes, DEL, and invalid bytes with
// U+FFFD so listing text cannot re-style or forge the operator's terminal —
// the same discipline the pipeline reporter applies to journal-derived text.
func stripControls(s string) string {
	isCtl := func(r rune) bool {
		return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
	}
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isCtl) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError || isCtl(r) {
			sb.WriteRune(utf8.RuneError)
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func runRunsPauseE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) == 0 {
		return usageErr(errors.New("usage: faber runs pause <run-id|name>"))
	}
	if deps.Runs == nil {
		return errors.New("faber runs pause: run administration requires the failure module, which is not wired into this binary yet")
	}
	runID, err := deps.Runs.ResolveRunRef(args[0])
	if err != nil {
		return err
	}
	if err := deps.Runs.RequestPause(runID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"faber runs pause: pause requested for run %s — in-flight steps finish and settle, nothing new dispatches, the run ends paused (exit 4); resume later with: faber resume %s\n",
		runID, runID)
	return nil
}

func runRunsPruneE(cmd *cobra.Command, args []string, deps Deps) error {
	// Hand-checked rather than cobra.NoArgs: cobra's own args error carries
	// no ExitCode() and would map to exit 1, not the usage exit 2.
	if len(args) > 0 {
		return usageErr(fmt.Errorf("faber runs prune: unexpected argument %q\nusage: faber runs prune [--all]", args[0]))
	}
	all, _ := cmd.Flags().GetBool("all")
	if deps.Runs == nil {
		return errors.New("faber runs prune: run administration requires the failure module, which is not wired into this binary yet")
	}
	removed, err := deps.Runs.PruneRuns(all)
	if err != nil {
		return err
	}
	stdout := cmd.OutOrStdout()
	if len(removed) == 0 {
		fmt.Fprintln(stdout, "faber runs prune: nothing to remove")
		return nil
	}
	fmt.Fprintf(stdout, "faber runs prune: removed %d run(s):\n", len(removed))
	for _, id := range removed {
		fmt.Fprintf(stdout, "  %s\n", id)
	}
	return nil
}
