#!/usr/bin/env bash
# Run one BuildKit solve with an optional external cache. A cache service
# failure gets one bounded cold-build retry; the cold solve is authoritative.
# BUILD_CACHE_STRICT_EXPORT=true switches to the provisioner semantics: the
# solve's only purpose is to write the external cache, so it requires
# available cache credentials, drops the tolerant exporter, and never falls
# back to a cache-less retry. It retries a failed solve once with the same
# arguments; green still means the cache export completed. BUILD_CACHE_TIMEOUT
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

strict_export="${BUILD_CACHE_STRICT_EXPORT:-false}"
[[ "${strict_export}" == true || "${strict_export}" == false ]] || usage
cache_mode="${BUILD_CACHE_MODE:-max}"
[[ "${cache_mode}" == min || "${cache_mode}" == max ]] || usage
if [[ "${strict_export}" == true && "${export_cache}" != true ]]; then
  echo "::error::strict cache export requires export-cache=true" >&2
  exit 2
fi

# No external cache flags means an ordinary local BuildKit solve.
if [[ "${BUILD_CACHE_AVAILABLE:-false}" != true ]]; then
  if [[ "${strict_export}" == true ]]; then
    echo "::error::strict cache export requires Actions cache credentials; a cache-less solve cannot provision" >&2
    exit 1
  fi
  exec docker buildx build "$@"
fi

cache_timeout="${BUILD_CACHE_TIMEOUT:-60s}"
cache_args=(
  --cache-from "type=gha,scope=${scope},timeout=${cache_timeout},version=2"
)
if [[ "${export_cache}" == true ]]; then
  # The tolerant exporter keeps cache problems non-fatal for validation
  # solves; the strict exporter makes an unfinished export fail the solve.
  if [[ "${strict_export}" == true ]]; then
    cache_args+=(
      --cache-to "type=gha,scope=${scope},mode=${cache_mode},timeout=${cache_timeout},version=2"
    )
  else
    cache_args+=(
      --cache-to "type=gha,scope=${scope},mode=${cache_mode},ignore-error=true,timeout=${cache_timeout},version=2"
    )
  fi
fi

log_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
log_dir="${BUILD_CACHE_FAILURE_DIR:-${log_root}/artifacts/buildkit-cache-fallback}"
mkdir -p "${log_dir}"
log_file="${log_dir}/${log_name}.log"
failure_marker="${log_dir}/cache-disabled"

# The provisioner retries the same strict solve once, keeping the builder's
# local layers and requiring a completed export. Docker's exit code does not
# distinguish transient export errors from build errors, so either can retry.
if [[ "${strict_export}" == true ]]; then
  if [[ -e "${failure_marker}" ]]; then
    echo "::error::strict cache export refused: an earlier solve in this job disabled the external cache" >&2
    exit 1
  fi
  trap 'exit 130' INT
  trap 'exit 143' TERM
  : >"${log_file}"
  for attempt in 1 2; do
    printf 'strict cache export attempt %s/2\n' "${attempt}" | tee -a "${log_file}"
    set +e
    docker buildx build "${cache_args[@]}" "$@" 2>&1 | tee -a "${log_file}"
    strict_status="${PIPESTATUS[0]}"
    set -e
    if [[ "${strict_status}" -eq 0 ]]; then exit 0; fi
    printf 'strict cache export solve failed with status %s; a completed cache export is required\n' \
      "${strict_status}" >>"${log_file}"
    # Do not retry interrupted/killed solves, including cancellation by CI.
    case "${strict_status}" in 130|137|143) exit "${strict_status}" ;; esac
    if [[ "${attempt}" == 1 ]]; then
      echo "::warning::Strict cache solve failed for ${log_name}; retrying once with the same cache settings in 10s" | tee -a "${log_file}"
      sleep 10
    fi
  done
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
