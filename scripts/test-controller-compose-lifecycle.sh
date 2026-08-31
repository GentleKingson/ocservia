#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "Controller Compose lifecycle tests require Docker" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || {
  echo "Controller Compose lifecycle tests require jq" >&2
  exit 2
}
docker compose version >/dev/null

fixture="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-controller-compose-test.XXXXXX")"
compose_file="${fixture}/compose.yaml"
project_a="ocservia-controller-lifecycle-a-$$"
project_b="ocservia-controller-lifecycle-b-$$"
volume_a="${project_a}_data"
volume_b="${project_b}_data"
image="${CONTROLLER_LIFECYCLE_TEST_IMAGE:-docker.io/library/alpine:3.22}"
artifact_dir="${ARTIFACT_DIR:-}"

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  docker compose --project-name "${project_a}" --file "${compose_file}" down --volumes --remove-orphans >/dev/null 2>&1
  docker compose --project-name "${project_b}" --file "${compose_file}" down --volumes --remove-orphans >/dev/null 2>&1
  docker volume rm -f "${volume_a}" "${volume_b}" >/dev/null 2>&1
  rm -rf -- "${fixture}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

mkdir -p "${fixture}"
if [[ -n "${artifact_dir}" ]]; then
  mkdir -p "${artifact_dir}"
  exec > >(tee "${artifact_dir}/controller-compose-lifecycle.log") 2>&1
fi

cat >"${compose_file}" <<EOF
services:
  healthy:
    image: ${image}
    command: ["sh", "-c", "printf '%s\\n' lifecycle > /data/sentinel; sleep 3; touch /tmp/ready; sleep 30"]
    volumes:
      - data:/data
    healthcheck:
      test: ["CMD-SHELL", "test -f /tmp/ready"]
      interval: 1s
      timeout: 1s
      retries: 10
volumes:
  data:
EOF

compose() {
  local project="$1"
  shift
  docker compose --project-name "${project}" --file "${compose_file}" "$@"
}

assert_healthy() {
  local project="$1" services
  services="$(compose "${project}" ps --format json healthy)"
  jq -s -e '
    flatten | any(.[]; .Service == "healthy" and .State == "running" and .Health == "healthy")
  ' <<<"${services}" >/dev/null
}

assert_volume_sentinel() {
  local volume="$1"
  docker run --rm --mount "type=volume,source=${volume},target=/data,readonly" \
    "${image}" sh -c 'test "$(cat /data/sentinel)" = lifecycle'
}

assert_project_removed() {
  local project="$1"
  [[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=${project}")" ]] || {
    echo "Compose down left containers for ${project}" >&2
    return 1
  }
  ! docker network inspect "${project}_default" >/dev/null 2>&1
}

compose "${project_a}" config --quiet
compose "${project_b}" config --quiet

start_time="$(date +%s)"
compose "${project_a}" up -d --wait --wait-timeout 15
elapsed=$(( $(date +%s) - start_time ))
(( elapsed >= 2 )) || {
  echo "docker compose up --wait returned before the required health delay" >&2
  exit 1
}
assert_healthy "${project_a}"
echo "up --wait waited for the required healthy service"

compose "${project_b}" up -d --wait --wait-timeout 15
assert_healthy "${project_b}"
assert_volume_sentinel "${volume_a}"
assert_volume_sentinel "${volume_b}"

compose "${project_a}" down --remove-orphans
assert_project_removed "${project_a}"
docker volume inspect "${volume_a}" >/dev/null
assert_volume_sentinel "${volume_a}"
assert_healthy "${project_b}"
docker volume inspect "${volume_b}" >/dev/null
assert_volume_sentinel "${volume_b}"
echo "default down removed containers and networks while retaining named volumes"

compose "${project_b}" down --volumes --remove-orphans
assert_project_removed "${project_b}"
if docker volume inspect "${volume_b}" >/dev/null 2>&1; then
  echo "down --volumes retained the project named volume" >&2
  exit 1
fi
docker volume inspect "${volume_a}" >/dev/null
echo "down --volumes removed only the selected Compose project's named volume"

echo "Controller Docker Compose lifecycle tests passed"
