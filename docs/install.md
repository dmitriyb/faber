# Installing faber

Two binaries are published on the [GitHub Releases page][releases]: `faber` (the host CLI, linux/darwin, amd64/arm64) and `faber-box` (the in-container phase sequencer, linux only, amd64/arm64).
`faber-box` runs as every box's entrypoint, bind-mounted into the container, and never on the host directly (see [`deploy.md`](deploy.md)); both are installed side by side and upgraded as a pair.
Every release archive is signed with SSHSIG (`ssh-keygen -Y sign`), verifiable with the `ssh-keygen` that already ships with OpenSSH — no extra tool to install just to verify.

[releases]: https://github.com/dmitriyb/faber/releases

## Primary: verified install script

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
`install.sh` then resolves the latest release, detects your OS/arch, downloads the matching `faber` archive plus the `faber-box` archive (linux/arch always) and their signatures, and verifies both with the same key (embedded in the script, trusted because the script was just verified) before installing them side by side.
Set `VERSION=v0.1.0` before the final `sh install.sh` to install a specific release instead of the latest.

The block above needs bash or zsh (`<(…)` process substitution).
Under a plain `sh`, write the allowed-signers line to a file first:

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
sh install.sh
```

## Maximal: verify the binary archives directly

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

## What each channel protects, and what it doesn't

- **Primary** verifies both the install script and the binaries it fetches, end to end — `download → verify → run`, never a piped script. A piped `curl … | sh` executes as it streams and cannot verify itself before running, so verification has to wrap the download from outside the stream; that is why the primary path is not a one-liner pipe.
- **Maximal** gives you the strongest per-artifact check for a single file, with no script in between.
- The trust anchor in both cases is the public key **copied from this document**. That defeats tampering of the download in transit; the residual risk is being sent to a look-alike copy of this repository, closed by using the known repository URL and by pinning the public key **once** — copy it a single time, then verify every future release against that pinned copy.
- Signatures and attestations give **authenticity, not freshness**: a channel attacker who can intercept your download could still steer you to a genuine-but-older, vulnerable release. This applies to every channel above equally at *first install*, where there is no installed version to floor against. For *updates* it is closed: `faber upgrade` is forward-only (see Upgrading).

## Public key

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

This is the same key across all three verification paths above (SSHSIG install script, SSHSIG archives, and the `allowed_signers` line either way), and the same key faber's sibling tools (portitor, spexmachina) use.
It can also be pinned and cross-checked against GitHub's own copy at `https://api.github.com/users/dmitriyb/ssh_signing_keys`, once it is added under Settings → SSH and GPG keys → Signing keys — useful if this document itself is ever suspected of being tampered with in a fork or mirror.

## Upgrading

Once faber is installed, `faber upgrade` updates it — and its coupled `faber-box` — to the latest signed release in place, without re-downloading `install.sh`:

```sh
faber upgrade                    # forward to the latest release
faber upgrade --version v0.1.4   # a specific release, any direction
faber upgrade --check            # resolve and verify the target, change nothing (also: --dry-run)
faber upgrade --rollback         # restore the previous pair from their .bak backups
faber upgrade --force            # proceed despite live/unfinished runs
```

`faber upgrade` reuses the exact `install.sh` above, embedded byte-for-byte into the already-verified faber binary, so the same resolve → download → SSHSIG-verify path runs with nothing separate to fetch or trust.
It first runs the active-runs guard: it refuses while a run is live or unfinished, since faber is not swapped mid-run (`--force` overrides that guard).
It then replaces **both** binaries as a unit, because a mismatched `faber`/`faber-box` pair is a broken state (the two share a contract version).
Both signatures are verified before either binary is touched (fail closed), and the previous pair is kept alongside the new one for `--rollback`.

Upgrade is **forward-only**: it moves toward the latest release and hard-refuses a latest that is OLDER than the installed version. A latest that moved backward is a rollback anomaly (a compromised origin serving an old but validly-signed release as "latest"), and no flag overrides it.
To install an older release deliberately, name it with `--version`, which installs that exact release in any direction with no guard.
`faber upgrade --check` reports availability and changes nothing; it warns about (but does not block on) active runs and exits 0 whenever it could resolve the latest release.
