#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${SOURCE_COMMIT:?}" "${CONTROLLER_ARCH:?}" "${BUILDX_BUILDER:?}"
export_cache=false
case "${1:-}" in
  '') : "${OUTPUT_DIR:?}"; mkdir -p "${OUTPUT_DIR}" ;;
  --cache-only)
    # Only the trusted main push provisioner writes shared release caches.
    [[ "${GITHUB_REF:-}" == refs/heads/codex/controller-perf-* ]] || exit 2
    export_cache=true
    export BUILD_CACHE_STRICT_EXPORT=true
    ;;
  *) exit 2 ;;
esac
[[ $# -le 1 ]] || exit 2
cache_images=(gateway control transport backup edge relay signer)
if [[ "${export_cache}" == true && -n "${CONTROLLER_CACHE_IMAGES:-}" ]]; then
  [[ "${CONTROLLER_CACHE_IMAGES}" != *$'\n'* && "${CONTROLLER_CACHE_IMAGES}" != *$'\r'* ]] || exit 2
  read -r -a selected_images <<<"${CONTROLLER_CACHE_IMAGES}"
  if ((${#selected_images[@]})); then
    for image in "${selected_images[@]}"; do
      case "${image}" in
        gateway|control|transport|backup|edge|relay|signer) ;;
        *) echo "unknown Controller cache image: ${image}" >&2; exit 2 ;;
      esac
    done
    cache_images=("${selected_images[@]}")
  fi
fi
case "${CONTROLLER_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
# buildx forwards SOURCE_DATE_EPOCH as a build arg, which stamps WORKDIR and
# changes cache keys per commit; keep keys identical to the main cache refresh.
unset SOURCE_DATE_EPOCH
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
timing_dir="$(mktemp -d)"
trap 'rm -rf -- "${timing_dir}"' EXIT

build_image() {
  local name="$1" dockerfile="$2" cache_mode=min
  if [[ "${export_cache}" == true && " ${cache_images[*]} " != *" ${name} "* ]]; then return 0; fi
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
    args+=(--build-arg "VERSION=${VERSION}" --build-arg "COMMIT=${SOURCE_COMMIT}")
  fi
  if [[ "$export_cache" == true && "${BENCH_VARIANT}" =~ ^(compiler|combined)$ && "$name" =~ ^(transport|relay)$ ]]; then
    args+=(--target build)
  fi
  if [[ "$name" == transport && "${BENCH_VARIANT}" =~ ^(recipe|combined)$ ]]; then
    args+=(--file "${ROOT}/rust/transportd-bench.Dockerfile")
  fi
  # A fixed base digest and restored compiler cache must not freeze OS updates.
  if [[ " ${args[*]} " != *" --target build "* ]]; then
  case "${name}" in
    control|transport) args+=(--no-cache-filter runtime-base) ;;
    relay) args+=(--no-cache-filter relay-runtime) ;;
    gateway|backup|edge) args+=(--no-cache-filter runtime) ;;
  esac
  fi
  # Keep compiler/dependency layers only for the expensive native builds.
  case "${name}" in control|transport|relay|signer) cache_mode=max ;; esac
  # Release and Business restore only; main CI refreshes both native arches.
  local started="${SECONDS}" result=0 log_file="${timing_dir}/${name}.log"
  BUILD_CACHE_MODE="${cache_mode}" bash "${ROOT}/scripts/buildx-cache.sh" "controller-v1-${name}-linux-${CONTROLLER_ARCH}" "${export_cache}" \
    "controller-${name}-${CONTROLLER_ARCH}" --progress plain "${args[@]}" "${ROOT}" 2>&1 | tee "${log_file}" || result=$?
  local compiler_steps
  compiler_steps="$(awk '
    /#0 building with/ { delete compilation; delete observed }
    /^#[0-9]+ .* RUN (cargo chef cook |cargo build |bash scripts\/build-relay\.sh )/ {
      line = $0
      sub(/^.* RUN /, "", line)
      compilation[$1] = (line ~ /^cargo chef cook / ? "dependencies" : "binary")
    }
    /^#[0-9]+ (CACHED|DONE)/ && ($1 in compilation) {
      observed[compilation[$1]] = ($2 == "CACHED" ? "cached" : $3)
    }
    END {
      printf "dependencies=%s binary=%s", (("dependencies" in observed) ? observed["dependencies"] : "n/a"), \
        (("binary" in observed) ? observed["binary"] : "n/a")
    }
  ' "${log_file}")"
  local cache_retry=no
  if grep -qF 'retrying once without external cache' "${log_file}"; then cache_retry=yes; fi
  printf 'controller-build-timing image=%s arch=%s seconds=%s %s external_cache_retry=%s exit=%s\n' \
    "${name}" "${CONTROLLER_ARCH}" "$((SECONDS - started))" "${compiler_steps}" "${cache_retry}" "${result}"
  ((result == 0)) || return "${result}"
  if [[ "${export_cache}" == false ]]; then
    test -s "${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  fi
}
build_image gateway deploy/production/gateway.Dockerfile
build_image control control-plane/Dockerfile
if [[ "$export_cache" == false && "$CONTROLLER_ARCH" == amd64 && "$BENCH_VARIANT" =~ ^(parallel|combined)$ ]]; then
  build_image transport rust/transportd.Dockerfile &
  transport_pid=$!
  build_image relay deploy/production/relay.Dockerfile &
  relay_pid=$!
  status=0
  wait "$transport_pid" || status=$?
  wait "$relay_pid" || status=$?
  ((status == 0)) || exit "$status"
  build_image backup deploy/production/backup.Dockerfile
  build_image edge deploy/production/edge.Dockerfile
else
  build_image transport rust/transportd.Dockerfile
  build_image backup deploy/production/backup.Dockerfile
  build_image edge deploy/production/edge.Dockerfile
  build_image relay deploy/production/relay.Dockerfile
fi
build_image signer deploy/production/signer.Dockerfile
