# Delivery test scenarios

Unlike most of faber's modules, this module's behavior lives mostly in CI-only YAML and one shell script rather than Go functions `go test` can exercise directly.
Verification is therefore split between what CI enforces on every run and what was proven locally, once, against the real tool chain.

## What CI enforces (regression coverage)

1. **PR fast gate** — `go build ./...`, `go vet ./...`, `go test ./...` must all pass before a PR can land; this is the existing repo-wide test suite, not delivery-specific tests. A break here blocks merge, not just the eventual release.
2. **Push-to-main race gate** — the same suite plus `go test -race ./...`; a data race introduced by any change (not just delivery code) surfaces within one push of merge, not at release time.
3. **Nightly fuzz** — every `func Fuzz*` discovered in the tree runs for a bounded `-fuzztime`; a crashing input uploads as a `fuzz-failures` artifact (see `arch_ci.md`). This is regression coverage for the parsing/journal/extraction functions named in `spec/reviews/2026-07-18.md` §5, not for the delivery pipeline itself.
4. **Release-job test gate** — `.github/workflows/release.yml` runs `go test ./...` before invoking GoReleaser, so a release build never ships from a red tree even if CI's own gate was somehow bypassed for the tagged commit.

## What was proven locally (one-time, this module's local proof)

Run as `goreleaser release --snapshot --clean` against an untagged tree, with a throwaway ed25519 keypair (`ssh-keygen -t ed25519 -N ""`, generated solely for this proof — never committed, never used for a real release), with GoReleaser's `dist:` output redirected to a scratch directory outside the repository (never written into the tracked tree):

1. **Both binaries' cross-arch builds succeed** — all four `faber` `goos`×`goarch` combinations (`linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`) and both `faber-box` combinations (`linux_amd64`, `linux_arm64`) compile with `CGO_ENABLED=0` and produce a `<binary>_<version>_<os>_<arch>.tar.gz` archive containing the binary, `README.md`, and `LICENSE` — six archives total.
2. **ldflags stamping is correct** — the `linux_arm64` binaries of both `faber` and `faber-box` were extracted and run directly (the dev host is linux/arm64); `faber version` / `faber-box version` printed the snapshot version, the real commit SHA, and the build timestamp GoReleaser injected — confirming each binary's own `main.version`/`main.commit`/`main.date` vars receive exactly what `-ldflags -X` sends them, independently (they are separate `main` packages).
3. **Signing round-trips** — `ssh-keygen -Y verify` against the snapshot's own public key succeeded for all six archives, reporting `Good "file" signature`.
4. **Checksums are internally consistent** — `sha256sum -c checksums.txt` against all six archives in `dist/`, run from within `dist/`, reported `OK` for all six.
5. **Manifest generation matches reality** — `scripts/generate-manifest.sh dist <ref> ok` was run against the real `dist/artifacts.json`/`metadata.json` this snapshot produced (not a hand-constructed fixture). Every `sha256`/`size_bytes` pair in the resulting `manifest.json` was cross-checked against `checksums.txt` and byte-verified against the archives directly — an exact match for all six targets.
6. **Fuzz-target discovery matches the tree** — the `grep`/`awk` discovery loop `fuzz.yml` uses was run standalone against the working tree and found all eight `func Fuzz*` targets across `config`, `failure`, and `pipeline`.
7. **`install.sh` signing and verification round-trips** — a copy of `install.sh` was signed with the same throwaway key (`ssh-keygen -Y sign -n file`, the identical invocation the release workflow's dedicated signing step uses) and verified successfully, alongside the six archives.
8. **`install.sh`'s network path was exercised end to end against a fake server** — a temp copy of the script with the GitHub API/download URLs pointed at `127.0.0.1` and `SIGNING_PUBKEY` swapped for the throwaway key's public half (the only three lines that differ from the real script, confirmed via `diff`) was run against a `python3 -m http.server` serving the renamed snapshot archives under GitHub-shaped paths:
   - **Golden path**: resolved the fake "latest release" tag, downloaded the matching `faber` and `faber-box` archives plus their `.sig` files, verified both, extracted, installed to a scratch `INSTALL_DIR`, and ran `<install_dir>/faber version` / `<install_dir>/faber-box version` successfully — exit 0.
   - **Fail-closed, tampered archive**: one byte flipped in a served archive. The re-run reported `Could not verify signature.` and `install.sh: error: signature verification FAILED ... — refusing to install an unverified binary`, exited non-zero, and left the install directory empty.
   - **Fail-closed, nonexistent release**: `VERSION=v9.9.9-does-not-exist` produced a 404 from the fake server, `install.sh: error: download failed: ...`, exit non-zero, empty install directory.
   - A subsequent clean golden-path run (after restoring the untampered archive) confirmed the fail-closed runs left no residue affecting a later install.

Item 2 is a *manual* substitute for a release workflow step pattern seen in some GoReleaser templates ("verify binary version matches tag") — faber's release workflow does not include an equivalent automated step, because GoReleaser's snapshot mode has no tag to compare against and a real tagged release's `{{.Version}}` is derived directly from the tag by GoReleaser itself (there is no separate version file that could drift from it).

Item 8's `sh -n install.sh` was also run and passed, but proves only syntax — the fake-server dry run above is what actually exercises the resolve/download/verify/install logic, including the fail-closed paths.
