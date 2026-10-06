#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if (($# < 1 || $# > 2)); then
  echo "usage: prepare-bootstrap-release-assets.sh <asset-dir> [<release-tag>]" >&2
  exit 2
fi

asset_dir="$1"
release_tag="${2-}"
if [[ $# -eq 2 && ! "${release_tag}" =~ ^v[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?$ ]]; then
  echo "release tag must be vX.Y.Z or vX.Y.Z-rc.N: ${release_tag}" >&2
  exit 2
fi
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

# The released Controller Stage-0 differs from its Git source only in the
# stamped default version used by --quick.
if [[ -n "${release_tag}" ]]; then
  copy_asset "${ROOT}/deploy/bootstrap/install-controller" "install-controller"
  default_line='readonly DEFAULT_VERSION=""'
  [[ "$(grep -cxF "${default_line}" "${asset_dir}/install-controller")" == 1 ]] || {
    echo "Controller Stage-0 must contain exactly one empty default version line" >&2
    exit 1
  }
  sed -i "s|^${default_line}\$|readonly DEFAULT_VERSION=\"${release_tag}\"|" "${asset_dir}/install-controller"
  "${ROOT}/scripts/check-release-stage0.sh" "${asset_dir}/install-controller" "${release_tag}"
fi
