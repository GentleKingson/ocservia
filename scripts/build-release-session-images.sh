#!/usr/bin/env bash
# Native application fixture images; no registry publication or node rebuild.
set -euo pipefail
: "${CANDIDATE_SHA:?}" "${PACKAGE_ARCH:?}" "${SESSION_EVIDENCE:?}" "${GITHUB_ENV:?}"
[[ "${CANDIDATE_SHA}" =~ ^[0-9a-f]{40}$ && "$(git rev-parse HEAD)" == "${CANDIDATE_SHA}" ]]
[[ "${GITHUB_SHA:?}" == "${CANDIDATE_SHA}" && -z "$(git status --porcelain)" ]]
mkdir -m 0700 -- "${SESSION_EVIDENCE}"
printf 'SESSION_EVIDENCE=%s\n' "${SESSION_EVIDENCE}" >>"${GITHUB_ENV}"
bash scripts/release-upgrade-native.sh "${PACKAGE_ARCH}" >"${SESSION_EVIDENCE}/native.json"
exec > >(tee "${SESSION_EVIDENCE}/build.log") 2>&1
# The existing fixture uses this image before Compose starts, with --pull=never.
docker pull --platform "linux/${PACKAGE_ARCH}" postgres:17.10-bookworm
docker image inspect postgres:17.10-bookworm >"${SESSION_EVIDENCE}/postgres.image.json"
driver_opts=()
if [[ "${G6_CACHE_AVAILABLE:-false}" == true ]]; then
  driver_opts+=(--driver-opt "env.ACTIONS_RUNTIME_TOKEN=${ACTIONS_RUNTIME_TOKEN}")
  for name in ACTIONS_CACHE_URL ACTIONS_RESULTS_URL; do
    [[ -z "${!name:-}" ]] || driver_opts+=(--driver-opt "env.${name}=${!name}")
  done
fi
builder="session-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
docker buildx create --driver docker-container \
  --driver-opt image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8 \
  "${driver_opts[@]}" --name "${builder}" --bootstrap --use
build_image() {
  local variable="$1" scope="$2" dockerfile="$3" id
  shift 3
  bash scripts/g6-buildx-cache.sh "session-${scope}-${PACKAGE_ARCH}" true "${variable}" \
    --builder "${builder}" --platform "linux/${PACKAGE_ARCH}" --provenance=false --load \
    --label "org.opencontainers.image.revision=${CANDIDATE_SHA}" \
    --metadata-file "${SESSION_EVIDENCE}/${variable}.metadata.json" \
    --tag "session-${variable,,}:candidate" --file "${dockerfile}" "$@" .
  id="$(docker image inspect --format '{{.Id}}' "session-${variable,,}:candidate")"
  printf '%s=%s\n' "${variable}" "${id}" >>"${GITHUB_ENV}"
  docker image inspect "${id}" >"${SESSION_EVIDENCE}/${variable}.image.json"
}
build_image G6RD_CONTROL_PLANE_IMAGE control control-plane/Dockerfile \
  --build-arg "COMMIT=${CANDIDATE_SHA}" --no-cache-filter runtime-base
# The single-Relay chain needs the launcher shipped in the production image.
build_image G6RD_TRANSPORTD_IMAGE transport rust/transportd.Dockerfile
docker run --rm --entrypoint /bin/sh session-g6rd_transportd_image:candidate \
  -ec 'test -x /usr/local/libexec/ocservia-transportd-relays'
build_image G6RD_PROBE_IMAGE rust rust/g6-runtime.Dockerfile --target g6-probe-runtime
build_image G6RD_RELAY_IMAGE relay deploy/production/relay.Dockerfile --no-cache-filter relay-runtime
build_image SINGLE_NODE_IMAGE node scripts/single-relay-node.Dockerfile
# Reuse the real-process TLS PKI fixture without any candidate node binaries.
# The default builder can resolve these job-local images; no registry push.
docker tag session-g6rd_transportd_image:candidate ocservia-pr02-transport:e2e
docker tag session-g6rd_relay_image:candidate ocservia-pr02-relay:e2e
docker build --builder default --target workflow-base \
  --label "org.opencontainers.image.revision=${CANDIDATE_SHA}" \
  -f deploy/database-e2e/Dockerfile -t session-workflow:candidate .
workflow_image="$(docker image inspect --format '{{.Id}}' session-workflow:candidate)"
printf 'RELEASE_WORKFLOW_IMAGE=%s\n' "${workflow_image}" >>"${GITHUB_ENV}"
docker image inspect "${workflow_image}" >"${SESSION_EVIDENCE}/RELEASE_WORKFLOW_IMAGE.image.json"
mkdir -p .cache/go-mod .cache/go-build
docker run --rm -v "$PWD:/workspace:ro" -v "$PWD/.cache/go-mod:/go-mod" \
  -e GOMODCACHE=/go-mod -e GOTOOLCHAIN=local "${workflow_image}" go mod download
docker run --rm --entrypoint /usr/local/bin/iroh-relay session-g6rd_relay_image:candidate --version \
  >"${SESSION_EVIDENCE}/relay-version.txt"
docker version >"${SESSION_EVIDENCE}/docker-version.txt"
docker compose version >>"${SESSION_EVIDENCE}/docker-version.txt"
node --version >"${SESSION_EVIDENCE}/node-version.txt"
uname -a >"${SESSION_EVIDENCE}/kernel.txt"
