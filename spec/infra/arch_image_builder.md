# ImageBuilder — pinned toolsets to immutable images

## What it is

The build half of a lifecycle template. A template's `build:` section —
`packages` (the pinned Nix package list) plus an optional `overlay` (a
user-supplied Nix file adding derivations) — is compiled into an immutable
docker image with no Dockerfile: ImageBuilder renders a
`dockerTools.buildLayeredImage` expression over a pinned nixpkgs revision,
builds the tarball via `NixClient`, and loads it into the docker daemon via
`DockerClient`. The image is a pure function of the toolset: no repository
content, no secrets, no config — those all arrive at run time as params and
bindings. One image serves any repo.

## Deterministic tagging

Every image is tagged `faber/<template>:<toolset-hash>`, where the toolset hash
is derived from exactly the inputs that determine image content: the pinned
nixpkgs revision, the sorted package list, and the overlay file's content hash.
The tag is computed without building, so:

- `faber build` skips a template whose tag already exists in the daemon
  (image-exists check) — rebuilds happen only when the toolset actually changed;
- the journal's input-hash can include the image tag, making "same step, same
  toolset" a checkable fact at resume time;
- two templates with identical `build:` sections produce identical content and
  differ only in name — the layered build shares their layers via the Nix store.

## The overlay seam

Custom binaries that are not in nixpkgs (the user's agent CLI, tracker CLI,
context tools) enter through the overlay: an opaque Nix file defining
derivations (typically `buildGoModule` for Go tools, or `fetchurl` +
`autoPatchelfHook` for prebuilt binaries). ImageBuilder applies it as a nixpkgs
overlay, so overlay names are addressable exactly like stock packages and
participate in the resolution proof below. Faber never inspects the overlay's
contents — it is user policy; only its bytes enter the toolset hash.

## Package resolution proof (validate time)

At `faber validate`, before anything is built, ImageBuilder proves every name
in every template's package list resolves in the pinned nixpkgs with the
overlay applied — a single `nix eval --json` per template over an expression
that maps each name to whether the attribute exists. No derivation is built and
nothing is downloaded beyond the pinned nixpkgs source. An unresolvable name is
a validate-time error carrying the template and package name
(`templates.review.build.packages: "spec-mapper-cli" does not resolve in pinned
nixpkgs (with overlay ./nix/overlay.nix)`), joined with all other validation
errors rather than reported first-error-only.

## Immutability at runtime

Package contents are Nix store paths: root-owned, read-only, with the
non-root agent user unable to modify them. There is no package manager in the
image and no writable layer trick to install one's way around the toolset —
the environment *is* the restriction, which is what makes the in-box agent's
`bypassPermissions` safe. Anything writable (workspace, result dir, tmp) is a
run-time mount or tmpfs, never image content.

## Deferred seam: image GC and cache eviction

Superseded toolsets leave behind docker images and Nix store paths. The
retention policy and a faber-owned cleanup command are backlog (design edge
case 11). The seam reserved now: ImageBuilder records every tag it loads (and
the Nix out-path it came from) in a small local manifest, so a future
`faber gc` can enumerate faber-owned artifacts and enact retention without
guessing. The first pass relies on manual `docker image prune` /
`nix-collect-garbage`, and the manifest is append-only bookkeeping.

Requirements implemented: Nix image build; Package resolution proof; Deferred:
image GC and cache eviction (seam only).
