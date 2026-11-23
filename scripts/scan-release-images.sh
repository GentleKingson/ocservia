#!/usr/bin/env bash
# CI-only OS vulnerability gate; temporary scan data is never a Release asset.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image_archives_tsv="${IMAGE_ARCHIVES_TSV:?IMAGE_ARCHIVES_TSV is required}"
[[ $# == 0 && -f "$image_archives_tsv" && ! -L "$image_archives_tsv" && -s "$image_archives_tsv" ]] || exit 2
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT INT TERM
export GRYPE_DB_AUTO_UPDATE=false
db_updated=false
for attempt in 1 2 3; do
  if grype db update >/dev/null; then
    db_updated=true
    break
  fi
  if ((attempt < 3)); then
    sleep "${IMAGE_SCAN_DB_RETRY_DELAY:-2}"
  fi
done
[[ "${db_updated}" == true ]] || exit 1
# Missing or invalid vulnerability data cannot report PASS.
grype db status -o json | jq -e '(.built | type == "string" and length > 0)' >/dev/null
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

controller_dockerfile_for() {
  case "$1" in
    gateway) printf 'deploy/production/gateway.Dockerfile\n' ;;
    control) printf 'control-plane/Dockerfile\n' ;;
    transport) printf 'rust/transportd.Dockerfile\n' ;;
    backup) printf 'deploy/production/backup.Dockerfile\n' ;;
    edge|relay|signer) printf 'deploy/production/%s.Dockerfile\n' "$1" ;;
    *) return 1 ;;
  esac
}

image_base_reference() {
  local dockerfile="$1"
  awk '
    /^FROM[ \t]/ && $2 ~ /^[^@[:space:]]+@sha256:[0-9a-f]{64}$/ { base = $2 }
    /^FROM[ \t]+scratch([ \t]|$)/ { base = "scratch" }
    END { if (base != "") print base; else exit 1 }
  ' "${ROOT:?}/${dockerfile}"
}

today="$(date -u +%Y-%m-%d)"
declare -A seen=()
row_names=()
row_arches=()
row_archives=()
row_bases=()
while IFS=$'\t' read -r name arch archive; do
  [[ "$name" =~ ^[a-z][a-z0-9_-]*$ && ( "$arch" == amd64 || "$arch" == arm64 ) ]] || exit 2
  [[ -f "$archive" && ! -L "$archive" && -s "$archive" && -z "${seen[$name/$arch]:-}" ]] || exit 2
  seen["$name/$arch"]=1
  dockerfile="$(controller_dockerfile_for "$name")"
  base_ref="$(image_base_reference "$dockerfile")"
  row_names+=("$name")
  row_arches+=("$arch")
  row_archives+=("$archive")
  row_bases+=("$base_ref")
done <"$image_archives_tsv"
rows="${#row_names[@]}"
((rows > 0)) || exit 2

max_jobs="${IMAGE_SCAN_PARALLELISM:-4}"
[[ "$max_jobs" =~ ^[1-9][0-9]*$ ]] || exit 2

scan_one_archive() {
  local idx="$1" name="$2" arch="$3" archive="$4" base_ref="$5"
  local os_sbom_file="$work/row-${idx}.spdx.json"
  local scan_file="$work/row-${idx}.scan.json"
  local out_file="$work/row-${idx}.out"
  local fail_file="$work/row-${idx}.fail"
  local findings fixable_count
  syft "docker-archive:$archive" \
    --override-default-catalogers 'apk-db-cataloger,dpkg-db-cataloger,rpm-db-cataloger' \
    -o spdx-json --file "$os_sbom_file" -q
  grype "sbom:$os_sbom_file" -o json --file "$scan_file" -q
  jq -c --arg image "$name" --arg arch "$arch" --slurpfile sbom "$os_sbom_file" '
    {image:$image, arch:$arch, db_built:.descriptor.db.status.built, db_source:.descriptor.db.status.from,
     pcre2:[$sbom[0].packages[] | select(.name == "pcre2") | .versionInfo]}
  ' "$scan_file" >"$out_file"
  findings="$(jq -c --arg image "${name}" --arg base "${base_ref}" --arg today "${today}" --argjson _exemptions "${exemptions_json}" '
    [.matches[]
    | .artifact.name as $package | .artifact.version as $version
    | .vulnerability.id as $id | .vulnerability.severity as $severity
    | ((.vulnerability.fix.versions // []) | length > 0) as $fixable
    | {package: $package, version: $version, id: $id, severity: $severity, fixable: $fixable,
       exempted: any($_exemptions[];
         .image == $image and .package == $package and .installed_version == $version
         and .vulnerability_id == $id and .base_image == $base and .review_by >= $today)}]
    | ([.[] | select(.exempted and .fixable and (.severity == "High" or .severity == "Critical"))]) as $exempted
    | ([.[] | select(.exempted | not)]) as $reportable
    | {
        total: ($reportable | length),
        fixable_high_critical: ([$reportable[] | select(.fixable and (.severity == "High" or .severity == "Critical"))] | length),
        fixable_details: [$reportable[] | select(.fixable and (.severity == "High" or .severity == "Critical")) | {package, version, id, severity}],
        exempted: ($exempted | length),
        exempted_details: [$exempted[] | {package, version, id, severity}]
      }
      + (([$reportable[].severity] | group_by(.))
        | map({(.[0] | ascii_downcase): length}) | add // {})
  ' "${scan_file}")"
  fixable_count="$(jq -r '.fixable_high_critical' <<<"$findings")"
  echo "$name linux/$arch: $findings" >>"$out_file"
  if ((fixable_count > 0)); then
    printf '%s\n' "$name linux/$arch: $fixable_count fixable High/Critical OS findings" >"$fail_file"
  fi
}

pids=()
scan_errors=0
for ((idx = 0; idx < rows; idx++)); do
  scan_one_archive "$idx" "${row_names[idx]}" "${row_arches[idx]}" "${row_archives[idx]}" "${row_bases[idx]}" &
  pids+=("$!")
  if ((idx + 1 >= max_jobs)); then
    if ! wait "${pids[idx + 1 - max_jobs]}"; then
      scan_errors=1
    fi
  fi
done
start_wait=$((rows >= max_jobs ? rows - max_jobs + 1 : 0))
for ((idx = start_wait; idx < rows; idx++)); do
  if ! wait "${pids[idx]}"; then
    scan_errors=1
  fi
done

failures=()
for ((idx = 0; idx < rows; idx++)); do
  if [[ -f "$work/row-${idx}.out" ]]; then
    cat "$work/row-${idx}.out"
  fi
  if [[ -f "$work/row-${idx}.fail" ]]; then
    failures+=("$(cat "$work/row-${idx}.fail")")
  fi
done
if ((scan_errors != 0)); then
  echo 'Controller image scan ERROR' >&2
  exit 1
fi
if ((${#failures[@]} > 0)); then
  printf '%s\n' "${failures[@]}" >&2
  echo 'Controller image scan FAIL' >&2
  exit 1
fi
echo 'Controller image scan PASS'
