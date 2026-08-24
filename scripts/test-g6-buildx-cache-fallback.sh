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
if [[ " $* " == *" --cache-from "* ]]; then
  echo "simulated cache importer failure" >&2
  exit 23
fi
echo "cold solve succeeded"
if [[ "${G6_FAKE_COLD_FAILURE:-false}" == true ]]; then
  exit 29
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

echo "G6 BuildKit cache fallback checks passed"
