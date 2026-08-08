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
cleanup() { rm -rf -- "${work}"; }
trap cleanup EXIT INT TERM
mkdir -p "${work}/secrets" "${work}/relay-secrets" "${work}/backups" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/secrets" "${work}/relay-secrets" "${work}/backups"
for secret in tls.crt tls.key postgres-owner-password postgres-app-password postgres.pgpass \
  postgres-backup-password database-owner-url database-app-url oidc-client-secret session-key audit-checkpoint-key \
  certificate-signer-token relay-access-token controller-iroh.key otel-client.crt otel-client.key otel-ca.crt; do
  printf 'test-only\n' >"${work}/secrets/${secret}"
done
for secret in relay-access-token tls.crt tls.key; do
  printf 'test-only\n' >"${work}/relay-secrets/${secret}"
done

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

docker compose -f "${ROOT}/deploy/production/compose.yaml" config --format json \
  >"${ARTIFACT_DIR}/platform-compose.json"
docker compose -f "${ROOT}/deploy/production/relay/compose.yaml" config --format json \
  >"${ARTIFACT_DIR}/relay-compose.json"

python3 - "${ARTIFACT_DIR}/platform-compose.json" "${ARTIFACT_DIR}/relay-compose.json" <<'PY'
import json
import pathlib
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
    serialized = json.dumps(service)
    for forbidden in ("docker.sock", "/proc", "/sys"):
        assert forbidden not in serialized

services = platform["services"]
assert set(services) == {"gateway", "postgres", "migrate", "control-plane", "transportd", "otel-collector", "backup"}
for service in services.values():
    hardened(service)
    assert "@sha256:" in service["image"]
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
assert services["control-plane"]["command"] == ["--role=all"]

relay_service = relay["services"]["relay"]
hardened(relay_service)
assert "@sha256:" in relay_service["image"]
published = {(int(item["published"]), item["protocol"]) for item in relay_service["ports"]}
assert published == {(80, "tcp"), (443, "tcp"), (7842, "udp")}
assert relay_service["environment"]["IROH_RELAY_ACCESS_TOKEN_FILE"] == "/run/secrets/relay_access_token"
print("I18 production topology validation passed")
PY

(cd "${ROOT}/rust" && cargo test --locked -p ocservia-transportd \
  tests::dedicated_relay_failure_moves_traffic_to_second_relay -- --exact) \
  >"${ARTIFACT_DIR}/relay-failover.log" 2>&1
RUN_ID="${RUN_ID}-backup" ARTIFACT_DIR="${ARTIFACT_DIR}/backup-restore" \
  "${ROOT}/scripts/i18-backup-restore-smoke.sh"
RUN_ID="${RUN_ID}-package" ARTIFACT_DIR="${ARTIFACT_DIR}/agent-package" \
  "${ROOT}/scripts/i18-agent-package-smoke.sh"

echo "I18 production topology, relay failover, restore, and package checks passed"
