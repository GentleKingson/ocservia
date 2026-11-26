#!/usr/bin/env bash
# CI-only OS vulnerability gate; temporary scan data is never a Release asset.
set -euo pipefail
mode=scan
if (($# > 1)); then
  exit 2
elif (($# == 1)); then
  case "$1" in
    --update-db-only) mode=update-db-only ;;
    --use-prefetched-db) mode=use-prefetched-db ;;
    *) exit 2 ;;
  esac
fi

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT INT TERM
export GRYPE_DB_AUTO_UPDATE=false

update_and_verify_db() {
  local db_started="${SECONDS}" db_updated=false attempt
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
  printf 'controller-scan-timing stage=database seconds=%s\n' "$((SECONDS - db_started))"
}

if [[ "${mode}" == update-db-only ]]; then
  update_and_verify_db
  exit 0
fi

image_archives_tsv="${IMAGE_ARCHIVES_TSV:?IMAGE_ARCHIVES_TSV is required}"
[[ -f "$image_archives_tsv" && ! -L "$image_archives_tsv" && -s "$image_archives_tsv" ]] || exit 2
if [[ "${mode}" == use-prefetched-db ]]; then
  [[ -n "${GRYPE_DB_CACHE_DIR:-}" && -d "${GRYPE_DB_CACHE_DIR}" && ! -L "${GRYPE_DB_CACHE_DIR}" ]] || exit 2
  grype db status -o json | jq -e '(.built | type == "string" and length > 0)' >/dev/null
else
  update_and_verify_db
fi

declare -A seen=()
row_names=()
row_arches=()
row_archives=()
while IFS=$'\t' read -r name arch archive; do
  [[ "$name" =~ ^[a-z][a-z0-9_-]*$ && ( "$arch" == amd64 || "$arch" == arm64 ) ]] || exit 2
  [[ -f "$archive" && ! -L "$archive" && -s "$archive" && -z "${seen[$name/$arch]:-}" ]] || exit 2
  seen["$name/$arch"]=1
  row_names+=("$name")
  row_arches+=("$arch")
  row_archives+=("$archive")
done <"$image_archives_tsv"
rows="${#row_names[@]}"
((rows > 0)) || exit 2

max_jobs="${IMAGE_SCAN_PARALLELISM:-4}"
[[ "$max_jobs" =~ ^[1-9][0-9]*$ ]] || exit 2

summarize_findings() {
  jq -cs '
    [.[].matches[]
      | {package: .artifact.name, version: .artifact.version,
         id: .vulnerability.id, severity: .vulnerability.severity,
         fixable: ((.vulnerability.fix.versions // []) | length > 0)}]
    | {
        total: length,
        fixable_high_critical: ([.[] | select(.fixable and (.severity == "High" or .severity == "Critical"))] | length),
        fixable_details: [.[] | select(.fixable and (.severity == "High" or .severity == "Critical")) | {package, version, id, severity}]
      }
      + (([.[].severity] | group_by(.))
        | map({(.[0] | ascii_downcase): length}) | add // {})
  ' "$@"
}

scan_one_archive() {
  local idx="$1" name="$2" arch="$3" archive="$4"
  local primary_sbom_file="$work/row-${idx}.spdx.json"
  local scan_file="$work/row-${idx}.scan.json"
  local out_file="$work/row-${idx}.out"
  local fail_file="$work/row-${idx}.fail"
  local catalogers='apk-db-cataloger,dpkg-db-cataloger,rpm-db-cataloger'
  local findings fixable_count started="${SECONDS}"
  local -a scan_files=("$scan_file")
  if [[ "$name" == otel ]]; then
    catalogers='go-module-binary-cataloger'
  fi
  syft "docker-archive:$archive" \
    --override-default-catalogers "$catalogers" \
    -o spdx-json --file "$primary_sbom_file" -q
  grype "sbom:$primary_sbom_file" -o json --file "$scan_file" -q
  jq -c --arg image "$name" --arg arch "$arch" '
    {image: $image, arch: $arch, db_built: .descriptor.db.status.built, db_source: .descriptor.db.status.from}
  ' "$scan_file" >"$out_file"
  # PGDG postgresql-18 packages are not tracked by Debian main security feeds;
  # check the upstream PostgreSQL CPE explicitly when postgresql-18 is installed.
  if [[ "$name" == postgres || "$name" == backup ]]; then
    local pg_raw_version pg_upstream_version pg_cpe_sbom pg_scan_file
    pg_raw_version="$(jq -r '[.packages[] | select(.name == "postgresql-18") | .versionInfo][0] // empty' "$primary_sbom_file")"
    if [[ -n "$pg_raw_version" ]]; then
      pg_upstream_version="${pg_raw_version%%-*}"
      pg_cpe_sbom="$work/row-${idx}-pg.cdx.json"
      pg_scan_file="$work/row-${idx}-pg.scan.json"
      jq -n --arg version "$pg_upstream_version" '{
        bomFormat: "CycloneDX",
        specVersion: "1.5",
        version: 1,
        components: [{
          type: "application",
          name: "postgresql",
          version: $version,
          cpe: ("cpe:2.3:a:postgresql:postgresql:" + $version + ":*:*:*:*:*:*:*")
        }]
      }' >"$pg_cpe_sbom"
      grype "sbom:$pg_cpe_sbom" -o json --file "$pg_scan_file" -q
      scan_files+=("$pg_scan_file")
    fi
  fi
  findings="$(summarize_findings "${scan_files[@]}")"
  fixable_count="$(jq -r '.fixable_high_critical' <<<"$findings")"
  echo "$name linux/$arch: $findings" >>"$out_file"
  printf 'controller-scan-timing image=%s arch=%s seconds=%s\n' "$name" "$arch" "$((SECONDS - started))" >>"$out_file"
  if ((fixable_count > 0)); then
    printf '%s\n' "$name linux/$arch: $fixable_count fixable High/Critical findings" >"$fail_file"
  fi
}

pids=()
scan_errors=0
for ((idx = 0; idx < rows; idx++)); do
  scan_one_archive "$idx" "${row_names[idx]}" "${row_arches[idx]}" "${row_archives[idx]}" &
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
