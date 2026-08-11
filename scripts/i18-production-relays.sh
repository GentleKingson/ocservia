#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi

work="${RUNNER_TEMP:-/tmp}/ocservia-i18-production-${RUN_ID}"
runtime_transport_image="ocservia-i18-transport-runtime:${RUN_ID}"
runtime_control_image="ocservia-i18-control-runtime:${RUN_ID}"
trust_volume="ocservia-i18-trust-${RUN_ID}"
cleanup() {
  local status=$?
  docker volume rm -f "${trust_volume}" >/dev/null 2>&1 || true
  docker image rm -f "${runtime_transport_image}" "${runtime_control_image}" >/dev/null 2>&1 || true
  sudo chown -R "$(id -u):$(id -g)" "${work}" >/dev/null 2>&1 || status=1
  rm -rf -- "${work}"
  if docker volume inspect "${trust_volume}" >/dev/null 2>&1; then
    echo "scoped trust volume cleanup failed" >&2
    status=1
  fi
  for image in "${runtime_transport_image}" "${runtime_control_image}"; do
    if docker image inspect "${image}" >/dev/null 2>&1; then
      echo "scoped runtime image cleanup failed: ${image}" >&2
      status=1
    fi
  done
  exit "${status}"
}
trap cleanup EXIT INT TERM
mkdir -p "${work}/secrets" "${work}/relay-secrets" "${work}/backups" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/secrets" "${work}/relay-secrets" "${work}/backups"
for secret in tls.crt tls.key postgres-owner-password postgres-app-password postgres.pgpass \
  postgres-backup-password database-owner-url database-app-url oidc-client-secret session-key audit-checkpoint-key \
  certificate-signer-token relay-access-token controller-iroh.key otel-client.crt otel-client.key otel-ca.crt; do
  printf 'test-only\n' >"${work}/secrets/${secret}"
done
openssl genpkey -algorithm ED25519 -out "${work}/secrets/controller-command-signing-key.pem" >/dev/null 2>&1
for secret in relay-access-token tls.crt tls.key; do
  printf 'test-only\n' >"${work}/relay-secrets/${secret}"
done
general_secrets=(tls.crt tls.key postgres-owner-password postgres-app-password postgres-backup-password \
  postgres.pgpass database-owner-url database-app-url oidc-client-secret session-key \
  audit-checkpoint-key certificate-signer-token otel-client.crt otel-client.key otel-ca.crt)
chmod 0444 "${general_secrets[@]/#/${work}\/secrets/}"
chmod 0400 "${work}/secrets/controller-command-signing-key.pem"
chmod 0444 "${work}/relay-secrets/tls.crt" "${work}/relay-secrets/tls.key"
chmod 0400 "${work}/secrets/relay-access-token" "${work}/secrets/controller-iroh.key" \
  "${work}/relay-secrets/relay-access-token"
sudo chown 65534:65532 "${work}/secrets/controller-command-signing-key.pem"
sudo chown 65532:65532 "${work}/secrets/relay-access-token" "${work}/secrets/controller-iroh.key" \
  "${work}/relay-secrets/relay-access-token"

image="example.invalid/ocservia/test@sha256:$(printf '%064d' 0)"
export OCSERV_GATEWAY_IMAGE="${image}" OCSERV_CONTROL_IMAGE="${image}"
export OCSERV_TRANSPORT_IMAGE="${image}" OCSERV_BACKUP_IMAGE="${image}" OCSERV_RELAY_IMAGE="${image}"
export OCSERV_POSTGRES_IMAGE="${image}" OCSERV_OTEL_IMAGE="${image}"
export OCSERV_PUBLIC_HOST=ocservia.example.test OCSERV_SECRET_DIR="${work}/secrets"
export OCSERV_BACKUP_DIR="${work}/backups"
export OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=ocservia
export OCSERV_CONTROLLER_ENDPOINT_ID=0000000000000000000000000000000000000000000000000000000000000000
export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test/v1
export OCSERV_OTEL_BACKEND_ENDPOINT=otel.example.test:4317
export OCSERV_RELAY_URL_A=https://relay-a.example.test OCSERV_RELAY_URL_B=https://relay-b.example.test
export OCSERV_RELAY_SECRET_DIR="${work}/relay-secrets"

validate_digest_image() {
  [[ "$1" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]
}
for variable in OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE \
  OCSERV_BACKUP_IMAGE OCSERV_RELAY_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE; do
  if ! validate_digest_image "${!variable}"; then
    echo "${variable} must contain a full sha256 image digest" >&2
    exit 1
  fi
done
if validate_digest_image "example.invalid/ocservia/control:latest"; then
  echo "mutable image tag passed digest validation" >&2
  exit 1
fi
grep -Fq 'host replication ocservia_backup all scram-sha-256' \
  "${ROOT}/deploy/production/postgres-init/001-runtime-role.sh"

"${ROOT}/deploy/production/compose.sh" config --format json \
  >"${ARTIFACT_DIR}/platform-compose.json"
"${ROOT}/deploy/production/relay/compose.sh" config --format json \
  >"${ARTIFACT_DIR}/relay-compose.json"
if OCSERV_CONTROL_IMAGE=example.invalid/ocservia/control:latest \
  "${ROOT}/deploy/production/compose.sh" config >/dev/null 2>&1; then
  echo "production launcher accepted a mutable image tag" >&2
  exit 1
fi
if OCSERV_RELAY_IMAGE=example.invalid/ocservia/relay:latest \
  "${ROOT}/deploy/production/relay/compose.sh" config >/dev/null 2>&1; then
  echo "relay launcher accepted a mutable image tag" >&2
  exit 1
fi
mkdir "${work}/invalid-backup-owner"
chmod 0755 "${work}/invalid-backup-owner"
if OCSERV_BACKUP_DIR="${work}/invalid-backup-owner" \
  "${ROOT}/deploy/production/compose.sh" up --no-start >/dev/null 2>&1; then
  echo "production launcher accepted an unsafe backup directory" >&2
  exit 1
fi

python3 - "${ARTIFACT_DIR}/platform-compose.json" "${ARTIFACT_DIR}/relay-compose.json" <<'PY'
import json
import pathlib
import re
import sys

platform = json.loads(pathlib.Path(sys.argv[1]).read_text())
relay = json.loads(pathlib.Path(sys.argv[2]).read_text())

def hardened(service):
    assert service.get("read_only") is True
    assert service.get("cap_drop") == ["ALL"]
    assert not service.get("cap_add") and service.get("privileged") is not True
    assert "no-new-privileges:true" in service.get("security_opt", [])
    limits = service.get("deploy", {}).get("resources", {}).get("limits", {})
    assert limits.get("pids", 0) > 0 and limits.get("memory")
    nofile = service.get("ulimits", {}).get("nofile", {})
    assert nofile.get("soft", 0) >= 256 and nofile.get("hard", 0) <= 8192
    serialized = json.dumps(service)
    for forbidden in ("docker.sock", "/proc", "/sys"):
        assert forbidden not in serialized

services = platform["services"]
assert set(services) == {"gateway", "postgres", "migrate", "control-plane", "transportd", "otel-collector", "backup"}
for service in services.values():
    hardened(service)
    assert re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", service["image"])
assert services["gateway"].get("ports") and all(not service.get("ports") for name, service in services.items() if name != "gateway")
for name in ("application", "database", "observability"):
    assert platform["networks"][name]["internal"] is True
command = services["transportd"]["command"]
assert command.count("--relay-url") == 2 and "--relay-mode" in command and "custom" in command
urls = [command[index + 1] for index, value in enumerate(command) if value == "--relay-url"]
assert len(set(urls)) == 2 and all(url.startswith("https://") for url in urls)
assert all("n0" not in url and "iroh.link" not in url for url in urls)
assert "/run/secrets/relay_access_token" in command
assert "/run/secrets/controller_iroh_key" in command
assert command[command.index("--control-plane-uid") + 1] == "65534"
assert command[command.index("--control-plane-gid") + 1] == "65532"
transport_secrets = {item["target"]: item for item in services["transportd"]["secrets"]}
for name in ("relay_access_token", "controller_iroh_key"):
    assert transport_secrets[name]["uid"] == "65532"
    assert transport_secrets[name]["gid"] == "65532"
    assert transport_secrets[name]["mode"] == "0400"
assert services["control-plane"]["command"] == ["--role=all"]
assert services["control-plane"]["environment"]["OCSERV_COMMAND_SIGNING_KEY_FILE"] == "/run/secrets/controller_command_signing_key"
assert services["control-plane"]["environment"]["OCSERV_TRANSPORT_UID"] == "65532"
assert services["control-plane"]["environment"]["OCSERV_TRANSPORT_GID"] == "65532"
control_secrets = {item["target"]: item for item in services["control-plane"]["secrets"]}
command_key = control_secrets["controller_command_signing_key"]
assert command_key["uid"] == "65534" and command_key["gid"] == "65532" and command_key["mode"] == "0400"
assert "transportd" not in services["control-plane"].get("depends_on", {})
assert any("uid=999" in item and "gid=999" in item and "mode=0700" in item for item in services["backup"]["tmpfs"])
assert "BACKUP_INTERVAL_SECONDS" in services["backup"]["healthcheck"]["test"][1]

relay_service = relay["services"]["relay"]
hardened(relay_service)
assert re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", relay_service["image"])
published = {(int(item["published"]), item["protocol"]) for item in relay_service["ports"]}
assert published == {(80, "tcp"), (443, "tcp"), (7842, "udp")}
assert relay_service["environment"]["IROH_RELAY_ACCESS_TOKEN_FILE"] == "/run/secrets/relay_access_token"
relay_token = next(item for item in relay_service["secrets"] if item["target"] == "relay_access_token")
assert relay_token["uid"] == "65532" and relay_token["gid"] == "65532" and relay_token["mode"] == "0400"
print("I18 production topology validation passed")
PY

if grep -R -E 'docker compose .*deploy/production/(compose|relay/compose)\.yaml' \
  "${ROOT}/docs/operations"; then
  echo "production documentation bypasses the digest-validating launcher" >&2
  exit 1
fi

docker build --check -f "${ROOT}/control-plane/Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/control-image-check.log"
docker build --check -f "${ROOT}/rust/transportd.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/transport-image-check.log"
docker build --check -f "${ROOT}/deploy/production/gateway.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/gateway-image-check.log"
docker build --check -f "${ROOT}/deploy/production/relay.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/relay-image-check.log"
docker build --target runtime-base -t "${runtime_transport_image}" \
  -f "${ROOT}/rust/transportd.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/transport-runtime-build.log"
docker build --target runtime-base -t "${runtime_control_image}" \
  -f "${ROOT}/control-plane/Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/control-runtime-build.log"
docker volume create "${trust_volume}" >/dev/null
docker run --rm --name "${trust_volume}-init" \
  -v "${trust_volume}:/run/ocserv-trust" --entrypoint /bin/true "${runtime_transport_image}"
docker run --rm --name "${trust_volume}-control" \
  -v "${trust_volume}:/run/ocserv-trust" --entrypoint /bin/sh "${runtime_control_image}" \
  -c 'test "$(stat -c %u:%g:%a /run/ocserv-trust)" = "65534:65532:750" && test -w /run/ocserv-trust && : > /run/ocserv-trust/control-plane.sock'
docker run --rm -v "${work}/secrets/database-app-url:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${runtime_control_image}" -c 'test -r /run/secrets/test && test ! -w /run/secrets/test'
docker run --rm -v "${work}/secrets/controller-command-signing-key.pem:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${runtime_control_image}" \
  -c 'test "$(stat -c %u:%g:%a /run/secrets/test)" = "65534:65532:400" && test -r /run/secrets/test && test ! -w /run/secrets/test'
docker run --rm --user 999:999 -v "${work}/secrets/postgres-app-password:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${POSTGRES_IMAGE:-postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f}" \
  -c 'test -r /run/secrets/test && test ! -w /run/secrets/test'
docker run --rm --user 10001:10001 -v "${work}/secrets/otel-client.key:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${runtime_control_image}" -c 'test -r /run/secrets/test && test ! -w /run/secrets/test'
docker volume rm "${trust_volume}" >/dev/null
docker image rm "${runtime_transport_image}" "${runtime_control_image}" >/dev/null

(cd "${ROOT}/rust" && cargo test --locked -p ocservia-transportd \
  tests::dedicated_relay_failure_moves_traffic_to_second_relay -- --exact) \
  >"${ARTIFACT_DIR}/relay-failover.log" 2>&1
(cd "${ROOT}/control-plane" && go test ./internal/auth \
  -run TestOIDCTLSAndIssuerOutagesFailClosed -count=1) \
  >"${ARTIFACT_DIR}/oidc-tls-outage.log" 2>&1
RUN_ID="${RUN_ID}-backup" ARTIFACT_DIR="${ARTIFACT_DIR}/backup-restore" \
  "${ROOT}/scripts/i18-backup-restore-smoke.sh"
RUN_ID="${RUN_ID}-package" ARTIFACT_DIR="${ARTIFACT_DIR}/agent-package" \
  "${ROOT}/scripts/i18-agent-package-smoke.sh"

echo "I18 production topology, relay failover, restore, and package checks passed"
