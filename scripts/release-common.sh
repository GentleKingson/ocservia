#!/usr/bin/env bash
# Shared helpers for Release Check acceptance scripts (native verification,
# pinned third-party runtime images, image loading, and local manifest generation).

readonly RELEASE_CONTROLLER_ROLES=(gateway control transport backup edge relay signer)
readonly OCSERV_POSTGRES_IMAGE="docker.io/library/postgres@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af"
readonly OCSERV_OTEL_IMAGE="docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23"
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
  local images_dir="${1:?}" version="${2:?}" arch="${3:?}" name source
  for name in "${RELEASE_CONTROLLER_ROLES[@]}"; do
    source="ghcr.io/gentlekingson/ocservia/${name}:${version}-linux-${arch}"
    docker load -i "${images_dir}/${name}-linux-${arch}.tar"
    # Unrelated binfmt handlers could otherwise run a foreign image emulated.
    [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${source}")" == "linux/${arch}" ]]
  done
}

release_publish_local_manifest() {
  local root="${1:?}" registry_prefix="${2:?}" repo_prefix="${3}" version="${4:?}" arch="${5:?}" source_commit="${6:?}" output="${7:?}"
  local name source target ref
  local -a manifest_args=(
    --image "postgres=${OCSERV_POSTGRES_IMAGE}"
    --image "otel=${OCSERV_OTEL_IMAGE}"
  )
  for name in "${RELEASE_CONTROLLER_ROLES[@]}"; do
    source="ghcr.io/gentlekingson/ocservia/${name}:${version}-linux-${arch}"
    target="${registry_prefix}/${repo_prefix}${name}:v${version}"
    docker tag "${source}" "${target}"
    docker push "${target}"
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
