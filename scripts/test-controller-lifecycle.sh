#!/usr/bin/env bash
# Exercise the release-history transaction against a disposable Compose project.
# The release-check job exercises the full production stack and public probes.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${HOME}/.ocservia-lifecycle.XXXXXX")"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_CONTROLLER_COMPOSE_SH="${work}/compose.sh"
export OCSERV_CONTROLLER_SMOKE_SH="${work}/health.sh"
project="${work##*.}"
export COMPOSE_PROJECT_NAME="ocservia-history-${project,,}" COMPOSE_FILE="${work}/compose.yaml"
trap 'docker compose down --remove-orphans; rm -rf -- "${work}"' EXIT
# Each release has the same immutable test image. The release label identifies
# the container actually replaced by Compose during activation and rollback.
export OCSERV_TEST_IMAGE="python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df"
cat >"${COMPOSE_FILE}" <<'YAML'
x-service: &service
    image: ${OCSERV_TEST_IMAGE}
    command: [sleep, infinity]
    init: true
    healthcheck:
      test: [CMD, python, -V]
      interval: 1s
      timeout: 1s
      retries: 5
services:
  backup: *service
  postgres: *service
  controller:
    <<: *service
    labels:
      release: ${OCSERV_CONTROL_IMAGE:-}
YAML
cat >"${OCSERV_CONTROLLER_COMPOSE_SH}" <<'SH'
#!/usr/bin/env bash
exec docker compose "$@"
SH
cat >"${OCSERV_CONTROLLER_SMOKE_SH}" <<'SH'
#!/usr/bin/env bash
set -eu
id="$(docker compose ps -q controller)"
[[ "$(docker inspect -f '{{.State.Health.Status}}' "${id}")" == healthy ]]
[[ "$(docker inspect -f '{{index .Config.Labels "release"}}' "${id}")" == "$(jq -er '.images.control' "$2")" ]]
[[ ! -e "${OCSERV_CONTROLLER_STATE_ROOT}/fail-smoke" ]]
SH
chmod 0700 "${OCSERV_CONTROLLER_COMPOSE_SH}" "${OCSERV_CONTROLLER_SMOKE_SH}"
commit="$(git -C "${ROOT}" rev-parse HEAD)"
arch="$(docker version --format '{{.Server.Arch}}')"
for version in 2.0.0 2.0.1; do
  args=()
  for role in gateway control transport backup postgres otel; do
    args+=(--image "${role}=test.invalid/${role}-${version}@${OCSERV_TEST_IMAGE#*@}")
  done
  python3 "${ROOT}/scripts/controller_release_manifest.py" \
    --output "${work}/${version}.json" --release-version "${version}" --release-tag "v${version}" \
    --source-commit "${commit}" --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${arch}" "${args[@]}"
done
ctl="${ROOT}/deploy/production/controller.sh"
history="${OCSERV_CONTROLLER_STATE_ROOT}/release-history.json"
"${ctl}" install --release-file "${work}/2.0.0.json"
jq -e '.current.release_version == "2.0.0" and .previous == null' "${history}" >/dev/null
cp "${history}" "${work}/confirmed"
touch "${OCSERV_CONTROLLER_STATE_ROOT}/fail-smoke"
if "${ctl}" upgrade --release-file "${work}/2.0.1.json"; then exit 1; fi
cmp "${history}" "${work}/confirmed"
rm "${OCSERV_CONTROLLER_STATE_ROOT}/fail-smoke"
"${ctl}" upgrade --release-file "${work}/2.0.1.json"
jq -e '.current.release_version == "2.0.1" and .previous.release_version == "2.0.0"' "${history}" >/dev/null
jq -S . "${work}/2.0.1.json" >"${work}/reordered.json"
"${ctl}" upgrade --release-file "${work}/reordered.json"
"${ctl}" rollback
jq -e '.current.release_version == "2.0.0" and .previous.release_version == "2.0.1"' "${history}" >/dev/null
"${ctl}" health
"${ctl}" uninstall --purge-data
[[ ! -e "${history}" ]]
