#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
frozen="${1:?frozen.json required}"
destination="${2:?protected destination required}"
mkdir -p "${destination}/bundle" "${destination}/trust"
chmod 700 "${destination}" "${destination}/bundle" "${destination}/trust"
tag="$(jq -er '.baseline_tag' "${frozen}")"
sums_pin="$(jq -er '.baseline.sums_sha256' "${frozen}")"
key_pin="$(jq -er '.baseline.key_der_sha256' "${frozen}")"
while IFS= read -r name; do
  # Only metadata in prepare; packages are fetched by their native unit.
  case "${name}" in *.deb|*.rpm) continue ;; esac
  url="$(jq -er --arg name "${name}" '.assets[] | select(.name == $name) | .url' "${frozen}")"
  [[ "${url}" == "https://github.com/GentleKingson/ocservia/releases/download/${tag}/${name}" ]]
  curl --fail --silent --show-error --location --retry 3 -o "${destination}/bundle/${name}" "${url}"
done < <(jq -r '.assets[].name' "${frozen}")
printf '%s  %s\n' "${sums_pin}" "${destination}/bundle/SHA256SUMS" | sha256sum -c --strict -
key="${destination}/trust/release-signing.pub.pem"
mv "${destination}/bundle/release-signing.pub.pem" "${key}"
observed="$(openssl pkey -pubin -in "${key}" -outform DER | sha256sum | awk '{print $1}')"
[[ "${observed}" == "${key_pin}" ]]
for arch in amd64 arm64; do
  manifest="${destination}/bundle/controller-release-${arch}.json"
  bash "${ROOT}/scripts/verify-controller-release-bundle.sh" "${manifest}" "${key}"
  jq -e --arg arch "linux/${arch}" --arg tag "${tag}" \
    --arg commit "$(jq -r '.baseline_commit' "${frozen}")" \
    --argjson migration "$(jq '.baseline.controller.migration' "${frozen}")" \
    '.platform == $arch and .release_tag == $tag and .source_commit == $commit and .database_migration == $migration' "${manifest}" >/dev/null
  for format in deb rpm; do
    name="$(jq -er --arg arch "${arch}" --arg format "${format}" '
      .assets[].name | select(if $format == "deb" then endswith("_" + $arch + ".deb")
        else endswith("." + (if $arch == "amd64" then "x86_64" else "aarch64" end) + ".rpm") end)' "${frozen}")"
    awk -v name="${name}" '$2 == name && NF == 2 && length($1) == 64 {n++} END {exit n != 1}' "${destination}/bundle/SHA256SUMS"
  done
done
