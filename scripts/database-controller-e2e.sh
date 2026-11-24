#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Legacy positional arguments are checked only here.
(($# <= 2)) || { echo 'usage: database-controller-e2e.sh [postgres [all]]' >&2; exit 2; }
case "${1:-postgres}" in postgres) ;; *) echo 'expected postgres; MySQL is retired' >&2; exit 2 ;; esac
case "${2:-all}" in all) ;; *) echo 'expected all role mode; split roles are retired' >&2; exit 2 ;; esac
NAME="ocservia-database-controller-postgres-$(date +%s)-$$"
ARTIFACT_DIR="${ARTIFACT_DIR:-${ROOT}/artifacts/${NAME}}"
mkdir -p "${ARTIFACT_DIR}" "${ROOT}/.cache/go-build" "${ROOT}/.cache/go-mod"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if docker container inspect "${NAME}" >/dev/null 2>&1; then
    docker logs "${NAME}" >"${ARTIFACT_DIR}/database.log" 2>&1 || status=1
  fi
  for container in "${NAME}-workflow" "${NAME}"; do
    if docker container inspect "${container}" >/dev/null 2>&1; then
      docker rm -fv "${container}" >/dev/null || status=1
    fi
  done
  if docker network inspect "${NAME}" >/dev/null 2>&1; then
    docker network rm "${NAME}" >/dev/null || status=1
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Keep the production Rust feature partitions and frozen source inputs. The
# final test image adds real Ocserv/OpenSSL, not a transport or Agent stub.
WORKFLOW_IMAGE=ocservia-database-e2e-workflow:e2e
if [[ -n "${RELEASE_WORKFLOW_IMAGE:-}" ]]; then
  : "${SINGLE_AGENT_DEB:?}" "${SINGLE_EXPECTED_AGENT_VERSION:?}"
  [[ "${RELEASE_WORKFLOW_IMAGE}" =~ ^sha256:[0-9a-f]{64}$ ]]
  WORKFLOW_IMAGE="${RELEASE_WORKFLOW_IMAGE}"
else
docker build -f "${ROOT}/rust/test-runtime.Dockerfile" --target agent-runtime -t ocservia-database-e2e-agent:e2e "${ROOT}" >"${ARTIFACT_DIR}/agent-build.log" 2>&1
docker build -f "${ROOT}/rust/test-runtime.Dockerfile" --target transportd-runtime -t ocservia-database-e2e-transport:e2e "${ROOT}" >"${ARTIFACT_DIR}/transport-build.log" 2>&1
docker build -f "${ROOT}/deploy/production/relay.Dockerfile" -t ocservia-database-e2e-relay:e2e "${ROOT}" >"${ARTIFACT_DIR}/relay-build.log" 2>&1
docker build -f "${ROOT}/deploy/database-e2e/Dockerfile" -t ocservia-database-e2e-workflow:e2e "${ROOT}" >"${ARTIFACT_DIR}/workflow-build.log" 2>&1
docker image inspect ocservia-database-e2e-agent:e2e ocservia-database-e2e-transport:e2e ocservia-database-e2e-relay:e2e ocservia-database-e2e-workflow:e2e --format '{{.Id}} {{json .RepoTags}}' >"${ARTIFACT_DIR}/images.txt"
fi
docker image inspect "${WORKFLOW_IMAGE}" >"${ARTIFACT_DIR}/workflow-image.json"

# Nothing is published on the host, and runtime processes cannot use public
# discovery as an accidental substitute for the one dedicated TLS Relay.
docker network create --internal "${NAME}" >/dev/null
ENVIRONMENT=(-e OCSERVIA_DATABASE_TEST_CONTROLLER_E2E=1 -e OCSERV_E2E_ARTIFACT_DIR=/artifacts)
MOUNTS=()
COMMAND=(go test -buildvcs=false -count=1 -race -timeout=15m -v ./internal/platform/app -run '^TestControllerTransportBackendE2E$')
POSTGRES_IMAGE=postgres:18-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af
if [[ -n "${RELEASE_WORKFLOW_IMAGE:-}" ]]; then
  POSTGRES_IMAGE=postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af
  MOUNTS=(-v "$(dirname "${SINGLE_AGENT_DEB}"):/published:ro")
  ENVIRONMENT+=(-e GOWORK=off -e "PUBLISHED_AGENT_DEB=/published/$(basename "${SINGLE_AGENT_DEB}")" \
    -e "PUBLISHED_AGENT_VERSION=${SINGLE_EXPECTED_AGENT_VERSION}")
  # shellcheck disable=SC2016 # Expanded only inside the disposable container.
  COMMAND=(bash -euo pipefail -c '
    package="$(mktemp -d)"
    dpkg-deb -x "$PUBLISHED_AGENT_DEB" "$package"
    for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
      install -o root -g root -m 0755 "$package/usr/libexec/ocservia/$binary" "/usr/local/bin/$binary"
      "/usr/local/bin/$binary" --version
    done > /artifacts/published-binaries.txt
    dpkg-query -W ocserv openssl libc6
    /usr/local/bin/iroh-relay --version
    exec "$@"
  ' -- "${COMMAND[@]}")
fi
docker run -d --name "${NAME}" --network "${NAME}" \
  -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only -e POSTGRES_DB=ocservia \
  "${POSTGRES_IMAGE}" >/dev/null
for _ in {1..90}; do
  if docker exec "${NAME}" pg_isready -h 127.0.0.1 -U ocservia_owner -d ocservia >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "${NAME}" pg_isready -h 127.0.0.1 -U ocservia_owner -d ocservia >/dev/null
docker exec "${NAME}" psql -U ocservia_owner -d ocservia -v ON_ERROR_STOP=1 -c "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
docker image inspect "${POSTGRES_IMAGE}" >"${ARTIFACT_DIR}/database-image.json"
docker exec "${NAME}" psql -U ocservia_owner -d ocservia -Atc 'SELECT version()' >"${ARTIFACT_DIR}/database-version.txt"
ENVIRONMENT+=(-e 'OCSERV_TEST_OWNER_DATABASE_URL=postgres://ocservia_owner:test-owner-only@127.0.0.1:5432/ocservia?sslmode=disable' \
  -e 'OCSERV_TEST_DATABASE_URL=postgres://ocservia_app:test-runtime-only@127.0.0.1:5432/ocservia?sslmode=disable')

docker run --rm --name "${NAME}-workflow" --network "container:${NAME}" \
  --cap-drop=ALL --cap-add=CHOWN --cap-add=FOWNER --cap-add=SETUID --cap-add=SETGID --cap-add=DAC_OVERRIDE --cap-add=KILL --cap-add=NET_ADMIN \
  -v "${ROOT}:/workspace:ro" -v "${ROOT}/.cache/go-build:/go-cache" -v "${ROOT}/.cache/go-mod:/go-mod:ro" \
  -v "${ARTIFACT_DIR}:/artifacts" -e GOCACHE=/go-cache -e GOMODCACHE=/go-mod -e GOPROXY=off -e GOTOOLCHAIN=local \
  "${MOUNTS[@]}" "${ENVIRONMENT[@]}" "${WORKFLOW_IMAGE}" "${COMMAND[@]}" \
  >"${ARTIFACT_DIR}/workflow.log" 2>&1
grep -q '^--- PASS: TestControllerTransportBackendE2E ' "${ARTIFACT_DIR}/workflow.log"
printf 'Controller PostgreSQL E2E passed; evidence: %s\n' "${ARTIFACT_DIR}"
