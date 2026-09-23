#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${HOME}/ocservia-database-deploy.XXXXXX")"
trap 'rm -rf -- "${work}"' EXIT
mkdir -m 0700 "${work}/secrets" "${work}/backups"
for name in tls.crt tls.key postgres-owner-password postgres-app-password postgres-backup-password \
  postgres.pgpass database-owner-url database-app-url database-backup.cnf database-ca.pem \
  session-key audit-checkpoint-key certificate-signer-token; do
  printf 'test-only\n' >"${work}/secrets/${name}"
  chmod 0444 "${work}/secrets/${name}"
done
for name in audit-event-key controller-command-signing-key.pem relay-access-token controller-iroh.key; do
  printf 'test-only\n' >"${work}/secrets/${name}"
  chmod 0400 "${work}/secrets/${name}"
done
owner=(); ((EUID == 0)) || owner=(sudo)
"${owner[@]}" install -o 0 -g 65532 -m 440 /dev/null "${work}/secrets/controller-command-verification-key.pem"
"${owner[@]}" chown 65534:65532 "${work}/secrets/audit-event-key" "${work}/secrets/controller-command-signing-key.pem"
"${owner[@]}" chown 65532:65532 "${work}/secrets/relay-access-token" "${work}/secrets/controller-iroh.key"
"${owner[@]}" chown 999:999 "${work}/backups"
"${owner[@]}" chmod 0700 "${work}/backups"

image="example.invalid/test@sha256:$(printf '%064d' 0)"
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backups"
export OCSERV_GATEWAY_IMAGE="${image}" OCSERV_CONTROL_IMAGE="${image}" OCSERV_TRANSPORT_IMAGE="${image}"
export OCSERV_BACKUP_IMAGE="${image}" OCSERV_POSTGRES_IMAGE="${image}" OCSERV_OTEL_IMAGE="${image}"
export OCSERV_DATABASE_BACKUP_IMAGE="${image}" OCSERV_LOCAL_AUTH_ENABLED=true
export OCSERV_PUBLIC_HOST=controller.example.test OCSERV_AUDIT_EVENT_KEY_ID=test
export OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '%064d' 1)"
export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test
export OCSERV_RELAY_URL_A=https://relay-a.example.test OCSERV_RELAY_URL_B=https://relay-b.example.test

"${ROOT}/deploy/production/compose.sh" config --format json >"${work}/postgres.json"
jq -e '
  (.services | has("postgres")) and
  .services.migrate.depends_on.postgres.condition == "service_healthy" and
  .services.backup.depends_on.postgres.condition == "service_healthy" and
  (.services.postgres.healthcheck.test | length > 0) and
  (.services.backup.healthcheck.test | length > 0) and
  .services.backup.healthcheck.start_period == "1m0s" and
  .services.backup.healthcheck.interval == "5m0s" and
  .services.backup.healthcheck.timeout == "5s" and
  .services.backup.healthcheck.retries == 2 and
  (.services.backup.healthcheck | has("start_interval") | not) and
  (.volumes | has("postgres-data")) and
  ([.services["control-plane"].secrets[].source] | index("database_owner_url") | not)
' "${work}/postgres.json" >/dev/null

OCSERV_DATABASE_DEPLOYMENT=external OCSERV_DATABASE_BACKUP_HOST=postgres.example.test \
  "${ROOT}/deploy/production/compose.sh" config --format json >"${work}/external-postgres.json"
jq -e '
  (.services | has("postgres") | not) and
  .services.backup.depends_on.migrate.condition == "service_completed_successfully" and
  .services.backup.environment.PGHOST == "postgres.example.test" and
  .services.backup.environment.PGSSLMODE == "verify-full" and
  .services.backup.environment.POSTGRES_SERVER_MAJOR == "17" and
  .services.migrate.environment.OCSERV_DATABASE_TLS_CA_FILE == "/run/secrets/database_ca" and
  .services["control-plane"].environment.OCSERV_DATABASE_TLS_CA_FILE == "/run/secrets/database_ca" and
  (.services.backup.healthcheck.test | length > 0) and
  ([.services.backup.secrets[].source] | index("postgres_pgpass") != null) and
  ([.services.backup.secrets[].source] | index("database_ca") != null) and
  (.services.migrate.networks | has("database-egress")) and
  (.services["control-plane"].networks | has("database-egress")) and
  (.services.backup.networks | has("database-egress")) and
  ((.networks["database-egress"].internal // false) == false) and
  ([.services.backup.volumes[].target] | index("/var/lib/ocservia-backup") != null) and
  .services.backup.deploy.resources.limits.memory == "536870912"
' "${work}/external-postgres.json" >/dev/null

mv "${work}/secrets/database-ca.pem" "${work}/database-ca.pem"
if OCSERV_DATABASE_DEPLOYMENT=external OCSERV_DATABASE_BACKUP_HOST=postgres.example.test \
  "${ROOT}/deploy/production/compose.sh" config --quiet >"${work}/missing-postgres-ca.log" 2>&1; then
  echo "external PostgreSQL without a CA unexpectedly accepted" >&2
  exit 1
fi
mv "${work}/database-ca.pem" "${work}/secrets/database-ca.pem"

for backend in mysql mariadb; do
  OCSERV_DATABASE_BACKEND="${backend}" OCSERV_DATABASE_DEPLOYMENT=external \
    "${ROOT}/deploy/production/compose.sh" config --format json >"${work}/external-${backend}.json"
  jq -e --arg backend "${backend}" '
    (.services | has("postgres") | not) and
    .services.migrate.environment.OCSERV_DATABASE_BACKEND == $backend and
    .services["control-plane"].environment.OCSERV_DATABASE_BACKEND == $backend and
    .services.backup.environment.DATABASE_BACKEND == $backend and
    (.services.backup.healthcheck.test | length > 0) and
    ([.services["control-plane"].secrets[].source] | index("database_owner_url") | not) and
    ([.services["control-plane"].secrets[].source] | index("database_ca") != null) and
    ([.services.migrate.secrets[].source] | index("database_owner_url") != null) and
    ([.services.backup.secrets[].source] | index("database_backup_config") != null) and
    ([.services.backup.secrets[].source] | index("database_ca") != null) and
    (.services.migrate.networks | has("database-egress")) and
    (.services["control-plane"].networks | has("database-egress")) and
    (.services.backup.networks | has("database-egress")) and
    ((.networks["database-egress"].internal // false) == false) and
    ([.services.backup.volumes[].target] | index("/var/lib/ocservia-backup") != null) and
    .services.backup.deploy.resources.limits.memory == "536870912"
  ' "${work}/external-${backend}.json" >/dev/null
done

if OCSERV_DATABASE_BACKEND=mysql OCSERV_DATABASE_DEPLOYMENT=bundled \
  "${ROOT}/deploy/production/compose.sh" config --quiet >"${work}/rejected.log" 2>&1; then
  echo "bundled MySQL unexpectedly accepted" >&2
  exit 1
fi

for backend in mysql mariadb; do
  OCSERV_DATABASE_BACKEND="${backend}" "${ROOT}/deploy/compose/compose.sh" config --format json >"${work}/dev-${backend}.json"
  jq -e --arg backend "${backend}" '
    (.services | has("postgres") | not) and .services.database != null and
    .services.migrate.depends_on.database.condition == "service_healthy" and
    .services["control-plane"].depends_on.migrate.condition == "service_completed_successfully" and
    .services["control-plane"].environment.OCSERV_DATABASE_BACKEND == $backend and
    (.services.database.healthcheck.test | length > 0) and
    (.volumes | has("database-data"))
  ' "${work}/dev-${backend}.json" >/dev/null
done

for rendered in postgres external-postgres external-mysql external-mariadb; do
  jq -e '
    (.services.transportd.networks | keys) == ["application", "observability", "relay-egress"] and
    (.networks["relay-egress"].internal // false) == false and
    .networks.application.internal == true and .networks.observability.internal == true and
    [.services | to_entries[] | select(.value.networks | has("relay-egress")) | .key] == ["transportd"]
  ' "${work}/${rendered}.json" >/dev/null
done
echo "database deployment configuration checks passed"
