package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
)

// fakeBoxBin is a faber-box stand-in the builder accepts: the smallest linux
// ELF header of the host arch.
func fakeBoxBin(t *testing.T) string {
	t.Helper()
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(b[16:], 2)
	machine := uint16(0x3E) // amd64
	if runtime.GOARCH == "arm64" {
		machine = 0xB7
	}
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[58:], 64)
	path := filepath.Join(t.TempDir(), "faber-box")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Verifies 0acac696dcf1 (regression): the run-time tag the imageTagger seam
// recomputes from a ResolvedTemplate equals the build-time tag faber build
// derives from the resolved BuildDef and the same hooks and skills — the pin
// and the engine inputs must both ride the reconstruction, or a run looks for
// an image under a tag nobody built. The memo hands the same tag back for the
// run's lifetime even after the hook changed on disk.
func TestRunTimeTagEqualsBuildTimeTag(t *testing.T) {
	b := infra.NewImageBuilder(nil, nil, infra.DefaultNixpkgsPin(), "", fakeBoxBin(t), nil)
	tagger := newImageTagger(b)
	hook := filepath.Join(t.TempDir(), "context")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nctx\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := &config.ResolvedTemplate{
		Name:     "box",
		Packages: []string{"git", "go"},
		Pin:      &config.PinDef{Rev: "25.11", SHA256: "sha256:deadbeef"},
		Hooks:    config.HookSet{Context: hook},
	}

	// Build-time path: the tag faber build derives from the resolved BuildDef
	// and the resolved hooks and skills.
	buildTag, err := b.ImageTag(pinned.Name, config.BuildDef{
		Packages: pinned.Packages,
		Overlay:  pinned.Overlay,
		Pin:      pinned.Pin,
	}, infra.ImageInputs{Hooks: pinned.Hooks, Skills: pinned.Skills})
	if err != nil {
		t.Fatal(err)
	}

	// Run-time path: the tag recomputed from the ResolvedTemplate via the seam.
	runTag, err := tagger.Tag(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if runTag != buildTag {
		t.Fatalf("pinned run-time tag %q != build-time tag %q (dropped template.Pin or the hooks?)", runTag, buildTag)
	}

	// The pin must actually move the tag: a pin-less template of the same shape
	// resolves to the default and MUST differ — otherwise the equality above
	// would hold trivially even with a dropped pin. Same for the hook.
	noPin := &config.ResolvedTemplate{Name: "box-nopin", Packages: pinned.Packages, Hooks: pinned.Hooks}
	defaultTag, err := tagger.Tag(noPin)
	if err != nil {
		t.Fatal(err)
	}
	if runTag == defaultTag {
		t.Fatalf("pinned and default tags must differ, both %q", runTag)
	}
	noHook := &config.ResolvedTemplate{Name: "box-nohook", Packages: pinned.Packages, Pin: pinned.Pin}
	noHookTag, err := tagger.Tag(noHook)
	if err != nil {
		t.Fatal(err)
	}
	if noHookTag == runTag {
		t.Fatalf("with and without the hook the tags must differ, both %q", runTag)
	}

	// The memo: an edit on disk does not move a tag already handed out in
	// this run; a fresh tagger sees the new bytes.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nctx edited\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	again, _ := tagger.Tag(pinned)
	if again != runTag {
		t.Fatalf("memoized tag moved mid-run: %q -> %q", runTag, again)
	}
	fresh, _ := newImageTagger(b).Tag(pinned)
	if fresh == runTag {
		t.Fatal("a fresh tagger did not see the edited hook")
	}
}

// Verifies 0acac696dcf1: the live view's image probe recomputes on every
// probe — it exists to pick up edits — while a run's tagger memoizes; the
// two must not share a memo.
func TestImageProbeRecomputes(t *testing.T) {
	b := infra.NewImageBuilder(nil, nil, infra.DefaultNixpkgsPin(), "", fakeBoxBin(t), nil)
	hook := filepath.Join(t.TempDir(), "context")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nctx\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tpl := &config.ResolvedTemplate{Name: "box", Packages: []string{"git"}, Hooks: config.HookSet{Context: hook}}
	probe := imageProbe{b: b}
	before, err := probe.Tag(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nctx edited\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := probe.Tag(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("the probe kept a stale tag %q after the hook changed", before)
	}
}
