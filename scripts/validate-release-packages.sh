#!/usr/bin/env bash
# Check the built package set and the native payloads it embeds.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSET_DIR="${ASSET_DIR:?ASSET_DIR is required}"
VERSION="${VERSION:?VERSION is required}"
[[ $# == 0 && "$VERSION" =~ ^[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?$ ]] || exit 2
ASSET_DIR="$(cd -- "$ASSET_DIR" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT INT TERM
require_asset() {
  [[ -f "$ASSET_DIR/$1" && ! -L "$ASSET_DIR/$1" && -s "$ASSET_DIR/$1" ]] || {
    echo "missing or unsafe release asset: $1" >&2
    exit 1
  }
}
for asset in install-controller install-node install.env.example controller-bootstrap.sh managed-node-bootstrap.sh \
  "controller-runtime-v$VERSION.tar.gz" "controller-runtime-v$VERSION.tar.gz.sha256"; do
  require_asset "$asset"
done
# Stage-1 now embeds shared helpers and all four scripts carry the exact tag.
# Compare against a fresh preparation from this build's source, preserving the
# old source/asset equality gate without treating the stamped bytes as source.
mkdir "$work/expected"
"$ROOT/scripts/prepare-bootstrap-release-assets.sh" "$work/expected" "v$VERSION"
for asset in install-controller install-node controller-bootstrap.sh managed-node-bootstrap.sh install.env.example; do
  cmp "$ASSET_DIR/$asset" "$work/expected/$asset"
done
cmp "$ASSET_DIR/install-controller" "$ASSET_DIR/controller-bootstrap.sh"
# Reuse the installer's archive checks, reading local build assets here.
# shellcheck source=deploy/lib/controller-runtime.sh
source "$ROOT/deploy/lib/controller-runtime.sh"
fail() { echo "release runtime: $*" >&2; exit 1; }
runtime_download() { cp -- "$ASSET_DIR/${1##*/}" "$2"; }
runtime_fetch "$work/runtime" "v$VERSION" "$(git -C "$ROOT" rev-parse HEAD)"
tar -xOf "$work/expected/controller-runtime-v$VERSION.tar.gz" SHA256SUMS >"$work/expected-runtime-checksums"
cmp "$work/runtime/SHA256SUMS" "$work/expected-runtime-checksums"
for arch in amd64 arm64; do
  case "$arch" in amd64) rpm_arch=x86_64; elf_word=x86-64 ;; arm64) rpm_arch=aarch64; elf_word=aarch64 ;; esac
  deb="ocservia-agent_${VERSION}-1_${arch}.deb"
  rpm="ocservia-agent-$VERSION-1.$rpm_arch.rpm"
  require_asset "$deb"
  require_asset "$rpm"
  [[ "$(dpkg-deb -f "$ASSET_DIR/$deb" Architecture)" == "$arch" ]]
  [[ "$(rpm -qp --qf '%{ARCH}' --nosignature "$ASSET_DIR/$rpm" 2>/dev/null)" == "$rpm_arch" ]]
  [[ "$(dpkg-deb -f "$ASSET_DIR/$deb" Version)" == "${VERSION/-rc./~rc.}-1" ]]
  [[ "$(rpm -qp --qf '%{VERSION}-%{RELEASE}' --nosignature "$ASSET_DIR/$rpm" 2>/dev/null)" == "${VERSION/-rc./~rc.}-1" ]]
  mkdir -p "$work/deb-$arch" "$work/rpm-$arch"
  dpkg-deb -x "$ASSET_DIR/$deb" "$work/deb-$arch"
  (cd "$work/rpm-$arch" && rpm2cpio "$ASSET_DIR/$rpm" | cpio -idm --quiet --no-absolute-filenames)
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    path="usr/libexec/ocservia/$binary"
    [[ "$(file -b "$work/deb-$arch/$path")" == *"$elf_word"* ]]
    cmp "$work/deb-$arch/$path" "$work/rpm-$arch/$path"
  done
done
echo "Release package set PASS"
