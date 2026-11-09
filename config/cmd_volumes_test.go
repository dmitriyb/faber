package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVolumes is a recording VolumeController.
type fakeVolumes struct {
	names   []string // what the daemon holds, any prefix
	prefix  string   // the last prefix asked for
	removed []string
}

func (f *fakeVolumes) ListVolumes(_ context.Context, prefix string) ([]string, error) {
	f.prefix = prefix
	var out []string
	for _, n := range f.names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeVolumes) RemoveVolume(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

// Verifies 67c77533453d (test scenario 18): the volumes group is thin
// dispatch — the listing is the controller's names under the project's
// prefix as template and docker names, prune removes each and is refused
// while a run is live, an unwired controller and an unknown positional are
// the structured errors.
func TestCLIVolumesGroup(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "orchestrator.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prefix := VolumePrefix(ProjectID(cfg))
	daemon := func() *fakeVolumes {
		return &fakeVolumes{names: []string{prefix + "cache", prefix + "models", "faber-other-cache", "unrelated"}}
	}

	t.Run("list shows the project's volumes", func(t *testing.T) {
		vols := daemon()
		code, stdout, stderr := runCLI(t, Deps{Volumes: vols}, "volumes", "--config", cfg)
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if vols.prefix != prefix {
			t.Fatalf("asked for prefix %q, want %q", vols.prefix, prefix)
		}
		for _, want := range []string{"cache", prefix + "cache", "models", prefix + "models"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("listing must contain %q:\n%s", want, stdout)
			}
		}
		if strings.Contains(stdout, "other") || strings.Contains(stdout, "unrelated") {
			t.Errorf("another project's volume listed:\n%s", stdout)
		}
	})

	t.Run("list --json", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, Deps{Volumes: daemon()}, "volumes", "--config", cfg, "--json")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		var rows []map[string]string
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("stdout must be a JSON array: %v\n%s", err, stdout)
		}
		if len(rows) != 2 || rows[0]["name"] != "cache" || rows[0]["docker_name"] != prefix+"cache" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("empty listing says so", func(t *testing.T) {
		code, stdout, _ := runCLI(t, Deps{Volumes: &fakeVolumes{}}, "volumes", "--config", cfg)
		if code != 0 || !strings.Contains(stdout, "no named volumes") {
			t.Fatalf("got %d: %s", code, stdout)
		}
	})

	t.Run("prune removes the project's volumes", func(t *testing.T) {
		vols := daemon()
		code, stdout, stderr := runCLI(t, Deps{Volumes: vols, Audit: fakeAudit{runs: []RunAudit{{RunID: "r-done", Complete: true}}}}, "volumes", "prune", "--config", cfg)
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if len(vols.removed) != 2 || vols.removed[0] != prefix+"cache" || vols.removed[1] != prefix+"models" {
			t.Fatalf("removed %q", vols.removed)
		}
		if !strings.Contains(stdout, "removed cache") {
			t.Errorf("prune must report what it removed:\n%s", stdout)
		}
	})

	t.Run("prune is refused while a run is live", func(t *testing.T) {
		vols := daemon()
		code, _, stderr := runCLI(t, Deps{Volumes: vols, Audit: fakeAudit{runs: []RunAudit{{RunID: "r-live", Live: true}}}}, "volumes", "prune", "--config", cfg)
		if code != 1 || !strings.Contains(stderr, "r-live is live") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if len(vols.removed) != 0 {
			t.Fatalf("a refused prune removed %q", vols.removed)
		}
	})

	t.Run("a config that does not exist is an error, not an empty project", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{Volumes: daemon()}, "volumes", "--config", filepath.Join(t.TempDir(), "typo.yaml"))
		if code != 1 || !strings.Contains(stderr, "typo.yaml") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("unwired controller is a structured error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "volumes", "--config", cfg)
		if code != 1 || !strings.Contains(stderr, "requires the infra module") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("unknown subcommand is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{Volumes: daemon()}, "volumes", "bogus")
		if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})
}
