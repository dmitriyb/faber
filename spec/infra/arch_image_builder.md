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
is derived from exactly the inputs that determine image content: the *resolved*
nixpkgs pin revision, the sorted package list, and the overlay file's content hash.
The tag is computed without building, so:

- `faber build` skips a template whose tag already exists in the daemon
  (image-exists check) — rebuilds happen only when the toolset actually changed;
- the journal's input-hash can include the image tag, making "same step, same
  toolset" a checkable fact at resume time;
- two templates with identical `build:` sections produce identical content and
  differ only in name — the layered build shares their layers via the Nix store.

## Per-toolset pin, engine default fallback

The nixpkgs pin is no longer a single engine constant. A toolset may declare its
own `pin: {rev, sha256}` — different projects need different toolchains (a box
whose `spex` needs Go 1.25 requires a newer nixpkgs than faber's default ships).
ImageBuilder still carries a compiled-in `DefaultNixpkgsPin`, now as the
**fallback**, and resolves the **effective** pin per build/template: the build's
`pin` when set, else the default. The compiled-in default is not removed.

The *resolved* pin — not the raw default — is what folds into the toolset hash,
what the rendered `buildLayeredImage` expression fetches, and what the resolution
proof evaluates against. Two consequences that must both hold:

- a per-image pin yields a **distinct** tag and a distinct image (a Go-1.25
  toolset never collides with a default-pin one);
- a build with **no** pin resolves to the default, so its tag, its rendered expr,
  and its proof are **byte-identical to today** — the smoke, every `examples/*`,
  and the golden IR fixtures stay stable.

The `pin` arrives as config's `config.PinDef` on the resolved build; the mapping
`config.PinDef → infra.NixpkgsPin` happens at the config→infra boundary (see
`impl_nix_build.md`), keeping the dependency direction infra→config.

The pin must also survive the **run-time tag path**, or build-time and run-time tags
diverge. `faber build` computes the tag from the resolved `BuildDef` (which carries
the pin). But at run/resume the pipeline never holds that `BuildDef` — it recomputes
the tag from the node's `*config.ResolvedTemplate` through the `imageTagger` seam,
which reconstructs a `config.BuildDef{Packages, Overlay, Pin: template.Pin}`. That
reconstruction **must** carry `template.Pin` (the flat field from MUST-FIX 1): omit
it and a pinned template's run/journal tag resolves to the *default* pin
(`…:<default-hash>`) while `faber build` produced `…:<pin-hash>` — so the daemon has
no image under the recomputed tag and the journal input-hash never matches. A
pin-*less* template stays byte-stable either way (nil → default at both build and
run); the break is only for pinned toolsets. See `impl_nix_build.md`, "Run-time tag
reconstruction".

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

Package contents are Nix store paths: root-owned, read-only, with the non-root
agent user unable to modify them. There is no package manager in the image and
no writable layer trick to install one's way around the toolset — the
environment *is* the restriction, which is what makes the in-box agent's
`bypassPermissions` safe.

Nothing writable is ever baked: the image carries no writable dirs and no baked
user, so one image stays generic across hosts. Every writable path is a run-time
mount, sized to its content:

- **`/workspace`** (the repo clone, potentially gigabytes) is a **disk-backed
  volume** — a per-container anonymous volume on the daemon's storage, discarded
  with `--rm`. It is deliberately *not* tmpfs: a multi-gigabyte clone in RAM
  would exhaust the daemon's memory, and on the macOS VM the volume's disk is
  fast local storage, not the slow host-bind (virtiofs) path.
- **`/faber/bundle`, `/tmp`, and `HOME`** are small and ephemeral, so they are
  **tmpfs** (RAM) — regenerated every run, gone on exit.
- **`/faber/result`** stays a host bind mount: it is the one writable path whose
  content must survive the container to reach the host.

Because a freshly created volume or tmpfs is root-owned, the box cannot start as
its non-root user and still write them. Instead the container starts as root and
the box's entry program (`faber-box`) `chown`s exactly these writable mounts to
the run uid:gid, then **drops privileges** to that non-root user before any hook
or agent runs — the same root-entry-then-drop shape the network binding already
uses. The run uid:gid is the host user's, so files the box writes to the result
bind stay host-owned. See PhaseSequencer for the privileged preamble.

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
