#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
fake_bin="${test_dir}/bin"
mkdir -p "${fake_bin}"
cleanup() {
  rm -rf -- "${test_dir}"
}
trap cleanup EXIT

cat >"${fake_bin}/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail

log_file="${G6_FAKE_DOCKER_LOG:?G6_FAKE_DOCKER_LOG is required}"
printf '%s\n' "$*" >>"${log_file}"
[[ "${1:-}" == buildx && "${2:-}" == build ]] || exit 64
if [[ "${G6_FAKE_IMPORT_OK:-false}" != true ]] && [[ " $* " == *" --cache-from "* ]]; then
  echo "simulated cache importer failure" >&2
  exit 23
fi
echo "cold solve succeeded"
if [[ "${G6_FAKE_COLD_FAILURE:-false}" == true ]]; then
  exit 29
fi
if [[ "${G6_FAKE_STRICT_EXPORT_FAILURE:-false}" == true ]] \
  && [[ " $* " == *" --cache-to "* ]] \
  && [[ " $* " != *" ignore-error=true "* ]]; then
  echo "simulated cache exporter failure" >&2
  exit 31
fi
DOCKER
chmod 0755 "${fake_bin}/docker"

docker_log="${test_dir}/docker.log"
PATH="${fake_bin}:${PATH}" \
  G6_CACHE_AVAILABLE=true \
  G6_CACHE_FAILURE_DIR="${test_dir}/cache-failures" \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${docker_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-control-plane true control-plane-build \
  --pull=false --load --file control-plane/Dockerfile .

[[ "$(wc -l <"${docker_log}" | tr -d '[:space:]')" == 2 ]] || {
  echo "cache importer failure must cause exactly one cold retry" >&2
  exit 1
}
cached_invocation="$(sed -n '1p' "${docker_log}")"
cold_invocation="$(sed -n '2p' "${docker_log}")"
[[ "${cached_invocation}" == *"--cache-from type=gha,scope=g6-control-plane,timeout=60s,version=2"* ]] || {
  echo "the first invocation must use the bounded GHA importer" >&2
  exit 1
}
[[ "${cached_invocation}" == *"--cache-to type=gha,scope=g6-control-plane,mode=max,ignore-error=true,timeout=60s,version=2"* ]] || {
  echo "the first invocation must use the tolerant GHA exporter" >&2
  exit 1
}
if [[ "${cold_invocation}" == *"--cache-from"* || "${cold_invocation}" == *"--cache-to"* ]]; then
  echo "the cold retry must not carry external cache flags" >&2
  exit 1
fi
grep -Fq 'simulated cache importer failure' "${test_dir}/cache-failures/control-plane-build.log"

PATH="${fake_bin}:${PATH}" \
  G6_CACHE_AVAILABLE=true \
  G6_CACHE_FAILURE_DIR="${test_dir}/cache-failures" \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${docker_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-control-plane true control-plane-after-failure \
  --pull=false --load --file control-plane/Dockerfile .
[[ "$(wc -l <"${docker_log}" | tr -d '[:space:]')" == 3 ]] || {
  echo "a cache failure must disable repeated external-cache attempts in the job" >&2
  exit 1
}
after_failure_invocation="$(sed -n '3p' "${docker_log}")"
if [[ "${after_failure_invocation}" == *"--cache-from"* || "${after_failure_invocation}" == *"--cache-to"* ]]; then
  echo "subsequent solves must stay on the cold path after a cache failure" >&2
  exit 1
fi

no_cache_log="${test_dir}/no-cache.log"
PATH="${fake_bin}:${PATH}" \
  G6_CACHE_AVAILABLE=false \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${no_cache_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-relay true relay-build \
  --pull=false --load --file deploy/production/relay.Dockerfile .
if grep -Eq -- '--cache-(from|to)' "${no_cache_log}"; then
  echo "the credential-free path must execute a local solve directly" >&2
  exit 1
fi

# Strict export mode: provisioner semantics. A refused or failed strict
# export must fail the solve without a cache-less retry, while a completed
# strict export exits zero exactly once.
strict_refusal_log="${test_dir}/strict-refusal.log"
if PATH="${fake_bin}:${PATH}" \
  G6_CACHE_STRICT_EXPORT=true \
  G6_CACHE_AVAILABLE=false \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${strict_refusal_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-rust-runtime true strict-no-credentials \
  --pull=false --file rust/g6-runtime.Dockerfile . >"${test_dir}/strict-refusal.out" 2>&1; then
  echo "strict export without cache credentials must fail" >&2
  exit 1
fi
if [[ -e "${strict_refusal_log}" ]]; then
  echo "a refused strict export must not invoke docker" >&2
  exit 1
fi
grep -Fq 'strict cache export requires Actions cache credentials' "${test_dir}/strict-refusal.out"

strict_no_export_log="${test_dir}/strict-no-export.log"
if PATH="${fake_bin}:${PATH}" \
  G6_CACHE_STRICT_EXPORT=true \
  G6_CACHE_AVAILABLE=true \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${strict_no_export_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-rust-runtime false strict-without-export \
  --pull=false --file rust/g6-runtime.Dockerfile . >"${test_dir}/strict-no-export.out" 2>&1; then
  echo "strict export with export-cache=false must fail validation" >&2
  exit 1
fi
if [[ -e "${strict_no_export_log}" ]]; then
  echo "the strict validation refusal must not invoke docker" >&2
  exit 1
fi

strict_marker_dir="${test_dir}/cache-failures-strict-marker"
mkdir -p "${strict_marker_dir}"
: >"${strict_marker_dir}/cache-disabled"
strict_marker_log="${test_dir}/strict-marker.log"
if PATH="${fake_bin}:${PATH}" \
  G6_CACHE_STRICT_EXPORT=true \
  G6_CACHE_AVAILABLE=true \
  G6_CACHE_FAILURE_DIR="${strict_marker_dir}" \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${strict_marker_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-rust-runtime true strict-with-marker \
  --pull=false --file rust/g6-runtime.Dockerfile . >"${test_dir}/strict-marker.out" 2>&1; then
  echo "strict export must refuse a job that already disabled the external cache" >&2
  exit 1
fi
if [[ -e "${strict_marker_log}" ]]; then
  echo "the strict marker refusal must not invoke docker" >&2
  exit 1
fi

strict_export_log="${test_dir}/strict-export.log"
if PATH="${fake_bin}:${PATH}" \
  G6_CACHE_STRICT_EXPORT=true \
  G6_CACHE_AVAILABLE=true \
  G6_FAKE_IMPORT_OK=true \
  G6_FAKE_STRICT_EXPORT_FAILURE=true \
  G6_CACHE_TIMEOUT=300s \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${strict_export_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-rust-runtime true strict-export-failure \
  --pull=false --file rust/g6-runtime.Dockerfile . >"${test_dir}/strict-export.out" 2>&1; then
  echo "a failed strict cache export must fail the solve" >&2
  exit 1
fi
[[ "$(wc -l <"${strict_export_log}" | tr -d '[:space:]')" == 1 ]] || {
  echo "the strict exporter must not fall back to a cache-less retry" >&2
  exit 1
}
strict_export_invocation="$(sed -n '1p' "${strict_export_log}")"
[[ "${strict_export_invocation}" == *"--cache-to type=gha,scope=g6-rust-runtime,mode=max,timeout=300s,version=2"* ]] || {
  echo "the strict exporter must carry the configured timeout without ignore-error" >&2
  exit 1
}

strict_ok_log="${test_dir}/strict-ok.log"
mkdir -p "${test_dir}/cache-failures-strict-ok"
PATH="${fake_bin}:${PATH}" \
  G6_CACHE_STRICT_EXPORT=true \
  G6_CACHE_AVAILABLE=true \
  G6_FAKE_IMPORT_OK=true \
  G6_CACHE_FAILURE_DIR="${test_dir}/cache-failures-strict-ok" \
  RUNNER_TEMP="${test_dir}" \
  G6_FAKE_DOCKER_LOG="${strict_ok_log}" \
  "${ROOT}/scripts/g6-buildx-cache.sh" \
  g6-rust-runtime true strict-ok \
  --pull=false --file rust/g6-runtime.Dockerfile .
[[ "$(wc -l <"${strict_ok_log}" | tr -d '[:space:]')" == 1 ]] || {
  echo "a successful strict export must be a single solve" >&2
  exit 1
}
strict_ok_invocation="$(sed -n '1p' "${strict_ok_log}")"
[[ "${strict_ok_invocation}" == *"--cache-to type=gha,scope=g6-rust-runtime,mode=max,timeout=60s,version=2"* ]] || {
  echo "the strict exporter must default to the 60s cache timeout" >&2
  exit 1
}
if [[ "${strict_ok_invocation}" == *"ignore-error"* ]]; then
  echo "the strict exporter must not tolerate export errors" >&2
  exit 1
fi

echo "G6 BuildKit cache fallback checks passed"
