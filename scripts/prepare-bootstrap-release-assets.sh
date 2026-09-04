#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if (($# != 1)); then
  echo "usage: prepare-bootstrap-release-assets.sh <asset-dir>" >&2
  exit 2
fi

asset_dir="$1"
[[ -d "${asset_dir}" ]] || {
  echo "bootstrap asset directory does not exist: ${asset_dir}" >&2
  exit 1
}

copy_asset() {
  local source="$1" name="$2"
  [[ -f "${source}" && ! -L "${source}" && -x "${source}" ]] || {
    echo "bootstrap source is missing, not executable, or a symlink: ${source}" >&2
    exit 1
  }
  if LC_ALL=C grep -q $'\r' "${source}"; then
    echo "bootstrap source must use LF line endings: ${source}" >&2
    exit 1
  fi
  install -m 0755 -- "${source}" "${asset_dir}/${name}"
}

copy_asset "${ROOT}/deploy/production/controller-bootstrap.sh" "controller-bootstrap.sh"
copy_asset "${ROOT}/deploy/managed-node/install.sh" "managed-node-bootstrap.sh"
