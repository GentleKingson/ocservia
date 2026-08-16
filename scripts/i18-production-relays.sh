#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-full}"
if (($# > 1)) || [[ "${MODE}" != "full" && "${MODE}" != "--contract-only" ]]; then
  echo "usage: $0 [--contract-only]" >&2
  exit 2
fi
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi

tmp_base="$(realpath -e "${RUNNER_TEMP:-${TMPDIR:-/tmp}}")"
work="${tmp_base}/ocservia-i18-production-${RUN_ID}"
runtime_transport_image="ocservia-i18-transport-runtime:${RUN_ID}"
runtime_control_image="ocservia-i18-control-runtime:${RUN_ID}"
trust_volume="ocservia-i18-trust-${RUN_ID}"
transport_volume="ocservia-i18-transport-${RUN_ID}"
development_transport_volume="ocservia-i18-development-transport-${RUN_ID}"
cleanup() {
  local status=$?
  docker volume rm -f "${trust_volume}" "${transport_volume}" \
    "${development_transport_volume}" >/dev/null 2>&1 || true
  docker image rm -f "${runtime_transport_image}" "${runtime_control_image}" >/dev/null 2>&1 || true
  sudo chown -R "$(id -u):$(id -g)" "${work}" >/dev/null 2>&1 || status=1
  rm -rf -- "${work}"
  for volume in "${trust_volume}" "${transport_volume}" "${development_transport_volume}"; do
    if docker volume inspect "${volume}" >/dev/null 2>&1; then
      echo "scoped runtime volume cleanup failed: ${volume}" >&2
      status=1
    fi
  done
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
printf '%064d\n' 1 >"${work}/secrets/audit-event-key"
for secret in relay-access-token tls.crt tls.key; do
  printf 'test-only\n' >"${work}/relay-secrets/${secret}"
done
general_secrets=(tls.crt tls.key postgres-owner-password postgres-app-password postgres-backup-password \
  postgres.pgpass database-owner-url database-app-url oidc-client-secret session-key \
  audit-checkpoint-key certificate-signer-token otel-client.crt otel-client.key otel-ca.crt)
chmod 0444 "${general_secrets[@]/#/${work}\/secrets/}"
chmod 0400 "${work}/secrets/controller-command-signing-key.pem" "${work}/secrets/audit-event-key"
chmod 0444 "${work}/relay-secrets/tls.crt" "${work}/relay-secrets/tls.key"
chmod 0400 "${work}/secrets/relay-access-token" "${work}/secrets/controller-iroh.key" \
  "${work}/relay-secrets/relay-access-token"
sudo chown 65534:65532 "${work}/secrets/controller-command-signing-key.pem" "${work}/secrets/audit-event-key"
sudo chown 65532:65532 "${work}/secrets/relay-access-token" "${work}/secrets/controller-iroh.key" \
  "${work}/relay-secrets/relay-access-token"

image="example.invalid/ocservia/test@sha256:$(printf '%064d' 0)"
export OCSERV_GATEWAY_IMAGE="${image}" OCSERV_CONTROL_IMAGE="${image}"
export OCSERV_TRANSPORT_IMAGE="${image}" OCSERV_BACKUP_IMAGE="${image}" OCSERV_RELAY_IMAGE="${image}"
export OCSERV_POSTGRES_IMAGE="${image}" OCSERV_OTEL_IMAGE="${image}"
export OCSERV_PUBLIC_HOST=ocservia.example.test OCSERV_SECRET_DIR="${work}/secrets"
export OCSERV_BACKUP_DIR="${work}/backups"
export OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=ocservia
export OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1
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
assert set(services) == {"transport-runtime-init", "gateway", "postgres", "migrate", "control-plane", "transportd", "otel-collector", "backup"}
for name, service in services.items():
    if name == "transport-runtime-init":
        continue
    hardened(service)
    assert re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", service["image"])
runtime_init = services["transport-runtime-init"]
assert re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", runtime_init["image"])
assert runtime_init["user"] == "0:0"
assert runtime_init["read_only"] is True
assert runtime_init["cap_drop"] == ["ALL"]
assert set(runtime_init["cap_add"]) == {"CHOWN", "FOWNER", "DAC_OVERRIDE"}
assert runtime_init["network_mode"] == "none"
assert runtime_init["restart"] == "no"
assert runtime_init["entrypoint"] == ["/usr/local/libexec/ocservia-prepare-transport-runtime"]
assert runtime_init["command"] == ["/run/ocserv-platform", "65532", "65532", "65532"]
for name in ("transport-runtime-init", "transportd", "control-plane"):
    runtime_mount = next(
        item for item in services[name]["volumes"]
        if item["target"] == "/run/ocserv-platform"
    )
    assert runtime_mount["source"] == "transport-runtime"
    assert runtime_mount["volume"]["nocopy"] is True
assert services["transportd"]["depends_on"]["transport-runtime-init"]["condition"] == "service_completed_successfully"
assert services["control-plane"]["depends_on"]["transport-runtime-init"]["condition"] == "service_completed_successfully"
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
assert services["control-plane"]["environment"]["OCSERV_AUDIT_EVENT_KEY_ID"] == "audit-event-v1"
assert services["migrate"]["environment"]["OCSERV_AUDIT_EVENT_KEY_ID"] == "audit-event-v1"
assert services["control-plane"]["environment"]["OCSERV_TRANSPORT_UID"] == "65532"
assert services["control-plane"]["environment"]["OCSERV_TRANSPORT_GID"] == "65532"
control_secrets = {item["target"]: item for item in services["control-plane"]["secrets"]}
command_key = control_secrets["controller_command_signing_key"]
assert command_key["uid"] == "65534" and command_key["gid"] == "65532" and command_key["mode"] == "0400"
for service_name in ("migrate", "control-plane"):
    event_key = next(item for item in services[service_name]["secrets"] if item["target"] == "audit_event_key")
    assert event_key["uid"] == "65534" and event_key["gid"] == "65532" and event_key["mode"] == "0400"
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
docker volume create "${transport_volume}" >/dev/null
docker run --rm --user 0:0 --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
  -v "${transport_volume}:/run/ocserv-platform" --entrypoint /bin/sh "${runtime_transport_image}" \
  -c 'chown 65532:65532 /run/ocserv-platform && chmod 0770 /run/ocserv-platform'
docker run --rm --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
  --network none --mount "type=volume,source=${transport_volume},target=/run/ocserv-platform,volume-nocopy" \
  --entrypoint /usr/local/libexec/ocservia-prepare-transport-runtime "${runtime_transport_image}" \
  /run/ocserv-platform 65532 65532 65532
docker run --rm --user 65532:65532 \
  --mount "type=volume,source=${transport_volume},target=/run/ocserv-platform,volume-nocopy" \
  --entrypoint /bin/sh "${runtime_transport_image}" \
  -c 'test "$(stat -c %u:%g:%a /run/ocserv-platform)" = "65532:65532:750" && : > /run/ocserv-platform/bind-probe && rm /run/ocserv-platform/bind-probe'

docker volume create "${development_transport_volume}" >/dev/null
docker run --rm --user 0:0 --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
  -v "${development_transport_volume}:/run/ocserv-platform" --entrypoint /bin/sh "${runtime_transport_image}" \
  -c 'chown 65534:65532 /run/ocserv-platform && chmod 0770 /run/ocserv-platform'
docker run --rm --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
  --network none --mount "type=volume,source=${development_transport_volume},target=/run/ocserv-platform,volume-nocopy" \
  --entrypoint /usr/local/libexec/ocservia-prepare-transport-runtime "${runtime_transport_image}" \
  /run/ocserv-platform 65533 65532 65534
docker run --rm --user 65533:65532 \
  --mount "type=volume,source=${development_transport_volume},target=/run/ocserv-platform,volume-nocopy" \
  --entrypoint /bin/sh "${runtime_transport_image}" \
  -c 'test "$(stat -c %u:%g:%a /run/ocserv-platform)" = "65533:65532:750" && : > /run/ocserv-platform/bind-probe && rm /run/ocserv-platform/bind-probe'
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
docker run --rm -v "${work}/secrets/audit-event-key:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${runtime_control_image}" \
  -c 'test "$(stat -c %u:%g:%a /run/secrets/test)" = "65534:65532:400" && test -r /run/secrets/test && test ! -w /run/secrets/test'
docker run --rm --user 999:999 -v "${work}/secrets/postgres-app-password:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${POSTGRES_IMAGE:-postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f}" \
  -c 'test -r /run/secrets/test && test ! -w /run/secrets/test'
docker run --rm --user 10001:10001 -v "${work}/secrets/otel-client.key:/run/secrets/test:ro" \
  --entrypoint /bin/sh "${runtime_control_image}" -c 'test -r /run/secrets/test && test ! -w /run/secrets/test'
docker volume rm "${trust_volume}" "${transport_volume}" "${development_transport_volume}" >/dev/null
docker image rm "${runtime_transport_image}" "${runtime_control_image}" >/dev/null

if [[ "${MODE}" == "full" ]]; then
  (cd "${ROOT}/rust" && cargo test --locked -p ocservia-transportd \
    tests::dedicated_relay_failure_moves_traffic_to_second_relay -- --exact) \
    >"${ARTIFACT_DIR}/relay-failover.log" 2>&1
  (cd "${ROOT}/control-plane" && go test ./internal/auth \
    -run TestOIDCTLSAndIssuerOutagesFailClosed -count=1) \
    >"${ARTIFACT_DIR}/oidc-tls-outage.log" 2>&1
else
  printf 'covered by full Go and Rust validation jobs\n' >"${ARTIFACT_DIR}/relay-failover.log"
  printf 'covered by full Go and Rust validation jobs\n' >"${ARTIFACT_DIR}/oidc-tls-outage.log"
fi
RUN_ID="${RUN_ID}-backup" ARTIFACT_DIR="${ARTIFACT_DIR}/backup-restore" \
  "${ROOT}/scripts/i18-backup-restore-smoke.sh"
RUN_ID="${RUN_ID}-package" ARTIFACT_DIR="${ARTIFACT_DIR}/agent-package" \
  "${ROOT}/scripts/i18-agent-package-smoke.sh"

echo "I18 production topology, relay failover, restore, and package checks passed"
