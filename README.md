# faber

A generic containerized-agent workflow engine.
You declare a workflow in one `orchestrator.yaml`; faber compiles it to a deterministic JSON IR, builds immutable agent images from pinned Nix package sets, and executes the workflow as a host-side DAG of single-purpose containers ("boxes").

```
orchestrator.yaml ──validate──▶ JSON IR (acyclic, byte-deterministic)
                                   │
                        host-side scheduler (topological, parallel, CEL conditions)
                                   │            ┌─ journal (resume) ─┐
                            one box per step ───┤  metering (admit)  │
                                   │            └─ on_failure hooks ─┘
              ┌────────────────────┴────────────────────┐
              │ container: context → prelude → agent →  │
              │ result   (fixed phase order, engine-owned) │
              └─────────────────────────────────────────┘
```

Faber is **mechanism, not policy**: it knows `docker build`/`docker run`, a workflow DAG, and a handful of pluggable interfaces.
It never learns your issue tracker, your review gate, or your agent vendor — all opinionated behavior arrives as user config: opaque scripts, typed params, data-source commands, and companion services on a docker network faber treats as opaque.
See [`docs/architecture.md`](docs/architecture.md) for the full model.

---

## Install

Two binaries are published on the [GitHub Releases page][releases]: `faber` (the host CLI, linux/darwin, amd64/arm64) and `faber-box` (the in-container phase sequencer, linux only, amd64/arm64 — it runs as every box's entrypoint, never on the host directly; see [`docs/deploy.md`](docs/deploy.md)).
Every release archive is signed with SSHSIG (`ssh-keygen -Y sign`), verifiable with the `ssh-keygen` that already ships with OpenSSH on essentially every machine — no extra tool to install just to verify.

[releases]: https://github.com/dmitriyb/faber/releases

### Primary: verified install script

**bash / zsh:**

```bash
curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

**fish:**

```fish
curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh -o install.sh
and curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh.sig -o install.sh.sig
and ssh-keygen -Y verify -f (printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' | psub) -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
and sh install.sh
and rm -f install.sh install.sh.sig
```

This downloads `install.sh`, verifies **the script itself** against the public key below, and only then runs it — never `curl | sh`.
`install.sh` then resolves the latest release, detects your OS/arch, downloads the matching `faber` archive plus the `faber-box` archive (linux/arch always — see above) and their signatures, and verifies both with the same key (embedded in the script, trusted because the script was just verified) before installing them side by side.
Set `VERSION=v0.1.0` before the final `sh install.sh` to install a specific release instead of the latest.

The block above needs bash or zsh (`<(…)` process substitution).
Under a plain `sh`, write the allowed-signers line to a file first:

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
```

### Maximal: verify the binary archives directly

No install script — download the archives for your platform from the [Releases page][releases], then verify each by any one of:

```bash
# SSHSIG, against the same pinned key as above
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file \
  -s faber_<version>_<os>_<arch>.tar.gz.sig < faber_<version>_<os>_<arch>.tar.gz

# SLSA provenance via Sigstore/Rekor — identity-anchored, no key to manage
gh attestation verify faber_<version>_<os>_<arch>.tar.gz --repo dmitriyb/faber

# Go users: the Go module checksum database
go install github.com/dmitriyb/faber/cmd/faber@<tag>
CGO_ENABLED=0 GOOS=linux go install github.com/dmitriyb/faber/cmd/faber-box@<tag>
```

The same three checks apply to `faber-box_<version>_linux_<arch>.tar.gz`.
Each release also carries a consolidated `checksums.txt`, one `.sha256` per archive, and a machine-readable `manifest.json` (schema, target, sha256, size per artifact).

### What each channel protects, and what it doesn't

- **Primary** verifies both the install script and the binaries it fetches, end to end — `download → verify → run`, never a piped script: a piped `curl … | sh` executes as it streams and cannot verify itself before running, so verification has to wrap the download from outside the stream, which is exactly why the primary path is not a one-liner pipe.
- **Maximal** gives you the strongest per-artifact check for a single file, with no script in between.
- The trust anchor in both cases is the public key **copied from this README** — that defeats tampering of the download in transit; the residual risk is being sent to a look-alike or phishing copy of this repository, closed by using the known repository URL and by pinning the public key **once** — copy it a single time, then verify every future release against that pinned copy rather than re-copying it from wherever you happen to land.
- Signatures and attestations give **authenticity, not freshness**: a channel attacker who can intercept your download could still steer you to a genuine-but-older, vulnerable release (a downgrade); this applies to every channel above equally, and there is no minimum-version floor enforced today — note it as a residual risk rather than a solved one.

### Public key

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

This is the same key across all three verification paths above (SSHSIG install script, SSHSIG archives, and the `allowed_signers` line either way) — and the same key faber's sibling tools (portitor, spexmachina) use.
It can also be pinned and cross-checked against GitHub's own copy at `https://api.github.com/users/dmitriyb/ssh_signing_keys`, once it is added under Settings → SSH and GPG keys → Signing keys — useful if this README itself is ever suspected of being tampered with in a fork or mirror.

## Usage sketch

```sh
faber validate --config orchestrator.yaml   # schema, wiring, types, cycles, nix resolution
faber build    --config orchestrator.yaml   # nix-build + docker-load every template image
faber run task --config orchestrator.yaml \
    --param repo=sandbox --param item=I-1   # execute; prints the run report + journal path
faber resume <run-id>                       # skip journaled hits, restart at the first gap
```

Everything wrong with a config surfaces at `validate` with field paths — missing params, unknown output fields, type mismatches, reference cycles, unresolvable packages — never mid-run.
See [`examples/`](examples/) for working configurations.

## Learn more

- [`docs/architecture.md`](docs/architecture.md) — how a run works: the box, the security boundary, the DAG, the failure/resume model.
- [`docs/configuration.md`](docs/configuration.md) — the `orchestrator.yaml` schema by example.
- [`docs/commands.md`](docs/commands.md) — the full CLI reference.
- [`docs/deploy.md`](docs/deploy.md) — deploying faber: host requirements, companion topology, credentials, operations.
- `spec/**` — the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0 (see `LICENSE`).
