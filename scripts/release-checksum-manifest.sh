#!/usr/bin/env bash
set -euo pipefail

release_checksum_manifest() {
  if (($# < 3)); then
    echo "usage: release-checksum-manifest.sh <asset-dir> <require-controller> <release-file>..." >&2
    return 2
  fi

  local asset_dir="$1" require_controller="$2"
  local controller_checksum expected_checksum actual_checksum
  local controller_present=false file manifest_name
  local -a controller_manifests=(
    "controller-release.json"
    "controller-release-amd64.json"
    "controller-release-arm64.json"
  )
  local -a release_files ordered=()
  shift 2
  release_files=("$@")

  for manifest_name in "${controller_manifests[@]}"; do
    controller_checksum="${manifest_name}.sha256"
    if [[ -e "${asset_dir}/${manifest_name}" || -L "${asset_dir}/${manifest_name}" ||
      -e "${asset_dir}/${controller_checksum}" || -L "${asset_dir}/${controller_checksum}" ]]; then
      controller_present=true
    fi
  done
  if [[ "${controller_present}" != true ]]; then
    if [[ "${require_controller}" == "1" ]]; then
      echo "controller release manifest is required" >&2
      return 1
    fi
  else
    # A release bundle carries the platform manifests as one unit: a partial
    # set can never be signed or published.
    for manifest_name in "${controller_manifests[@]}"; do
      controller_checksum="${manifest_name}.sha256"
      if [[ ! -f "${asset_dir}/${manifest_name}" || -L "${asset_dir}/${manifest_name}" ||
        ! -s "${asset_dir}/${manifest_name}" || ! -f "${asset_dir}/${controller_checksum}" ||
        -L "${asset_dir}/${controller_checksum}" || ! -s "${asset_dir}/${controller_checksum}" ]]; then
        echo "controller release manifest or checksum is missing, empty, or a symlink: ${manifest_name}" >&2
        return 1
      fi
      expected_checksum="$(cd -- "${asset_dir}" && sha256sum -- "${manifest_name}")"
      actual_checksum="$(cat -- "${asset_dir}/${controller_checksum}")"
      if [[ "${actual_checksum}" != "${expected_checksum}" ]]; then
        echo "controller release manifest checksum mismatch: ${manifest_name}" >&2
        return 1
      fi
      release_files+=("${manifest_name}")
    done
  fi
  while IFS= read -r file; do
    ordered+=("${file}")
  done < <(printf '%s\n' "${release_files[@]}" | LC_ALL=C sort)
  (cd -- "${asset_dir}" && sha256sum -- "${ordered[@]}")
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  release_checksum_manifest "$@"
fi
