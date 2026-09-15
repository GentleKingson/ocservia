#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${SOURCE_COMMIT:?}" "${CONTROLLER_ARCH:?}" "${OUTPUT_DIR:?}" "${BUILDX_BUILDER:?}"
case "${CONTROLLER_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
[[ "$(git -C "${ROOT}" rev-parse HEAD)" == "${SOURCE_COMMIT}" ]]
mkdir -p "${OUTPUT_DIR}"
build_image() {
  local name="$1" dockerfile="$2"
  local -a args=(--builder "${BUILDX_BUILDER}" --platform "linux/${CONTROLLER_ARCH}"
    --provenance=false --pull --output "type=docker,dest=${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
    --metadata-file "${OUTPUT_DIR}/${name}.metadata.json"
    --label "org.opencontainers.image.version=${VERSION}"
    --label "org.opencontainers.image.revision=${SOURCE_COMMIT}"
    --tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}"
    --file "${ROOT}/${dockerfile}")
  if [[ "${name}" == control ]]; then args+=(--build-arg "VERSION=${VERSION}" --build-arg "COMMIT=${SOURCE_COMMIT}"); fi
  docker buildx build "${args[@]}" "${ROOT}"
  test -s "${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  jq -er '."containerimage.digest" | strings | select(test("^sha256:[0-9a-f]{64}$"))' "${OUTPUT_DIR}/${name}.metadata.json" >/dev/null
}
build_image gateway deploy/production/gateway.Dockerfile
build_image control control-plane/Dockerfile
build_image transport rust/transportd.Dockerfile
build_image backup deploy/production/backup.Dockerfile
