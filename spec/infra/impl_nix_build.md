# Implementation: Nix build pipeline

Covers ImageBuilder.

## Types (internal/infra/image.go)

```go
type ImageBuilder struct {
    docker DockerClient
    nix    NixClient
    pin    NixpkgsPin // rev + sha256 of the pinned nixpkgs tarball
    logger *slog.Logger
}

type NixpkgsPin struct {
    Rev    string // e.g. a nixpkgs commit
    SHA256 string // fetchTarball hash
}

func (b *ImageBuilder) ImageTag(name string, build config.BuildDef) (string, error)
func (b *ImageBuilder) ProvePackages(ctx context.Context, tpl string, build config.BuildDef) error
func (b *ImageBuilder) Build(ctx context.Context, tpl string, build config.BuildDef) (string, error)
```

The pin is engine-owned (a compiled-in default revision, bumped deliberately
with faber releases) rather than per-config: the config schema deliberately has
no nixpkgs field, and the overlay is the user seam for anything the pin lacks.
Because the pin participates in the toolset hash, bumping it retags and
rebuilds every image — never silently mutates one.

## Toolset hash and tag

```go
func (b *ImageBuilder) ImageTag(name string, build config.BuildDef) (string, error) {
    h := sha256.New()
    fmt.Fprintln(h, b.pin.Rev)
    for _, p := range slices.Sorted(slices.Values(build.Packages)) {
        fmt.Fprintln(h, p)
    }
    if build.Overlay != "" {
        data, err := os.ReadFile(build.Overlay)
        if err != nil { return "", fmt.Errorf("infra: overlay %s: %w", build.Overlay, err) }
        fmt.Fprintf(h, "%x\n", sha256.Sum256(data))
    }
    return fmt.Sprintf("faber/%s:%x", name, h.Sum(nil)[:6]), nil
}
```

Sorted packages make the hash order-insensitive; overlay bytes (not path) make
it content-addressed. Twelve hex chars of tag suffice — a collision only risks
skipping a rebuild, and the namespace is one user's templates.

## Rendered expression

`Build` writes `image.nix` into a temp dir and hands it to `NixClient.Build`:

```nix
let
  pkgs = import (fetchTarball {
    url = "https://.../archive/${rev}.tar.gz"; sha256 = "${sha256}";
  }) { overlays = [ ${overlayImport} ]; };   # overlay import only when declared
in pkgs.dockerTools.buildLayeredImage {
  name = "faber/${template}";
  tag  = "${toolsetHash}";
  contents = [ pkgs.git pkgs.openssh ... ];  # one attr per declared package
  config.Env = [ "PATH=/bin:/usr/bin" ];
}
```

Package names are validated against `^[A-Za-z0-9._-]+$` before substitution
(defense against expression injection via a package "name"); everything else in
the template is data, not splice material. `nix build --json` returns the
tarball out path; `DockerClient.Load` loads it and the returned tag is asserted
equal to the computed one. `Build` short-circuits when
`docker.ImageExists(tag)` — the daemon is the cache; no rebuild bookkeeping.

## Resolution proof (validate time)

One eval per template, no builds:

```nix
let pkgs = /* pinned + overlay, as above */;
in builtins.listToAttrs (map (n: {
  name = n; value = builtins.hasAttr n pkgs;
}) [ "git" "openssh" ... ])
```

`ProvePackages` decodes the `map[string]bool` and emits one field-path error
per false entry — `templates.review.build.packages: "spec-mapper-cli" does not
resolve in pinned nixpkgs (with overlay ./nix/overlay.nix)` — joined via
`errors.Join` so validate reports every missing name at once. An eval *crash*
(syntax error in the user overlay) is reported once against the overlay path.
This is the infra half of `faber validate`; the CLI runs it after wiring checks.

## GC manifest (the deferred seam)

After every successful load, `Build` appends one JSON line —
`{tag, template, out_path, loaded_at}` — to `<state-dir>/images.jsonl`. Nothing
reads it yet; it exists so the future `faber gc` can enumerate faber-owned
images and store roots without heuristics. Append failures are logged at warn
and never fail a build.
