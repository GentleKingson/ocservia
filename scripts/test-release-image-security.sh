#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/bin"
trace="$work/trace"
archives="$work/images.tsv"
exemptions="$work/exemptions.json"
printf 'scanner archive fixture' >"$work/image.tar"
printf 'gateway\tamd64\t%s\ngateway\tarm64\t%s\n' "$work/image.tar" "$work/image.tar" >"$archives"
cat >"$work/bin/syft" <<'EOF'
#!/usr/bin/env bash
[[ "${SYFT_FAIL:-false}" != true ]] || exit 1
while (($#)); do
  case "$1" in --file) destination="$2"; shift 2 ;; *) shift ;; esac
done
printf '{"spdxVersion":"SPDX-2.3","packages":%s}' "$SBOM_PACKAGES" >"$destination"
EOF
cat >"$work/bin/grype" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == db ]]; then
  case "$2" in
    update) echo update >>"$TRACE"; [[ "${DB_UPDATE_FAIL:-false}" != true ]] ;;
    status) printf '{"built":"%s"}' "${DB_BUILT-2026-09-22T00:00:00Z}"; [[ "${DB_STATUS_FAIL:-false}" != true ]] ;;
    *) exit 1 ;;
  esac
  exit 0
fi
echo "scan|$GRYPE_DB_AUTO_UPDATE" >>"$TRACE"
[[ "${GRYPE_FAIL:-false}" != true ]] || exit 1
while (($#)); do
  case "$1" in --file) destination="$2"; shift 2 ;; *) shift ;; esac
done
printf '{"matches":[%s],"descriptor":{"db":{"status":%s}}}' "$REPORT" "$SCAN_DB_STATUS" >"$destination"
EOF
chmod 755 "$work/bin/"*
export PATH="$work/bin:$PATH" TRACE="$trace"
export IMAGE_ARCHIVES_TSV="$archives" IMAGE_SCAN_EXEMPTIONS="$exemptions"
export SBOM_PACKAGES='[]'
export SCAN_DB_STATUS='{"built":"2001-02-03T04:05:06Z","from":"https://example.invalid/first-scan-db"}'
finding() {
  local fixes='[]'
  [[ "$5" != true ]] || fixes='["fixed-version"]'
  jq -nc --arg package "$1" --arg version "$2" --arg id "$3" --arg severity "$4" --argjson fixes "$fixes" \
    '{artifact:{name:$package,version:$version},vulnerability:{id:$id,severity:$severity,fix:{versions:$fixes}}}'
}
run_scan() { bash scripts/scan-release-images.sh >"$work/scan.log" 2>&1; }
expect_failure() { if run_scan; then echo "unexpected image scan PASS" >&2; exit 1; fi; }
printf '{"exemptions":[]}' >"$exemptions"
export REPORT
REPORT="$(finding package 1 CVE-new High true),$(finding other 1 CVE-unfixed Critical false),$(finding medium 1 CVE-medium Medium true)"
expect_failure
[[ "$(grep -c '^update$' "$trace")" == 1 ]]
[[ "$(grep -c '^scan|false$' "$trace")" == 2 ]]
REPORT="$(finding package 1 CVE-unfixed Critical false),$(finding medium 1 CVE-medium Medium true)"
run_scan
# Evidence comes from this scan and its SBOM, not the DB status command or matches.
evidence() { jq -Rsc '[split("\n")[] | fromjson? | select(has("db_built"))]' "$work/scan.log"; }
evidence | jq -e '
  length == 2 and map(.arch) == ["amd64", "arm64"] and all(.[];
    .image == "gateway" and .db_built == "2001-02-03T04:05:06Z" and
    .db_source == "https://example.invalid/first-scan-db" and .pcre2 == [])
' >/dev/null
printf 'edge\tamd64\t%s\nedge\tarm64\t%s\n' "$work/image.tar" "$work/image.tar" >"$archives"
SCAN_DB_STATUS='{"built":"2002-03-04T05:06:07Z","from":"https://example.invalid/second-scan-db"}'
SBOM_PACKAGES='[{"name":"pcre2","versionInfo":"fixture-pcre2-version"},{"name":"unrelated","versionInfo":"other-version"}]'
REPORT='' run_scan
evidence | jq -e '
  length == 2 and map(.arch) == ["amd64", "arm64"] and all(.[];
    .image == "edge" and .db_built == "2002-03-04T05:06:07Z" and
    .db_source == "https://example.invalid/second-scan-db" and .pcre2 == ["fixture-pcre2-version"])
' >/dev/null
# Missing log metadata is not fabricated and does not change the vulnerability gate.
SCAN_DB_STATUS='{}' REPORT='' run_scan
evidence | jq -e 'length == 2 and all(.[]; .db_built == null and .db_source == null)' >/dev/null
SBOM_PACKAGES='[]'
# Reviewed exceptions never transfer to another CVE, version, image or base.
printf 'backup\tamd64\t%s\n' "$work/image.tar" >"$archives"
base="$(awk '/^FROM / && $2 ~ /@sha256:/ {base=$2} END {print base}' deploy/production/backup.Dockerfile)"
write_exemption() {
  jq -n --arg base "$1" --arg review "$2" \
    '{exemptions:[{image:"backup",package:"package",installed_version:"1",vulnerability_id:"CVE-known",base_image:$base,review_by:$review,reason:"test fixture"}]}' >"$exemptions"
}
write_exemption "$base" 2999-01-01
REPORT="$(finding package 1 CVE-known High true)"
run_scan
REPORT="$(finding package 1 CVE-known High true),$(finding package 1 CVE-new High true)"
expect_failure
REPORT="$(finding package 2 CVE-known High true)"
expect_failure
REPORT="$(finding package 1 CVE-known High true)"
printf 'gateway\tamd64\t%s\n' "$work/image.tar" >"$archives"
expect_failure
printf 'backup\tamd64\t%s\n' "$work/image.tar" >"$archives"
write_exemption "$base" 2000-01-01
expect_failure
write_exemption wrong-base 2999-01-01
expect_failure
write_exemption "$base" 2999-01-01
jq 'del(.exemptions[0].installed_version)' "$exemptions" >"$work/invalid.json"
cp "$work/invalid.json" "$exemptions"
expect_failure
# Source-controlled exception policy and single-native-architecture scans work.
IMAGE_SCAN_EXEMPTIONS='' REPORT='' run_scan
printf '{"exemptions":[]}' >"$exemptions"
REPORT=''
SYFT_FAIL=true expect_failure
GRYPE_FAIL=true expect_failure
DB_UPDATE_FAIL=true expect_failure
DB_STATUS_FAIL=true expect_failure
DB_BUILT='' expect_failure
DB_BUILT='invalid"json' expect_failure
# Every published role is scanned on both native architectures; one DB update.
: >"$archives"
: >"$trace"
for arch in amd64 arm64; do
  for name in gateway control transport backup edge relay signer mysql_backup; do
    printf '%s\t%s\t%s\n' "$name" "$arch" "$work/image.tar" >>"$archives"
  done
done
IMAGE_SCAN_EXEMPTIONS='' REPORT='' run_scan
[[ "$(grep -c '^update$' "$trace")" == 1 ]]
[[ "$(grep -c '^scan|false$' "$trace")" == 16 ]]
[[ "$(grep -c 'linux/amd64:' "$work/scan.log")" == 8 ]]
[[ "$(grep -c 'linux/arm64:' "$work/scan.log")" == 8 ]]
printf 'gateway\tamd64\t%s\n' "$work/image.tar" >>"$archives"
REPORT='' expect_failure
printf 'gateway\tamd64\t%s\n' "$work/missing.tar" >"$archives"
expect_failure
printf 'gateway\tunknown\t%s\n' "$work/image.tar" >"$archives"
expect_failure
printf 'bad name\tamd64\t%s\n' "$work/image.tar" >"$archives"
expect_failure
echo 'CI image vulnerability gate tests passed'
