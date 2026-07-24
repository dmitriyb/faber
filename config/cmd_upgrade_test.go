package config

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// recordInstaller is a stand-in for the embedded install.sh runner: it records
// that (and how) it was invoked without touching the network or disk, so the
// pre-upgrade gate can be exercised in-process.
type recordInstaller struct {
	called bool
	plan   UpgradePlan
}

func (r *recordInstaller) Upgrade(_ context.Context, plan UpgradePlan, _, _ io.Writer) error {
	r.called = true
	r.plan = plan
	return nil
}

// Verifies the forward-only upgrade contract at the CLI seam. The plain path
// runs the active-runs guard BEFORE any download or replace: it refuses (exit
// 1) while live or unfinished runs exist and does not invoke the installer;
// --force overrides that guard and only that guard (it carries no version or
// direction meaning). --check reports and never blocks (warns on active runs,
// exit 0). --version names an exact release the installer takes in any
// direction. A clean store runs the installer with the resolved plan. The
// forward-only anomaly refusal (a latest older than installed) lives in the
// signed script and is exercised by the fake-server harness in
// install_upgrade_test.go, where it is proven non-overridable.
func TestCLIUpgradeGate(t *testing.T) {
	const box = "/opt/faber/faber-box"

	t.Run("installer not wired yields a structured error", func(t *testing.T) {
		rec := &recordInstaller{}
		code, _, stderr := runCLI(t, Deps{Audit: fakeAudit{}}, "upgrade")
		if code != 1 || !strings.Contains(stderr, "installer is not wired") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if rec.called {
			t.Fatal("installer must not run when it is not wired")
		}
	})

	t.Run("refuses while a run is live, installer untouched", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-live", Live: true, Format: 1}}}
		code, stdout, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box}, "upgrade")
		if code != 1 {
			t.Fatalf("got exit %d, want 1: %s", code, stderr)
		}
		if rec.called {
			t.Fatal("installer ran despite a live run blocking the upgrade")
		}
		for _, want := range []string{"r-live", "not upgraded mid-run", "--force"} {
			if !strings.Contains(stdout+stderr, want) {
				t.Errorf("output missing %q:\n%s%s", want, stdout, stderr)
			}
		}
	})

	t.Run("--force overrides the active-runs guard and runs the installer", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-live", Live: true, Format: 1}}}
		code, stdout, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box}, "upgrade", "--force")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called {
			t.Fatal("installer did not run under --force")
		}
		if !strings.Contains(stdout, "--force") {
			t.Errorf("expected a --force acknowledgement:\n%s", stdout)
		}
	})

	t.Run("--check warns about active runs but exits 0 without blocking", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-live", Live: true, Format: 1}}}
		code, stdout, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box}, "upgrade", "--check")
		if code != 0 {
			t.Fatalf("got exit %d, want 0 (--check reports, never blocks): %s", code, stderr)
		}
		if !rec.called || !rec.plan.DryRun {
			t.Fatalf("--check must still resolve/report via the installer in dry-run: called=%v plan=%+v", rec.called, rec.plan)
		}
		// The warning names the active run and says the check does not block.
		for _, want := range []string{"r-live", "does not block"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("--check output missing %q:\n%s", want, stdout)
			}
		}
		// It must NOT be the plain-path refusal.
		if strings.Contains(stdout+stderr, "refusing") {
			t.Errorf("--check must not refuse on active runs:\n%s%s", stdout, stderr)
		}
	})

	t.Run("clean store upgrades forward with the resolved plan (no version pin)", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-done", Complete: true, Format: 1}}}
		code, _, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box, BuildInfo: BuildInfo{Version: "v0.1.2"}},
			"upgrade")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called {
			t.Fatal("installer did not run for a clean store")
		}
		// Forward-only latest path: no version pin, so the script resolves
		// latest and the anomaly guard applies.
		if rec.plan.TargetVersion != "" {
			t.Errorf("plan.TargetVersion = %q, want empty (forward-only latest)", rec.plan.TargetVersion)
		}
		if rec.plan.CurrentVersion != "v0.1.2" {
			t.Errorf("plan.CurrentVersion = %q, want v0.1.2", rec.plan.CurrentVersion)
		}
		if rec.plan.BoxPath != box {
			t.Errorf("plan.BoxPath = %q, want %q", rec.plan.BoxPath, box)
		}
	})

	t.Run("--version names an exact release the installer takes in any direction", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-done", Complete: true, Format: 1}}}
		// Requested release is OLDER than installed; the CLI accepts it (the
		// forward-only guard does not apply to an explicitly named version) and
		// hands it to the installer, which prints the "as requested" notice.
		code, stdout, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box, BuildInfo: BuildInfo{Version: "v0.2.0"}},
			"upgrade", "--version", "v0.1.0")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called || rec.plan.TargetVersion != "v0.1.0" {
			t.Fatalf("plan.TargetVersion = %q, want v0.1.0 (called=%v)", rec.plan.TargetVersion, rec.called)
		}
		if !strings.Contains(stdout, "v0.1.0") || !strings.Contains(stdout, "any direction") {
			t.Errorf("expected an explicit-version notice naming the direction:\n%s", stdout)
		}
	})

	t.Run("rollback still runs the guard and sets the mode", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-done", Complete: true, Format: 1}}}
		code, _, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box}, "upgrade", "--rollback")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called || !rec.plan.Rollback {
			t.Fatalf("rollback not propagated: called=%v plan=%+v", rec.called, rec.plan)
		}
	})

	t.Run("an unwired faber-box path is a hard error, not a partial upgrade", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{}
		code, _, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: ""}, "upgrade")
		if code != 1 || !strings.Contains(stderr, "faber-box path is not wired") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if rec.called {
			t.Fatal("installer must not run when the coupled faber-box path is unresolved")
		}
	})
}

// Verifies the embedded==released identity that the whole security argument
// rests on: the install.sh embedded into the binary (config/install.sh) is
// byte-identical to the released, README-verified repo-root install.sh. Run
// `go generate ./config` after editing the canonical script.
func TestUpgradeEmbeddedMatchesReleased(t *testing.T) {
	released, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatalf("read released install.sh: %v", err)
	}
	if !bytes.Equal(released, installScript) {
		t.Fatalf("embedded config/install.sh differs from the released ../install.sh — run `go generate ./config` to resync")
	}
}

// Verifies the plan→argv mapping the embedded script parses: the mode flags
// are mutually consistent (--upgrade vs --rollback), --current is passed only
// for a stamped version, the release pin travels as VERSION in the env (not a
// flag), and no --force is ever emitted (the active-runs guard is Go-side and
// the forward-only anomaly refusal is not overridable, so the script exposes
// no override flag). VERSION set vs unset is the whole latest-vs-explicit
// distinction the script's version guard turns on.
func TestUpgradePlanArgs(t *testing.T) {
	has := func(ss []string, want string) bool {
		for _, s := range ss {
			if s == want {
				return true
			}
		}
		return false
	}
	// hasSeq reports whether flag is immediately followed by val.
	hasSeq := func(ss []string, flag, val string) bool {
		for i := 0; i < len(ss)-1; i++ {
			if ss[i] == flag && ss[i+1] == val {
				return true
			}
		}
		return false
	}

	t.Run("upgrade to a specific version (explicit-version path)", func(t *testing.T) {
		p := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", CurrentVersion: "v1.0.0"}
		args := p.args()
		if !has(args, "--upgrade") {
			t.Errorf("args missing --upgrade: %v", args)
		}
		// No override flag is ever emitted.
		if has(args, "--force") {
			t.Errorf("args must never carry --force: %v", args)
		}
		if !hasSeq(args, "--target", "/f") || !hasSeq(args, "--box-target", "/b") || !hasSeq(args, "--current", "v1.0.0") {
			t.Errorf("args missing a target/current pairing: %v", args)
		}
		if has(args, "--rollback") || has(args, "--check") {
			t.Errorf("upgrade args leaked a rollback/dry-run flag: %v", args)
		}
		// The release pin travels as VERSION in the env, never as a flag —
		// VERSION set is what puts the script on the explicit-version path.
		if has(args, "v1.2.3") || has(args, "--version") {
			t.Errorf("target version must not appear in argv: %v", args)
		}
		if !has(p.scriptEnv(), "VERSION=v1.2.3") {
			t.Error("scriptEnv missing VERSION=v1.2.3")
		}
	})

	t.Run("forward-only latest path sends no VERSION and passes --current", func(t *testing.T) {
		p := UpgradePlan{FaberPath: "/f", BoxPath: "/b", CurrentVersion: "v1.0.0"}
		args := p.args()
		if !hasSeq(args, "--current", "v1.0.0") {
			t.Errorf("latest path must still pass --current so the script can order: %v", args)
		}
		// Empty TargetVersion ⇒ no VERSION in the env ⇒ the script's
		// forward-only latest path.
		for _, e := range p.scriptEnv() {
			if strings.HasPrefix(e, "VERSION=") {
				t.Errorf("latest path must not set VERSION: %q", e)
			}
		}
	})

	t.Run("dev current version is not passed as --current", func(t *testing.T) {
		args := UpgradePlan{FaberPath: "/f", BoxPath: "/b", CurrentVersion: "dev"}.args()
		if has(args, "--current") {
			t.Errorf("dev build must not send --current: %v", args)
		}
	})

	t.Run("rollback carries no upgrade, current, or version signal", func(t *testing.T) {
		p := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", Rollback: true}
		args := p.args()
		if !has(args, "--rollback") {
			t.Errorf("rollback args missing --rollback: %v", args)
		}
		if has(args, "--upgrade") || has(args, "--current") {
			t.Errorf("rollback args must not select upgrade or --current: %v", args)
		}
		if has(p.scriptEnv(), "VERSION=v1.2.3") {
			t.Error("rollback must not carry a VERSION pin")
		}
	})

	t.Run("dry-run is an upgrade-mode variant", func(t *testing.T) {
		args := UpgradePlan{FaberPath: "/f", BoxPath: "/b", DryRun: true}.args()
		if !has(args, "--upgrade") || !has(args, "--check") {
			t.Errorf("dry-run args = %v", args)
		}
	})

	// An ambient VERSION exported in the operator's shell must not leak into the
	// child env: the embedded script keys the explicit-release path vs the
	// forward-only latest path solely on VERSION being set, so a stray VERSION
	// would flip a plain `upgrade` onto the explicit path and defeat the
	// non-overridable forward-only anomaly refusal. The test-only origin seams
	// FABER_API_BASE / FABER_DL_BASE must likewise never leak from the operator's
	// shell — an ambient override would redirect where a production `upgrade`
	// fetches from. scriptEnv strips all three and sets VERSION only from the
	// --version pin.
	t.Run("ambient VERSION and origin bases are stripped; only the --version pin sets VERSION", func(t *testing.T) {
		t.Setenv("VERSION", "v9.9.9")
		t.Setenv("FABER_API_BASE", "http://attacker.example/api")
		t.Setenv("FABER_DL_BASE", "http://attacker.example/dl")

		// hasBase reports whether any origin-base override survived into the env.
		hasBase := func(env []string) bool {
			for _, e := range env {
				if strings.HasPrefix(e, "FABER_API_BASE=") || strings.HasPrefix(e, "FABER_DL_BASE=") {
					return true
				}
			}
			return false
		}

		// Plain forward-only latest path: empty TargetVersion ⇒ no VERSION at all,
		// not even the ambient v9.9.9 inherited from the shell; and neither origin
		// base survives.
		latest := UpgradePlan{FaberPath: "/f", BoxPath: "/b", CurrentVersion: "v1.0.0"}
		for _, e := range latest.scriptEnv() {
			if strings.HasPrefix(e, "VERSION=") {
				t.Errorf("ambient VERSION leaked onto the latest path: %q", e)
			}
		}
		if hasBase(latest.scriptEnv()) {
			t.Errorf("an ambient origin base leaked onto the latest path: %v", latest.scriptEnv())
		}

		// Explicit path: exactly the pinned VERSION is present — never the ambient
		// one — and the origin bases are still stripped.
		pinned := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", CurrentVersion: "v1.0.0"}
		var got []string
		for _, e := range pinned.scriptEnv() {
			if strings.HasPrefix(e, "VERSION=") {
				got = append(got, e)
			}
		}
		if len(got) != 1 || got[0] != "VERSION=v1.2.3" {
			t.Errorf("scriptEnv VERSION entries = %v, want exactly [VERSION=v1.2.3] (not the ambient v9.9.9)", got)
		}
		if hasBase(pinned.scriptEnv()) {
			t.Errorf("an ambient origin base leaked onto the explicit path: %v", pinned.scriptEnv())
		}
	})
}
