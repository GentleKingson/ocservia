#!/usr/bin/env bash
# Run one G6 BuildKit solve with an optional external cache. A cache service
# failure gets one bounded cold-build retry; the cold solve is authoritative.
# G6_CACHE_STRICT_EXPORT=true switches to the provisioner semantics: the
# solve's only purpose is to write the external cache, so it requires
# available cache credentials, drops the tolerant exporter, and never falls
# back to a cache-less retry — a build failure or an unfinished export fails
# the solve, so green means the cache export completed. G6_CACHE_TIMEOUT
# overrides the bounded per-command cache timeout (default 60s).
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

strict_export="${G6_CACHE_STRICT_EXPORT:-false}"
[[ "${strict_export}" == true || "${strict_export}" == false ]] || usage
if [[ "${strict_export}" == true && "${export_cache}" != true ]]; then
  echo "::error::strict cache export requires export-cache=true" >&2
  exit 2
fi

# No external cache flags means an ordinary local BuildKit solve.
if [[ "${G6_CACHE_AVAILABLE:-false}" != true ]]; then
  if [[ "${strict_export}" == true ]]; then
    echo "::error::strict cache export requires Actions cache credentials; a cache-less solve cannot provision" >&2
    exit 1
  fi
  exec docker buildx build "$@"
fi

cache_timeout="${G6_CACHE_TIMEOUT:-60s}"
cache_args=(
  --cache-from "type=gha,scope=${scope},timeout=${cache_timeout},version=2"
)
if [[ "${export_cache}" == true ]]; then
  # The tolerant exporter keeps cache problems non-fatal for validation
  # solves; the strict exporter makes an unfinished export fail the solve.
  if [[ "${strict_export}" == true ]]; then
    cache_args+=(
      --cache-to "type=gha,scope=${scope},mode=max,timeout=${cache_timeout},version=2"
    )
  else
    cache_args+=(
      --cache-to "type=gha,scope=${scope},mode=max,ignore-error=true,timeout=${cache_timeout},version=2"
    )
  fi
fi

log_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
log_dir="${G6_CACHE_FAILURE_DIR:-${log_root}/artifacts/g6-buildkit-cache-fallback}"
mkdir -p "${log_dir}"
log_file="${log_dir}/${log_name}.log"
failure_marker="${log_dir}/cache-disabled"

# The provisioner path: one strict attempt, no tolerant exporter, no
# cache-less fallback. Exiting with the solve status is the point — a
# non-zero status means the cache was not provisioned.
if [[ "${strict_export}" == true ]]; then
  if [[ -e "${failure_marker}" ]]; then
    echo "::error::strict cache export refused: an earlier solve in this job disabled the external cache" >&2
    exit 1
  fi
  set +e
  docker buildx build "${cache_args[@]}" "$@" 2>&1 | tee "${log_file}"
  strict_status="${PIPESTATUS[0]}"
  set -e
  if [[ "${strict_status}" -ne 0 ]]; then
    printf 'strict cache export solve failed with status %s; a completed cache export is required\n' \
      "${strict_status}" >>"${log_file}"
  fi
  exit "${strict_status}"
fi

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
