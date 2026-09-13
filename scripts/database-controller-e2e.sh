#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGINE="${1:-postgres}"
case "${ENGINE}" in mysql|mariadb|postgres) ;; *) echo 'expected postgres, mysql or mariadb' >&2; exit 2 ;; esac
ROLE_MODE="${2:-all}"
case "${ROLE_MODE}" in all|split) ;; *) echo 'expected all or split role mode' >&2; exit 2 ;; esac
NAME="pr07-controller-${ENGINE}-${ROLE_MODE}-$(date +%s)-$$"
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
docker build -f "${ROOT}/rust/g6-runtime.Dockerfile" --target g6-agent-runtime -t ocservia-pr02-agent:e2e "${ROOT}" >"${ARTIFACT_DIR}/agent-build.log" 2>&1
docker build -f "${ROOT}/rust/g6-runtime.Dockerfile" --target transportd-runtime -t ocservia-pr02-transport:e2e "${ROOT}" >"${ARTIFACT_DIR}/transport-build.log" 2>&1
docker build -f "${ROOT}/deploy/production/relay.Dockerfile" -t ocservia-pr02-relay:e2e "${ROOT}" >"${ARTIFACT_DIR}/relay-build.log" 2>&1
docker build -f "${ROOT}/deploy/database-e2e/Dockerfile" -t ocservia-pr02-workflow:e2e "${ROOT}" >"${ARTIFACT_DIR}/workflow-build.log" 2>&1
docker image inspect ocservia-pr02-agent:e2e ocservia-pr02-transport:e2e ocservia-pr02-relay:e2e ocservia-pr02-workflow:e2e --format '{{.Id}} {{json .RepoTags}}' >"${ARTIFACT_DIR}/images.txt"

# Nothing is published on the host, and runtime processes cannot use public
# discovery as an accidental substitute for the two dedicated TLS relays.
docker network create --internal "${NAME}" >/dev/null
ENVIRONMENT=(-e PR02_CONTROLLER_E2E=1 -e "PR07_CONTROLLER_ROLE_MODE=${ROLE_MODE}" -e OCSERV_E2E_ARTIFACT_DIR=/artifacts)
if [[ "${ENGINE}" == postgres ]]; then
  docker run -d --name "${NAME}" --network "${NAME}" \
    -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only -e POSTGRES_DB=ocservia \
    postgres:18-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af >/dev/null
  for _ in {1..90}; do
    if docker exec "${NAME}" pg_isready -h 127.0.0.1 -U ocservia_owner -d ocservia >/dev/null 2>&1; then break; fi
    sleep 1
  done
  docker exec "${NAME}" pg_isready -h 127.0.0.1 -U ocservia_owner -d ocservia >/dev/null
  docker exec "${NAME}" psql -U ocservia_owner -d ocservia -v ON_ERROR_STOP=1 -c "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
  ENVIRONMENT+=(-e 'OCSERV_TEST_OWNER_DATABASE_URL=postgres://ocservia_owner:test-owner-only@127.0.0.1:5432/ocservia?sslmode=disable' \
    -e 'OCSERV_TEST_DATABASE_URL=postgres://ocservia_app:test-runtime-only@127.0.0.1:5432/ocservia?sslmode=disable')
else
  IMAGE=mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6
  CLIENT=mysql
  if [[ "${ENGINE}" == mariadb ]]; then IMAGE=mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc; CLIENT=mariadb; fi
  docker run -d --name "${NAME}" --network "${NAME}" \
    -e MYSQL_ROOT_PASSWORD=pr02-isolated-test-root -e MYSQL_DATABASE=ocservia \
    -e MARIADB_ROOT_PASSWORD=pr02-isolated-test-root -e MARIADB_DATABASE=ocservia \
    "${IMAGE}" --log-bin-trust-function-creators=1 >/dev/null
  for _ in {1..90}; do
    if docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null 2>&1; then break; fi
    sleep 1
  done
  docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null
  docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -e "CREATE USER 'ocservia_owner'@'%' IDENTIFIED BY 'pr02-owner-test-only'; CREATE USER 'ocservia_app'@'%' IDENTIFIED BY 'pr02-runtime-test-only'; CREATE USER 'ocservia_maintenance'@'%' IDENTIFIED BY 'pr02-maintenance-test-only';"
  ENVIRONMENT+=(-e "PR02_ENGINE=${ENGINE}" -e 'PR02_DSN=root:pr02-isolated-test-root@tcp(127.0.0.1:3306)/ocservia?tls=false')
fi

docker run --rm --name "${NAME}-workflow" --network "container:${NAME}" \
  --cap-drop=ALL --cap-add=CHOWN --cap-add=SETUID --cap-add=SETGID --cap-add=DAC_OVERRIDE --cap-add=KILL --cap-add=NET_ADMIN \
  -v "${ROOT}:/workspace:ro" -v "${ROOT}/.cache/go-build:/go-cache" -v "${ROOT}/.cache/go-mod:/go-mod:ro" \
  -v "${ARTIFACT_DIR}:/artifacts" -e GOCACHE=/go-cache -e GOMODCACHE=/go-mod -e GOPROXY=off -e GOTOOLCHAIN=local \
  "${ENVIRONMENT[@]}" ocservia-pr02-workflow:e2e \
  go test -buildvcs=false -count=1 -race -timeout=15m -v ./internal/platform/app -run '^TestControllerTransportBackendE2E$' \
  >"${ARTIFACT_DIR}/workflow.log" 2>&1
grep -q '^--- PASS: TestControllerTransportBackendE2E ' "${ARTIFACT_DIR}/workflow.log"
printf 'Controller %s E2E passed; evidence: %s\n' "${ENGINE}" "${ARTIFACT_DIR}"
