# Installing faber

Two binaries are published on the [GitHub Releases page][releases]: `faber`,
the host CLI (linux/darwin, amd64/arm64), and `faber-box`, the in-container
phase sequencer (linux only, amd64/arm64). `faber-box` is bind-mounted into
every box and never runs on the host directly, see [`deploy.md`](deploy.md);
the two are installed side by side and upgraded as a pair. Every release
archive is signed with SSHSIG (`ssh-keygen -Y sign`), verifiable with the
`ssh-keygen` that ships with OpenSSH — no extra tool to install just to verify.

[releases]: https://github.com/dmitriyb/faber/releases

## Why not `curl | sh`

A piped script executes as it streams and cannot verify itself before it
runs. Verification therefore has to wrap the download from outside the
stream: download the script, verify the script, then run it. That is the
whole reason the primary path is three commands rather than one pipe.

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

**plain `sh`** (no `<(…)` process substitution): write the allowed-signers
line to a file first, then verify against it.

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
```

The block verifies **the script itself** against the public key below and
only then runs it. `install.sh` resolves the latest release, detects your
OS/arch, downloads the matching `faber` archive plus the `faber-box` archive
(linux, your arch, always) and their signatures, and verifies **both** with
the same key (embedded in the script, trusted because the script was just
verified) before installing them side by side.

The script is POSIX `sh`. Set `VERSION=v0.1.0` before `sh install.sh` to
install a specific release, and `INSTALL_DIR=DIR` to choose the install
directory (default `/usr/local/bin`).

## Maximal: verify the binary archives directly

No install script — download the archives for your platform from the
[Releases page][releases], then verify each by any one of:

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
Each release also carries a consolidated `checksums.txt`, one `.sha256` per
archive, and a machine-readable `manifest.json` (schema, target, sha256, size
per artifact).

## What each channel protects, and what it doesn't

- **Primary** verifies both the install script and the binaries it fetches,
  end to end: `download → verify → run`, never a piped script.
- **Maximal** gives the strongest per-artifact check for a single file, with
  no script in between.
- The trust anchor in both cases is the public key **copied from this page**.
  That defeats tampering in transit; the residual risk is a look-alike copy
  of this repository, closed by using the known repository URL and by pinning
  the key **once** — copy it a single time, then verify every future release
  against that pinned copy.
- Signatures and attestations give **authenticity, not freshness**: an
  attacker who can intercept a download could still steer a *first install*
  to a genuine-but-older release. For *updates* this is closed: `faber upgrade`
  is forward-only and hard-refuses a resolved latest older than what is
  installed.

## Public key

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

The same key serves all three verification paths above, and the same line
is published by faber's sibling tools, [portitor](https://github.com/dmitriyb/portitor)
and [spexmachina](https://github.com/dmitriyb/spexmachina), so one pinned
copy serves all three. It can be cross-checked against GitHub's own copy at
`https://api.github.com/users/dmitriyb/ssh_signing_keys` once it is added
under Settings → SSH and GPG keys → Signing keys — useful if this page itself
is suspected of being tampered with in a fork or mirror.

## Upgrading

An installed pair updates itself with `faber upgrade`, which embeds the same
signed `install.sh` and runs it against the installed binaries: the same
resolve → download → SSHSIG-verify, then both binaries replaced as a unit,
since a mismatched `faber`/`faber-box` pair is a broken state (the two share
a contract version). Both signatures are verified before either binary is
touched, and the previous pair is kept as `.bak` for `--rollback`. It refuses
while a run is live or unfinished — faber is not swapped mid-run; `--force`
overrides that guard and only that guard.

```sh
faber upgrade                    # forward to the latest release
faber upgrade --version v0.1.4   # a specific release, any direction
faber upgrade --check            # resolve and verify the target, change nothing (also: --dry-run)
faber upgrade --rollback         # restore the previous pair from their .bak backups
faber upgrade --force            # proceed despite live/unfinished runs
```

Upgrade is **forward-only**: it hard-refuses, non-overridably, a resolved
latest that is *older* than the installed version — a signature proves
authenticity, not freshness, so a latest that moved backward is treated as a
rollback anomaly. `--check` reports the comparison without changing anything
(it warns about active runs and exits 0 whenever the latest resolved),
`--version vX.Y.Z` installs an exact release in any direction (the deliberate
path to an older release), and `--rollback` restores the backup pair. See
[`commands.md`](commands.md) for the flag reference.
