#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-backup-health-XXXXXX")"
project="$(basename "${work}" | tr '[:upper:]' '[:lower:]')"
compose=(docker compose -p "${project}" -f "${work}/compose.json")
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -f "${work}/compose.json" ]]; then
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Render only: never start the production project or mount production secrets.
image="$(sed -n 's/^FROM //p' "${ROOT}/deploy/production/backup.Dockerfile")"
export OCSERV_GATEWAY_IMAGE="${image}" OCSERV_CONTROL_IMAGE="${image}" OCSERV_TRANSPORT_IMAGE="${image}"
export OCSERV_BACKUP_IMAGE="${image}" OCSERV_POSTGRES_IMAGE="${image}" OCSERV_OTEL_IMAGE="${image}"
export OCSERV_SECRET_DIR="${work}" OCSERV_BACKUP_DIR="${work}"
export OCSERV_PUBLIC_HOST=controller.example.test OCSERV_AUDIT_EVENT_KEY_ID=test
OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '%064d' 1)"
export OCSERV_CONTROLLER_ENDPOINT_ID
export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test OCSERV_RELAY_URL_A=https://relay.example.test
export OCSERV_BACKUP_INTERVAL_SECONDS=900
docker compose --env-file /dev/null -p "${project}" \
  -f "${ROOT}/deploy/production/compose.yaml" \
  -f "${ROOT}/deploy/production/compose.postgres.yaml" config --format json >"${work}/production.json"
major="$(docker version --format '{{.Server.Version}}' | cut -d. -f1)"
[[ "${major}" =~ ^[0-9]+$ && "${major}" -ge 25 ]] || {
  echo 'Fast-start timing test requires Engine 25+; older engines retain the 5m interval' >&2
  exit 2
}

# Use the real predicate and schedule with controlled LATEST markers, not a DB.
jq --arg image "${image}" '
  .services.backup.healthcheck as $health |
  {services: (["fresh", "missing", "stale"] | map(. as $name | {
    key: $name, value: {
      image: $image, network_mode: "none", read_only: true,
      cap_drop: ["ALL"], security_opt: ["no-new-privileges:true"],
      tmpfs: ["/var/lib/ocservia-backup"],
      environment: {BACKUP_INTERVAL_SECONDS: "900"},
      entrypoint: ["sh", "-ec"],
      command: [(if $name == "fresh" then "sleep 7; touch /var/lib/ocservia-backup/LATEST; sleep 600"
        elif $name == "stale" then "touch -d '\''2 hours ago'\'' /var/lib/ocservia-backup/LATEST; sleep 600"
        else "sleep 600" end)],
      healthcheck: $health
    }}) | from_entries)}
' "${work}/production.json" >"${work}/compose.json"
"${compose[@]}" pull --quiet
started="$(date +%s)"
"${compose[@]}" up -d --wait --wait-timeout 30 fresh
elapsed=$(( $(date +%s) - started ))
(( elapsed >= 7 && elapsed < 30 ))
fresh="$("${compose[@]}" ps -q fresh)"
docker inspect "${fresh}" | jq -e '
  .[0].State.Health.Status == "healthy" and
  (.[0].State.Health.Log | any(.ExitCode != 0)) and
  (.[0].State.Health.Log | any(.ExitCode == 0))
' >/dev/null
echo "Fresh marker became healthy in ${elapsed}s (including 7s marker creation delay)"

if "${compose[@]}" up -d --wait --wait-timeout 20 missing stale; then
  echo 'Missing or stale backup marker unexpectedly passed health waiting' >&2
  exit 1
fi
for service in missing stale; do
  container="$("${compose[@]}" ps -q "${service}")"
  docker inspect "${container}" | jq -e '
    .[0].State.Health.Status != "healthy" and
    (.[0].State.Health.Log | length >= 2) and
    (.[0].State.Health.Log | all(.ExitCode != 0))
  ' >/dev/null
done
echo 'Missing and stale markers failed repeated startup probes and Compose waiting'
