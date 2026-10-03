#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${SOURCE_COMMIT:?}" "${CONTROLLER_ARCH:?}" "${BUILDX_BUILDER:?}"
export_cache=false
case "${1:-}" in
  '') : "${OUTPUT_DIR:?}"; mkdir -p "${OUTPUT_DIR}" ;;
  --cache-only)
    # Only the trusted main push provisioner writes shared release caches.
    [[ "${GITHUB_EVENT_NAME:-}" == push && "${GITHUB_REF:-}" == refs/heads/main ]] || exit 2
    export_cache=true
    export BUILD_CACHE_STRICT_EXPORT=true
    ;;
  *) exit 2 ;;
esac
[[ $# -le 1 ]] || exit 2
case "${CONTROLLER_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
driver_opts=()
if [[ "${BUILD_CACHE_AVAILABLE:-false}" == true ]]; then
  driver_opts+=(--driver-opt "env.ACTIONS_RUNTIME_TOKEN=${ACTIONS_RUNTIME_TOKEN}")
  for name in ACTIONS_CACHE_URL ACTIONS_RESULTS_URL; do
    [[ -z "${!name:-}" ]] || driver_opts+=(--driver-opt "env.${name}=${!name}")
  done
fi
docker buildx create --driver docker-container \
  --driver-opt image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8 \
  "${driver_opts[@]}" --name "${BUILDX_BUILDER}" --bootstrap --use
build_image() {
  local name="$1" dockerfile="$2" cache_mode=min
  local -a args=(--builder "${BUILDX_BUILDER}" --platform "linux/${CONTROLLER_ARCH}"
    --provenance=false --pull
    --label "org.opencontainers.image.version=${VERSION}"
    --label "org.opencontainers.image.revision=${SOURCE_COMMIT}"
    --tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}"
    --file "${ROOT}/${dockerfile}")
  if [[ "${export_cache}" == true ]]; then
    args+=(--output type=cacheonly)
  else
    args+=(--output "type=docker,dest=${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar")
  fi
  if [[ "${name}" == control ]]; then
    args+=(--build-arg "VERSION=${VERSION}" --build-arg "COMMIT=${SOURCE_COMMIT}" --no-cache-filter runtime-base)
  fi
  # Keep compiler/dependency layers only for the expensive native builds.
  case "${name}" in control|transport|relay|signer) cache_mode=max ;; esac
  # Release and Business restore only; main CI refreshes both native arches.
  # ponytail: GHA eviction can still cause cold builds; measure before adding another cache backend.
  BUILD_CACHE_MODE="${cache_mode}" bash "${ROOT}/scripts/buildx-cache.sh" "controller-v1-${name}-linux-${CONTROLLER_ARCH}" "${export_cache}" \
    "controller-${name}-${CONTROLLER_ARCH}" "${args[@]}" "${ROOT}"
  if [[ "${export_cache}" == false ]]; then
    test -s "${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  fi
}
build_image gateway deploy/production/gateway.Dockerfile
build_image control control-plane/Dockerfile
build_image transport rust/transportd.Dockerfile
build_image backup deploy/production/backup.Dockerfile
build_image edge deploy/production/edge.Dockerfile
build_image relay deploy/production/relay.Dockerfile
build_image signer deploy/production/signer.Dockerfile
build_image mysql_backup deploy/production/backup.mysql.Dockerfile
