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
printf '{"spdxVersion":"SPDX-2.3","packages":[]}' >"$destination"
EOF
cat >"$work/bin/grype" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == db ]]; then
  case "$2" in
    update) echo update >>"$TRACE" ;;
    status) printf '{"built":"%s"}' "${DB_BUILT-2026-09-22T00:00:00Z}" ;;
    *) exit 1 ;;
  esac
  exit 0
fi
echo "scan|$GRYPE_DB_AUTO_UPDATE" >>"$TRACE"
while (($#)); do
  case "$1" in --file) destination="$2"; shift 2 ;; *) shift ;; esac
done
printf '{"matches":[%s]}' "$REPORT" >"$destination"
EOF
chmod 755 "$work/bin/"*
export PATH="$work/bin:$PATH" TRACE="$trace"
export IMAGE_ARCHIVES_TSV="$archives" IMAGE_SCAN_EXEMPTIONS="$exemptions"
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
IMAGE_SCAN_EXEMPTIONS= REPORT='' run_scan
SYFT_FAIL=true expect_failure
DB_BUILT='' expect_failure
printf 'bad name\tamd64\t%s\n' "$work/image.tar" >"$archives"
expect_failure
echo 'CI image vulnerability gate tests passed'
