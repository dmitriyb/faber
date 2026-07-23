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

// Verifies the upgrade guard: `faber upgrade` runs the same read-only
// pre-upgrade check as `faber upgrade-check` BEFORE any download or replace —
// it refuses (exit 1) while live or unfinished runs exist and does not invoke
// the installer; --force acknowledges and proceeds. A clean store runs the
// installer with the resolved plan.
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
		for _, want := range []string{"r-live", "not upgraded mid-run"} {
			if !strings.Contains(stdout+stderr, want) {
				t.Errorf("output missing %q:\n%s%s", want, stdout, stderr)
			}
		}
	})

	t.Run("force proceeds past a live run and runs the installer", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-live", Live: true, Format: 1}}}
		code, stdout, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box}, "upgrade", "--force")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called {
			t.Fatal("installer did not run under --force")
		}
		if !rec.plan.Force {
			t.Error("plan.Force not propagated to the installer")
		}
		if !strings.Contains(stdout, "--force") {
			t.Errorf("expected a --force acknowledgement:\n%s", stdout)
		}
	})

	t.Run("clean store runs the installer with the resolved plan", func(t *testing.T) {
		rec := &recordInstaller{}
		audit := fakeAudit{runs: []RunAudit{{RunID: "r-done", Complete: true, Format: 1}}}
		code, _, stderr := runCLI(t, Deps{Audit: audit, Installer: rec, BoxBinary: box, BuildInfo: BuildInfo{Version: "v0.1.2"}},
			"upgrade", "--version", "v0.1.9")
		if code != 0 {
			t.Fatalf("got exit %d, want 0: %s", code, stderr)
		}
		if !rec.called {
			t.Fatal("installer did not run for a clean store")
		}
		if rec.plan.TargetVersion != "v0.1.9" {
			t.Errorf("plan.TargetVersion = %q, want v0.1.9", rec.plan.TargetVersion)
		}
		if rec.plan.CurrentVersion != "v0.1.2" {
			t.Errorf("plan.CurrentVersion = %q, want v0.1.2", rec.plan.CurrentVersion)
		}
		if rec.plan.BoxPath != box {
			t.Errorf("plan.BoxPath = %q, want %q", rec.plan.BoxPath, box)
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
// flag), and --force is orthogonal to the mode.
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

	t.Run("upgrade to a specific version, forced", func(t *testing.T) {
		p := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", CurrentVersion: "v1.0.0", Force: true}
		args := p.args()
		if !has(args, "--upgrade") || !has(args, "--force") {
			t.Errorf("args missing --upgrade/--force: %v", args)
		}
		if !hasSeq(args, "--target", "/f") || !hasSeq(args, "--box-target", "/b") || !hasSeq(args, "--current", "v1.0.0") {
			t.Errorf("args missing a target/current pairing: %v", args)
		}
		if has(args, "--rollback") || has(args, "--check") {
			t.Errorf("upgrade args leaked a rollback/dry-run flag: %v", args)
		}
		// The release pin travels as VERSION in the env, never as a flag.
		if has(args, "v1.2.3") || has(args, "--version") {
			t.Errorf("target version must not appear in argv: %v", args)
		}
		if !has(p.scriptEnv(), "VERSION=v1.2.3") {
			t.Error("scriptEnv missing VERSION=v1.2.3")
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
}
