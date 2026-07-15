package main

import (
	"testing"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
)

// Guards the run-time tag seam (wire.go's imageTagger reconstruction). For a
// PINNED toolset, the tag faber build computes from the resolved BuildDef must
// equal the tag the pipeline recomputes at run/resume from the node's
// ResolvedTemplate via imageTagger.Tag — which reconstructs
// BuildDef{Packages, Overlay, Pin: template.Pin}. Drop template.Pin from that
// reconstruction and resolvePin falls back to defaultPin, so the two tags
// diverge (image-not-found at run, journal input-hash never matches). ImageTag
// is pure (no docker/nix), so a bare builder suffices.
func TestRunTimeTagEqualsBuildTimeTag(t *testing.T) {
	b := infra.NewImageBuilder(nil, nil, infra.DefaultNixpkgsPin(), "", nil)
	tagger := imageTagger{b: b}

	pinned := &config.ResolvedTemplate{
		Name:     "box",
		Packages: []string{"git", "go"},
		Pin:      &config.PinDef{Rev: "25.11", SHA256: "sha256:deadbeef"},
	}

	// Build-time path: the tag faber build derives from the resolved BuildDef
	// (the pin carried by ResolveBuild).
	buildTag, err := b.ImageTag(pinned.Name, config.BuildDef{
		Packages: pinned.Packages,
		Overlay:  pinned.Overlay,
		Pin:      pinned.Pin,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run-time path: the tag recomputed from the ResolvedTemplate via the seam.
	runTag, err := tagger.Tag(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if runTag != buildTag {
		t.Fatalf("pinned run-time tag %q != build-time tag %q (dropped template.Pin?)", runTag, buildTag)
	}

	// The pin must actually move the tag: a pin-less template of the same shape
	// resolves to the default and MUST differ — otherwise the equality above
	// would hold trivially even with a dropped pin.
	noPin := &config.ResolvedTemplate{Name: pinned.Name, Packages: pinned.Packages}
	defaultTag, err := tagger.Tag(noPin)
	if err != nil {
		t.Fatal(err)
	}
	if runTag == defaultTag {
		t.Fatalf("pinned and default tags must differ, both %q", runTag)
	}
}
