#!/usr/bin/env bash
# Cache-correctness regression test for the persistent BuildKit caches of
# the G6 release producers: a cache hit must produce exactly the same trust
# result as a cache miss. Builds a deterministic fixture twice through
# docker-container builders — once cold with a cache export, once in a
# fresh builder session with only the cache import — and asserts identical
# image IDs, identical provenance labels, identical payload bytes, and
# that the second solve actually reused the cached layers. It also proves
# the cache cannot smuggle stale content: changing the inputs rebuilds the
# affected layer and produces a different image. The hosted release jobs
# use the same BuildKit cache machinery through type=gha; the local cache
# backend keeps this test runnable on any docker + buildx host.
set -euo pipefail

FIXTURE_BASE='busybox:1.37@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0'

command -v docker >/dev/null 2>&1 || {
  echo "this test requires docker" >&2
  exit 1
}
docker info >/dev/null 2>&1 || {
  echo "this test requires a reachable docker daemon" >&2
  exit 1
}
docker buildx version >/dev/null 2>&1 || {
  echo "this test requires the docker buildx plugin" >&2
  exit 1
}

fixture="$(mktemp -d)"
builder_cold="g6-cache-cold-$$"
builder_warm="g6-cache-warm-$$"
cleanup() {
  status=$?
  docker buildx rm --force "${builder_cold}" >/dev/null 2>&1 || true
  docker buildx rm --force "${builder_warm}" >/dev/null 2>&1 || true
  docker image rm --force g6-cache-fixture:cold g6-cache-fixture:warm \
    g6-cache-fixture:changed >/dev/null 2>&1 || true
  rm -rf -- "${fixture}"
  exit "${status}"
}
trap cleanup EXIT

cat >"${fixture}/Dockerfile" <<DOCKERFILE
FROM ${FIXTURE_BASE} AS build
RUN echo cache-fixture-payload > /payload

FROM ${FIXTURE_BASE}
COPY --from=build /payload /payload
LABEL org.opencontainers.image.revision=0123456789abcdef0123456789abcdef01234567
DOCKERFILE

docker buildx create --driver docker-container --name "${builder_cold}" --bootstrap >/dev/null
docker buildx create --driver docker-container --name "${builder_warm}" --bootstrap >/dev/null

# Cold solve: no cache import, export every layer for the warm solve.
docker buildx build --builder "${builder_cold}" --pull=false \
  --cache-to "type=local,dest=${fixture}/cache" \
  --load --tag g6-cache-fixture:cold \
  "${fixture}" >/dev/null

cold_id="$(docker image inspect --format '{{.Id}}' g6-cache-fixture:cold)"
cold_label="$(docker image inspect \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
  g6-cache-fixture:cold)"
[[ "${cold_label}" == 0123456789abcdef0123456789abcdef01234567 ]] || {
  echo "the cold build must carry the exact candidate revision label" >&2
  exit 1
}

# Drop the local tag so the warm --load cannot reuse the cold image bytes.
docker image rm --force g6-cache-fixture:cold >/dev/null

# Warm solve: fresh builder session, cache import only, and no cache
# export — a pure consumer, exactly like the next hosted run.
warm_log="${fixture}/warm.log"
if ! docker buildx build --builder "${builder_warm}" --pull=false \
    --cache-from "type=local,src=${fixture}/cache" \
    --progress plain \
    --load --tag g6-cache-fixture:warm \
    "${fixture}" >"${warm_log}" 2>&1; then
  echo "the warm build must succeed from the exported cache" >&2
  tail -20 "${warm_log}" >&2
  exit 1
fi
grep -q 'CACHED' "${warm_log}" || {
  echo "the warm build must actually reuse cached layers" >&2
  exit 1
}

warm_id="$(docker image inspect --format '{{.Id}}' g6-cache-fixture:warm)"
warm_label="$(docker image inspect \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
  g6-cache-fixture:warm)"
[[ "${warm_id}" == "${cold_id}" ]] || {
  echo "cache hit must produce the identical image ID (cold ${cold_id}, warm ${warm_id})" >&2
  exit 1
}
[[ "${warm_label}" == "${cold_label}" ]] || {
  echo "cache hit must preserve the exact provenance label" >&2
  exit 1
}

# Stale-content guard: changed inputs must never resolve to the cached
# image, even with the same cache import attached.
sed -i.bak 's/cache-fixture-payload/cache-fixture-payload-v2/' "${fixture}/Dockerfile"
rm -f "${fixture}/Dockerfile.bak"
docker buildx build --builder "${builder_warm}" --pull=false \
  --cache-from "type=local,src=${fixture}/cache" \
  --load --tag g6-cache-fixture:changed \
  "${fixture}" >/dev/null
changed_id="$(docker image inspect --format '{{.Id}}' g6-cache-fixture:changed)"
if [[ "${changed_id}" == "${cold_id}" ]]; then
  echo "changed inputs must produce a different image; the cache must not override content" >&2
  exit 1
fi

echo "buildkit cache correctness checks passed"
