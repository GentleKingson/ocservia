#!/usr/bin/env bash
# Scans the Controller release candidate images and emits per-image SBOM and
# OS-level vulnerability evidence.
#
# Runs inside the controller-image-security job, before any release image is
# written to the registry: evidence is bound to the Docker image archives the
# build legs produced, and the publish job later proves it loaded and pushed
# exactly those images. Any OS-level High/Critical finding with an available
# fix fails the gate and blocks the publish; unfixable findings stay in the
# report without blocking.
#
# usage: scan-release-images.sh <output-dir>
# env:
#   IMAGE_ARCHIVES_TSV   required; "<name>\t<arch>\t<archive-path>" per built
#                        image leg; every image needs one amd64 and one arm64 row
#   RELEASE_TAG          required; release tag recorded in the summary
#   SOURCE_COMMIT        required; release source commit recorded in the summary
#   IMAGE_SCAN_EXEMPTIONS  optional; overrides the exemptions file location
#                        (the release path always reads the checked-in file)
# Callers must put the bootstrap'd `syft`/`grype` on PATH (see the
# image-security bootstrap profile) — the script resolves them like any other
# command so stub-based contract tests can substitute them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

output_dir="${1:?usage: scan-release-images.sh <output-dir>}"
image_archives_tsv="${IMAGE_ARCHIVES_TSV:?IMAGE_ARCHIVES_TSV is required}"
release_tag="${RELEASE_TAG:?RELEASE_TAG is required}"
source_commit="${SOURCE_COMMIT:?SOURCE_COMMIT is required}"

[[ -f "${image_archives_tsv}" && ! -L "${image_archives_tsv}" && -s "${image_archives_tsv}" ]] || {
  echo "image archives table is missing or empty: ${image_archives_tsv}" >&2
  exit 1
}
mkdir -p "${output_dir}"

syft_version="$(syft --version | awk '{print $2}')"
grype_version="$(grype --version | awk '{print $2}')"

# The whole release is gated on one vulnerability database snapshot: update it
# once, record its build and the exact DB artifact it came from, and keep
# auto-update off so the first and the last scan of the run see the same DB.
# A fresh runner without a working DB fails closed here instead of scanning
# against nothing.
export GRYPE_DB_AUTO_UPDATE=false
grype db update >/dev/null
grype_db_json="$(grype db status -o json)"
grype_db_built="$(jq -er '.built | strings | select(length > 0)' <<<"${grype_db_json}")"
grype_db_schema="$(jq -er '.schemaVersion | strings | select(length > 0)' <<<"${grype_db_json}")"
grype_db_from="$(jq -er '.from | strings | select(length > 0)' <<<"${grype_db_json}")"
export GRYPE_DB_AUTO_UPDATE=false

# Explicit, reviewed escape hatch: a finding is only exempted when an entry
# binds the exact image, package, installed version, and vulnerability id,
# names the base image it came from, carries a reason, and its review date has
# not passed. Everything else keeps the fail-closed behavior.
exemptions_file="${IMAGE_SCAN_EXEMPTIONS:-${ROOT}/deploy/production/image-scan-exemptions.json}"
[[ -f "${exemptions_file}" && ! -L "${exemptions_file}" && -s "${exemptions_file}" ]] || {
  echo "image scan exemptions file is missing or empty: ${exemptions_file}" >&2
  exit 1
}
jq -e '
  (.exemptions | type == "array") and
  ([.exemptions[] |
    ((.image | type == "string" and length > 0) and
     (.package | type == "string" and length > 0) and
     (.installed_version | type == "string" and length > 0) and
     (.vulnerability_id | type == "string" and length > 0) and
     (.base_image | type == "string" and length > 0) and
     (.reason | type == "string" and length > 0) and
     (.review_by | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$"))) ] | all)
' "${exemptions_file}" >/dev/null || {
  echo "every image scan exemption must bind image, package, installed_version, vulnerability_id, base_image, reason, and review_by" >&2
  exit 1
}
exemptions_json="$(jq -c '.exemptions' "${exemptions_file}")"
exemptions_sha256="sha256:$(sha256sum "${exemptions_file}" | awk '{print $1}')"

summary="${output_dir}/controller-image-security.json"

# The evidence must bind to the exact image bytes being gated on, so the config
# digest is read back out of each archive instead of being trusted from the
# build metadata. The publish job compares it against the images it loads.
archive_config_digest() {
  local archive="$1" config_name
  config_name="$(tar -xOf "${archive}" manifest.json 2>/dev/null | jq -er '.[0].Config' 2>/dev/null)" || {
    echo "cannot read the image config reference from archive ${archive}" >&2
    return 1
  }
  config_name="$(basename "${config_name}")"
  [[ "${config_name}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "unexpected image config reference in archive ${archive}: ${config_name}" >&2
    return 1
  }
  printf 'sha256:%s\n' "${config_name}"
}

# Validate every row and require both platform legs per image before writing
# anything: a malformed or incomplete table must fail closed without leaving a
# half-written summary behind.
declare -A archive_for=()
declare -A image_seen=()
while IFS=$'\t' read -r name arch archive; do
  [[ "${name}" =~ ^[a-z][a-z0-9-]*$ ]] || {
    echo "invalid image name row: ${name}" >&2
    exit 1
  }
  [[ "${arch}" == "amd64" || "${arch}" == "arm64" ]] || {
    echo "invalid image arch row: ${name} ${arch}" >&2
    exit 1
  }
  [[ -n "${archive}" && -f "${archive}" && ! -L "${archive}" && -s "${archive}" ]] || {
    echo "image archive is missing, empty, or a symlink: ${name} linux/${arch}: ${archive}" >&2
    exit 1
  }
  [[ -z "${archive_for[${name}/${arch}]:-}" ]] || {
    echo "duplicate image archive row: ${name}/${arch}" >&2
    exit 1
  }
  archive_for["${name}/${arch}"]="${archive}"
  image_seen["${name}"]=1
done <"${image_archives_tsv}"
((${#image_seen[@]} > 0)) || {
  echo "image archives table must declare at least one image" >&2
  exit 1
}
for name in "${!image_seen[@]}"; do
  [[ -n "${archive_for[${name}/amd64]:-}" && -n "${archive_for[${name}/arm64]:-}" ]] || {
    echo "image ${name} must declare both amd64 and arm64 archives" >&2
    exit 1
  }
done

printf '{\n  "release_tag": "%s",\n  "source_commit": "%s",\n  "generated_at": "%s",\n  "tools": {"syft": "%s", "grype": "%s", "grype_db": {"built": "%s", "schema": "%s", "from": "%s", "auto_update": false}},\n  "exemptions_sha256": "%s",\n  "gate": "os-level high/critical findings with an available fix fail the publish",\n  "images": {' \
  "${release_tag}" "${source_commit}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  "${syft_version}" "${grype_version}" \
  "${grype_db_built}" "${grype_db_schema}" "${grype_db_from}" \
  "${exemptions_sha256}" >"${summary}"

today="$(date -u +%Y-%m-%d)"
failures=()
first_image=true
for name in $(printf '%s\n' "${!image_seen[@]}" | LC_ALL=C sort); do
  ${first_image} || printf ',\n' >>"${summary}"
  first_image=false
  printf '  "%s": {"platforms": {' "${name}" >>"${summary}"

  first_platform=true
  for arch in amd64 arm64; do
    archive="${archive_for[${name}/${arch}]}"
    config_digest="$(archive_config_digest "${archive}")"
    echo "scanning ${name} linux/${arch} at ${config_digest}" >&2
    sbom_file="${output_dir}/controller-image-sbom-${name}-linux-${arch}.spdx.json"
    os_sbom_file="${output_dir}/.scan-${name}-${arch}-os.spdx.json"
    scan_file="${output_dir}/controller-image-os-scan-${name}-linux-${arch}.json"
    syft "docker-archive:${archive}" -o spdx-json --file "${sbom_file}" -q
    # OS-level catalogers only: the release gate scopes to base-image
    # (OS package) vulnerabilities; the full SBOM above stays published for
    # independent first-party inventory.
    syft "docker-archive:${archive}" \
      --override-default-catalogers 'apk-db-cataloger,dpkg-db-cataloger,rpm-db-cataloger' \
      -o spdx-json --file "${os_sbom_file}" -q
    grype "sbom:${os_sbom_file}" -o json --file "${scan_file}" -q

    # Classify every finding first (fixable High/Critical are gate-relevant),
    # then count only the reportable (non-exempted) ones into the summary.
    findings="$(jq -c --arg image "${name}" --arg today "${today}" --argjson _exemptions "${exemptions_json}" '
      [.matches[]
      | .artifact.name as $package | .artifact.version as $version
      | .vulnerability.id as $id | .vulnerability.severity as $severity
      | ((.vulnerability.fix.versions // []) | length > 0) as $fixable
      | {package: $package, version: $version, id: $id, severity: $severity, fixable: $fixable,
         exempted: any($_exemptions[];
           .image == $image and .package == $package and .installed_version == $version
           and .vulnerability_id == $id and .review_by >= $today)}]
      | ([.[] | select(.exempted and .fixable and (.severity == "High" or .severity == "Critical"))]) as $exempted
      | ([.[] | select(.exempted | not)]) as $reportable
      | {
          total: ($reportable | length),
          fixable_high_critical: ([$reportable[] | select(.fixable and (.severity == "High" or .severity == "Critical"))] | length),
          exempted: ($exempted | length),
          exempted_details: [$exempted[] | {package, version, id, severity}]
        }
        + (([$reportable[].severity] | group_by(.))
          | map({(.[0] | ascii_downcase): length}) | add // {})
    ' "${scan_file}")"
    fixable_count="$(jq -r '.fixable_high_critical' <<<"${findings}")"
    exempted_count="$(jq -r '.exempted' <<<"${findings}")"
    gate="pass"
    if ((fixable_count > 0)); then
      gate="fail"
      failures+=("${name} linux/${arch}: ${fixable_count} fixable High/Critical OS findings")
    fi

    ${first_platform} || printf ',\n' >>"${summary}"
    first_platform=false
    printf '"linux-%s": {"archive": "%s", "config_digest": "%s", "sbom": "%s", "sbom_sha256": "sha256:%s", "scan": "%s", "scan_sha256": "sha256:%s", "os_findings": %s, "gate": "%s"}' \
      "${arch}" "$(basename "${archive}")" "${config_digest}" \
      "$(basename "${sbom_file}")" "$(sha256sum "${sbom_file}" | awk '{print $1}')" \
      "$(basename "${scan_file}")" "$(sha256sum "${scan_file}" | awk '{print $1}')" \
      "${findings}" "${gate}" >>"${summary}"
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
