#!/usr/bin/env bash
# Scans the published Controller images of one release and emits per-digest
# SBOM and OS-level vulnerability evidence.
#
# Runs inside the release publish job after the multi-platform index digests
# exist: results are recorded against the exact published digest users consume
# (the index digest referenced by controller-release.json). Any OS-level
# High/Critical finding with an available fix fails the gate and blocks the
# publish; unfixable findings stay in the report without blocking.
#
# usage: scan-release-images.sh <output-dir>
# env:
#   IMAGE_REFS_TSV   required; "<name>\t<prefix>/<name>@sha256:..." per image
#   RELEASE_TAG      required; release tag recorded in the summary
#   SOURCE_COMMIT    required; release source commit recorded in the summary
# Callers must put the bootstrap'd `syft`/`grype` on PATH (see the
# image-security bootstrap profile) — the script resolves them like any other
# command so stub-based contract tests can substitute them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

output_dir="${1:?usage: scan-release-images.sh <output-dir>}"
image_refs_tsv="${IMAGE_REFS_TSV:?IMAGE_REFS_TSV is required}"
release_tag="${RELEASE_TAG:?RELEASE_TAG is required}"
source_commit="${SOURCE_COMMIT:?SOURCE_COMMIT is required}"

[[ -f "${image_refs_tsv}" && ! -L "${image_refs_tsv}" && -s "${image_refs_tsv}" ]] || {
  echo "image refs table is missing or empty: ${image_refs_tsv}" >&2
  exit 1
}
mkdir -p "${output_dir}"

syft_version="$(syft --version | awk '{print $2}')"
grype_version="$(grype --version | awk '{print $2}')"
grype_db_built="$(grype db status 2>/dev/null | sed -n 's/^Built: *//p')"
[[ -n "${grype_db_built}" ]] || {
  echo "grype vulnerability database status is unavailable; refusing to scan without a known DB build" >&2
  exit 1
}

# Explicit, reviewed escape hatch: findings listed here are excluded from the
# gate. Everything not exempted keeps the fail-closed behavior.
exemptions_file="${ROOT}/deploy/production/image-scan-exemptions.json"
exempted_for() {
  local image="$1"
  jq -er --arg image "${image}" '
    .exemptions | map(select(.image == $image) | .package)
  ' "${exemptions_file}" 2>/dev/null || printf '[]\n'
}

summary="${output_dir}/controller-image-security.json"

# Validate every row before writing anything: a malformed table must fail
# closed without leaving a half-written summary behind.
names=()
refs=()
while IFS=$'\t' read -r name ref; do
  [[ "${name}" =~ ^[a-z][a-z0-9-]*$ && "${ref}" =~ ^[a-z0-9.-]+[a-z0-9./-]*/ocservia/[a-z][a-z0-9-]*@sha256:[0-9a-f]{64}$ ]] || {
    echo "invalid image ref row: ${name}" >&2
    exit 1
  }
  names+=("${name}")
  refs+=("${ref}")
done <"${image_refs_tsv}"
((${#names[@]} > 0)) || {
  echo "image refs table must declare at least one image" >&2
  exit 1
}

printf '{\n  "release_tag": "%s",\n  "source_commit": "%s",\n  "generated_at": "%s",\n  "tools": {"syft": "%s", "grype": "%s", "grype_db_built": "%s"},\n  "gate": "os-level high/critical findings with an available fix fail the publish",\n  "images": {' \
  "${release_tag}" "${source_commit}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  "${syft_version}" "${grype_version}" "${grype_db_built}" >"${summary}"

first_image=true
failures=()
for row_index in "${!names[@]}"; do
  name="${names[${row_index}]}"
  ref="${refs[${row_index}]}"
  digest="${ref##*@}"
  ${first_image} || printf ',\n' >>"${summary}"
  first_image=false
  printf '  "%s": {"index_digest": "%s", "platforms": {' "${name}" "${digest}" >>"${summary}"

  first_platform=true
  for arch in amd64 arm64; do
    echo "scanning ${name} linux/${arch} at ${digest}" >&2
    sbom_file="${output_dir}/controller-image-sbom-${name}-linux-${arch}.spdx.json"
    os_sbom_file="${output_dir}/.scan-${name}-${arch}-os.spdx.json"
    scan_file="${output_dir}/controller-image-os-scan-${name}-linux-${arch}.json"
    syft "registry:${ref}" --platform "linux/${arch}" -o spdx-json --file "${sbom_file}" -q
    # OS-level catalogers only: the release gate scopes to base-image
    # (OS package) vulnerabilities; the full SBOM above stays published for
    # independent first-party inventory.
    syft "registry:${ref}" --platform "linux/${arch}" \
      --override-default-catalogers 'apk-db-cataloger,dpkg-db-cataloger,rpm-db-cataloger' \
      -o spdx-json --file "${os_sbom_file}" -q
    grype "sbom:${os_sbom_file}" -o json --file "${scan_file}" -q

    exempt_packages="$(exempted_for "${name}")"
    fixable_count="$(jq --argjson exempt "${exempt_packages}" '
      [.matches[]
      | .artifact.name as $package
      | select(($exempt | index($package)) | not)
      | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
      | select((.vulnerability.fix.versions | length) > 0)] | length' "${scan_file}")"
    exempted_count="$(jq --argjson exempt "${exempt_packages}" '
      [.matches[]
      | .artifact.name as $package
      | select(($exempt | index($package)))
      | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
      | select((.vulnerability.fix.versions | length) > 0)] | length' "${scan_file}")"
    severity_counts="$(jq -c --argjson exempt "${exempt_packages}" '
      [.matches[]
      | .artifact.name as $package
      | select(($exempt | index($package)) | not)] as $reportable
      | {total: ($reportable | length)}
      + ([$reportable[] | .vulnerability.severity] | group_by(.)
        | map({(.[0] | ascii_downcase): length}) | add // {}) | . + {
        fixable_high_critical: '"${fixable_count}"',
        exempted: '"${exempted_count}"'}' "${scan_file}")"
    gate="pass"
    if ((fixable_count > 0)); then
      gate="fail"
      failures+=("${name} linux/${arch}: ${fixable_count} fixable High/Critical OS findings")
    fi

    ${first_platform} || printf ',\n' >>"${summary}"
    first_platform=false
    printf '"linux-%s": {"sbom": "%s", "sbom_sha256": "sha256:%s", "scan": "%s", "scan_sha256": "sha256:%s", "os_findings": %s, "gate": "%s"}' \
      "${arch}" "$(basename "${sbom_file}")" "$(sha256sum "${sbom_file}" | awk '{print $1}')" \
      "$(basename "${scan_file}")" "$(sha256sum "${scan_file}" | awk '{print $1}')" \
      "${severity_counts}" "${gate}" >>"${summary}"
    rm -f "${os_sbom_file}"
  done
  printf '}}' >>"${summary}"
done

printf '}\n}\n' >>"${summary}"

if ((${#failures[@]} > 0)); then
  printf 'release image scan gate failed:\n' >&2
  printf '  - %s\n' "${failures[@]}" >&2
  echo "refresh the affected base images and rebuild before publishing" >&2
  exit 1
fi

jq -e '.images | length >= 1' "${summary}" >/dev/null
echo "controller image security summary: ${summary}"
