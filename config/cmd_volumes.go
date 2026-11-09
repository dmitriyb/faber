package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const volumesUsageText = `usage: faber volumes [--config path] [--json]
       faber volumes prune [--config path]`

// newVolumesCmd is the named-volume administration group: list (the
// group's own RunE) and prune. A template's run.volumes entries live on the
// daemon as faber-<project>-<name>, the project derived from the config
// file (ProjectID), so the group takes --config to know whose volumes.
func newVolumesCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volumes",
		Short: "List this project's named docker volumes; prune removes them",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumesListE(cmd, args, deps)
		},
	}
	addCommonFlags(cmd)
	cmd.Flags().Bool("json", false, "emit the listing as JSON")

	prune := &cobra.Command{
		Use:   "prune",
		Short: "Remove this project's named volumes (refused while a run in this directory's run store is live; prune from where you run the project)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumesPruneE(cmd, deps)
		},
	}
	addCommonFlags(prune)
	cmd.AddCommand(prune)
	return cmd
}

// volumeRow is one listing line: the template's name for the volume and
// the daemon's.
type volumeRow struct {
	Name   string `json:"name"`
	Docker string `json:"docker_name"`
}

// projectVolumes lists the volumes under the config's project prefix.
func projectVolumes(ctx context.Context, cmd *cobra.Command, deps Deps, verb string) ([]volumeRow, error) {
	if deps.Volumes == nil {
		return nil, fmt.Errorf("faber volumes %s: volume administration requires the infra module, which is not wired into this binary yet", verb)
	}
	path := readCommonFlags(cmd).config
	// The id is of the file: a path that names none would be a clean-looking
	// project of its own.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("faber volumes %s: %w", verb, err)
	}
	prefix := VolumePrefix(ProjectID(path))
	names, err := deps.Volumes.ListVolumes(ctx, prefix)
	if err != nil {
		return nil, err
	}
	rows := make([]volumeRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, volumeRow{Name: strings.TrimPrefix(n, prefix), Docker: n})
	}
	return rows, nil
}

func runVolumesListE(cmd *cobra.Command, args []string, deps Deps) error {
	if len(args) > 0 {
		return usageErr(fmt.Errorf("faber volumes: unknown subcommand %q\n%s", args[0], volumesUsageText))
	}
	rows, err := projectVolumes(cmd.Context(), cmd, deps, "")
	if err != nil {
		return err
	}
	stdout := cmd.OutOrStdout()
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("faber volumes: encode listing: %w", err)
		}
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no named volumes for this project")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDOCKER VOLUME")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\n", stripControls(r.Name), stripControls(r.Docker))
	}
	return w.Flush()
}

// runVolumesPruneE removes the project's volumes. A live run may be using
// one, so the runs audit is asked first and a live run refuses the whole
// prune: a volume is shared state across the project's runs, never one
// run's own. The audited store is the one runs started from this working
// directory (or the host config's state_dir) are journaled in, so prune
// from where the project is run.
func runVolumesPruneE(cmd *cobra.Command, deps Deps) error {
	if deps.Audit == nil {
		return errors.New("faber volumes prune: the live-run check requires the failure module, which is not wired into this binary yet")
	}
	audits, err := deps.Audit.AuditRuns()
	if err != nil {
		return err
	}
	for _, a := range audits {
		if a.Live {
			return fmt.Errorf("faber volumes prune: run %s is live; a volume may be in use, prune after it ends", a.RunID)
		}
	}
	rows, err := projectVolumes(cmd.Context(), cmd, deps, "prune")
	if err != nil {
		return err
	}
	stdout := cmd.OutOrStdout()
	for _, r := range rows {
		if err := deps.Volumes.RemoveVolume(cmd.Context(), r.Docker); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s (%s)\n", stripControls(r.Name), stripControls(r.Docker))
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no named volumes for this project")
	}
	return nil
}
