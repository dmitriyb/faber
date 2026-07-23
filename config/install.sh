#!/bin/sh
# faber installer / self-upgrader.
#
# Resolves the latest faber release (or VERSION, if set), downloads the
# matching faber archive and faber-box archive plus their SSHSIG signatures,
# verifies each signature against the embedded release-signing public key,
# and installs both binaries side by side.
#
# faber-box is bind-mounted into every box and must be a Linux binary
# regardless of the operator's own host OS (Docker Desktop shares the host
# path into its own Linux VM) — on a darwin host this script still fetches
# the linux/<arch> faber-box archive, matching the arch Docker Desktop's VM
# runs as by default; it never silently skips faber-box.
#
# This script's own content is version-agnostic: it does not change between
# releases, so it carries the same signature on every release it is attached
# to. It is meant to be downloaded and verified BEFORE it runs — see the
# README's install section for the verified copy-paste block. Running it
# via a pipe (curl ... | sh) skips that verification step and is not the
# supported path.
#
# Upgrade mode (the --upgrade flag) reuses the identical resolve/download/verify
# path but replaces the two currently-installed binaries in place instead of
# installing into INSTALL_DIR. It is what `faber upgrade` runs: the same
# script, embedded byte-for-byte into the faber binary, so there is nothing
# separate to fetch or trust. See the "upgrade mode" block below.
#
# Usage:
#   sh install.sh                    install the latest release
#   VERSION=v0.1.0 sh install.sh     install a specific release
#   INSTALL_DIR=~/bin sh install.sh  install somewhere other than /usr/local/bin
#
# Fails closed: if any download or signature verification does not succeed,
# this script exits non-zero without installing anything.

set -eu

OWNER="dmitriyb"
REPO="faber"

# The principal string used both here and in the README's allowed_signers
# line — it must match exactly, or verification fails even with the right
# key (ssh-keygen -Y verify checks -I against the allowed_signers entry).
SIGNER_ID="dvbozhko@gmail.com"

SIGNING_PUBKEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing"

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# Base URLs are overridable ONLY so the dry-run test harness can point the
# resolve/download steps at a local fake server; they default to the real
# GitHub endpoints, so the released script behaves identically when they are
# unset. The signing public key above is deliberately NOT overridable —
# verification is the trust anchor and must never depend on the environment.
API_BASE="${FABER_API_BASE:-https://api.github.com/repos/${OWNER}/${REPO}}"
DL_BASE="${FABER_DL_BASE:-https://github.com/${OWNER}/${REPO}/releases/download}"

fail() {
  echo "install.sh: error: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "'$1' is required but not found on PATH"
}

need curl
need ssh-keygen
need tar
need mktemp
need uname

# --- upgrade-mode flags (none in the default install path) ---
# `faber upgrade` invokes this script with flags rather than env, so the
# operator-facing contract is self-documenting; only VERSION (a release pin)
# and the test-only origin bases above stay env. --upgrade selects self-replace
# semantics instead of install(1)-ing into INSTALL_DIR; --target / --box-target
# are the exact installed faber and faber-box paths to replace (`faber upgrade`
# resolves them via os.Executable and the FABER_BOX_BIN convention); --current
# drives the downgrade guard; --force allows a downgrade / skips confirmation;
# --rollback restores the previous pair from their .bak backups; --check /
# --dry-run resolve and verify the target release without touching disk.
UPGRADE=""
FABER_TARGET=""
FABER_BOX_TARGET=""
CURRENT_VERSION=""
UPGRADE_FORCE=""
ROLLBACK=""
DRY_RUN=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --upgrade) UPGRADE=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    --check | --dry-run) DRY_RUN=1; shift ;;
    --force) UPGRADE_FORCE=1; shift ;;
    --target) FABER_TARGET="${2:?--target requires a value}"; shift 2 ;;
    --box-target) FABER_BOX_TARGET="${2:?--box-target requires a value}"; shift 2 ;;
    --current) CURRENT_VERSION="${2:?--current requires a value}"; shift 2 ;;
    -*) fail "unknown option: $1" ;;
    *) fail "unexpected argument: $1" ;;
  esac
done

# ver_cmp A B: prints -1 if release A is older than B, 0 if equal, 1 if newer,
# comparing the numeric major.minor.patch components (a leading v and any
# -prerelease/+build suffix are stripped; pre-release ordering is not
# interpreted). Three-way rather than a boolean so the downgrade guard can tell
# equal (already up to date) from older (refuse) from newer (proceed) in one
# call.
ver_cmp() {
  a="${1#v}"; a="${a%%-*}"; a="${a%%+*}"
  b="${2#v}"; b="${b%%-*}"; b="${b%%+*}"
  i=1
  while [ "$i" -le 3 ]; do
    ai=$(printf '%s' "$a" | cut -d. -f"$i"); ai=${ai:-0}
    bi=$(printf '%s' "$b" | cut -d. -f"$i"); bi=${bi:-0}
    if [ "$ai" -gt "$bi" ]; then echo 1; return; fi
    if [ "$ai" -lt "$bi" ]; then echo -1; return; fi
    i=$((i + 1))
  done
  echo 0
}

# require_writable fails (without escalating) unless the directory holding a
# target binary is writable: the swap stages a sibling <target>.new and
# renames within that directory, so write access there is what upgrade mode
# needs. Upgrade mode never auto-sudos — a running faber being replaced is
# not the moment to prompt for a password on the operator's behalf.
require_writable() {
  d=$(dirname "$1")
  if [ ! -w "$d" ]; then
    if [ "$(id -u)" = 0 ]; then
      fail "upgrade: $d is not writable even as root"
    fi
    fail "upgrade: $d is not writable — re-run elevated (e.g. sudo -E faber upgrade); no privileges are escalated automatically"
  fi
}

# --- rollback mode: restore the previous pair, no download ---
# Handled before resolving a release: rollback is a local, offline recovery
# from the .bak backups the last successful upgrade left behind. Both backups
# must be present, or it refuses and touches nothing (fail closed) — the pair
# is never left half-restored.
if [ -n "$ROLLBACK" ]; then
  [ -n "$FABER_TARGET" ] && [ -n "$FABER_BOX_TARGET" ] \
    || fail "rollback: FABER_TARGET and FABER_BOX_TARGET must be set"
  [ -e "${FABER_TARGET}.bak" ] && [ -e "${FABER_BOX_TARGET}.bak" ] \
    || fail "rollback: no backup found (${FABER_TARGET}.bak / ${FABER_BOX_TARGET}.bak) — nothing to roll back to"
  require_writable "$FABER_TARGET"
  require_writable "$FABER_BOX_TARGET"
  mv -f "${FABER_TARGET}.bak" "$FABER_TARGET" || fail "rollback: restoring faber failed"
  mv -f "${FABER_BOX_TARGET}.bak" "$FABER_BOX_TARGET" || fail "rollback: restoring faber-box failed"
  echo "faber rollback: restored faber and faber-box from their .bak backups" >&2
  "$FABER_TARGET" version || true
  exit 0
fi

# --- resolve the release tag ---
if [ -n "${VERSION:-}" ]; then
  tag="$VERSION"
else
  api_url="${API_BASE}/releases/latest"
  # grep (no -m1) + head: read the body to EOF so curl can't abort with (56).
  tag=$(curl -fsSL "$api_url" | grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  [ -n "$tag" ] || fail "could not resolve the latest release tag from $api_url"
fi
version="${tag#v}"

# --- downgrade guard (upgrade mode only) ---
# Equal → already up to date, exit 0 before any download. Older than the
# installed version → refuse unless forced. A dev/unstamped current version
# cannot be ordered against a release tag, so the guard is skipped for it.
if [ -n "$UPGRADE" ] && [ -n "$CURRENT_VERSION" ] && [ "$CURRENT_VERSION" != "dev" ]; then
  cmp=$(ver_cmp "$version" "$CURRENT_VERSION")
  if [ "$cmp" = 0 ]; then
    echo "faber upgrade: already at ${tag}; nothing to do" >&2
    exit 0
  elif [ "$cmp" = "-1" ]; then
    [ -n "$UPGRADE_FORCE" ] \
      || fail "upgrade: target ${tag} is older than the installed v${CURRENT_VERSION#v} — refusing to downgrade (pass --force to override)"
    echo "faber upgrade: --force: proceeding with downgrade ${CURRENT_VERSION} -> ${tag}" >&2
  fi
fi

# --- detect OS/arch (faber ships linux and darwin; faber-box ships linux only) ---
os=$(uname -s)
case "$os" in
  Linux) goos=linux ;;
  Darwin) goos=darwin ;;
  *) fail "unsupported OS: $os (faber ships linux and darwin binaries only)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) goarch=amd64 ;;
  arm64 | aarch64) goarch=arm64 ;;
  *) fail "unsupported architecture: $arch (faber ships amd64 and arm64 binaries only)" ;;
esac

if [ "$goos" != "linux" ]; then
  echo "install.sh: host is $goos; faber-box still ships linux/${goarch} (Docker Desktop runs containers in a Linux VM matching the host's arch by default) — fetching it explicitly, not skipping it" >&2
fi

faber_archive="faber_${version}_${goos}_${goarch}.tar.gz"
box_archive="faber-box_${version}_linux_${goarch}.tar.gz"
base_url="${DL_BASE}/${tag}"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT INT TERM

allowed_signers="${workdir}/allowed_signers"
printf '%s %s\n' "$SIGNER_ID" "$SIGNING_PUBKEY" >"$allowed_signers"

# fetch_and_verify downloads one archive and its .sig into workdir and
# verifies the signature, failing closed (no binary is extracted or
# installed until every fetch_and_verify call for this run has succeeded).
fetch_and_verify() {
  archive="$1"
  echo "faber installer: fetching ${archive} (${tag})" >&2
  curl -fsSL "${base_url}/${archive}" -o "${workdir}/${archive}" \
    || fail "download failed: ${base_url}/${archive}"
  curl -fsSL "${base_url}/${archive}.sig" -o "${workdir}/${archive}.sig" \
    || fail "download failed: ${base_url}/${archive}.sig"

  if ! ssh-keygen -Y verify \
    -f "$allowed_signers" \
    -I "$SIGNER_ID" \
    -n file \
    -s "${workdir}/${archive}.sig" \
    <"${workdir}/${archive}" >&2; then
    fail "signature verification FAILED for ${archive} — refusing to install an unverified binary"
  fi
  echo "faber installer: signature verified for ${archive}" >&2
}

fetch_and_verify "$faber_archive"
fetch_and_verify "$box_archive"

tar -xzf "${workdir}/${faber_archive}" -C "$workdir" faber \
  || fail "failed to extract the faber binary from ${faber_archive}"
tar -xzf "${workdir}/${box_archive}" -C "$workdir" faber-box \
  || fail "failed to extract the faber-box binary from ${box_archive}"
chmod 755 "${workdir}/faber" "${workdir}/faber-box"

# --- upgrade mode: self-replace both installed binaries ---
# Both signatures have already been verified above; only after that does any
# rename touch a live path. The two binaries are replaced as a unit so the
# contract-version-coupled pair (see agent/extract.go's ReasonContractVersion)
# is never left mismatched — either both old or both new.
if [ -n "$UPGRADE" ]; then
  [ -n "$FABER_TARGET" ] && [ -n "$FABER_BOX_TARGET" ] \
    || fail "upgrade: FABER_TARGET and FABER_BOX_TARGET must be set"
  require_writable "$FABER_TARGET"
  require_writable "$FABER_BOX_TARGET"

  if [ -n "$DRY_RUN" ]; then
    echo "faber upgrade (dry-run): verified ${tag}; would replace:" >&2
    echo "  ${FABER_TARGET}" >&2
    echo "  ${FABER_BOX_TARGET}" >&2
    exit 0
  fi

  # Stage each verified binary as a sibling of its target so the swap is a
  # same-filesystem rename(2), which is safe over a running binary; a
  # cross-filesystem mv from the temp workdir would fall back to opening and
  # writing the target in place and hit ETXTBSY on Linux. The staged name is
  # PID-suffixed so two concurrent upgrades cannot collide on it. Both are
  # staged before any rename, so a staging failure leaves every live path
  # untouched.
  sfx=".new.$$"
  cp "${workdir}/faber" "${FABER_TARGET}${sfx}" || fail "upgrade: staging faber failed"
  cp "${workdir}/faber-box" "${FABER_BOX_TARGET}${sfx}" \
    || { rm -f "${FABER_TARGET}${sfx}"; fail "upgrade: staging faber-box failed"; }
  chmod 755 "${FABER_TARGET}${sfx}" "${FABER_BOX_TARGET}${sfx}"

  # swap moves the running binary aside to <target>.bak, then renames the
  # staged <target>${sfx} into place — two same-directory rename(2)s. restore
  # puts <target>.bak back. On any failure mid-swap both binaries are rolled
  # back so the pair stays matched.
  swap() { mv -f "$1" "${1}.bak" && mv -f "${1}${sfx}" "$1"; }
  restore() { [ -e "${1}.bak" ] && mv -f "${1}.bak" "$1"; }

  if ! swap "$FABER_TARGET"; then
    restore "$FABER_TARGET"
    rm -f "${FABER_TARGET}${sfx}" "${FABER_BOX_TARGET}${sfx}"
    fail "upgrade: replacing faber failed; nothing changed"
  fi
  if ! swap "$FABER_BOX_TARGET"; then
    restore "$FABER_BOX_TARGET"
    rm -f "${FABER_BOX_TARGET}${sfx}"
    restore "$FABER_TARGET"
    fail "upgrade: replacing faber-box failed; rolled faber back — both left at the previous version"
  fi

  echo "faber upgrade: ${CURRENT_VERSION:-unknown} -> ${tag}" >&2
  echo "  ${FABER_TARGET}" >&2
  echo "  ${FABER_BOX_TARGET}" >&2
  echo "  previous binaries kept at *.bak (faber upgrade --rollback restores them)" >&2
  "$FABER_TARGET" version || true
  exit 0
fi

# --- default install mode ---
# install_binary installs one already-verified binary from workdir, next to
# any binary installed earlier in this run — faber looks for faber-box next
# to its own executable by default (FABER_BOX_BIN overrides), so keeping
# both in the same INSTALL_DIR is what makes that default resolution work.
install_binary() {
  name="$1"
  if [ -w "$INSTALL_DIR" ]; then
    install -m 0755 "${workdir}/${name}" "${INSTALL_DIR}/${name}"
  else
    echo "faber installer: ${INSTALL_DIR} is not writable, retrying with sudo" >&2
    need sudo
    sudo install -m 0755 "${workdir}/${name}" "${INSTALL_DIR}/${name}"
  fi
}

install_binary faber
install_binary faber-box

echo "faber installed to ${INSTALL_DIR}/faber" >&2
echo "faber-box installed to ${INSTALL_DIR}/faber-box" >&2
"${INSTALL_DIR}/faber" version || true
