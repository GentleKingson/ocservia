#!/usr/bin/env bash
# Run one G6 BuildKit solve with an optional external cache. A cache service
# failure gets one bounded cold-build retry; the cold solve is authoritative.
set -euo pipefail

usage() {
  echo "usage: $0 <cache-scope> <export-cache:true|false> <log-name> <buildx-options...>" >&2
  exit 2
}

[[ $# -ge 4 ]] || usage
scope="$1"
export_cache="$2"
log_name="$3"
shift 3

[[ "${scope}" =~ ^[A-Za-z0-9._-]+$ ]] || usage
[[ "${log_name}" =~ ^[A-Za-z0-9._-]+$ ]] || usage
[[ "${export_cache}" == true || "${export_cache}" == false ]] || usage

# No external cache flags means an ordinary local BuildKit solve.
if [[ "${G6_CACHE_AVAILABLE:-false}" != true ]]; then
  exec docker buildx build "$@"
fi

cache_timeout=60s
cache_args=(
  --cache-from "type=gha,scope=${scope},timeout=${cache_timeout},version=2"
)
if [[ "${export_cache}" == true ]]; then
  cache_args+=(
    --cache-to "type=gha,scope=${scope},mode=max,ignore-error=true,timeout=${cache_timeout},version=2"
  )
fi

log_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
log_dir="${G6_CACHE_FAILURE_DIR:-${log_root}/artifacts/g6-buildkit-cache-fallback}"
mkdir -p "${log_dir}"
log_file="${log_dir}/${log_name}.log"
failure_marker="${log_dir}/cache-disabled"
if [[ -e "${failure_marker}" ]]; then
  exec docker buildx build "$@"
fi

# Keep the cache-backed attempt visible in the job log and retain a complete
# copy before retrying. PIPESTATUS preserves the BuildKit status rather than
# the status of tee.
set +e
docker buildx build "${cache_args[@]}" "$@" 2>&1 | tee "${log_file}"
cache_status="${PIPESTATUS[0]}"
set -e
if [[ "${cache_status}" -eq 0 ]]; then
  exit 0
fi

: >"${failure_marker}"
printf 'cache-backed solve failed with status %s; retrying without external cache\n' \
  "${cache_status}" >>"${log_file}"
echo "::warning::BuildKit external cache solve failed for ${log_name}; retrying once without external cache"
docker buildx build "$@"
