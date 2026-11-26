#!/usr/bin/env bash
# Shared helpers for Release Check acceptance scripts (native verification,
# pinned third-party runtime images, image loading, and local manifest generation).

readonly RELEASE_CONTROLLER_ROLES=(gateway control transport backup edge relay signer)
readonly OCSERV_POSTGRES_IMAGE="docker.io/library/postgres@sha256:e742a2668255221388004f3a42b6150a2103f5054ab3a4a93ae9e4f01e551c35"
readonly OCSERV_OTEL_IMAGE="docker.io/otel/opentelemetry-collector@sha256:5dfb40297a8da893e4e9b6b98d3ddbdd9082f73611170d6381e355e08b235334"
export OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE

release_verify_native_runner() {
  local arch="${1:?architecture required}" runner_arch kernel
  case "${arch}" in
    amd64) runner_arch=X64; kernel=x86_64 ;;
    arm64) runner_arch=ARM64; kernel=aarch64 ;;
    *) return 2 ;;
  esac
  [[ "${RUNNER_ARCH:?RUNNER_ARCH required}" == "${runner_arch}" && "$(uname -m)" == "${kernel}" ]]
  [[ "$(dpkg --print-architecture)" == "${arch}" ]]
  [[ "$(docker version --format '{{.Server.Arch}}')" == "${arch}" ]]
  [[ "$(docker context inspect --format '{{.Endpoints.docker.Host}}')" == unix://* ]]
  [[ -z "${DOCKER_HOST:-}" || "${DOCKER_HOST}" == unix://* ]]
  jq -n --arg runner "${runner_arch}" --arg kernel "${kernel}" --arg docker "${arch}" \
    '{runner_arch:$runner,kernel:$kernel,docker:$docker}'
}

release_load_controller_images() {
  local images_dir="${1:?}" version="${2:?}" arch="${3:?}" image_prefix="${4:-ghcr.io/gentlekingson/ocservia}"
  local name source archive start_load_wait idx
  local -a pids=()
  local load_errors=0 max_load_jobs=4
  for idx in "${!RELEASE_CONTROLLER_ROLES[@]}"; do
    name="${RELEASE_CONTROLLER_ROLES[idx]}"
    archive="${images_dir}/${name}-linux-${arch}.tar"
    if [[ ! -s "${archive}" ]]; then
      echo "Controller image archive is missing or empty: ${archive}" >&2
      load_errors=1
      break
    fi
    docker load -i "${archive}" >/dev/null &
    pids+=("$!")
    if ((idx + 1 >= max_load_jobs)); then
      wait "${pids[idx + 1 - max_load_jobs]}" || load_errors=1
    fi
  done
  start_load_wait=$((${#pids[@]} >= max_load_jobs ? ${#pids[@]} - max_load_jobs + 1 : 0))
  for ((idx = start_load_wait; idx < ${#pids[@]}; idx++)); do
    wait "${pids[idx]}" || load_errors=1
  done
  ((load_errors == 0)) || return 1
  for name in "${RELEASE_CONTROLLER_ROLES[@]}"; do
    source="${image_prefix}/${name}:${version}-linux-${arch}"
    # Unrelated binfmt handlers could otherwise run a foreign image emulated.
    [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${source}")" == "linux/${arch}" ]] || return 1
  done
}

release_publish_local_manifest() {
  local root="${1:?}" registry_prefix="${2:?}" repo_prefix="${3}" version="${4:?}" arch="${5:?}" source_commit="${6:?}" output="${7:?}"
  local name source target ref
  local -a pids=() manifest_args=(
    --image "postgres=${OCSERV_POSTGRES_IMAGE}"
    --image "otel=${OCSERV_OTEL_IMAGE}"
  )
  local push_errors=0 pid
  for name in "${RELEASE_CONTROLLER_ROLES[@]}"; do
    source="ghcr.io/gentlekingson/ocservia/${name}:${version}-linux-${arch}"
    target="${registry_prefix}/${repo_prefix}${name}:v${version}"
    (
      docker tag "${source}" "${target}"
      docker push "${target}"
    ) &
    pids+=("$!")
  done
  for pid in "${pids[@]}"; do
    wait "${pid}" || push_errors=1
  done
  ((push_errors == 0)) || return 1
  for name in "${RELEASE_CONTROLLER_ROLES[@]}"; do
    target="${registry_prefix}/${repo_prefix}${name}:v${version}"
    ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${target}" |
      grep "^${registry_prefix}/${repo_prefix}${name}@sha256:")"
    manifest_args+=(--image "${name}=${ref}")
    if [[ -z "${repo_prefix}" ]]; then
      export "OCSERV_${name^^}_IMAGE=${ref}"
    fi
  done
  python3 "${root}/scripts/controller_release_manifest.py" --output "${output}" \
    --release-version "${version}" --release-tag "v${version}" --source-commit "${source_commit}" \
    --migration-dir "${root}/control-plane/migrations" --platform "linux/${arch}" \
    --manifest-version 2 "${manifest_args[@]}"
}
