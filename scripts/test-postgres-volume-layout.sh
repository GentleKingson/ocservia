#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${POSTGRES_IMAGE:-postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af}"
name="ocservia-pg18-layout-$$"
volume="${name}-data"
legacy="${name}-legacy"
cleanup() {
  docker rm -f "${name}" >/dev/null 2>&1 || true
  docker volume rm "${volume}" "${legacy}" >/dev/null
}
trap cleanup EXIT
docker volume create "${volume}" >/dev/null
docker volume create "${legacy}" >/dev/null
start() {
  docker run -d --name "${name}" --network none --read-only --user 999:999 \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --tmpfs /run/postgresql:uid=999,gid=999,mode=0770 --tmpfs /tmp:uid=999,gid=999,mode=0700 \
    -e PGDATA=/var/lib/postgresql/18/docker -e POSTGRES_PASSWORD=test-only-layout \
    -v "$1:/var/lib/postgresql" \
    -v "${ROOT}/deploy/lib/postgres-entrypoint.sh:/usr/local/bin/ocservia-postgres-entrypoint:ro" \
    --entrypoint /bin/bash "${image}" /usr/local/bin/ocservia-postgres-entrypoint postgres >/dev/null
}
ready() {
  for _ in $(seq 1 60); do
    if docker exec "${name}" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then return; fi
    [[ "$(docker inspect -f '{{.State.Running}}' "${name}")" == true ]] || break
    sleep 1
  done
  docker logs "${name}" >&2
  return 1
}
start "${volume}"
ready
docker exec "${name}" psql -U postgres -v ON_ERROR_STOP=1 -c "CREATE TABLE layout_marker(value text); INSERT INTO layout_marker VALUES ('preserved');" >/dev/null
[[ "$(docker exec "${name}" psql -U postgres -Atc 'SHOW data_directory')" == /var/lib/postgresql/18/docker ]]
docker stop "${name}" >/dev/null
docker rm "${name}" >/dev/null
start "${volume}"
ready
[[ "$(docker exec "${name}" psql -U postgres -Atc 'SELECT value FROM layout_marker')" == preserved ]]
docker stop "${name}" >/dev/null
docker rm "${name}" >/dev/null

# Model the exact reused PG17 volume root, without touching a user's volume.
docker run --rm --network none -v "${legacy}:/legacy" --entrypoint bash "${image}" \
  -ceu 'printf "17\n" >/legacy/PG_VERSION; printf "preserve-me\n" >/legacy/marker'
start "${legacy}"
[[ "$(docker wait "${name}")" != 0 ]]
docker logs "${name}" 2>&1 | grep -Fq 'legacy PostgreSQL data detected'
docker run --rm --network none -v "${legacy}:/legacy:ro" --entrypoint bash "${image}" \
  -ceu 'test "$(cat /legacy/PG_VERSION)" = 17; test "$(cat /legacy/marker)" = preserve-me; test ! -e /legacy/18'
echo 'PostgreSQL 18 volume persistence and PG17 volume rejection passed'
