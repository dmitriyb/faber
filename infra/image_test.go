package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
)

func testPin() NixpkgsPin {
	return NixpkgsPin{Rev: "24.05", SHA256: "sha256:1lr1h35prqkd1mkmzriwlpvxcb34kmhc9dnr48gkm8hh089hifmx"}
}

func writeOverlay(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "overlay.nix")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Verifies 0acac696dcf1: the image tag is a pure function of the pin, the
// sorted package list, and the overlay's bytes — order-insensitive, sensitive
// to every real input, computed with zero adapter calls.
func TestTagDeterminism(t *testing.T) {
	docker, nix := &fakeDocker{}, &fakeNix{}
	b := NewImageBuilder(docker, nix, testPin(), "", testLogger())
	overlay := writeOverlay(t, "final: prev: { }\n")

	base := config.BuildDef{Packages: []string{"git", "openssh", "go"}, Overlay: overlay}
	shuffled := config.BuildDef{Packages: []string{"go", "git", "openssh"}, Overlay: overlay}

	tag1, err := b.ImageTag("review", base)
	if err != nil {
		t.Fatal(err)
	}
	tag2, err := b.ImageTag("review", shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if tag1 != tag2 {
		t.Fatalf("shuffled packages changed the tag: %s vs %s", tag1, tag2)
	}
	if want := "faber/review:"; !strings.HasPrefix(tag1, want) || len(tag1) != len(want)+tagHexLen {
		t.Fatalf("tag shape %q, want %s<%d hex>", tag1, want, tagHexLen)
	}

	added, _ := b.ImageTag("review", config.BuildDef{Packages: []string{"git", "openssh", "go", "gopls"}, Overlay: overlay})
	if added == tag1 {
		t.Fatal("added package did not change the tag")
	}
	overlay2 := writeOverlay(t, "final: prev: { changed = true; }\n")
	changedOverlay, _ := b.ImageTag("review", config.BuildDef{Packages: base.Packages, Overlay: overlay2})
	if changedOverlay == tag1 {
		t.Fatal("overlay byte change did not change the tag")
	}
	bumped := NewImageBuilder(docker, nix, NixpkgsPin{Rev: "24.11", SHA256: testPin().SHA256}, "", testLogger())
	bumpedTag, _ := bumped.ImageTag("review", base)
	if bumpedTag == tag1 {
		t.Fatal("pin bump did not change the tag")
	}
	if len(docker.calls) != 0 || nix.evalCount() != 0 || nix.buildCount() != 0 {
		t.Fatalf("tag computation touched adapters: docker=%d eval=%d build=%d",
			len(docker.calls), nix.evalCount(), nix.buildCount())
	}
}

// Verifies 0acac696dcf1: a tag already in the daemon skips the build (the
// daemon is the cache), and concurrent builds of one tag are serialized so
// exactly one nix invocation happens.
func TestBuildSkipAndSingleFlight(t *testing.T) {
	build := config.BuildDef{Packages: []string{"git"}}

	t.Run("skip when image exists", func(t *testing.T) {
		nix := &fakeNix{}
		b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		tag, err := b.ImageTag("merge", build)
		if err != nil {
			t.Fatal(err)
		}
		docker := &fakeDocker{exists: map[string]bool{tag: true}}
		b = NewImageBuilder(docker, nix, testPin(), "", testLogger())
		got, err := b.Build(context.Background(), "merge", build)
		if err != nil {
			t.Fatal(err)
		}
		if got != tag {
			t.Fatalf("tag %q, want %q", got, tag)
		}
		if nix.buildCount() != 0 {
			t.Fatalf("nix build invoked %d times despite existing image", nix.buildCount())
		}
	})

	t.Run("single flight", func(t *testing.T) {
		nix := &fakeNix{buildOut: []string{"/nix/store/aaa-image.tar.gz"}, buildDelay: 30 * time.Millisecond}
		probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		tag, err := probe.ImageTag("merge", build)
		if err != nil {
			t.Fatal(err)
		}
		docker := &fakeDocker{loadTag: tag, markLoaded: true}
		b := NewImageBuilder(docker, nix, testPin(), "", testLogger())

		var wg sync.WaitGroup
		errs := make([]error, 2)
		tags := make([]string, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tags[i], errs[i] = b.Build(context.Background(), "merge", build)
			}(i)
		}
		wg.Wait()
		for i := 0; i < 2; i++ {
			if errs[i] != nil {
				t.Fatalf("goroutine %d: %v", i, errs[i])
			}
			if tags[i] != tag {
				t.Fatalf("goroutine %d tag %q, want %q", i, tags[i], tag)
			}
		}
		if nix.buildCount() != 1 {
			t.Fatalf("nix build invoked %d times, want exactly 1", nix.buildCount())
		}
	})
}

// Verifies b329adcbbfe4: unresolvable names come back as one joined error
// naming the template and every missing package, with no build attempted; an
// eval crash yields one error against the overlay path.
func TestProofFailureShape(t *testing.T) {
	overlay := writeOverlay(t, "final: prev: { }\n")
	build := config.BuildDef{
		Packages: []string{"git", "custom-cli-a", "custom-cli-b"},
		Overlay:  overlay,
	}

	t.Run("missing packages joined", func(t *testing.T) {
		nix := &fakeNix{evalResults: []json.RawMessage{
			json.RawMessage(`{"git":true,"custom-cli-a":false,"custom-cli-b":false}`),
		}}
		b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		err := b.ProvePackages(context.Background(), "review", build)
		if err == nil {
			t.Fatal("missing packages accepted")
		}
		msg := err.Error()
		for _, want := range []string{
			`templates.review.build.packages: "custom-cli-a" does not resolve in pinned nixpkgs`,
			`templates.review.build.packages: "custom-cli-b" does not resolve in pinned nixpkgs`,
			"(with overlay " + overlay + ")",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q missing %q", msg, want)
			}
		}
		if strings.Contains(msg, `"git" does not resolve`) {
			t.Fatalf("resolvable package reported missing: %q", msg)
		}
		if nix.buildCount() != 0 {
			t.Fatal("a build was attempted during the proof")
		}
	})

	t.Run("eval crash names the overlay", func(t *testing.T) {
		nix := &fakeNix{evalErr: fmt.Errorf("infra: nix eval: %w", &ExecError{
			Cmd: "nix", ExitCode: 1, Stderr: "syntax error at line 3",
		})}
		b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		err := b.ProvePackages(context.Background(), "review", build)
		if err == nil {
			t.Fatal("eval crash accepted")
		}
		if got := strings.Count(err.Error(), "\n"); got != 0 {
			t.Fatalf("eval crash reported as %d+1 errors, want exactly one line: %q", got, err)
		}
		if !strings.Contains(err.Error(), "overlay "+overlay) {
			t.Fatalf("error %q does not name the overlay path", err)
		}
	})

	t.Run("empty package list proves trivially", func(t *testing.T) {
		nix := &fakeNix{}
		b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		if err := b.ProvePackages(context.Background(), "empty", config.BuildDef{}); err != nil {
			t.Fatalf("empty package list rejected: %v", err)
		}
		if nix.evalCount() != 0 {
			t.Fatal("eval invoked for an empty package list")
		}
	})
}

// Verifies b329adcbbfe4 and 0acac696dcf1 (edge): a package name outside the
// safe charset is rejected before any expression is rendered, with an error
// naming the template and the offending name.
func TestPackageNameCharsetRejected(t *testing.T) {
	nix := &fakeNix{}
	b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
	offending := `x"; rm -rf /`
	bad := config.BuildDef{Packages: []string{"git", offending}}

	perr := b.ProvePackages(context.Background(), "review", bad)
	_, berr := b.Build(context.Background(), "review", bad)
	for _, err := range []error{perr, berr} {
		if err == nil {
			t.Fatal("injection-shaped package name accepted")
		}
		if !strings.Contains(err.Error(), "templates.review") || !strings.Contains(err.Error(), fmt.Sprintf("%q", offending)) {
			t.Fatalf("error %q does not name template and offending package", err)
		}
	}
	if nix.evalCount() != 0 || nix.buildCount() != 0 {
		t.Fatal("an expression was rendered/evaluated for a bad name")
	}
}

// Verifies 0acac696dcf1: the rendered expressions pin nixpkgs by rev+sha,
// import the staged overlay only when declared, and splice each package as a
// pkgs attribute (image) / proof entry (eval).
func TestRenderedExpressions(t *testing.T) {
	overlay := writeOverlay(t, "final: prev: { custom-cli = prev.hello; }\n")
	build := config.BuildDef{Packages: []string{"openssh", "git"}, Overlay: overlay}
	nix := &fakeNix{
		evalResults: []json.RawMessage{json.RawMessage(`{"git":true,"openssh":true}`)},
		buildOut:    []string{"/nix/store/bbb-image.tar.gz"},
	}
	probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
	tag, err := probe.ImageTag("review", build)
	if err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{loadTag: tag}
	b := NewImageBuilder(docker, nix, testPin(), "", testLogger())

	if err := b.ProvePackages(context.Background(), "review", build); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background(), "review", build); err != nil {
		t.Fatal(err)
	}

	proof := nix.evalExprs[0]
	for _, want := range []string{
		"nixpkgs/archive/24.05.tar.gz",
		`sha256 = "sha256:1lr1h35prqkd1mkmzriwlpvxcb34kmhc9dnr48gkm8hh089hifmx"`,
		"(import ./overlay.nix)",
		`"git" "openssh"`,
		"hasAttrByPath",
	} {
		if !strings.Contains(proof, want) {
			t.Fatalf("proof expression missing %q:\n%s", want, proof)
		}
	}
	if !nix.evalOverlays[0] {
		t.Fatal("overlay was not staged beside the proof expression")
	}

	image := nix.buildExprs[0]
	hash := tag[strings.LastIndexByte(tag, ':')+1:]
	for _, want := range []string{
		"dockerTools.buildLayeredImage",
		`name = "faber/review"`,
		fmt.Sprintf("tag = %q", hash),
		`pkgs."git"`,
		`pkgs."openssh"`,
		`config.Env = [ "PATH=/bin:/usr/bin" ]`,
		"(import ./overlay.nix)",
	} {
		if !strings.Contains(image, want) {
			t.Fatalf("image expression missing %q:\n%s", want, image)
		}
	}

	// No overlay declared -> no overlay import anywhere.
	nix2 := &fakeNix{evalResults: []json.RawMessage{json.RawMessage(`{"git":true}`)}}
	b2 := NewImageBuilder(&fakeDocker{}, nix2, testPin(), "", testLogger())
	if err := b2.ProvePackages(context.Background(), "merge", config.BuildDef{Packages: []string{"git"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nix2.evalExprs[0], "overlay.nix") || nix2.evalOverlays[0] {
		t.Fatal("overlay import rendered without a declared overlay")
	}
}

// Verifies 0acac696dcf1 + b329adcbbfe4 (regression): the proof and the image
// renderer agree on attribute-access semantics for every name nameRE admits.
// Digit-leading attributes (7zip) are valid nixpkgs names but not valid
// unquoted Nix identifiers — if the image expression spliced them unquoted
// (pkgs.7zip), a package could prove resolvable at validate time and still
// crash the build with a Nix parse error, violating validate-before-run. The
// image renderer must therefore use the proof's split-and-quote semantics:
// pkgs."7zip", pkgs."python3Packages"."requests".
func TestRenderedPackageAttrParity(t *testing.T) {
	build := config.BuildDef{Packages: []string{"7zip", "python3Packages.requests", "git"}}
	nix := &fakeNix{
		evalResults: []json.RawMessage{
			json.RawMessage(`{"7zip":true,"python3Packages.requests":true,"git":true}`),
		},
		buildOut: []string{"/nix/store/ggg-image.tar.gz"},
	}
	probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
	tag, err := probe.ImageTag("edgecase", build)
	if err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{loadTag: tag}
	b := NewImageBuilder(docker, nix, testPin(), "", testLogger())

	// The proof accepts the digit-leading and dotted names...
	if err := b.ProvePackages(context.Background(), "edgecase", build); err != nil {
		t.Fatalf("proof rejected names the charset admits: %v", err)
	}
	// ...so the image expression must access them with identical semantics.
	if _, err := b.Build(context.Background(), "edgecase", build); err != nil {
		t.Fatal(err)
	}
	image := nix.buildExprs[0]
	for _, want := range []string{
		`pkgs."7zip"`,
		`pkgs."python3Packages"."requests"`,
		`pkgs."git"`,
	} {
		if !strings.Contains(image, want) {
			t.Fatalf("image expression missing quoted attr access %q:\n%s", want, image)
		}
	}
	for _, forbidden := range []string{"pkgs.7zip", "pkgs.python3Packages", "pkgs.git\n"} {
		if strings.Contains(image, forbidden) {
			t.Fatalf("image expression contains unquoted attr access %q:\n%s", forbidden, image)
		}
	}
	// The unit renderer parity, pinned directly: every dot-split segment is
	// quoted exactly as the proof's splitString "." would address it.
	if got := renderPkgAttr("7zip"); got != `pkgs."7zip"` {
		t.Fatalf("renderPkgAttr(7zip) = %s", got)
	}
	if got := renderPkgAttr("python3Packages.requests"); got != `pkgs."python3Packages"."requests"` {
		t.Fatalf("renderPkgAttr(python3Packages.requests) = %s", got)
	}
}

// Verifies 0acac696dcf1 (edge): a loaded tag disagreeing with the computed
// tag fails the build loudly rather than returning either tag.
func TestLoadedTagMismatchFailsLoudly(t *testing.T) {
	nix := &fakeNix{buildOut: []string{"/nix/store/ccc-image.tar.gz"}}
	docker := &fakeDocker{loadTag: "faber/review:d00dd00dd00d"}
	b := NewImageBuilder(docker, nix, testPin(), "", testLogger())
	build := config.BuildDef{Packages: []string{"git"}}
	tag, err := b.ImageTag("review", build)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Build(context.Background(), "review", build)
	if err == nil {
		t.Fatal("tag mismatch accepted")
	}
	if !strings.Contains(err.Error(), docker.loadTag) || !strings.Contains(err.Error(), tag) {
		t.Fatalf("error %q does not carry both tags", err)
	}
}

// Verifies 7b7ad52d9123 (first-pass seam only): every successful load appends
// one manifest line for the future GC command, and a manifest append failure
// never fails a build. No retention or cleanup behavior exists.
func TestManifestSeam(t *testing.T) {
	build := config.BuildDef{Packages: []string{"git"}}

	t.Run("append on load", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		nix := &fakeNix{buildOut: []string{"/nix/store/ddd-image.tar.gz"}}
		probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		tag, err := probe.ImageTag("merge", build)
		if err != nil {
			t.Fatal(err)
		}
		docker := &fakeDocker{loadTag: tag}
		b := NewImageBuilder(docker, nix, testPin(), stateDir, testLogger())
		if _, err := b.Build(context.Background(), "merge", build); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(stateDir, "images.jsonl"))
		if err != nil {
			t.Fatalf("manifest not written: %v", err)
		}
		var entry struct {
			Tag      string `json:"tag"`
			Template string `json:"template"`
			OutPath  string `json:"out_path"`
			LoadedAt string `json:"loaded_at"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
			t.Fatalf("manifest line not JSON: %v (%q)", err, data)
		}
		if entry.Tag != tag || entry.Template != "merge" || entry.OutPath != "/nix/store/ddd-image.tar.gz" || entry.LoadedAt == "" {
			t.Fatalf("manifest entry %+v incomplete", entry)
		}
	})

	t.Run("append failure never fails a build", func(t *testing.T) {
		// stateDir collides with an existing regular file, so MkdirAll fails.
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		nix := &fakeNix{buildOut: []string{"/nix/store/eee-image.tar.gz"}}
		probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
		tag, err := probe.ImageTag("merge", build)
		if err != nil {
			t.Fatal(err)
		}
		docker := &fakeDocker{loadTag: tag}
		b := NewImageBuilder(docker, nix, testPin(), blocker, testLogger())
		if _, err := b.Build(context.Background(), "merge", build); err != nil {
			t.Fatalf("manifest failure failed the build: %v", err)
		}
	})
}

// Verifies b329adcbbfe4: the config.PackageProver seam proves every template
// and joins the errors, so validate reports all missing names at once.
func TestPackageProverSeam(t *testing.T) {
	cfg := &config.Config{Templates: map[string]config.TemplateDef{
		"review":    {Build: config.BuildDef{Packages: []string{"git", "custom-cli-a"}}},
		"implement": {Build: config.BuildDef{Packages: []string{"git", "custom-cli-b"}}},
	}}
	nix := &fakeNix{evalResults: []json.RawMessage{
		json.RawMessage(`{"git":true,"custom-cli-b":false}`), // implement (sorted first)
		json.RawMessage(`{"git":true,"custom-cli-a":false}`),
	}}
	b := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
	var prover config.PackageProver = b.PackageProver()
	err := prover.ProvePackages(context.Background(), cfg, testLogger())
	if err == nil {
		t.Fatal("missing packages accepted")
	}
	for _, want := range []string{
		`templates.implement.build.packages: "custom-cli-b"`,
		`templates.review.build.packages: "custom-cli-a"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("joined error %q missing %q", err, want)
		}
	}
}

// Verifies 0acac696dcf1: the config.ImageBuilder seam builds the named
// template and rejects unknown names.
func TestConfigBuilderSeam(t *testing.T) {
	build := config.BuildDef{Packages: []string{"git"}}
	cfg := &config.Config{Templates: map[string]config.TemplateDef{"merge": {Build: build}}}
	nix := &fakeNix{buildOut: []string{"/nix/store/fff-image.tar.gz"}}
	probe := NewImageBuilder(&fakeDocker{}, nix, testPin(), "", testLogger())
	tag, err := probe.ImageTag("merge", build)
	if err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{loadTag: tag}
	var seam config.ImageBuilder = NewImageBuilder(docker, nix, testPin(), "", testLogger()).ConfigBuilder()

	if err := seam.BuildImage(context.Background(), cfg, "merge", testLogger()); err != nil {
		t.Fatal(err)
	}
	if nix.buildCount() != 1 {
		t.Fatalf("nix build invoked %d times, want 1", nix.buildCount())
	}
	if err := seam.BuildImage(context.Background(), cfg, "nope", testLogger()); err == nil || !strings.Contains(err.Error(), `unknown template "nope"`) {
		t.Fatalf("unknown template error %v", err)
	}
}

// Verifies 0acac696dcf1: a missing overlay file surfaces as a wrapped error
// from tag computation (the overlay's bytes are a hash input).
func TestMissingOverlayFails(t *testing.T) {
	b := NewImageBuilder(&fakeDocker{}, &fakeNix{}, testPin(), "", testLogger())
	_, err := b.ImageTag("review", config.BuildDef{Packages: []string{"git"}, Overlay: "/nope/overlay.nix"})
	if err == nil || !strings.Contains(err.Error(), "/nope/overlay.nix") {
		t.Fatalf("missing overlay error %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error %v does not wrap os.ErrNotExist", err)
	}
}
