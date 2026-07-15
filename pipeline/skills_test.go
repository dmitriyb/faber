package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// Verifies the run-prep skills stager for the three ResolvedSkills shapes (see
// spec/pipeline/impl_scheduling.md "Skills staging"): a named Sources set is
// COPIED into a per-attempt tree of real files and returns the stage dir; an
// inline Root is returned directly (no copy, no <name> wrapper); an absent leg
// yields no mount. In every case exactly one host path (or none) feeds the
// single /faber/skills bind, so infra's argv builder sees the unchanged
// one-mount contract.
func TestStageSkills(t *testing.T) {
	t.Run("named sources copy real, readable files through", func(t *testing.T) {
		attempt := t.TempDir()
		dirA := t.TempDir()
		dirB := t.TempDir()
		// A nested file under a subdirectory must be copied through, not just the
		// top-level SKILL.md.
		if err := os.WriteFile(filepath.Join(dirA, "SKILL.md"), []byte("skill A body"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dirA, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirA, "nested", "extra.md"), []byte("nested body"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirB, "SKILL.md"), []byte("skill B body"), 0o600); err != nil {
			t.Fatal(err)
		}

		rs := &config.ResolvedSkills{
			Sources: []config.SkillSource{{Name: "a", Dir: dirA}, {Name: "b", Dir: dirB}},
			Primary: "a",
			Link:    ".claude/skills",
		}
		host, cleanup, err := stageSkills(rs, attempt)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if host != filepath.Join(attempt, "skills") {
			t.Fatalf("named stage host = %q, want <attempt>/skills", host)
		}

		// The staged entries must be REAL files/dirs, never symlinks: a symlink
		// target is a host path that is not mounted into the container and would
		// dangle. This is the assertion that fails against the old symlink farm.
		for _, name := range []string{"a", "b"} {
			entry := filepath.Join(host, name)
			li, err := os.Lstat(entry)
			if err != nil {
				t.Fatalf("lstat %s: %v", name, err)
			}
			if li.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("staged %s is a symlink; it must be a copied real directory", name)
			}
			if !li.IsDir() {
				t.Fatalf("staged %s is not a directory", name)
			}
		}

		// The SKILL.md files must be readable real files with the source contents.
		for name, want := range map[string]string{"a": "skill A body", "b": "skill B body"} {
			p := filepath.Join(host, name, "SKILL.md")
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatalf("lstat %s/SKILL.md: %v", name, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("%s/SKILL.md is a symlink; it must be a copied real file", name)
			}
			// World-readable so the non-root run user can read it through the :ro bind.
			if fi.Mode().Perm()&0o004 == 0 {
				t.Fatalf("%s/SKILL.md perms = %v, want world-readable", name, fi.Mode().Perm())
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s/SKILL.md: %v", name, err)
			}
			if string(got) != want {
				t.Fatalf("%s/SKILL.md = %q, want %q", name, got, want)
			}
		}

		// The nested subtree under source "a" is copied through.
		nested := filepath.Join(host, "a", "nested", "extra.md")
		got, err := os.ReadFile(nested)
		if err != nil {
			t.Fatalf("read nested copied file: %v", err)
		}
		if string(got) != "nested body" {
			t.Fatalf("nested file = %q, want %q", got, "nested body")
		}

		// Teardown removes the stage tree.
		cleanup()
		if _, err := os.Stat(host); !os.IsNotExist(err) {
			t.Fatalf("cleanup must remove the stage tree, stat err = %v", err)
		}
	})

	t.Run("restaging over a leftover tree is crash-safe", func(t *testing.T) {
		// A reused session dir (interactive re-entry) can carry a leftover stage
		// tree from a hard crash; restaging must not fail EEXIST and must not leak
		// stale files.
		session := t.TempDir()
		dirA := t.TempDir()
		if err := os.WriteFile(filepath.Join(dirA, "SKILL.md"), []byte("fresh"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Plant a leftover farm with a stale entry.
		stale := filepath.Join(session, "skills", "stale")
		if err := os.MkdirAll(stale, 0o755); err != nil {
			t.Fatal(err)
		}
		rs := &config.ResolvedSkills{
			Sources: []config.SkillSource{{Name: "a", Dir: dirA}},
			Primary: "a",
			Link:    ".claude/skills",
		}
		host, cleanup, err := stageSkills(rs, session)
		if err != nil {
			t.Fatalf("restage: %v", err)
		}
		defer cleanup()
		if _, err := os.Stat(filepath.Join(host, "stale")); !os.IsNotExist(err) {
			t.Fatalf("leftover stale entry must be cleared, stat err = %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(host, "a", "SKILL.md")); err != nil || string(got) != "fresh" {
			t.Fatalf("restaged content = %q err = %v, want %q", got, err, "fresh")
		}
	})

	t.Run("inline root is mounted directly", func(t *testing.T) {
		attempt := t.TempDir()
		root := t.TempDir()
		rs := &config.ResolvedSkills{Root: root, Primary: "x", Link: ".claude/skills"}
		host, cleanup, err := stageSkills(rs, attempt)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		defer cleanup()
		if host != root {
			t.Fatalf("inline host = %q, want the root %q (direct mount)", host, root)
		}
		// No staging directory is created for the direct mount.
		if _, err := os.Stat(filepath.Join(attempt, "skills")); !os.IsNotExist(err) {
			t.Fatalf("inline form must not stage a copy, stat err = %v", err)
		}
	})

	t.Run("absent leg yields no mount", func(t *testing.T) {
		host, cleanup, err := stageSkills(nil, t.TempDir())
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		defer cleanup()
		if host != "" {
			t.Fatalf("absent skills must yield no host path, got %q", host)
		}
	})

	t.Run("unsafe source name is rejected before any write escapes the stage", func(t *testing.T) {
		// Belt-and-suspenders: even if validation is bypassed and a traversal name
		// reaches run-prep, staging must refuse — never write outside <stage>. A
		// "../escape" name joined onto <attempt>/skills would land at
		// <attempt>/escape; assert that host path is never created.
		attempt := t.TempDir()
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("pwn"), 0o600); err != nil {
			t.Fatal(err)
		}
		rs := &config.ResolvedSkills{
			Sources: []config.SkillSource{{Name: "../escape", Dir: src}},
			Primary: "x",
			Link:    ".claude/skills",
		}
		host, cleanup, err := stageSkills(rs, attempt)
		if err == nil {
			cleanup()
			t.Fatal("an unsafe source name must error, staging must not proceed")
		}
		if host != "" {
			t.Fatalf("failed staging must return no host path, got %q", host)
		}
		// Nothing was written outside the stage dir, and the stage dir itself was
		// torn down on the failed attempt.
		if _, err := os.Stat(filepath.Join(attempt, "escape")); !os.IsNotExist(err) {
			t.Fatalf("traversal target must never be written, stat err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(attempt, "skills")); !os.IsNotExist(err) {
			t.Fatalf("stage tree must be removed on a rejected name, stat err = %v", err)
		}
	})
}
