package infra

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dmitriyb/faber/config"
)

// NixpkgsPin identifies the exact nixpkgs snapshot every image is built from:
// a revision plus the fetchTarball hash of its archive. The pin is
// engine-owned (a compiled-in default, bumped deliberately with faber
// releases) — the config schema has no nixpkgs field, and the overlay is the
// user seam for anything the pin lacks. Because the pin participates in the
// toolset hash, bumping it retags and rebuilds every image, never silently
// mutates one.
type NixpkgsPin struct {
	Rev    string // e.g. a nixpkgs commit or release tag
	SHA256 string // fetchTarball hash of the archive
}

// DefaultNixpkgsPin is the engine's compiled-in nixpkgs snapshot.
func DefaultNixpkgsPin() NixpkgsPin {
	return NixpkgsPin{
		Rev:    "24.05",
		SHA256: "sha256:1lr1h35prqkd1mkmzriwlpvxcb34kmhc9dnr48gkm8hh089hifmx",
	}
}

// nameRE is the charset every spliced identifier (template name, package
// name) must match before it enters a rendered Nix expression — defense
// against expression injection via a package "name". Dots are allowed so
// attribute paths like set.attr address nested packages.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// pinRE is the charset for pin fields spliced into the fetchTarball call.
var pinRE = regexp.MustCompile(`^[A-Za-z0-9:+/=._-]+$`)

// tagHexLen is how many hex chars of the toolset hash form the image tag.
// Twelve suffice: a collision only risks skipping a rebuild, and the
// namespace is one user's templates.
const tagHexLen = 12

// stagedOverlayName is the file name a declared overlay is copied to beside
// the rendered expression, so the expression imports it by relative path and
// no user-controlled path is ever spliced into Nix source.
const stagedOverlayName = "overlay.nix"

// ImageBuilder compiles a template's build section (pinned package list plus
// optional overlay) into an immutable docker image: it renders a
// dockerTools.buildLayeredImage expression over the pinned nixpkgs, builds
// the tarball via NixClient, and loads it via DockerClient under a
// deterministic tag. It also owns the validate-time package resolution proof.
type ImageBuilder struct {
	docker   DockerClient
	nix      NixClient
	pin      NixpkgsPin
	stateDir string // manifest home; empty disables the GC-seam manifest
	logger   *slog.Logger

	mu       sync.Mutex
	tagLocks map[string]*sync.Mutex
}

// NewImageBuilder constructs the build pipeline. stateDir hosts the
// append-only image manifest (the deferred-GC seam); empty disables it.
func NewImageBuilder(docker DockerClient, nix NixClient, pin NixpkgsPin, stateDir string, logger *slog.Logger) *ImageBuilder {
	return &ImageBuilder{
		docker:   docker,
		nix:      nix,
		pin:      pin,
		stateDir: stateDir,
		logger:   ensureLogger(logger).With("component", "image-builder"),
		tagLocks: map[string]*sync.Mutex{},
	}
}

// ImageTag computes the deterministic image tag faber/<name>:<toolset-hash>
// without building and with zero adapter calls. The hash covers exactly the
// inputs that determine image content: the pin revision, the sorted package
// list, and the overlay file's content hash (bytes, not path).
func (b *ImageBuilder) ImageTag(name string, build config.BuildDef) (string, error) {
	h := sha256.New()
	fmt.Fprintln(h, b.pin.Rev)
	for _, p := range slices.Sorted(slices.Values(build.Packages)) {
		fmt.Fprintln(h, p)
	}
	if build.Overlay != "" {
		data, err := os.ReadFile(build.Overlay)
		if err != nil {
			return "", fmt.Errorf("infra: overlay %s: %w", build.Overlay, err)
		}
		fmt.Fprintf(h, "%x\n", sha256.Sum256(data))
	}
	hash := fmt.Sprintf("%x", h.Sum(nil))[:tagHexLen]
	return fmt.Sprintf("faber/%s:%s", name, hash), nil
}

// ProvePackages proves, without building, that every name in the template's
// package list resolves in the pinned nixpkgs with the overlay applied — one
// nix eval per template. Unresolvable names come back joined, one field-path
// error per name; an eval crash (e.g. a syntax error in the user overlay) is
// reported once against the overlay path.
func (b *ImageBuilder) ProvePackages(ctx context.Context, tpl string, build config.BuildDef) error {
	if len(build.Packages) == 0 {
		return nil
	}
	if err := checkSpliceNames(tpl, build); err != nil {
		return err
	}
	exprFile, cleanup, err := b.stageExpr(renderProofExpr(b.pin, build.Overlay != "", build.Packages), build.Overlay)
	if err != nil {
		return err
	}
	defer cleanup()

	raw, err := b.nix.Eval(ctx, exprFile, nil)
	if err != nil {
		if build.Overlay != "" {
			return fmt.Errorf("templates.%s.build: package resolution proof failed (overlay %s): %w", tpl, build.Overlay, err)
		}
		return fmt.Errorf("templates.%s.build: package resolution proof failed: %w", tpl, err)
	}
	resolved, err := decodeProof(raw)
	if err != nil {
		return fmt.Errorf("templates.%s.build: %w", tpl, err)
	}
	withOverlay := ""
	if build.Overlay != "" {
		withOverlay = fmt.Sprintf(" (with overlay %s)", build.Overlay)
	}
	var errs []error
	for _, name := range slices.Sorted(slices.Values(build.Packages)) {
		if !resolved[name] {
			errs = append(errs, fmt.Errorf("templates.%s.build.packages: %q does not resolve in pinned nixpkgs%s", tpl, name, withOverlay))
		}
	}
	return errors.Join(errs...)
}

// Build compiles the template's image and loads it into the docker daemon,
// returning the deterministic tag. The daemon is the cache: when the tag
// already exists nothing is built. Builds of one tag are serialized behind a
// per-tag lock so concurrent steps of one template build once.
func (b *ImageBuilder) Build(ctx context.Context, tpl string, build config.BuildDef) (string, error) {
	if err := checkSpliceNames(tpl, build); err != nil {
		return "", err
	}
	tag, err := b.ImageTag(tpl, build)
	if err != nil {
		return "", err
	}
	lock := b.tagLock(tag)
	lock.Lock()
	defer lock.Unlock()

	exists, err := b.docker.ImageExists(ctx, tag)
	if err != nil {
		return "", err
	}
	if exists {
		b.logger.DebugContext(ctx, "image exists, skipping build", "template", tpl, "tag", tag)
		return tag, nil
	}

	hash := tag[strings.LastIndexByte(tag, ':')+1:]
	exprFile, cleanup, err := b.stageExpr(renderImageExpr(b.pin, tpl, hash, build.Packages, build.Overlay != ""), build.Overlay)
	if err != nil {
		return "", err
	}
	defer cleanup()

	b.logger.InfoContext(ctx, "building image", "template", tpl, "tag", tag)
	outPaths, err := b.nix.Build(ctx, exprFile)
	if err != nil {
		return "", fmt.Errorf("infra: build image for template %s: %w", tpl, err)
	}
	tarball := outPaths[0]
	loadedTag, err := b.docker.Load(ctx, tarball)
	if err != nil {
		return "", fmt.Errorf("infra: load image for template %s: %w", tpl, err)
	}
	if loadedTag != tag {
		return "", fmt.Errorf("infra: template %s: loaded tag %q does not match computed tag %q", tpl, loadedTag, tag)
	}
	b.appendManifest(ctx, tag, tpl, tarball)
	b.logger.InfoContext(ctx, "image loaded", "template", tpl, "tag", tag)
	return tag, nil
}

// tagLock returns the mutex serializing builds of one tag.
func (b *ImageBuilder) tagLock(tag string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.tagLocks[tag]
	if !ok {
		l = &sync.Mutex{}
		b.tagLocks[tag] = l
	}
	return l
}

// stageExpr writes the rendered expression (and a copy of the declared
// overlay, so the expression only ever imports ./overlay.nix and no
// user-controlled path is spliced into Nix source) into a fresh temp dir.
func (b *ImageBuilder) stageExpr(expr, overlayPath string) (exprFile string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "faber-image-")
	if err != nil {
		return "", nil, fmt.Errorf("infra: stage nix expression: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }
	if overlayPath != "" {
		data, rerr := os.ReadFile(overlayPath)
		if rerr != nil {
			cleanup()
			return "", nil, fmt.Errorf("infra: overlay %s: %w", overlayPath, rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, stagedOverlayName), data, 0o644); werr != nil {
			cleanup()
			return "", nil, fmt.Errorf("infra: stage overlay: %w", werr)
		}
	}
	exprFile = filepath.Join(dir, "expr.nix")
	if werr := os.WriteFile(exprFile, []byte(expr), 0o644); werr != nil {
		cleanup()
		return "", nil, fmt.Errorf("infra: stage nix expression: %w", werr)
	}
	return exprFile, cleanup, nil
}

// checkSpliceNames rejects any identifier that would be spliced into a Nix
// expression but fails the safe charset — before anything is rendered.
func checkSpliceNames(tpl string, build config.BuildDef) error {
	var errs []error
	if !nameRE.MatchString(tpl) {
		errs = append(errs, fmt.Errorf("templates.%s: template name is not a safe identifier ([A-Za-z0-9._-])", tpl))
	}
	for _, p := range build.Packages {
		if !nameRE.MatchString(p) {
			errs = append(errs, fmt.Errorf("templates.%s.build.packages: invalid package name %q ([A-Za-z0-9._-])", tpl, p))
		}
	}
	return errors.Join(errs...)
}

// renderPinnedPkgs renders the shared expression head: pinned nixpkgs with
// the (staged) overlay applied when declared.
func renderPinnedPkgs(pin NixpkgsPin, withOverlay bool) string {
	overlays := "[ ]"
	if withOverlay {
		overlays = "[ (import ./" + stagedOverlayName + ") ]"
	}
	return fmt.Sprintf(`let
  pkgs = import (fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/%s.tar.gz";
    sha256 = "%s";
  }) { overlays = %s; };
`, sanitizePin(pin.Rev), sanitizePin(pin.SHA256), overlays)
}

// sanitizePin guards the two engine-owned pin fields at the splice point.
func sanitizePin(v string) string {
	if !pinRE.MatchString(v) {
		// The pin is compiled in; a bad value is a programming error surfaced
		// as an unresolvable expression rather than silent injection.
		return "invalid-pin-value"
	}
	return v
}

// renderProofExpr renders the resolution-proof expression: a map from each
// declared package name to whether its attribute path exists in the pinned
// (overlaid) package set. hasAttrByPath keeps dotted names (set.attr)
// addressable exactly like the image expression's pkgs.<name> splice.
func renderProofExpr(pin NixpkgsPin, withOverlay bool, packages []string) string {
	var sb strings.Builder
	sb.WriteString(renderPinnedPkgs(pin, withOverlay))
	sb.WriteString(`in builtins.listToAttrs (map (n: {
  name = n;
  value = pkgs.lib.attrsets.hasAttrByPath (pkgs.lib.splitString "." n) pkgs;
}) [`)
	for _, p := range slices.Sorted(slices.Values(packages)) {
		fmt.Fprintf(&sb, " %q", p)
	}
	sb.WriteString(" ])\n")
	return sb.String()
}

// renderPkgAttr renders one package's attribute access with exactly the
// split-and-quote semantics the proof expression uses (hasAttrByPath over
// splitString "." n): split on dots, quote every segment. Quoting matters —
// nameRE admits valid nixpkgs attributes that are not valid unquoted Nix
// identifiers (digit-leading names like 7zip, dash-leading segments), and
// the proof and the image renderer must agree on attribute-access semantics
// or a name could prove resolvable at validate time and still crash the
// build, violating validate-before-run.
func renderPkgAttr(name string) string {
	var sb strings.Builder
	sb.WriteString("pkgs")
	for _, seg := range strings.Split(name, ".") {
		fmt.Fprintf(&sb, ".%q", seg)
	}
	return sb.String()
}

// renderImageExpr renders the dockerTools.buildLayeredImage expression for
// one template: no Dockerfile, no repository content, one contents attr per
// declared package. Package names were charset-checked before this point.
func renderImageExpr(pin NixpkgsPin, tpl, tag string, packages []string, withOverlay bool) string {
	var sb strings.Builder
	sb.WriteString(renderPinnedPkgs(pin, withOverlay))
	fmt.Fprintf(&sb, `in pkgs.dockerTools.buildLayeredImage {
  name = "faber/%s";
  tag = "%s";
  contents = [
`, tpl, tag)
	for _, p := range slices.Sorted(slices.Values(packages)) {
		fmt.Fprintf(&sb, "    %s\n", renderPkgAttr(p))
	}
	sb.WriteString(`  ];
  config.Env = [ "PATH=/bin:/usr/bin" ];
}
`)
	return sb.String()
}

// decodeProof decodes the proof expression's payload.
func decodeProof(raw json.RawMessage) (map[string]bool, error) {
	var m map[string]bool
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode resolution proof: %w", err)
	}
	return m, nil
}

// manifestEntry is one line of the append-only image manifest — the reserved
// seam for the deferred GC/retention pass ("Deferred: image GC and cache
// eviction"): it lets a future cleanup command enumerate faber-owned images
// and store roots without heuristics. Nothing reads it yet; the first pass
// relies on manual docker/nix GC.
type manifestEntry struct {
	Tag      string    `json:"tag"`
	Template string    `json:"template"`
	OutPath  string    `json:"out_path"`
	LoadedAt time.Time `json:"loaded_at"`
}

// appendManifest records a successful load in <stateDir>/images.jsonl.
// Append failures are logged at warn and never fail a build.
func (b *ImageBuilder) appendManifest(ctx context.Context, tag, tpl, outPath string) {
	if b.stateDir == "" {
		b.logger.DebugContext(ctx, "image manifest disabled (no state dir)")
		return
	}
	entry := manifestEntry{Tag: tag, Template: tpl, OutPath: outPath, LoadedAt: time.Now().UTC()}
	line, err := json.Marshal(entry)
	if err != nil {
		b.logger.WarnContext(ctx, "image manifest append failed", "tag", tag, "err", err)
		return
	}
	if err := os.MkdirAll(b.stateDir, 0o755); err != nil {
		b.logger.WarnContext(ctx, "image manifest append failed", "tag", tag, "err", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(b.stateDir, "images.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		b.logger.WarnContext(ctx, "image manifest append failed", "tag", tag, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		b.logger.WarnContext(ctx, "image manifest append failed", "tag", tag, "err", err)
	}
}
