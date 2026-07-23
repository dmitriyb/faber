#!/bin/sh
# faber installer.
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

# --- resolve the release tag ---
if [ -n "${VERSION:-}" ]; then
  tag="$VERSION"
else
  api_url="https://api.github.com/repos/${OWNER}/${REPO}/releases/latest"
  # grep (no -m1) + head: read the body to EOF so curl can't abort with (56).
  tag=$(curl -fsSL "$api_url" | grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  [ -n "$tag" ] || fail "could not resolve the latest release tag from $api_url"
fi
version="${tag#v}"

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
base_url="https://github.com/${OWNER}/${REPO}/releases/download/${tag}"

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
