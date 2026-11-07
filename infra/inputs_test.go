package infra

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// fakeELF is the smallest header debug/elf accepts: a 64-bit little-endian
// executable for the given Go arch, no program or section headers.
func fakeELF(goarch string, tail string) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1               // ELFCLASS64, little-endian, EV_CURRENT
	binary.LittleEndian.PutUint16(b[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(b[18:], uint16(elfMachines[goarch]))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64) // e_ehsize
	binary.LittleEndian.PutUint16(b[54:], 56) // e_phentsize
	binary.LittleEndian.PutUint16(b[58:], 64) // e_shentsize
	return append(b, tail...)
}

// testBox writes a faber-box stand-in the builder accepts: a linux ELF of
// the host arch with a marker tail, so two stand-ins can differ by bytes.
func testBox(t *testing.T) string { return testBoxWith(t, "box-v1") }

func testBoxWith(t *testing.T, tail string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "faber-box")
	if err := os.WriteFile(path, fakeELF(runtime.GOARCH, tail), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeHook(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSkill(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Verifies 0acac696dcf1: the engine inputs fold into the tag as bytes — a
// changed hook, a changed skill file or a different faber-box moves it; the
// same bytes read from other paths do not; a template without hooks or
// skills still folds the faber-box bytes.
func TestTagFoldsEngineInputs(t *testing.T) {
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testBox(t), testLogger())
	build := config.BuildDef{Packages: []string{"git"}}
	hooks := t.TempDir()
	ctxHook := writeHook(t, hooks, "context", "#!/bin/sh\necho ctx\n")
	skills := writeSkill(t, t.TempDir(), map[string]string{"SKILL.md": "skill a", "nested/extra.md": "more"})
	inputs := ImageInputs{
		Hooks:  config.HookSet{Context: ctxHook, OnFailure: "/nope/on-failure"}, // on_failure is never read
		Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: skills}}, Primary: "a", Link: ".x/skills"},
	}
	tag := func(bb *ImageBuilder, in ImageInputs) string {
		t.Helper()
		got, err := bb.ImageTag("review", build, in)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := tag(b, inputs)
	bare := tag(b, ImageInputs{})
	if base == bare {
		t.Fatal("hooks and skills did not enter the tag")
	}
	// same bytes, other paths: identical
	hooks2 := t.TempDir()
	ctx2 := writeHook(t, hooks2, "context", "#!/bin/sh\necho ctx\n")
	skills2 := writeSkill(t, t.TempDir(), map[string]string{"SKILL.md": "skill a", "nested/extra.md": "more"})
	same := tag(b, ImageInputs{Hooks: config.HookSet{Context: ctx2}, Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: skills2}}, Primary: "a", Link: ".y"}})
	if same != base {
		t.Fatalf("same bytes from other paths moved the tag: %s vs %s", same, base)
	}
	// one hook byte
	writeHook(t, hooks2, "context", "#!/bin/sh\necho ctx!\n")
	if tag(b, ImageInputs{Hooks: config.HookSet{Context: ctx2}, Skills: inputs.Skills}) == base {
		t.Fatal("a hook edit did not move the tag")
	}
	// one skill byte, and the slot the hook sits in
	os.WriteFile(filepath.Join(skills2, "nested", "extra.md"), []byte("less"), 0o644)
	if tag(b, ImageInputs{Hooks: inputs.Hooks, Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: skills2}}}}) == base {
		t.Fatal("a skill edit did not move the tag")
	}
	if tag(b, ImageInputs{Hooks: config.HookSet{Prelude: ctxHook}, Skills: inputs.Skills}) == base {
		t.Fatal("the same script in another slot did not move the tag")
	}
	// another faber-box
	b2 := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testBoxWith(t, "box-v2"), testLogger())
	if tag(b2, inputs) == base {
		t.Fatal("a different faber-box did not move the tag")
	}
	if tag(b2, ImageInputs{}) == bare {
		t.Fatal("a template with no hooks or skills ignores the faber-box bytes")
	}
}

// Verifies 0acac696dcf1: faber build refuses, before anything is staged, a
// faber-box that is not a linux ELF of the image's arch, a hook that fails
// the hook-file check, a skills source that is not a directory, and a
// missing faber-box — each naming the template and the path.
func TestBuildRefusesBadInputs(t *testing.T) {
	build := config.BuildDef{Packages: []string{"git"}}
	nix := &fakeNix{}
	try := func(boxPath string, in ImageInputs) error {
		b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", boxPath, testLogger())
		_, err := b.Build(context.Background(), "review", build, in)
		return err
	}
	want := func(err error, parts ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("want a refusal containing %q", parts)
		}
		for _, p := range parts {
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error %q lacks %q", err, p)
			}
		}
	}
	script := filepath.Join(t.TempDir(), "faber-box")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	want(try(script, ImageInputs{}), script, "not a linux ELF")
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	wrong := filepath.Join(t.TempDir(), "faber-box")
	os.WriteFile(wrong, fakeELF(other, ""), 0o755)
	want(try(wrong, ImageInputs{}), wrong, "the image needs linux/"+runtime.GOARCH)
	want(try("", ImageInputs{}), "no faber-box binary to bake")
	want(try(filepath.Join(t.TempDir(), "absent"), ImageInputs{}), "faber-box to bake")

	noexec := filepath.Join(t.TempDir(), "prelude")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	want(try(testBox(t), ImageInputs{Hooks: config.HookSet{Prelude: noexec}}), "templates.review.hooks.prelude", noexec, "not executable")
	want(try(testBox(t), ImageInputs{Hooks: config.HookSet{Context: "/nope/context"}}), "templates.review.hooks.context", "not found")
	file := filepath.Join(t.TempDir(), "SKILL.md")
	os.WriteFile(file, []byte("x"), 0o644)
	want(try(testBox(t), ImageInputs{Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: file}}}}), "templates.review.skills", `skill "a"`, "is not a directory")
	want(try(testBox(t), ImageInputs{Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "../a", Dir: t.TempDir()}}}}), "unsafe name")
	if nix.buildCount() != 0 {
		t.Fatalf("a refused build reached nix %d times", nix.buildCount())
	}
}

// Verifies 0acac696dcf1: the rendered expression carries the faber-inputs
// derivation — one install line per declared slot, the skills copy only with
// a leg — and the build stages exactly the bytes the tag hashed beside the
// expression: faber-box, hooks/<slot>, skills/<name>/<rel>; an inline root is
// staged with no name wrapper; a template without inputs stages faber-box
// alone.
func TestBuildStagesEngineInputs(t *testing.T) {
	build := config.BuildDef{Packages: []string{"git"}}
	box := testBox(t)
	hooks := t.TempDir()
	pre := writeHook(t, hooks, "prelude", "#!/bin/sh\npre\n")
	post := writeHook(t, hooks, "postlude", "#!/bin/sh\npost\n")
	skillA := writeSkill(t, t.TempDir(), map[string]string{"SKILL.md": "A", "deep/x.md": "X"})
	inputs := ImageInputs{Hooks: config.HookSet{Prelude: pre, Postlude: post}, Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: skillA}}}}

	nix := &fakeNix{buildOut: []string{"/nix/store/aaa-image.tar.gz"}}
	probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", box, testLogger())
	tag, err := probe.ImageTag("review", build, inputs)
	if err != nil {
		t.Fatal(err)
	}
	b := NewImageBuilder(&fakeDocker{loadTag: tag}, nix, testPin(), "", box, testLogger())
	if _, err := b.Build(context.Background(), "review", build, inputs); err != nil {
		t.Fatal(err)
	}
	expr := nix.buildExprs[0]
	for _, want := range []string{
		`pkgs.runCommand "faber-inputs"`,
		"install -m 0755 ${./faber-box} $out/faber/bin/faber-box",
		"install -m 0755 ${./hooks/prelude} $out/faber/hooks/prelude",
		"install -m 0755 ${./hooks/postlude} $out/faber/hooks/postlude",
		"cp -r ${./skills} $out/faber/skills",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("image expression lacks %q:\n%s", want, expr)
		}
	}
	if strings.Contains(expr, "hooks/context") {
		t.Errorf("an undeclared slot was rendered:\n%s", expr)
	}
	tree := nix.buildTrees[0]
	boxBytes, _ := os.ReadFile(box)
	for rel, want := range map[string]string{"faber-box": string(boxBytes), "hooks/prelude": "#!/bin/sh\npre\n", "hooks/postlude": "#!/bin/sh\npost\n", "skills/a/SKILL.md": "A", "skills/a/deep/x.md": "X"} {
		if string(tree[rel]) != want {
			t.Errorf("staged %s = %q, want %q (tree %v)", rel, tree[rel], want, keys(tree))
		}
	}
	if len(tree) != 5 {
		t.Errorf("staged tree %v, want exactly the five inputs", keys(tree))
	}

	// inline root: no <name> wrapper; no hooks: faber-box alone
	root := writeSkill(t, t.TempDir(), map[string]string{"s/SKILL.md": "S"})
	nix2 := &fakeNix{buildOut: []string{"/nix/store/bbb-image.tar.gz"}}
	probe2 := NewImageBuilder(&fakeDocker{}, nix2, testPin(), "", box, testLogger())
	in2 := ImageInputs{Skills: &config.ResolvedSkills{Root: root}}
	tag2, _ := probe2.ImageTag("merge", build, in2)
	b2 := NewImageBuilder(&fakeDocker{loadTag: tag2}, nix2, testPin(), "", box, testLogger())
	if _, err := b2.Build(context.Background(), "merge", build, in2); err != nil {
		t.Fatal(err)
	}
	if tree := nix2.buildTrees[0]; string(tree["skills/s/SKILL.md"]) != "S" || len(tree) != 2 {
		t.Errorf("inline root staged as %v", keys(tree))
	}
	if strings.Contains(nix2.buildExprs[0], "install -m 0755 ${./hooks") {
		t.Errorf("hook lines rendered without hooks:\n%s", nix2.buildExprs[0])
	}
	nix3 := &fakeNix{buildOut: []string{"/nix/store/ccc-image.tar.gz"}}
	probe3 := NewImageBuilder(&fakeDocker{}, nix3, testPin(), "", box, testLogger())
	tag3, _ := probe3.ImageTag("bare", build, ImageInputs{})
	b3 := NewImageBuilder(&fakeDocker{loadTag: tag3}, nix3, testPin(), "", box, testLogger())
	if _, err := b3.Build(context.Background(), "bare", build, ImageInputs{}); err != nil {
		t.Fatal(err)
	}
	if tree := nix3.buildTrees[0]; len(tree) != 1 || tree["faber-box"] == nil {
		t.Errorf("bare template staged %v, want faber-box alone", keys(tree))
	}
	if strings.Contains(nix3.buildExprs[0], "cp -r ${./skills}") {
		t.Errorf("skills copy rendered without a leg:\n%s", nix3.buildExprs[0])
	}
}

// Verifies 0acac696dcf1: the config.ImageBuilder seam resolves hooks and
// skills the way the desugarer does — a bare hook name through the library —
// so the staged bytes are the library script's.
func TestConfigBuilderSeamBakesResolvedInputs(t *testing.T) {
	hookPath := writeHook(t, t.TempDir(), "release", "#!/bin/sh\nrelease\n")
	build := config.BuildDef{Packages: []string{"git"}}
	cfg := &config.Config{
		Hooks:     map[string]config.HookDef{"release": {Path: hookPath}},
		Templates: map[string]config.TemplateDef{"merge": {Build: &build, Hooks: config.HookSet{Prelude: "release"}}},
	}
	nix := &fakeNix{buildOut: []string{"/nix/store/fff-image.tar.gz"}}
	box := testBox(t)
	probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", box, testLogger())
	tag, err := probe.ImageTag("merge", build, ImageInputs{Hooks: config.HookSet{Prelude: hookPath}})
	if err != nil {
		t.Fatal(err)
	}
	seam := NewImageBuilder(&fakeDocker{loadTag: tag}, nix, testPin(), "", box, testLogger()).ConfigBuilder()
	if err := seam.BuildImage(context.Background(), cfg, "merge", testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := string(nix.buildTrees[0]["hooks/prelude"]); got != "#!/bin/sh\nrelease\n" {
		t.Fatalf("staged prelude = %q, want the library script", got)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Verifies 0acac696dcf1: a symlinked skills directory — a root or a named
// source — is followed: its files bake and fold into the tag exactly as the
// real directory's do (a walk over the link itself would stage nothing and
// say nothing).
func TestSkillsFollowSymlinkedRoot(t *testing.T) {
	real := writeSkill(t, t.TempDir(), map[string]string{"a/SKILL.md": "A"})
	link := filepath.Join(t.TempDir(), "skills-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testBox(t), testLogger())
	build := config.BuildDef{Packages: []string{"git"}}
	viaReal, err := b.ImageTag("review", build, ImageInputs{Skills: &config.ResolvedSkills{Root: real}})
	if err != nil {
		t.Fatal(err)
	}
	viaLink, err := b.ImageTag("review", build, ImageInputs{Skills: &config.ResolvedSkills{Root: link}})
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := b.ImageTag("review", build, ImageInputs{Skills: &config.ResolvedSkills{Root: t.TempDir()}})
	if viaLink != viaReal || viaLink == empty {
		t.Fatalf("symlinked root: link %s real %s empty %s", viaLink, viaReal, empty)
	}
	files, err := readTree(filepath.Join(link, "a"), "a")
	if err != nil || len(files) != 1 || files[0].rel != "a/SKILL.md" {
		t.Fatalf("named source through a link: %+v %v", files, err)
	}
}

// Verifies 0acac696dcf1: the staged skill file keeps the executable bit the
// tag folded, and a non-executable one stays 0644.
func TestStagedSkillModes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("S"), 0o644)
	in, err := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testBox(t), testLogger()).readInputs("t", config.BuildDef{}, ImageInputs{Skills: &config.ResolvedSkills{Sources: []config.SkillSource{{Name: "a", Dir: dir}}}})
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := stageInputs(stage, in); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(stage, "skills", "a", "run.sh"))
	md, _ := os.Stat(filepath.Join(stage, "skills", "a", "SKILL.md"))
	if st.Mode().Perm() != 0o755 || md.Mode().Perm() != 0o644 {
		t.Fatalf("staged modes run.sh=%v SKILL.md=%v", st.Mode().Perm(), md.Mode().Perm())
	}
}

// Verifies 0acac696dcf1: an ELF of the right machine but another OS ABI is
// refused too — a FreeBSD build cannot run in a linux box.
func TestCheckLinuxELFRefusesOtherOSABI(t *testing.T) {
	bsd := fakeELF(runtime.GOARCH, "")
	bsd[7] = 9 // ELFOSABI_FREEBSD
	err := checkLinuxELF(bsd, runtime.GOARCH)
	if err == nil || !strings.Contains(err.Error(), "needs linux") {
		t.Fatalf("FreeBSD ELF accepted or misreported: %v", err)
	}
	if err := checkLinuxELF(fakeELF(runtime.GOARCH, ""), runtime.GOARCH); err != nil {
		t.Fatalf("a linux ELF of the host arch refused: %v", err)
	}
}

// Verifies 0acac696dcf1: a hook the image cannot bake is a typed error that
// names the template, the slot and the path and unwraps to the hook-file
// check's reason.
func TestHookInputErrorIsTyped(t *testing.T) {
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testBox(t), testLogger())
	_, err := b.ImageTag("review", config.BuildDef{Packages: []string{"git"}}, ImageInputs{Hooks: config.HookSet{Prelude: filepath.Join(t.TempDir(), "absent")}})
	var hook *HookInputError
	if !errors.As(err, &hook) || hook.Template != "review" || hook.Slot != "prelude" || !errors.Is(err, config.ErrHookNotFound) {
		t.Fatalf("want a typed hook error unwrapping to not found, got %v", err)
	}
}
