#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
ENGINE="${ENGINE:?ENGINE must be mysql or mariadb}"
case "${ENGINE}" in
  mysql) IMAGE='mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6'; CLIENT=mysql ;;
  mariadb) IMAGE='mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc'; CLIENT=mariadb ;;
  *) echo 'ENGINE must be mysql or mariadb' >&2; exit 2 ;;
esac
NAME="ocservia-pr02-${ENGINE}-$$"
TLS_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-pr02-tls-XXXXXX")"
TLS_DIR="${TLS_ROOT}/certs"
mkdir "${TLS_DIR}"
cleanup() { docker rm -fv "${NAME}" >/dev/null 2>&1 || true; rm -rf "${TLS_ROOT}"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=ocservia-pr02-test' \
  -addext 'subjectAltName=IP:127.0.0.1' -keyout "${TLS_DIR}/server-key.pem" \
  -out "${TLS_DIR}/server-cert.pem" >/dev/null 2>&1
# The enclosing temporary directory is private on the host. The container's
# unprivileged database user must be able to read its isolated test key.
chmod 755 "${TLS_DIR}"
chmod 644 "${TLS_DIR}"/*.pem
# Ephemeral test credentials only, never deployment defaults. Loopback binding
# is required even in CI; the port is allocated by Docker to avoid collisions.
docker run -d --name "${NAME}" -p 127.0.0.1::3306 \
  -v "${TLS_DIR}:/tls:ro" \
  -e MYSQL_ROOT_PASSWORD=pr02-isolated-test-root -e MYSQL_DATABASE=ocservia \
  -e MARIADB_ROOT_PASSWORD=pr02-isolated-test-root -e MARIADB_DATABASE=ocservia \
  "${IMAGE}" --log-bin-trust-function-creators=1 \
  --ssl-ca=/tls/server-cert.pem --ssl-cert=/tls/server-cert.pem --ssl-key=/tls/server-key.pem >/dev/null
ready=false
for ((i=0; i<90; i++)); do
  # The image initializes through a temporary socket-only server. Wait for
  # the final TCP listener, not that server which is about to shut down.
  if docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
[[ "${ready}" == true ]] || { echo 'database did not become ready' >&2; exit 1; }
docker exec -i -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot <<'SQL'
CREATE USER 'ocservia_owner'@'%' IDENTIFIED BY 'pr02-owner-test-only';
CREATE USER 'ocservia_app'@'%' IDENTIFIED BY 'pr02-runtime-test-only';
CREATE USER 'ocservia_maintenance'@'%' IDENTIFIED BY 'pr02-maintenance-test-only';
SQL
PORT="$(docker port "${NAME}" 3306/tcp | sed 's/127.0.0.1://')"
export PR02_ENGINE="${ENGINE}"
export PR02_TLS_CA_FILE="${TLS_DIR}/server-cert.pem"
export PR02_DSN="root:pr02-isolated-test-root@tcp(127.0.0.1:${PORT})/ocservia?tls=false"
(cd "${ROOT}/control-plane" && go test -count=1 -race -timeout=20m -v ./internal/database/mysql)
# A schema-only tool must never unlock Controller production or business startup.
(cd "${ROOT}/control-plane" && go test -count=1 ./internal/platform/config ./cmd/ocserv-db-foundation)
