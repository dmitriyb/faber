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

// Verifies the plan→environment mapping the embedded script reads: the mode
// signals are mutually consistent (upgrade vs rollback), the target version is
// only passed when requested, and force is orthogonal to the mode.
func TestUpgradePlanEnv(t *testing.T) {
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}

	t.Run("upgrade to a specific version, forced", func(t *testing.T) {
		env := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", CurrentVersion: "v1.0.0", Force: true}.env()
		for _, want := range []string{"FABER_UPGRADE=1", "VERSION=v1.2.3", "FABER_TARGET=/f", "FABER_BOX_TARGET=/b", "FABER_CURRENT_VERSION=v1.0.0", "FABER_UPGRADE_FORCE=1"} {
			if !has(env, want) {
				t.Errorf("env missing %q", want)
			}
		}
		if has(env, "FABER_ROLLBACK=1") || has(env, "FABER_DRY_RUN=1") {
			t.Error("upgrade env leaked a rollback/dry-run signal")
		}
	})

	t.Run("rollback carries no upgrade or version signal", func(t *testing.T) {
		env := UpgradePlan{FaberPath: "/f", BoxPath: "/b", TargetVersion: "v1.2.3", Rollback: true}.env()
		if !has(env, "FABER_ROLLBACK=1") {
			t.Error("rollback env missing FABER_ROLLBACK=1")
		}
		if has(env, "FABER_UPGRADE=1") || has(env, "VERSION=v1.2.3") {
			t.Error("rollback env must not select upgrade or a target version")
		}
	})

	t.Run("dry-run is an upgrade-mode variant", func(t *testing.T) {
		env := UpgradePlan{FaberPath: "/f", BoxPath: "/b", DryRun: true}.env()
		if !has(env, "FABER_UPGRADE=1") || !has(env, "FABER_DRY_RUN=1") {
			t.Errorf("dry-run env = %v", env)
		}
	})
}
