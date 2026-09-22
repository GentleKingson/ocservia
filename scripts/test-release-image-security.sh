#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

stub_dir="$(mktemp -d)"
trap 'rm -rf "${stub_dir}"' EXIT

cat >"${stub_dir}/syft" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    --version) echo "syft 1.0.0-stub"; exit 0 ;;
    --file)
      destination="$2"
      shift 2
      ;;
    -o) shift ;;
    *) shift ;;
  esac
done
printf '{"spdxVersion":"SPDX-2.3","packages":[]}\n' >"${destination}"
STUB

cat >"${stub_dir}/grype" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    --version) echo "grype 0.0.0-stub"; exit 0 ;;
    db) echo "Built: 2026-09-22T00:00:00Z"; exit 0 ;;
    --file)
      destination="$2"
      shift 2
      ;;
    -o) shift ;;
    *) shift ;;
  esac
done
cat >"${destination}" <<'REPORT'
{"matches": [
  {"artifact": {"type": "apk", "name": "libssl3"}, "vulnerability": {"severity": "High", "fix": {"versions": ["3.5.8-r0"]}}},
  {"artifact": {"type": "apk", "name": "unfixable"}, "vulnerability": {"severity": "Critical", "fix": {"versions": []}}},
  {"artifact": {"type": "apk", "name": "medium"}, "vulnerability": {"severity": "Medium", "fix": {"versions": ["1.2.3"]}}}
]}
REPORT
STUB

chmod 0755 "${stub_dir}/syft" "${stub_dir}/grype"

export PATH="${stub_dir}:${PATH}"

image_refs="$(mktemp)"
output_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${image_refs}" "${output_dir}"
}
trap cleanup EXIT

digest="sha256:$(printf 'a%.0s' {1..64})"
printf 'gateway\tghcr.io/gentlekingson/ocservia/gateway@%s\n' "${digest}" >"${image_refs}"

run_scan() {
  IMAGE_REFS_TSV="${image_refs}" RELEASE_TAG="v9.9.9-stub" \
    SOURCE_COMMIT="1111111111111111111111111111111111111111" \
    bash scripts/scan-release-images.sh "${output_dir}"
}

# A fixable High must fail the gate and block the publish even though the
# summary is still written as evidence.
if run_scan >"${output_dir}/stdout.log" 2>&1; then
  echo "stub report with a fixable High finding must fail the scan gate" >&2
  exit 1
fi
summary="${output_dir}/controller-image-security.json"
[[ -s "${summary}" ]] || { echo "failed gate must still record the summary" >&2; exit 1; }
jq -e --arg digest "${digest}" '
  .release_tag == "v9.9.9-stub" and
  .tools.syft == "1.0.0-stub" and .tools.grype == "0.0.0-stub" and
  .tools.grype_db_built == "2026-09-22T00:00:00Z" and
  .images.gateway.index_digest == $digest and
  .images.gateway.platforms["linux-amd64"].gate == "fail" and
  .images.gateway.platforms["linux-amd64"].os_findings.fixable_high_critical == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.total == 3 and
  .images.gateway.platforms["linux-amd64"].os_findings.high == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.critical == 1 and
  .images.gateway.platforms["linux-amd64"].os_findings.medium == 1 and
  (.images.gateway.platforms["linux-amd64"].sbom_sha256 | startswith("sha256:")) and
  (.images.gateway.platforms["linux-amd64"].scan_sha256 | startswith("sha256:"))
' "${summary}" >/dev/null || { echo "summary must bind the published digest and finding counts" >&2; exit 1; }
[[ "$(jq -r '.images.gateway.platforms["linux-arm64"].gate' "${summary}")" == "fail" ]] || {
  echo "both platforms must be scanned" >&2
  exit 1
}
[[ -s "${output_dir}/controller-image-sbom-gateway-linux-amd64.spdx.json" ]] || {
  echo "full SBOM must be emitted per image and platform" >&2
  exit 1
}
[[ -s "${output_dir}/controller-image-os-scan-gateway-linux-amd64.json" ]] || {
  echo "OS-level scan report must be emitted per image and platform" >&2
  exit 1
}

# Removing the fixable finding (keeping the unfixable Critical) must pass: the
# gate blocks on actionable findings only, and unfixable ones stay reported.
cleanup
cat >"${stub_dir}/grype" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    --version) echo "grype 0.0.0-stub"; exit 0 ;;
    db) echo "Built: 2026-09-22T00:00:00Z"; exit 0 ;;
    --file)
      destination="$2"
      shift 2
      ;;
    -o) shift ;;
    *) shift ;;
  esac
done
printf '{"matches": [{"artifact": {"type": "apk", "name": "unfixable"}, "vulnerability": {"severity": "Critical", "fix": {"versions": []}}}]}\n' >"${destination}"
STUB
chmod 0755 "${stub_dir}/grype"
printf 'gateway\tghcr.io/gentlekingson/ocservia/gateway@%s\n' "${digest}" >"${image_refs}"
run_scan >/dev/null 2>&1
jq -e '
  .images.gateway.platforms["linux-amd64"].gate == "pass" and
  .images.gateway.platforms["linux-amd64"].os_findings.fixable_high_critical == 0 and
  .images.gateway.platforms["linux-amd64"].os_findings.critical == 1
' "${summary}" >/dev/null || { echo "unfixable findings must not fail the gate but must stay reported" >&2; exit 1; }

# A finding matching a recorded exemption must not fail the gate but must stay
# in the report and count as exempted.
cat >"${stub_dir}/grype" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    --version) echo "grype 0.0.0-stub"; exit 0 ;;
    db) echo "Built: 2026-09-22T00:00:00Z"; exit 0 ;;
    --file)
      destination="$2"
      shift 2
      ;;
    -o) shift ;;
    *) shift ;;
  esac
done
printf '{"matches": [{"artifact": {"type": "apk", "name": "libpcre2-8-0"}, "vulnerability": {"severity": "High", "fix": {"versions": ["10.42-1+deb12u1"]}}}]}\n' >"${destination}"
STUB
chmod 0755 "${stub_dir}/grype"
printf 'backup\tghcr.io/gentlekingson/ocservia/backup@%s\n' "${digest}" >"${image_refs}"
run_scan >/dev/null 2>&1
jq -e '
  .images.backup.platforms["linux-amd64"].gate == "pass" and
  .images.backup.platforms["linux-amd64"].os_findings.fixable_high_critical == 0 and
  .images.backup.platforms["linux-amd64"].os_findings.exempted == 1
' "${summary}" >/dev/null || { echo "an exempted finding must pass the gate and stay reported" >&2; exit 1; }
printf 'gateway\tghcr.io/gentlekingson/ocservia/gateway@%s\n' "${digest}" >"${image_refs}"
if run_scan >/dev/null 2>&1; then
  echo "an exempted package on a non-exempt image must still fail the gate" >&2
  exit 1
fi

# A malformed image ref row must fail closed.
printf 'bad name\t%s\n' "${digest}" >"${image_refs}"
if run_scan >/dev/null 2>&1; then
  echo "malformed image ref row must fail closed" >&2
  exit 1
fi

# An unknown grype DB build must fail closed.
printf 'gateway\tghcr.io/gentlekingson/ocservia/gateway@%s\n' "${digest}" >"${image_refs}"
cat >"${stub_dir}/grype" <<'STUB'
#!/usr/bin/env bash
while (($# > 0)); do
  case "$1" in
    db) echo "Built:"; exit 0 ;;
    --version) echo "grype 0.0.0-stub"; exit 0 ;;
  esac
done
exit 1
STUB
chmod 0755 "${stub_dir}/grype"
if run_scan >/dev/null 2>&1; then
  echo "unknown grype DB build must fail closed" >&2
  exit 1
fi

echo "release image scan gate contracts passed"
