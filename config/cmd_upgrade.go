package config

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

//go:generate cp ../install.sh install.sh

// installScript is the canonical, signed install.sh embedded verbatim into the
// binary. `faber upgrade` runs THIS copy in upgrade mode rather than
// reimplementing resolve/download/verify/install in Go: one implementation,
// one copy of the signing key. Because the script is embedded in the
// already-trusted, signed binary (not fetched at upgrade time), there is
// nothing to substitute and so no fetch-and-verify-the-script step.
//
// go:embed cannot traverse "..", so the repo-root install.sh (the file the
// release uploads and the README verifies) cannot be embedded directly from
// this subpackage. config/install.sh is a byte-identical copy kept in sync by
// `go generate ./config` (the directive above); TestUpgradeEmbeddedMatchesReleased
// fails the build on any divergence — that identity is the whole security
// argument.
//
//go:embed install.sh
var installScript []byte

// Installer runs the embedded install.sh in self-replace upgrade mode against
// the pre-resolved paths of the currently-installed pair. cmd/faber/wire.go
// provides the real exec-`sh` implementation (EmbeddedInstaller); the
// in-process CLI tests inject a recorder so the pre-upgrade gate can be
// exercised without touching the network or disk.
type Installer interface {
	Upgrade(ctx context.Context, plan UpgradePlan, stdout, stderr io.Writer) error
}

// UpgradePlan is everything the embedded script needs that the Go side
// resolves: the exact target paths of the coupled pair, the requested version
// (empty = latest), the current version (for the downgrade guard), and the
// mode flags. It is translated to the script's env in env().
type UpgradePlan struct {
	FaberPath      string // exact path of the running faber to replace
	BoxPath        string // exact path of the installed faber-box to replace
	TargetVersion  string // "" = latest; else a "vX.Y.Z" release tag
	CurrentVersion string // the running faber's version (BuildInfo.Version; "dev" if unstamped)
	DryRun         bool   // resolve and verify only; replace nothing
	Rollback       bool   // restore the previous pair from their .bak backups
	Force          bool   // acknowledge the run guard; allow a downgrade; skip confirmation
}

// env renders the plan as the environment the embedded install.sh reads. It
// extends the caller's environment so PATH and the like reach the script.
func (p UpgradePlan) env() []string {
	env := append(os.Environ(),
		"FABER_TARGET="+p.FaberPath,
		"FABER_BOX_TARGET="+p.BoxPath,
		"FABER_CURRENT_VERSION="+p.CurrentVersion,
	)
	if p.Rollback {
		env = append(env, "FABER_ROLLBACK=1")
	} else {
		env = append(env, "FABER_UPGRADE=1")
		if p.TargetVersion != "" {
			env = append(env, "VERSION="+p.TargetVersion)
		}
		if p.DryRun {
			env = append(env, "FABER_DRY_RUN=1")
		}
	}
	if p.Force {
		env = append(env, "FABER_UPGRADE_FORCE=1")
	}
	return env
}

// EmbeddedInstaller is the real Installer: it stages the embedded install.sh
// into a private temp directory and runs it synchronously with `sh`, streaming
// the script's own output to the operator. It holds no state — the script
// bytes are the package-level embed.
type EmbeddedInstaller struct{}

// Upgrade writes the embedded script to a private temp file and runs it in the
// mode the plan selects, synchronously. The script owns resolve, download,
// SSHSIG verification, and the ETXTBSY-safe swap; this method only wires the
// environment and the exit status.
func (EmbeddedInstaller) Upgrade(ctx context.Context, plan UpgradePlan, stdout, stderr io.Writer) error {
	dir, err := os.MkdirTemp("", "faber-upgrade-")
	if err != nil {
		return fmt.Errorf("faber upgrade: create a temp dir for the installer: %w", err)
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, installScript, 0o700); err != nil {
		return fmt.Errorf("faber upgrade: stage the embedded installer: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Env = plan.env()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("faber upgrade: installer failed: %w", err)
	}
	return nil
}

func newUpgradeCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update the installed faber and faber-box to a newer signed release",
		Long: `Update the installed faber — and its contract-version-coupled faber-box — to a
newer signed release, as a unit.

upgrade runs the read-only upgrade-check guard first (it refuses while a run is
live or unfinished; --force acknowledges), then runs the install.sh that is
embedded byte-for-byte in this signed binary: it resolves the target release,
downloads both archives, verifies each SSHSIG signature, and self-replaces both
binaries in place (move-aside + rename, keeping the previous pair at *.bak).

Both signatures are verified before either binary is replaced (fail closed), and
a mid-replace failure rolls both back so the coupled pair is never left
mismatched. This updates only the binaries, never a container image (faber
builds its boxes from pinned Nix toolsets at run time).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgradeE(cmd, deps)
		},
	}
	addLogFlags(cmd)
	cmd.Flags().Bool("check", false, "resolve and verify the target release but make no changes (alias for --dry-run)")
	cmd.Flags().Bool("dry-run", false, "resolve and verify the target release but make no changes")
	cmd.Flags().String("version", "", "upgrade to a specific release (vX.Y.Z) instead of the latest")
	cmd.Flags().Bool("rollback", false, "restore the previous faber and faber-box from their .bak backups")
	cmd.Flags().Bool("force", false, "acknowledge live/unfinished runs, allow a downgrade, and skip confirmation")
	return cmd
}

// runUpgradeE updates the coupled faber/faber-box pair to a newer signed
// release. It runs the read-only pre-upgrade guard first (the same logic as
// `faber upgrade-check`): faber is never swapped out from under a live or
// unfinished run; --force acknowledges and proceeds. Only after the guard
// passes does it resolve the two installed paths and run the embedded,
// already-verified install.sh in upgrade mode — the whole update lives in that
// one signed script, reused rather than reimplemented.
func runUpgradeE(cmd *cobra.Command, deps Deps) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	check, _ := cmd.Flags().GetBool("check")
	targetVersion, _ := cmd.Flags().GetString("version")
	rollback, _ := cmd.Flags().GetBool("rollback")
	force, _ := cmd.Flags().GetBool("force")
	dryRun = dryRun || check

	if deps.Installer == nil {
		return errors.New("faber upgrade: the installer is not wired into this binary yet")
	}
	stdout := cmd.OutOrStdout()

	// A. The read-only pre-upgrade guard, first — before any download or
	// replace. Applies to rollback too: a rollback still swaps both binaries,
	// so it is subject to the same "not mid-run" rule.
	total, blocking, err := auditGate(deps)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		fmt.Fprintf(stdout, "faber upgrade: %d of %d journaled run(s) block an upgrade:\n%s\n",
			len(blocking), total, strings.Join(blocking, "\n"))
		if !force {
			return errors.New("faber upgrade: refusing — faber is not upgraded mid-run; finish or resume the listed runs first, or pass --force to acknowledge")
		}
		fmt.Fprintln(stdout, "--force: proceeding despite the listed runs; they must be finished on the old binary or restarted with --fresh after the swap")
	}

	// B. Resolve the exact paths of the coupled pair to replace.
	faberPath, err := resolveSelfPath()
	if err != nil {
		return fmt.Errorf("faber upgrade: locate the running faber binary: %w", err)
	}
	boxPath, err := resolveBoxPath(deps.BoxBinary)
	if err != nil {
		return fmt.Errorf("faber upgrade: locate the installed faber-box binary: %w", err)
	}

	plan := UpgradePlan{
		FaberPath:      faberPath,
		BoxPath:        boxPath,
		TargetVersion:  targetVersion,
		CurrentVersion: orDefault(deps.BuildInfo.Version, "dev"),
		DryRun:         dryRun,
		Rollback:       rollback,
		Force:          force,
	}
	switch {
	case rollback:
		fmt.Fprintln(stdout, "faber upgrade: rolling back faber and faber-box from their .bak backups")
	case dryRun:
		fmt.Fprintln(stdout, "faber upgrade: checking for a newer signed release (no changes will be made)")
	default:
		fmt.Fprintln(stdout, "faber upgrade: running the embedded signed installer in upgrade mode")
	}
	return deps.Installer.Upgrade(cmd.Context(), plan, stdout, cmd.ErrOrStderr())
}

// resolveSelfPath is the exact on-disk path of the running faber: os.Executable
// resolved through any symlink so the swap renames the real binary, not an
// alias pointing at it.
func resolveSelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// resolveBoxPath is the exact on-disk path of the installed faber-box. The
// integration layer resolves it with the same FABER_BOX_BIN-or-next-to-faber
// convention it uses to bind-mount it (cmd/faber/wire.go) and injects it as
// deps.BoxBinary; here it is only symlink-resolved. An unwired box path is a
// binary that cannot upgrade its coupled half — a hard error, not a partial
// upgrade.
func resolveBoxPath(box string) (string, error) {
	if box == "" {
		return "", errors.New("faber-box path is not wired (it is resolved from FABER_BOX_BIN or the faber binary's directory)")
	}
	if resolved, err := filepath.EvalSymlinks(box); err == nil {
		return resolved, nil
	}
	return box, nil
}
