#!/usr/bin/env bash
# Check the built package set and the native payloads it embeds.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSET_DIR="${ASSET_DIR:?ASSET_DIR is required}"
VERSION="${VERSION:?VERSION is required}"
[[ $# == 0 && "$VERSION" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]] || exit 2
ASSET_DIR="$(cd -- "$ASSET_DIR" && pwd)"
work="$(mktemp -d)"
trap 'sudo rm -rf -- "$work"' EXIT INT TERM
require_asset() {
  [[ -f "$ASSET_DIR/$1" && ! -L "$ASSET_DIR/$1" && -s "$ASSET_DIR/$1" ]] || {
    echo "missing or unsafe release asset: $1" >&2
    exit 1
  }
}
for asset in controller-bootstrap.sh managed-node-bootstrap.sh controller-release-amd64.json controller-release-arm64.json controller-release.json; do
  require_asset "$asset"
done
cmp "$ASSET_DIR/controller-bootstrap.sh" "$ROOT/deploy/production/controller-bootstrap.sh"
cmp "$ASSET_DIR/managed-node-bootstrap.sh" "$ROOT/deploy/managed-node/install.sh"
for arch in amd64 arm64; do
  case "$arch" in amd64) rpm_arch=x86_64; elf_word=x86-64 ;; arm64) rpm_arch=aarch64; elf_word=aarch64 ;; esac
  archive="ocservia-agent-$VERSION-linux-$arch.tar.gz"
  deb="ocservia-agent_${VERSION}-1_${arch}.deb"
  rpm="ocservia-agent-$VERSION-1.$rpm_arch.rpm"
  for asset in "$archive" "$archive.sha256" "$deb" "$rpm"; do require_asset "$asset"; done
  (cd "$ASSET_DIR" && sha256sum --check "$archive.sha256")
  rootfs="$work/rootfs-$arch"
  sudo install -d -o root -g root -m 0700 "$rootfs" "$rootfs/var/lib"
  expected="$(awk '{print $1}' "$ASSET_DIR/$archive.sha256")"
  package_root="$(sudo env DESTDIR="$rootfs" "$ROOT/scripts/verify-agent-package.sh" "$ASSET_DIR/$archive" "$expected")"
  sudo grep -Fxq "arch=$arch" "$package_root/MANIFEST"
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    description="$(sudo file -b "$package_root/rust/target/release/$binary")"
    [[ "$description" == *"$elf_word"* ]] || { echo "$binary is not native $arch" >&2; exit 1; }
  done
  [[ "$(dpkg-deb -f "$ASSET_DIR/$deb" Architecture)" == "$arch" ]]
  [[ "$(rpm -qp --qf '%{ARCH}' --nosignature "$ASSET_DIR/$rpm" 2>/dev/null)" == "$rpm_arch" ]]
  mkdir -p "$work/deb-$arch" "$work/rpm-$arch"
  dpkg-deb -x "$ASSET_DIR/$deb" "$work/deb-$arch"
  # Some rpm2cpio versions return nonzero after writing a complete payload.
  rpm2cpio "$ASSET_DIR/$rpm" >"$work/payload.cpio" || true
  test -s "$work/payload.cpio"
  (cd "$work/rpm-$arch" && cpio -idm --quiet --no-absolute-filenames <"$work/payload.cpio")
  for family in deb rpm; do
    payload="$work/$family-$arch/usr/share/ocservia-agent"
    cmp "$payload/$archive" "$ASSET_DIR/$archive"
    cmp "$payload/$archive.sha256" "$ASSET_DIR/$archive.sha256"
    cmp "$payload/verify-agent-package.sh" "$ROOT/scripts/verify-agent-package.sh"
  done
  jq -e --arg version "$VERSION" --arg platform "linux/$arch" \
    '.release_version == $version and .release_tag == ("v"+$version) and .platform == $platform and (.images | type == "object")' \
    "$ASSET_DIR/controller-release-$arch.json" >/dev/null
 done
cmp "$ASSET_DIR/controller-release.json" "$ASSET_DIR/controller-release-amd64.json"
echo "Release package set PASS"
