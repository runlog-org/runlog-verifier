#!/bin/sh
# Install or update runlog-verifier.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/runlog-org/runlog-verifier/main/install.sh | sh
#
# Or to pin a version / install dir:
#   curl -sSL https://raw.githubusercontent.com/runlog-org/runlog-verifier/main/install.sh | VERSION=v0.2.0 INSTALL_DIR=$HOME/.local/bin sh
#
# Re-running upgrades to the latest release. The script is idempotent: if the
# requested version is already installed, it exits without touching anything.

set -eu

REPO=runlog-org/runlog-verifier
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"

err() { printf '%s\n' "error: $*" >&2; exit 1; }
log() { printf '%s\n' "$*"; }

# ── Detect os/arch ─────────────────────────────────────────────────────────
uname_s=$(uname -s)
case "$uname_s" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *)      err "unsupported OS: $uname_s (this build ships linux + darwin only)" ;;
esac

uname_m=$(uname -m)
case "$uname_m" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) err "unsupported arch: $uname_m (this build ships amd64 + arm64 only)" ;;
esac

binary="runlog-verifier-${os}-${arch}"

# ── Resolve version ────────────────────────────────────────────────────────
# `latest` redirects to /releases/tag/vX.Y.Z; the final URL component is the tag.
if [ "$VERSION" = "latest" ]; then
    VERSION=$(curl -sSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/${REPO}/releases/latest" \
        | sed 's@.*/tag/@@')
    [ -n "$VERSION" ] || err "could not resolve latest version (network issue?)"
fi

# ── Skip if already installed ──────────────────────────────────────────────
target="${INSTALL_DIR}/runlog-verifier"
if [ -x "$target" ]; then
    current=$("$target" version 2>/dev/null | awk 'NR==1 {print $2}' || true)
    if [ "$current" = "$VERSION" ]; then
        log "runlog-verifier ${VERSION} already installed at ${target}"
        exit 0
    fi
    log "upgrading runlog-verifier ${current:-unknown} → ${VERSION}"
else
    log "installing runlog-verifier ${VERSION} to ${target}"
fi

# ── Download + checksum ────────────────────────────────────────────────────
base="https://github.com/${REPO}/releases/download/${VERSION}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

log "  fetching ${binary}"
curl -fsSL -o "${tmp}/${binary}" "${base}/${binary}" \
    || err "download failed: ${base}/${binary}"

log "  fetching SHA256SUMS"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS" \
    || err "download failed: ${base}/SHA256SUMS"

log "  verifying checksum"
( cd "$tmp" && grep " ${binary}\$" SHA256SUMS | sha256sum -c - >/dev/null ) \
    || err "checksum mismatch — refusing to install"

# ── Install ────────────────────────────────────────────────────────────────
mkdir -p "$INSTALL_DIR"
chmod +x "${tmp}/${binary}"
mv "${tmp}/${binary}" "$target"

log "installed runlog-verifier ${VERSION} at ${target}"
log
"$target" version
log
case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *) log "note: ${INSTALL_DIR} is not on your PATH — add it to your shell init (\$HOME/.profile, .bashrc, .zshrc) so 'runlog-verifier' is callable." ;;
esac
