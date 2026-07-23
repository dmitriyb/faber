# Release pipeline (build, sign, checksum, manifest, attest, publish)

Triggered by `push` of a `v*` tag.
Two files own it: `.goreleaser.yaml` (what GoReleaser builds) and `.github/workflows/release.yml` (the steps GoReleaser doesn't do itself).
Everything downstream of "the six archives exist in `dist/`" — per-artifact checksums, the manifest, the signed install script, the provenance attestation — is explicit workflow steps, not GoReleaser plugins, so each is independently readable and independently rerunnable.

## Build (GoReleaser)

`.goreleaser.yaml` declares two `builds` entries, each with its own target matrix:

- `faber` (`./cmd/faber`): `linux`/`darwin` × `amd64`/`arm64` (4 targets) — the host CLI, which may run on either OS.
- `faber-box` (`./cmd/faber-box`): `linux` × `amd64`/`arm64` (2 targets) — bind-mounted read-only into every box and set as its entrypoint; it never runs on the host process directly, so it ships linux-only.

Both builds share:

- `CGO_ENABLED=0` — matches `faber-box`'s static-binary requirement (it must run inside a minimal container with no libc dependency to cross-compile); applied identically to `faber` for build-matrix simplicity.
- `-trimpath` and ldflags `-s -w` — no build-machine paths or symbol table in the shipped binary.
- ldflags `-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}` — stamps the three vars each binary's own `version.go` declares (default `"dev"`/`"none"`/`"unknown"` for a plain `go build`). `faber version`/`--version`/`-v` dispatches through `config.RunWithDeps` via `config.Deps.BuildInfo`; `faber-box version`/`--version`/`-v` is a direct pre-check in its own `main()` (it has no config-package CLI layer). A downloaded binary's version output is then a direct, unfalsifiable link back to the tag, commit, and build time that produced it.

Each build is archived (`archives`, one entry per build id) as `faber_<version>_<os>_<arch>.tar.gz` / `faber-box_<version>_<os>_<arch>.tar.gz`, bundling the binary with `README.md` and `LICENSE`.
`checksum.name_template: checksums.txt` produces one consolidated sha256 file over all six archives (GoReleaser's `checksum` block only ever produces this single combined file — per-artifact `.sha256` files are a workflow step, below).

## Signing (SSHSIG)

`.goreleaser.yaml`'s `signs` block runs, once per archive:

```
ssh-keygen -Y sign -f {{ .Env.SSH_SIGNING_KEY_PATH }} -n file ${artifact}
```

with `signature: "${artifact}.sig"` — `ssh-keygen -Y sign` always writes `<artifact>.sig` next to the input with no flag to redirect it, so this field must match that fixed naming exactly or GoReleaser looks for an output file that never appears.
`-n file` is the SSHSIG namespace: it domain-separates release-artifact signatures from git commit signatures (`-n git`), so a signature produced under one namespace is never valid under the other.

`SSH_SIGNING_KEY_PATH` is not the secret itself — the release workflow's "Write SSH signing key" step writes the `SSH_SIGNING_KEY` repository secret to `$RUNNER_TEMP/ssh-signing.key` under `umask 077` **and** an explicit `chmod 600` (ssh-keygen refuses to load a private key with group/other permissions set, even under a strict umask on some runner images — umask alone is not a contract GitHub-hosted runners document as stable) and exports the path via `GITHUB_ENV`, then a later step (`if: always()`) removes it.
The key material therefore never appears in a process argv (visible to any other process on the runner via `/proc`) or in a template-expanded command line that GitHub Actions might echo to a log.
The public half is committed to README.md's install section, so a verifier can run `ssh-keygen -Y verify` entirely offline.
It is the same key faber's sibling tools (portitor, spexmachina) reuse.

Signing, the sha256 checksums, and the SLSA attestation (below) are three independent ways to trust a downloaded binary — deliberately redundant, not layered fallbacks; a verifier who trusts only one of the three still gets a real guarantee.

## Signing `install.sh`

`install.sh` is not a GoReleaser-managed artifact (it is not built per target, and its content is version-agnostic — it does not change between releases, so it carries the same signature on every release it is attached to).
The release workflow signs it directly, immediately after `goreleaser release --clean` and before the signing key is removed, using the identical `ssh-keygen -Y sign -f "$SSH_SIGNING_KEY_PATH" -n file` invocation the `signs` block uses.
It is then uploaded to the release alongside its `.sig`, so `releases/latest/download/install.sh` and `.../install.sh.sig` are always both present at a stable URL.

## Upgrade mode (self-replace) — what `faber upgrade` runs

The same `install.sh` carries an upgrade mode (the `--upgrade` flag) reusing its identical resolve/download/SSHSIG-verify path, but replacing the two currently-installed binaries in place instead of installing into `INSTALL_DIR`.
It is what the config module's `faber upgrade` command runs (`spec/config/arch_cli.md`): the script is embedded byte-for-byte into the faber binary via `//go:embed` and executed from there — so for a given release the embedded copy is identical to the standalone `install.sh` a user verifies and runs by hand, and because it rides inside the already-trusted signed binary rather than being fetched, there is nothing to substitute and no fetch-and-verify-the-script step.
The embedded copy lives at `config/install.sh`, kept byte-identical to this canonical repo-root `install.sh` by `go generate ./config` (a `//go:generate cp ../install.sh install.sh` directive) and enforced by a build-failing identity test — that identity is the whole security argument.

Upgrade mode replaces **both** binaries as a unit, because a mismatched `faber`/`faber-box` pair is a broken state (they share a contract version — `agent/extract.go` `ReasonContractVersion`).
Each verified binary is staged as a sibling `<target>.new` so the swap is a same-filesystem `rename(2)` (safe over a running binary; a `cp`/`install` over the running file would hit `ETXTBSY` on Linux, and a cross-filesystem `mv` from the temp workdir would fall back to the same in-place write), then `mv target → target.bak; mv target.new → target`.
Both signatures are verified before either binary is touched, and on any mid-swap failure both roll back from their `.bak` — so the pair is never left mismatched (either both old or both new), which the config-side `upgrade-check` gate ("faber is not upgraded mid-run") makes safe by guaranteeing no live run spans the brief window between the two renames.
It also adds a downgrade guard (equal → already up to date, exit 0; older → refuse unless `--force`), `--rollback` (restore both from `.bak`), a `RequireRoot` check with a clear "re-run elevated" message (no auto-sudo, unlike install mode's `sudo` retry), and a `--dry-run` that resolves and verifies without touching disk.
The exact target paths and the mode signals arrive as flags (`--upgrade`/`--rollback`/`--check`, `--target`/`--box-target`, `--current`, `--force`) from the `faber upgrade` command — a self-documenting operator-facing contract; only the release pin (`VERSION`) and the test-only origin bases stay env.
Two base-URL overrides (`FABER_API_BASE`/`FABER_DL_BASE`) exist solely so the fake-server harness can retarget resolve/download at a local server; they default to the exact real GitHub URLs, so the released script is unchanged when they are unset — and `SIGNING_PUBKEY` is deliberately **not** overridable, because verification is the trust anchor and must never depend on the environment.

## Publish (GoReleaser → GitHub Release)

`goreleaser release --clean` (invoked via `goreleaser/goreleaser-action`) creates the GitHub release itself from the `v*` tag and uploads the six archives, `checksums.txt`, and the six `.sig` files.
`release.prerelease: auto` in `.goreleaser.yaml` marks a tag containing a pre-release suffix (e.g. `v0.1.1-rc.1`) as a GitHub pre-release automatically — no separate `workflow_dispatch` input to remember to set.

## Per-artifact checksums

A consolidated `checksums.txt` is enough to verify all six archives together, but not to verify one binary in isolation without the other five.
The release workflow's "Generate per-artifact checksums" step covers that case directly:

```bash
cd dist
for f in faber_*.tar.gz faber-box_*.tar.gz; do sha256sum "$f" > "$f.sha256"; done
```

producing `faber_<version>_<os>_<arch>.tar.gz.sha256` / `faber-box_<version>_linux_<arch>.tar.gz.sha256` per archive, uploaded to the release alongside `manifest.json` and the signed `install.sh` via `gh release upload … --clobber` (GoReleaser has already created the release by this point; `gh release upload` adds assets to it rather than creating a second one).

## Manifest (`scripts/generate-manifest.sh`)

Invoked as `scripts/generate-manifest.sh dist "${GITHUB_REF_NAME}" ok`, from the repository root (the script reads `dist/artifacts.json` and `dist/metadata.json`, and `artifacts.json`'s `path` fields are relative to the root GoReleaser was invoked from).

- `dist/metadata.json` supplies `version` (tag with the leading `v` stripped), `commit`, and `date` directly.
- `dist/artifacts.json` is an array covering every artifact GoReleaser produced across both builds; the script filters to `type == "Archive"` — exactly the six published `.tar.gz` files, not the intermediate per-target binary directories GoReleaser also lists.
- For each archive entry, the script does **not** trust `artifacts.json`'s own `extra.Checksum` field — it recomputes `sha256sum` and `stat -c%s` directly against the archive file on disk. The manifest is therefore self-verifying: it says what the bytes in `dist/` actually hash to, not what GoReleaser's internal bookkeeping claims they hash to.
- `target` is `<goos>_<goarch>` (e.g. `linux_amd64`); `name` alone distinguishes a `faber_*` entry from a `faber-box_*` one — the schema carries no bespoke per-binary field.

Output shape (`schema_version` pins the shape itself, independent of `tool`'s version):

```json
{
  "schema_version": 1,
  "tool": "faber",
  "version": "0.1.0",
  "git_sha": "…",
  "git_ref": "v0.1.0",
  "built_at": "2026-…",
  "status": "ok",
  "artifacts": [
    {
      "name": "faber_0.1.0_linux_amd64.tar.gz",
      "target": "linux_amd64",
      "sha256": "…",
      "size_bytes": 4025105,
      "archive_format": "tar.gz"
    }
  ]
}
```

This is a **generic** shape — no faber-specific field, no bespoke delivery-metadata schema elsewhere in the codebase.
`.goreleaser.yaml` remains the sole source of truth for *how* artifacts are built; `manifest.json` is a derived, disposable projection a downstream consumer (e.g. a Nix overlay pinning a `sha256` per platform) can consume programmatically instead of scraping the release page or hand-copying a checksum out of `checksums.txt`.
Re-running the script against the same `dist/` is idempotent and side-effect-free (it only reads).

## SLSA provenance

The release job's SLSA step, `actions/attest-build-provenance`, runs with `subject-path` covering both `dist/faber_*.tar.gz` and `dist/faber-box_*.tar.gz` (the six archives — not the checksum/signature/manifest files, which aren't independently executable artifacts).
The workflow's `permissions: {id-token: write, attestations: write}` are what let this step mint a Sigstore-backed attestation from the job's OIDC identity; a verifier runs `gh attestation verify <archive> --repo dmitriyb/faber` and gets an independent, GitHub-native chain of custody back to *this exact workflow run* — distinct from, and not dependent on, the SSHSIG signature.

## Local proof (no tag, no CI, no secrets)

Every piece above runs identically outside CI:

```bash
export SSH_SIGNING_KEY_PATH=/path/to/a/local/throwaway/key   # any ed25519 key, e.g. `ssh-keygen -t ed25519 -N ""`
goreleaser release --snapshot --clean
./scripts/generate-manifest.sh dist v0.0.0-snapshot-test ok > dist/manifest.json
```

`--snapshot` skips the "must be on a tag" / "must publish" checks GoReleaser normally enforces, so the full build → archive → sign → checksum chain runs on an uncommitted or untagged tree.
This is precisely how this module's local proof was captured: a throwaway ed25519 keypair, a snapshot release, `ssh-keygen -Y verify` against the resulting signatures, `sha256sum -c` against `checksums.txt`, `generate-manifest.sh` run against the real `dist/artifacts.json`/`metadata.json` it produced, and a separate `install.sh` sign-and-verify pass plus a fake-local-server dry run of the script itself (see `test_delivery.md`) — not a hypothetical shape guessed in advance.

## Boundaries

This module ships binaries and the install script only.
Faber's boxes are built locally from pinned Nix toolsets at run time (`faber build`, no Dockerfile, no repo content baked in) — there is no container image for this pipeline to build or publish, unlike a tool whose runtime is itself a container image.
No package-manager formula (Homebrew/Scoop/AUR) and no cross-repo checksum-dispatch notification are in scope: building one for a hypothetical future consumer is exactly the premature-coupling this module's Manifest component substitutes for — `manifest.json` gives any future consumer a stable, generic integration point without faber knowing who they are.
