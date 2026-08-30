#!/usr/bin/env bash
set -euo pipefail

release_checksum_manifest() {
  if (($# < 3)); then
    echo "usage: release-checksum-manifest.sh <asset-dir> <require-controller> <release-file>..." >&2
    return 2
  fi

  local asset_dir="$1" require_controller="$2" controller_manifest="controller-release.json"
  local controller_checksum="${controller_manifest}.sha256" expected_checksum actual_checksum
  local controller_present=false file
  local -a release_files ordered=()
  shift 2
  release_files=("$@")

  if [[ -e "${asset_dir}/${controller_manifest}" || -L "${asset_dir}/${controller_manifest}" ||
    -e "${asset_dir}/${controller_checksum}" || -L "${asset_dir}/${controller_checksum}" ]]; then
    if [[ ! -f "${asset_dir}/${controller_manifest}" || -L "${asset_dir}/${controller_manifest}" ||
      ! -s "${asset_dir}/${controller_manifest}" || ! -f "${asset_dir}/${controller_checksum}" ||
      -L "${asset_dir}/${controller_checksum}" || ! -s "${asset_dir}/${controller_checksum}" ]]; then
      echo "controller release manifest or checksum is missing, empty, or a symlink" >&2
      return 1
    fi
    expected_checksum="$(cd -- "${asset_dir}" && sha256sum -- "${controller_manifest}")"
    actual_checksum="$(cat -- "${asset_dir}/${controller_checksum}")"
    if [[ "${actual_checksum}" != "${expected_checksum}" ]]; then
      echo "controller release manifest checksum mismatch" >&2
      return 1
    fi
    controller_present=true
  elif [[ "${require_controller}" == "1" ]]; then
    echo "controller release manifest is required" >&2
    return 1
  fi

  if [[ "${controller_present}" == true ]]; then
    release_files+=("${controller_manifest}")
  fi
  while IFS= read -r file; do
    ordered+=("${file}")
  done < <(printf '%s\n' "${release_files[@]}" | LC_ALL=C sort)
  (cd -- "${asset_dir}" && sha256sum -- "${ordered[@]}")
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  release_checksum_manifest "$@"
fi
