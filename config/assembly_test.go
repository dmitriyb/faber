package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes content to dir/name (creating parent dirs) and returns the
// full path. Test configs are written to disk so the include/assembly path
// resolution is exercised end to end.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mustLoad assembles + validates a config, failing on either error.
func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	cfg, viols, err := Load(path)
	if err != nil {
		t.Fatalf("assemble %s: %v", path, err)
	}
	if err := Validate(cfg, viols); err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
	return cfg
}

const fragWorkflows = `
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps:
      - id: first
        use: box
        with: {input: "${params.subject}"}
`

// Scenario 9: a root project file with include: [images, templates, workflows]
// assembles to a Config whose library maps are the union of the fragments, and
// desugars to the SAME IR as the equivalent single-file config.
func TestAssembleIncludeMerge(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "images.yaml", "images:\n  base: {packages: [git]}\n")
	writeFile(t, dir, "templates.yaml", `
templates:
  box:
    image: base
    run: {env: {FABER_AGENT_CLI: agent-cli}}
    skill: act
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
`)
	writeFile(t, dir, "workflows.yaml", fragWorkflows)
	root := writeFile(t, dir, "orchestrator.yaml", `
version: 1
include: [images.yaml, templates.yaml, workflows.yaml]
`)
	cfg := mustLoad(t, root)
	if _, ok := cfg.Images["base"]; !ok {
		t.Fatal("images.base not merged from fragment")
	}
	if _, ok := cfg.Templates["box"]; !ok {
		t.Fatal("templates.box not merged")
	}
	if _, ok := cfg.Workflows["flow"]; !ok {
		t.Fatal("workflows.flow not merged")
	}

	// Equivalent single-file config with the inline build: form.
	single := writeFile(t, t.TempDir(), "orchestrator.yaml", `
version: 1
templates:
  box:
    build: {packages: [git]}
    run: {env: {FABER_AGENT_CLI: agent-cli}}
    skill: act
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
`+fragWorkflows)
	scfg := mustLoad(t, single)

	multiIR, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatalf("desugar assembled: %v", err)
	}
	singleIR, err := Desugar(scfg, "flow")
	if err != nil {
		t.Fatalf("desugar single: %v", err)
	}
	a, _ := EncodeIR(multiIR)
	b, _ := EncodeIR(singleIR)
	if string(a) != string(b) {
		t.Fatalf("assembled IR differs from single-file IR:\n%s\n---\n%s", a, b)
	}
}

// Scenario 10: two included files each defining templates.review — Assemble
// records the duplicate (naming both files) and Validate surfaces it, collected
// (not a hard stop, since a duplicate still yields a mergeable Config).
func TestAssembleDuplicateKeyAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.yaml", "templates:\n  review: {image: base, skill: a}\n")
	b := writeFile(t, dir, "b.yaml", "templates:\n  review: {image: base, skill: b}\n")
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [a.yaml, b.yaml]\n")

	cfg, viols, err := Load(root)
	if err != nil {
		t.Fatalf("duplicate key must not hard-stop: %v", err)
	}
	if len(viols) == 0 {
		t.Fatal("duplicate key across files must be recorded as a violation")
	}
	joined := Validate(cfg, viols).Error()
	if !strings.Contains(joined, "templates.review") ||
		!strings.Contains(joined, filepath.Base(a)) || !strings.Contains(joined, filepath.Base(b)) {
		t.Fatalf("duplicate violation must name both files, got:\n%s", joined)
	}
}

// Scenario 11: an include cycle hard-stops Assemble with the cycle chain (a
// cycle cannot yield a Config, so it is never a collected violation).
func TestAssembleIncludeCycleHardStop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.yaml", "include: [a.yaml]\n")
	root := writeFile(t, dir, "a.yaml", "version: 1\ninclude: [b.yaml]\n")
	_, _, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "include cycle") {
		t.Fatalf("include cycle must hard-stop with a cycle message, got %v", err)
	}
}

// Scenario 11 (other hard stop): an unreadable/unparseable included file hard-
// stops Assemble — a missing include means the config is not fully known.
func TestAssembleUnreadableIncludeHardStop(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [ghost.yaml]\n")
	if _, _, err := Load(root); err == nil {
		t.Fatal("unreadable include must hard-stop")
	}
}

// Scenario 11 (diamond): a->b, a->c, both b and c include d — d is merged once,
// not flagged as a duplicate, and assembly succeeds.
func TestAssembleDiamondMergedOnce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "d.yaml", "images:\n  shared: {packages: [git]}\n")
	writeFile(t, dir, "b.yaml", "include: [d.yaml]\n")
	writeFile(t, dir, "c.yaml", "include: [d.yaml]\n")
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [b.yaml, c.yaml]\n")
	cfg, viols, err := Load(root)
	if err != nil {
		t.Fatalf("diamond must succeed: %v", err)
	}
	if len(viols) != 0 {
		t.Fatalf("diamond must not flag a duplicate, got %v", viols)
	}
	if _, ok := cfg.Images["shared"]; !ok {
		t.Fatal("diamond target image not merged")
	}
}

// Scenario 11b (symlink diamond): b reaches the shared library via its real path
// d.yaml while c reaches the SAME file via a symlink alias d-alias.yaml. The two
// spellings must canonicalize to one identity (EvalSymlinks) so the file merges
// once and is not wrongly rejected as a duplicate key.
func TestAssembleDiamondThroughSymlinkMergedOnce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "d.yaml", "images:\n  shared: {packages: [git]}\n")
	if err := os.Symlink(filepath.Join(dir, "d.yaml"), filepath.Join(dir, "d-alias.yaml")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	writeFile(t, dir, "b.yaml", "include: [d.yaml]\n")
	writeFile(t, dir, "c.yaml", "include: [d-alias.yaml]\n")
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [b.yaml, c.yaml]\n")
	cfg, viols, err := Load(root)
	if err != nil {
		t.Fatalf("symlink diamond must succeed: %v", err)
	}
	if len(viols) != 0 {
		t.Fatalf("symlink diamond must not flag a duplicate, got %v", viols)
	}
	if _, ok := cfg.Images["shared"]; !ok {
		t.Fatal("symlink diamond target image not merged")
	}
}

// Scenario (substrate placement): a substrate key on a non-root included file is
// recorded as a violation and surfaced through Validate.
func TestAssembleSubstrateOnNonRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib.yaml", "identities:\n  rogue: {key: ./k}\n")
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [lib.yaml]\n")
	cfg, viols, err := Load(root)
	if err != nil {
		t.Fatalf("substrate on non-root must not hard-stop: %v", err)
	}
	joined := Validate(cfg, viols).Error()
	if !strings.Contains(joined, "lib.yaml") || !strings.Contains(joined, "may only contribute libraries") {
		t.Fatalf("substrate-on-non-root must be recorded, got:\n%s", joined)
	}
}

// Scenario 12: an included lib/images.yaml with overlay: ./overlay.nix resolves
// relative to the INCLUDED file's dir (lib/), not the process CWD nor the root's
// dir. Root-file paths (the CWD reference) are left verbatim.
func TestAssembleDeclarerRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/images.yaml", "images:\n  base: {packages: [git], overlay: ./overlay.nix}\n")
	root := writeFile(t, dir, "orchestrator.yaml", "version: 1\ninclude: [lib/images.yaml]\n")
	cfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	want := filepath.Join(dir, "lib", "overlay.nix")
	if got := cfg.Images["base"].Overlay; got != want {
		t.Fatalf("included overlay resolved to %q, want declarer-relative %q", got, want)
	}
}
