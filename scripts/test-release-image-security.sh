#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

stub_dir="$(mktemp -d)"
trace="$(mktemp)"
trap 'rm -rf "${stub_dir}" "${trace}"' EXIT

# A minimal docker-archive fixture: the scanner derives the config digest from
# the archive's manifest.json, so the stub archives carry one real digest each.
config_hex="$(printf 'c%.0s' {1..64})"
config_digest="sha256:${config_hex}"

make_archive() {
  local archive="$1" with_manifest="${2:-true}"
  local staging="${stub_dir}/staging"
  rm -rf "${staging}"
  mkdir -p "${staging}/blobs/sha256"
  printf '{"os":"linux"}' >"${staging}/blobs/sha256/${config_hex}"
  if [[ "${with_manifest}" == "true" ]]; then
    printf '[{"Config":"blobs/sha256/%s","RepoTags":["stub"],"Layers":[]}]\n' "${config_hex}" >"${staging}/manifest.json"
    tar -cf "${archive}" -C "${staging}" manifest.json "blobs/sha256/${config_hex}"
  else
    tar -cf "${archive}" -C "${staging}" "blobs/sha256/${config_hex}"
  fi
  rm -rf "${staging}"
}

write_syft_stub() {
  cat >"${stub_dir}/syft" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    --version) echo "syft 1.0.0-stub"; exit 0 ;;
    --file)
      destination="$2"
      shift 2
      ;;
    *) shift ;;
  esac
done
printf '{"spdxVersion":"SPDX-2.3","packages":[]}\n' >"${destination}"
STUB
  chmod 0755 "${stub_dir}/syft"
}

write_grype_stub() {
  local matches_json="$1" db_built="${2:-2026-09-22T00:00:00Z}"
  cat >"${stub_dir}/grype" <<STUB
#!/usr/bin/env bash
trace="\${CI_STUB_TRACE:?}"
while ((\$# > 0)); do
  case "\$1" in
    --version) echo "grype 0.0.0-stub"; exit 0 ;;
    db)
      if [[ "\${2:-}" == "update" ]]; then
        echo "grype-db-update" >>"\${trace}"
        exit 0
      fi
      if [[ "\${2:-}" == "status" ]]; then
        echo "grype-db-status" >>"\${trace}"
        printf '{"built": "%s", "schemaVersion": "v6.0.0-stub", "from": "https://grype.anchore.io/databases/v6/stub", "valid": true}\n' "${db_built}"
        exit 0
      fi
      exit 1 ;;
    --file)
      destination="\$2"
      shift 2
      ;;
    *) shift ;;
  esac
done
cat >"\${destination}" <<'REPORT'
{"matches": [${matches_json}]}
REPORT
echo "scan|GRYPE_DB_AUTO_UPDATE=\${GRYPE_DB_AUTO_UPDATE:-unset}" >>"\${trace}"
STUB
  chmod 0755 "${stub_dir}/grype"
}

finding_json() {
  local package="$1" version="$2" id="$3" severity="$4" fixable="$5"
  local fix_versions="[]"
  [[ "${fixable}" == "true" ]] && fix_versions='["1.2.3-r0"]'
  printf '{"artifact": {"type": "dpkg", "name": "%s", "version": "%s"}, "vulnerability": {"id": "%s", "severity": "%s", "fix": {"versions": %s}}}' \
    "${package}" "${version}" "${id}" "${severity}" "${fix_versions}"
}

write_syft_stub
export PATH="${stub_dir}:${PATH}"

work_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

archives_tsv="${work_dir}/image-archives.tsv"
exemptions="${work_dir}/exemptions.json"
output_dir="${work_dir}/out"
summary="${output_dir}/controller-image-security.json"
high_fixable_report="$(finding_json libssl3 3.5.7-r0 CVE-2026-1111 High true),$(finding_json unfixable 1.0 CVE-2026-2222 Critical false),$(finding_json medium 1.0 CVE-2026-3333 Medium true)"

run_scan() {
  mkdir -p "${output_dir}"
  rm -f "${summary}"
  IMAGE_SCAN_EXEMPTIONS="${exemptions}" IMAGE_ARCHIVES_TSV="${archives_tsv}" \
    RELEASE_TAG="v9.9.9-stub" SOURCE_COMMIT="1111111111111111111111111111111111111111" \
    CI_STUB_TRACE="${trace}" \
    bash scripts/scan-release-images.sh "${output_dir}"
}

write_fixtures() {
  make_archive "${work_dir}/gateway-linux-amd64.tar"
  make_archive "${work_dir}/gateway-linux-arm64.tar"
  printf 'gateway\tamd64\t%s\ngateway\tarm64\t%s\n' \
    "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
  cat >"${exemptions}" <<'JSON'
{"exemptions": [{"image": "backup", "package": "libpcre2-8-0", "installed_version": "10.42-1+deb12u0", "vulnerability_id": "CVE-2026-4444", "base_image": "postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f", "reason": "stub", "review_by": "2999-01-01"}]}
JSON
}

# A fixable High must fail the gate and block the publish even though the
# summary is still written as evidence, and the run must rest on one frozen
# DB snapshot recorded in the summary.
write_fixtures
write_grype_stub "${high_fixable_report}"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "stub report with a fixable High finding must fail the scan gate" >&2
  exit 1
fi
[[ -s "${summary}" ]] || { echo "failed gate must still record the summary" >&2; exit 1; }
jq -e --arg digest "${config_digest}" '
  .release_tag == "v9.9.9-stub" and
  .tools.syft == "1.0.0-stub" and .tools.grype == "0.0.0-stub" and
  .tools.grype_db.built == "2026-09-22T00:00:00Z" and
  .tools.grype_db.schema == "v6.0.0-stub" and
  .tools.grype_db.from == "https://grype.anchore.io/databases/v6/stub" and
  .tools.grype_db.auto_update == false and
  .images.gateway.base_image == "caddy:2.11.4-alpine@sha256:de23def33b17fb5d1290b0f6c2add1d70780e52341896c00a4c8a2a2fe9d355e" and
  .images.gateway.platforms["linux-amd64"].config_digest == $digest and
  .images.gateway.platforms["linux-amd64"].archive == "gateway-linux-amd64.tar" and
  .images.gateway.platforms["linux-amd64"].gate == "fail" and
  .images.gateway.platforms["linux-amd64"].os_findings.fixable_high_critical == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.total == 3 and
  .images.gateway.platforms["linux-amd64"].os_findings.high == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.critical == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.medium == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.exempted == 0 and
  (.images.gateway.platforms["linux-amd64"].sbom_sha256 | startswith("sha256:")) and
  (.images.gateway.platforms["linux-amd64"].scan_sha256 | startswith("sha256:"))
' "${summary}" >/dev/null || { echo "summary must bind the scanned config digest, frozen DB, and finding counts" >&2; exit 1; }
[[ "$(jq -r '.images.gateway.platforms["linux-arm64"].gate' "${summary}")" == "fail" ]] || {
  echo "both platforms must be scanned" >&2
  exit 1
}
grep -Fxq "grype-db-update" "${trace}" || { echo "the grype DB must be updated explicitly before scanning" >&2; exit 1; }
grep -Fxq "grype-db-status" "${trace}" || { echo "the frozen DB build must be recorded via db status" >&2; exit 1; }
grep -Fxq "scan|GRYPE_DB_AUTO_UPDATE=false" "${trace}" || {
  echo "scans must run with the DB auto-update disabled" >&2
  exit 1
}

# A finding is only exempted when the entry binds the exact package version and
# vulnerability id: the recorded backup exemption must pass, and the same
# package with a brand-new CVE must still fail the gate.
write_fixtures
printf 'backup\tamd64\t%s\nbackup\tarm64\t%s\n' \
  "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
write_grype_stub "$(finding_json libpcre2-8-0 10.42-1+deb12u0 CVE-2026-4444 High true),$(finding_json libpcre2-8-0 10.42-1+deb12u0 CVE-2026-9999 High true)"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "a new CVE on an exempted package must re-fail the gate" >&2
  exit 1
fi
jq -e '
  .images.backup.platforms["linux-amd64"].os_findings.fixable_high_critical == 1 and
  .images.backup.platforms["linux-amd64"].os_findings.exempted == 1 and
  .images.backup.platforms["linux-amd64"].os_findings.exempted_details[0].id == "CVE-2026-4444"
' "${summary}" >/dev/null || { echo "only the bound vulnerability may count as exempted" >&2; exit 1; }

# Without the new CVE the exemption carries the gate to a pass, with the
# finding still reported.
rm -rf "${output_dir}"
write_grype_stub "$(finding_json libpcre2-8-0 10.42-1+deb12u0 CVE-2026-4444 High true)"
run_scan >/dev/null 2>&1
jq -e '
  .images.backup.platforms["linux-amd64"].gate == "pass" and
  .images.backup.platforms["linux-amd64"].os_findings.fixable_high_critical == 0 and
  .images.backup.platforms["linux-amd64"].os_findings.exempted == 1
' "${summary}" >/dev/null || { echo "the bound exemption must honor only the recorded finding" >&2; exit 1; }

# The same finding on a different image keeps failing: exemptions never
# transfer across images.
make_archive "${work_dir}/gateway-linux-amd64.tar"
make_archive "${work_dir}/gateway-linux-arm64.tar"
printf 'gateway\tamd64\t%s\ngateway\tarm64\t%s\n' \
  "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
write_grype_stub "$(finding_json libpcre2-8-0 10.42-1+deb12u0 CVE-2026-4444 High true)"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "exemptions must not transfer across images" >&2
  exit 1
fi

# An exemption whose review date has passed stops covering its finding.
cat >"${exemptions}" <<'JSON'
{"exemptions": [{"image": "backup", "package": "libpcre2-8-0", "installed_version": "10.42-1+deb12u0", "vulnerability_id": "CVE-2026-4444", "base_image": "postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f", "reason": "stub", "review_by": "2000-01-01"}]}
JSON
printf 'backup\tamd64\t%s\nbackup\tarm64\t%s\n' \
  "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "an expired exemption must re-fail the gate" >&2
  exit 1
fi
jq -e '.images.backup.platforms["linux-amd64"].os_findings.exempted == 0' "${summary}" >/dev/null || {
  echo "an expired exemption must not count as exempted" >&2
  exit 1
}

# An exemption recorded against a different base image stops covering its
# finding: the entry binds the image's actual digest-pinned base, so a base
# refresh invalidates stale exemptions.
cat >"${exemptions}" <<'JSON'
{"exemptions": [{"image": "backup", "package": "libpcre2-8-0", "installed_version": "10.42-1+deb12u0", "vulnerability_id": "CVE-2026-4444", "base_image": "postgres:17.10-bookworm@sha256:1111111111111111111111111111111111111111111111111111111111111111", "reason": "stub", "review_by": "2999-01-01"}]}
JSON
rm -rf "${output_dir}"
write_grype_stub "$(finding_json libpcre2-8-0 10.42-1+deb12u0 CVE-2026-4444 High true)"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "an exemption recorded against a different base image must re-fail the gate" >&2
  exit 1
fi
jq -e '.images.backup.platforms["linux-amd64"].os_findings.exempted == 0' "${summary}" >/dev/null || {
  echo "an exemption for a different base image must not count as exempted" >&2
  exit 1
}

# An incomplete exemption record must fail closed.
write_fixtures
cat >"${exemptions}" <<'JSON'
{"exemptions": [{"image": "backup", "package": "libpcre2-8-0", "vulnerability_id": "CVE-2026-4444", "base_image": "postgres:17.10-bookworm", "reason": "stub", "review_by": "2999-01-01"}]}
JSON
rm -rf "${output_dir}"
write_grype_stub "$(finding_json medium 1.0 CVE-2026-3333 Medium true)"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "an exemption without installed_version must fail closed" >&2
  exit 1
fi

# The checked-in release exemptions file must satisfy the binding contract.
make_archive "${work_dir}/gateway-linux-amd64.tar"
make_archive "${work_dir}/gateway-linux-arm64.tar"
printf 'gateway\tamd64\t%s\ngateway\tarm64\t%s\n' \
  "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
write_grype_stub "$(finding_json medium 1.0 CVE-2026-3333 Medium true)"
IMAGE_SCAN_EXEMPTIONS="" IMAGE_ARCHIVES_TSV="${archives_tsv}" \
  RELEASE_TAG="v9.9.9-stub" SOURCE_COMMIT="1111111111111111111111111111111111111111" \
  CI_STUB_TRACE="${trace}" \
  bash scripts/scan-release-images.sh "${output_dir}" >/dev/null 2>&1
jq -e '.images.gateway.platforms["linux-amd64"].gate == "pass"' "${summary}" >/dev/null || {
  echo "the checked-in exemptions file must satisfy the binding contract" >&2
  exit 1
}

# An archive without a readable manifest.json must fail closed instead of
# guessing what was scanned.
make_archive "${work_dir}/gateway-linux-amd64.tar" false
rm -rf "${output_dir}"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "an archive without a config reference must fail closed" >&2
  exit 1
fi

# An image with only one platform leg must fail closed.
make_archive "${work_dir}/gateway-linux-amd64.tar"
printf 'gateway\tamd64\t%s\n' "${work_dir}/gateway-linux-amd64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "a missing platform leg must fail closed" >&2
  exit 1
fi

# A malformed image row must fail closed.
printf 'bad name\tamd64\t%s\n' "${work_dir}/gateway-linux-amd64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
if run_scan >"${work_dir}/stdout.log" 2>&1; then
  echo "malformed image row must fail closed" >&2
  exit 1
fi

# An unknown grype DB build must fail closed.
make_archive "${work_dir}/gateway-linux-arm64.tar"
printf 'gateway\tamd64\t%s\ngateway\tarm64\t%s\n' \
  "${work_dir}/gateway-linux-amd64.tar" "${work_dir}/gateway-linux-arm64.tar" >"${archives_tsv}"
rm -rf "${output_dir}"
write_grype_stub "" ""
if run_scan >/dev/null 2>&1; then
  echo "unknown grype DB build must fail closed" >&2
  exit 1
fi

echo "release image scan gate contracts passed"
