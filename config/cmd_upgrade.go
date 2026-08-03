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
// (empty = resolve the latest, forward-only; else a named release the script
// installs in any direction), the current version (which the script orders
// against), and the mode flags. It is translated to the script's flags in
// args(), with the release pin carried as VERSION by scriptEnv() — VERSION
// unset is exactly what tells the script it is on the forward-only latest path.
type UpgradePlan struct {
	FaberPath      string // exact path of the running faber to replace
	BoxPath        string // exact path of the installed faber-box to replace
	TargetVersion  string // "" = resolve latest (forward-only); else a "vX.Y.Z" release tag installed in any direction
	CurrentVersion string // the running faber's version (BuildInfo.Version; "dev" if unstamped)
	DryRun         bool   // resolve and verify only; replace nothing
	Rollback       bool   // restore the previous pair from their .bak backups
}

// args renders the plan as the flags the embedded install.sh parses. The
// operator-facing contract is flags, not env, so it is self-documenting; only
// the release pin (VERSION) and the test-only origin bases stay env — see
// scriptEnv. --current is passed only for a stamped release version, since a
// dev/unstamped build cannot be ordered against a release tag. There is no
// --force here: the active-runs guard is resolved entirely Go-side, and the
// forward-only anomaly refusal is not overridable, so the script exposes no
// override flag for either.
func (p UpgradePlan) args() []string {
	if p.Rollback {
		return []string{"--rollback", "--target", p.FaberPath, "--box-target", p.BoxPath}
	}
	a := []string{"--upgrade", "--target", p.FaberPath, "--box-target", p.BoxPath}
	if p.CurrentVersion != "" && p.CurrentVersion != "dev" {
		a = append(a, "--current", p.CurrentVersion)
	}
	if p.DryRun {
		a = append(a, "--check")
	}
	return a
}

// scriptEnv is the environment the embedded install.sh runs under: the
// caller's environment (PATH and the like) plus the release pin as VERSION
// when one was requested. Everything else the script needs arrives as flags
// (see args); the signing key is never overridable.
//
// Three seams the operator's shell could carry are STRIPPED before the pin is
// set — VERSION, FABER_API_BASE, and FABER_DL_BASE:
//   - VERSION: the embedded script selects the explicit-release path vs the
//     forward-only latest path SOLELY by whether VERSION is present in its
//     environment, so an ambient VERSION (a very common name) exported in the
//     operator's shell would leak in during a plain `upgrade`, flip it onto the
//     explicit path, and silently disable the non-overridable forward-only
//     anomaly refusal. VERSION is set here only from the --version flag
//     (p.TargetVersion), never inherited.
//   - FABER_API_BASE / FABER_DL_BASE: these redirect where the script resolves
//     and downloads from. They default to the real GitHub endpoints when unset
//     and exist ONLY for the shell test, which invokes install.sh directly (not
//     via this command). An operator's `upgrade` must never honor an ambient
//     override of the origin, so any inherited value is dropped rather than
//     passed through — the production upgrade path always fetches from GitHub.
func (p UpgradePlan) scriptEnv() []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "VERSION=") ||
			strings.HasPrefix(kv, "FABER_API_BASE=") ||
			strings.HasPrefix(kv, "FABER_DL_BASE=") {
			continue
		}
		env = append(env, kv)
	}
	if !p.Rollback && p.TargetVersion != "" {
		env = append(env, "VERSION="+p.TargetVersion)
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
// flags/environment and the exit status.
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
	cmd := exec.CommandContext(ctx, "sh", append([]string{script}, plan.args()...)...)
	cmd.Env = plan.scriptEnv()
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
		Short: "Update the installed faber and faber-box forward to the latest signed release",
		Long: `Update the installed faber — and its contract-version-coupled faber-box — to the
latest signed release, as a unit. Upgrade is forward-only: it resolves the
latest release and moves toward it.

upgrade runs the active-runs guard first (it refuses while a run is live or
unfinished; --force proceeds anyway), then runs the install.sh that is embedded
byte-for-byte in this signed binary: it resolves the target release, downloads
both archives, verifies each SSHSIG signature, and self-replaces both binaries
in place (move-aside + rename, keeping the previous pair at *.bak).

If the resolved latest is OLDER than the installed version, upgrade hard-refuses
and no flag overrides it — a latest that moved backward is a rollback anomaly
(a compromised origin serving an old release as "latest"). To install an older
release deliberately, name it with --version, which installs any release in any
direction with no guard.

  faber upgrade                 upgrade forward to the latest release
  faber upgrade --check         report availability only; change nothing
  faber upgrade --force         upgrade despite live/unfinished runs
  faber upgrade --version vX.Y.Z install that exact release, any direction
  faber upgrade --rollback      restore the previous pair from their .bak backups

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
	cmd.Flags().Bool("check", false, "report the latest release and change nothing; warns about (does not block on) active runs (alias for --dry-run)")
	cmd.Flags().Bool("dry-run", false, "report the latest release and change nothing (alias for --check)")
	cmd.Flags().String("version", "", "install a specific release (vX.Y.Z), any direction, instead of the forward-only latest")
	cmd.Flags().Bool("rollback", false, "restore the previous faber and faber-box from their .bak backups")
	cmd.Flags().Bool("force", false, "proceed even though live or unfinished runs exist (overrides the active-runs guard only)")
	return cmd
}

// runUpgradeE updates the coupled faber/faber-box pair. The default is
// forward-only: resolve the latest signed release and move toward it; the
// script hard-refuses a latest that is older than installed (a rollback
// anomaly), non-overridable. --version names an exact release the script
// installs in any direction with no guard. --check reports availability and
// changes nothing.
//
// The active-runs guard runs first (`auditGate`, the body the retired
// `upgrade-check` command used). On the plain upgrade path a blocking run
// refuses (faber is never swapped out from under a live or unfinished run);
// --force overrides that guard and only that guard — it carries no version or
// direction meaning and cannot bypass the forward-only anomaly refusal. On the
// --check path the same guard only WARNS: --check's job is to report, so it
// never blocks and exits 0 whenever it could resolve the latest release. Only
// after the guard is settled does it resolve the two installed paths and run
// the embedded, already-verified install.sh — the whole update lives in that
// one signed script, reused rather than reimplemented.
func runUpgradeE(cmd *cobra.Command, deps Deps) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	check, _ := cmd.Flags().GetBool("check")
	targetVersion, _ := cmd.Flags().GetString("version")
	rollback, _ := cmd.Flags().GetBool("rollback")
	force, _ := cmd.Flags().GetBool("force")
	report := dryRun || check

	if deps.Installer == nil {
		return errors.New("faber upgrade: the installer is not wired into this binary yet")
	}
	stdout := cmd.OutOrStdout()

	// A. The active-runs guard, first — before any download or replace.
	// Applies to rollback too: a rollback still swaps both binaries, so it is
	// subject to the same "not mid-run" rule. --check only warns (its purpose
	// is to report, not to gate); the plain path refuses unless --force
	// overrides. --force overrides this guard and nothing else.
	total, blocking, err := auditGate(deps)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		switch {
		case report:
			fmt.Fprintf(stdout, "faber upgrade --check: NOTE — %d of %d journaled run(s) are live or unfinished; an upgrade would refuse until they finish (this check does not block):\n%s\n",
				len(blocking), total, strings.Join(blocking, "\n"))
		case force:
			fmt.Fprintf(stdout, "faber upgrade: %d of %d journaled run(s) are live or unfinished:\n%s\n",
				len(blocking), total, strings.Join(blocking, "\n"))
			fmt.Fprintln(stdout, "--force: proceeding despite the listed run(s); they must be finished on the old binary or restarted with --fresh after the swap")
		default:
			fmt.Fprintf(stdout, "faber upgrade: %d of %d journaled run(s) block an upgrade:\n%s\n",
				len(blocking), total, strings.Join(blocking, "\n"))
			return errors.New("faber upgrade: refusing — faber is not upgraded mid-run; finish or resume the listed runs first, or pass --force to proceed anyway")
		}
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
		DryRun:         report,
		Rollback:       rollback,
	}
	switch {
	case rollback:
		fmt.Fprintln(stdout, "faber upgrade: rolling back faber and faber-box from their .bak backups")
	case report:
		fmt.Fprintln(stdout, "faber upgrade --check: resolving the target signed release (no changes will be made)")
	case targetVersion != "":
		// Display the requested release v-prefixed to match the release-tag form,
		// normalizing to exactly one leading v (the flag value may or may not carry
		// it). Display only — plan.TargetVersion below still carries the raw flag.
		fmt.Fprintf(stdout, "faber upgrade: installing the requested release v%s (any direction; the forward-only guard does not apply to an explicitly named version)\n", strings.TrimPrefix(targetVersion, "v"))
	default:
		fmt.Fprintln(stdout, "faber upgrade: resolving the latest signed release and upgrading forward")
	}
	return deps.Installer.Upgrade(cmd.Context(), plan, stdout, cmd.ErrOrStderr())
}

// auditGate is the read-only active-runs guard reused by the plain upgrade
// path (where a blocking run refuses) and by `faber upgrade --check` (where it
// only warns): it enumerates journaled runs and returns the human-readable
// lines for those that block an upgrade (live, or unfinished with no run-end
// marker). total is the count of all journaled runs. It encodes the rule
// "faber is not upgraded mid-run" and never mutates a journal. This is the
// body the standalone `faber upgrade-check` command used before it was folded
// into `faber upgrade --check`.
func auditGate(deps Deps) (total int, blocking []string, err error) {
	if deps.Audit == nil {
		return 0, nil, errors.New("faber upgrade: run auditing requires the failure module, which is not wired into this binary yet")
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
// integration layer resolves it with the same host-config-box_bin-or-next-to-faber
// convention it uses to bind-mount it (cmd/faber/wire.go) and injects it as
// deps.BoxBinary; here it is only symlink-resolved. An unwired box path is a
// binary that cannot upgrade its coupled half — a hard error, not a partial
// upgrade.
func resolveBoxPath(box string) (string, error) {
	if box == "" {
		return "", errors.New("faber-box path is not wired (it is resolved from the host config's box_bin or the faber binary's directory)")
	}
	if resolved, err := filepath.EvalSymlinks(box); err == nil {
		return resolved, nil
	}
	return box, nil
}
