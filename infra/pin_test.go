package infra

import (
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// Verifies the per-toolset pin resolution folds into the tag: two builds that
// differ only by a per-build pin produce DIFFERENT tags (the resolved pin's rev
// enters the hash), while a build with NO pin resolves to the compiled-in
// defaultPin and produces the SAME tag as a build whose pin equals that default
// — the byte-stability guarantee for pin-less toolsets.
func TestPerBuildPinAffectsTag(t *testing.T) {
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testLogger())
	pkgs := []string{"git", "go"}

	noPin := config.BuildDef{Packages: pkgs}
	explicitDefault := config.BuildDef{Packages: pkgs, Pin: &config.PinDef{Rev: testPin().Rev, SHA256: testPin().SHA256}}
	newerPin := config.BuildDef{Packages: pkgs, Pin: &config.PinDef{Rev: "25.11", SHA256: "sha256:deadbeef"}}

	noPinTag, err := b.ImageTag("box", noPin)
	if err != nil {
		t.Fatal(err)
	}
	defaultTag, err := b.ImageTag("box", explicitDefault)
	if err != nil {
		t.Fatal(err)
	}
	pinnedTag, err := b.ImageTag("box", newerPin)
	if err != nil {
		t.Fatal(err)
	}

	if noPinTag != defaultTag {
		t.Fatalf("a pin equal to the default must produce the default tag: %s vs %s", noPinTag, defaultTag)
	}
	if pinnedTag == noPinTag {
		t.Fatalf("a distinct per-build pin must change the tag, got %s for both", pinnedTag)
	}
	if !strings.HasPrefix(pinnedTag, "faber/box:") {
		t.Fatalf("unexpected tag shape %q", pinnedTag)
	}
}

// Verifies resolvePin: a build's pin overrides the default; a nil pin falls back
// to the compiled-in default.
func TestResolvePinFallback(t *testing.T) {
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testLogger())

	if got := b.resolvePin(config.BuildDef{}); got != testPin() {
		t.Fatalf("nil pin must resolve to the default, got %+v", got)
	}
	want := NixpkgsPin{Rev: "25.11", SHA256: "sha256:abc"}
	got := b.resolvePin(config.BuildDef{Pin: &config.PinDef{Rev: "25.11", SHA256: "sha256:abc"}})
	if got != want {
		t.Fatalf("build pin must override the default: got %+v want %+v", got, want)
	}
}
