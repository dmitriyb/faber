# Implementation: Nix build pipeline

Covers ImageBuilder.

## Types (internal/infra/image.go)

```go
type ImageBuilder struct {
    docker     DockerClient
    nix        NixClient
    defaultPin NixpkgsPin // compiled-in FALLBACK: used when a build declares no pin
    logger     *slog.Logger
}

type NixpkgsPin struct {
    Rev    string // e.g. a nixpkgs commit
    SHA256 string // fetchTarball hash
}

func (b *ImageBuilder) ImageTag(name string, build config.BuildDef) (string, error)
func (b *ImageBuilder) ProvePackages(ctx context.Context, tpl string, build config.BuildDef) error
func (b *ImageBuilder) Build(ctx context.Context, tpl string, build config.BuildDef) (string, error)
```

`NewImageBuilder` still takes the compiled-in default pin (bumped deliberately
with faber releases) — it is now the **fallback**, stored as `defaultPin`. A
toolset may override it with its own `pin: {rev, sha256}` carried on the build
(`config.BuildDef.Pin`), because different projects need different toolchains and
the overlay only adds derivations, it cannot change the nixpkgs base. ImageBuilder
resolves the **effective** pin per build — the build's pin when set, else
`defaultPin` — and uses that resolved pin everywhere the pin participates: the tag
hash, the rendered expression, and the resolution proof. Because the pin
participates in the toolset hash, a per-image pin yields a distinct tag/image while
a build with no pin resolves to the default and is byte-identical to today; bumping
either pin retags and rebuilds — never silently mutates one.

## Config→infra pin boundary

The build carries config's own `*config.PinDef`; infra owns `NixpkgsPin`. A single
adapter maps one to the other at the boundary and applies the fallback — this is
the only place the two pin types meet, and the mapping direction respects
infra→config (config never imports infra):

```go
// resolvePin maps a build's optional config pin into the infra pin, falling back
// to the compiled-in default when the toolset declares none.
func (b *ImageBuilder) resolvePin(build config.BuildDef) NixpkgsPin {
    if build.Pin != nil {
        return NixpkgsPin{Rev: build.Pin.Rev, SHA256: build.Pin.SHA256}
    }
    return b.defaultPin
}
```

`ImageTag`, `ProvePackages`, and `Build` each open with `pin := b.resolvePin(build)`
and thread that value through; none of them read `b.defaultPin` directly.

## Toolset hash and tag

```go
func (b *ImageBuilder) ImageTag(name string, build config.BuildDef) (string, error) {
    pin := b.resolvePin(build) // build's pin, else the compiled-in default
    h := sha256.New()
    fmt.Fprintln(h, imageSchemaVersion) // image-expr schema version, folded FIRST
    fmt.Fprintln(h, pin.Rev)            // then the RESOLVED rev string
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
it content-addressed. The hash order matches the code exactly — `imageSchemaVersion`
folded first (a render-shape bump invalidates every tag), then the resolved
`pin.Rev`, then the sorted packages, then the overlay content hash. Folding the
*resolved* `pin.Rev` (not `b.defaultPin.Rev`) is what makes a per-image pin produce a
distinct tag — and what keeps a build with no pin resolving to the identical rev,
hence the identical tag, hence byte-stable goldens (the resolved rev enters as an
unchanged string, so nothing about the fold changes for a pin-less build). Twelve hex
chars of tag suffice — a collision only risks skipping a rebuild, and the namespace
is one user's templates.

### Run-time tag reconstruction (the `imageTagger` seam)

`ImageTag` takes a `config.BuildDef`, but the pipeline scheduler and the interactive
re-entry recompute a step's tag from the node's `*config.ResolvedTemplate` — they
never hold the original `BuildDef`. The `imageTagger` adapter (`cmd/faber/wire.go`)
therefore reconstructs one:
`config.BuildDef{Packages: template.Packages, Overlay: template.Overlay, Pin: template.Pin}`.
Carrying `template.Pin` here is load-bearing: it is what makes the run/resume tag
equal the `faber build` tag for a pinned toolset. Drop it and `resolvePin` sees a
nil pin, falls back to `defaultPin`, and the recomputed tag diverges from the built
image's tag (image-not-found at run, and a journal input-hash that never matches).
For a pin-less template the reconstruction's nil `Pin` resolves to the default at
both build and run, so the tag stays byte-stable.

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

The `${rev}` and `${sha256}` splices are the resolved pin's fields
(`b.resolvePin(build)`), so a per-image pin fetches its own nixpkgs snapshot and a
pin-less build fetches the default's — the same expression bytes as today. Since a
pin may now be **user-supplied** YAML (no longer only the engine default), `rev` and
`sha256` *are* splice material and must be treated as such: they are charset-validated
**at the Loader** — field-pathed and collected, alongside the both-fields-required
completeness check (see `spec/config/impl_schema_structs.md`) — restricted to
`^[A-Za-z0-9:+/=._-]+$`. The splice point keeps a `sanitizePin` guard matching that
same charset purely as **defense-in-depth**; it is not the authoritative check and no
longer frames the pin as engine-owned or a bad value as a programming error — a bad
user pin is a Loader error with a field path, not an opaque nix failure. Package names
are likewise validated against `^[A-Za-z0-9._-]+$` before substitution (defense
against expression injection via a package "name"); the rest of the template is fixed
structure, not splice material. `nix build --json` returns the
tarball out path; `DockerClient.Load` loads it and the returned tag is asserted
equal to the computed one. `Build` short-circuits when
`docker.ImageExists(tag)` — the daemon is the cache; no rebuild bookkeeping.

## Resolution proof (validate time)

One eval per template, no builds:

```nix
let pkgs = /* resolved-pin + overlay, as above */;
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
