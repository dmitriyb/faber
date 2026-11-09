package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Verifies 67c77533453d (test scenario 18): a project is its config file —
// the id is the same for the path's spellings and a symlink to it, and
// differs between two files — and the volume names hang off it.
func TestProjectID(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "orchestrator.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := ProjectID(cfg)
	if len(id) != 12 {
		t.Fatalf("id %q, want 12 hex digits", id)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(cfg, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if ProjectID(link) != id {
		t.Fatalf("a symlink to the config is another project: %s vs %s", ProjectID(link), id)
	}
	wd, _ := os.Getwd()
	if rel, err := filepath.Rel(wd, cfg); err == nil && ProjectID(rel) != id {
		t.Fatalf("a relative spelling is another project: %s vs %s", ProjectID(rel), id)
	}
	other := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(other, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ProjectID(other) == id {
		t.Fatal("two files share a project id")
	}
	if VolumeName(id, "cache") != "faber-"+id+"-cache" || VolumePrefix(id) != "faber-"+id+"-" {
		t.Fatalf("volume naming: %s %s", VolumeName(id, "cache"), VolumePrefix(id))
	}
}
