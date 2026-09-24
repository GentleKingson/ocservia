#!/usr/bin/env bash
# Source-only fixtures. Consumers verify the producer manifest before loading.
set -euo pipefail
mode="${1:?build or load}" component="${2:?fixture component}" arch="${3:?architecture}" directory="${4:?artifact directory}"
: "${GITHUB_SHA:?}" "${VERSION:?}"
[[ "${arch}" == amd64 || "${arch}" == arm64 ]]
case "${component}" in
  test-helpers) names=(probe relay) ;;
  session-base) names=(node workflow-tools) ;;
  rpm-test) names=(rpm) ;;
  *) exit 2 ;;
esac
if [[ "${mode}" == build ]]; then
  [[ "$(git rev-parse HEAD)" == "${GITHUB_SHA}" ]]
  mkdir -p "${directory}"
elif [[ "${mode}" == load ]]; then
  node scripts/release-artifacts.mjs verify "${directory}" "${component}" "${arch}" "${VERSION}" "${TEST_IMAGES_SHA256:?}"
else exit 2; fi
for name in "${names[@]}"; do
  tag="ocservia-release-${name}:${GITHUB_SHA}-${arch}"
  if [[ "${mode}" == build ]]; then
    args=()
    case "${name}" in
      probe) file=rust/g6-runtime.Dockerfile; args+=(--target g6-probe-runtime) ;;
      relay) file=deploy/production/relay.Dockerfile; args+=(--no-cache-filter relay-runtime) ;;
      node) file=scripts/single-relay-node.Dockerfile ;;
      workflow-tools) file=deploy/database-e2e/Dockerfile; args+=(--target workflow-tools) ;;
      rpm) file=scripts/release-rpm-test.Dockerfile ;;
    esac
    started="$(date +%s)"
    bash scripts/g6-buildx-cache.sh "release-fixture-${name}-${arch}" true "fixture-${name}" \
      --platform "linux/${arch}" --provenance=false --load \
      --label "org.opencontainers.image.revision=${GITHUB_SHA}" \
      --file "${file}" --tag "${tag}" "${args[@]}" .
    echo "${name} image preparation: $(( $(date +%s) - started ))s"
    docker save -o "${directory}/${name}.tar" "${tag}"
  else
    docker load -i "${directory}/${name}.tar"
  fi
  docker image inspect "${tag}" | jq -e --arg sha "${GITHUB_SHA}" --arg arch "${arch}" '
    length == 1 and .[0].Architecture == $arch and
    .[0].Config.Labels["org.opencontainers.image.revision"] == $sha
  ' >/dev/null
  if [[ "${mode}" == load ]]; then
    variable="RELEASE_${name^^}_IMAGE"
    printf '%s=%s\n' "${variable//-/_}" "$(docker image inspect --format '{{.Id}}' "${tag}")" >>"${GITHUB_ENV:?}"
  fi
done
if [[ "${mode}" == build && "${component}" == test-helpers ]]; then
  # The probe target already compiled the tunnel with the same feature graph.
  bash scripts/g6-buildx-cache.sh "release-fixture-probe-${arch}" false fixture-tunnel \
    --platform "linux/${arch}" --file rust/g6-runtime.Dockerfile --target g6-tunnel-artifact \
    --output "type=local,dest=${directory}" .
fi
if [[ "${mode}" == build ]]; then
  node scripts/release-artifacts.mjs seal "${directory}" "${component}" "${arch}" "${VERSION}"
fi
