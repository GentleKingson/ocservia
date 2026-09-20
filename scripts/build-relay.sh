#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if (($# < 1 || $# > 2)) || [[ "${2:-release}" != release && "${2:-release}" != debug ]]; then
  echo "usage: $0 <install-root> [release|debug]" >&2
  exit 2
fi
version="$(awk -F= '$1 == "iroh" {print $2}' "$ROOT/toolchains.lock")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'invalid Relay version' >&2; exit 1; }
archive="iroh-relay-$version.crate"
expected="$(awk -v name="$archive" '$2 == name {print $1}' "$ROOT/scripts/checksums.txt")"
[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || { echo "missing or ambiguous checksum: $archive" >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  "https://static.crates.io/crates/iroh-relay/$archive" -o "$work/$archive"
if command -v sha256sum >/dev/null; then
  actual="$(sha256sum "$work/$archive")"
else
  actual="$(shasum -a 256 "$work/$archive")"
fi
[[ "${actual%% *}" == "$expected" ]] || { echo "checksum mismatch: $archive" >&2; exit 1; }
tar -xf "$work/$archive" -C "$work"
# cargo install --version uses the registry lock, not the workspace security fixes.
cp "$ROOT/deploy/production/relay.Cargo.lock" "$work/iroh-relay-$version/Cargo.lock"
profile=()
if [[ "${2:-release}" == debug ]]; then profile=(--debug); fi
cargo install --locked --path "$work/iroh-relay-$version" --features server \
  --root "$1" "${profile[@]}"
