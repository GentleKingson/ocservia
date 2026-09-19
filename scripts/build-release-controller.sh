#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${SOURCE_COMMIT:?}" "${CONTROLLER_ARCH:?}" "${OUTPUT_DIR:?}" "${BUILDX_BUILDER:?}"
case "${CONTROLLER_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
[[ "$(git -C "${ROOT}" rev-parse HEAD)" == "${SOURCE_COMMIT}" ]]
mkdir -p "${OUTPUT_DIR}"
driver_opts=()
if [[ "${G6_CACHE_AVAILABLE:-false}" == true ]]; then
  driver_opts+=(--driver-opt "env.ACTIONS_RUNTIME_TOKEN=${ACTIONS_RUNTIME_TOKEN}")
  for name in ACTIONS_CACHE_URL ACTIONS_RESULTS_URL; do
    [[ -z "${!name:-}" ]] || driver_opts+=(--driver-opt "env.${name}=${!name}")
  done
fi
docker buildx create --driver docker-container \
  --driver-opt image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8 \
  "${driver_opts[@]}" --name "${BUILDX_BUILDER}" --bootstrap --use
build_image() {
  local name="$1" dockerfile="$2"
  local -a args=(--builder "${BUILDX_BUILDER}" --platform "linux/${CONTROLLER_ARCH}"
    --provenance=false --pull --output "type=docker,dest=${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
    --metadata-file "${OUTPUT_DIR}/${name}.metadata.json"
    --label "org.opencontainers.image.version=${VERSION}"
    --label "org.opencontainers.image.revision=${SOURCE_COMMIT}"
    --tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}"
    --file "${ROOT}/${dockerfile}")
  if [[ "${name}" == control ]]; then
    args+=(--build-arg "VERSION=${VERSION}" --build-arg "COMMIT=${SOURCE_COMMIT}" --no-cache-filter runtime-base)
  fi
  # Reuse the existing bounded cache wrapper; image and native architecture
  # have independent scopes. Candidate labels and output digests stay fresh.
  bash "${ROOT}/scripts/g6-buildx-cache.sh" "controller-v1-${name}-linux-${CONTROLLER_ARCH}" true \
    "controller-${name}-${CONTROLLER_ARCH}" "${args[@]}" "${ROOT}"
  test -s "${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  jq -er '."containerimage.digest" | strings | select(test("^sha256:[0-9a-f]{64}$"))' "${OUTPUT_DIR}/${name}.metadata.json" >/dev/null
}
build_image gateway deploy/production/gateway.Dockerfile
build_image control control-plane/Dockerfile
build_image transport rust/transportd.Dockerfile
build_image backup deploy/production/backup.Dockerfile
