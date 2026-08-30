#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SMOKE="${ROOT}/deploy/production/controller-release-smoke.sh"

fixture="$(realpath "$(mktemp -d)")"
trap 'rm -rf -- "${fixture}"' EXIT
bin="${fixture}/bin"
mkdir -m 700 -- "${bin}" "${fixture}/release"

cat >"${bin}/compose.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${SMOKE_TEST_COMPOSE_LOG}"
printf '%s\n' \
  '{"Service":"postgres","State":"running","Health":"healthy"}' \
  '{"Service":"control-plane","State":"running","Health":"healthy"}' \
  '{"Service":"transportd","State":"running","Health":"healthy"}' \
  '{"Service":"backup","State":"running","Health":"healthy"}'
for variable in OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE \
  OCSERV_BACKUP_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE; do
  [[ "${!variable}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]
done
EOF
chmod 0755 "${bin}/compose.sh"

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${SMOKE_TEST_CURL_LOG}"
url="${*: -1}"
case "${url}" in
  */api/v1/readyz) printf '%s\n' '{"status":"ok","schema_version":28}' ;;
  */api/v1/version) printf '%s\n' '{"version":"0.3.0","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}' ;;
  *) exit 22 ;;
esac
EOF
chmod 0755 "${bin}/curl"

release_file="${fixture}/release/controller-release.json"
jq -n '{release_version:"0.3.0", source_commit:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", images:{
  gateway:"registry.example/gateway@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  control:"registry.example/control@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  transport:"registry.example/transport@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  backup:"registry.example/backup@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  postgres:"registry.example/postgres@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  otel:"registry.example/otel@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}}' \
  >"${release_file}"

PATH="${bin}:${PATH}" \
  OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
  OCSERV_CONTROLLER_PUBLIC_URL="https://controller.example.test" \
  SMOKE_TEST_COMPOSE_LOG="${fixture}/compose.log" \
  SMOKE_TEST_CURL_LOG="${fixture}/curl.log" \
  "${SMOKE}" --release-file "${release_file}"

test "$(cat "${fixture}/compose.log")" = "ps --format json postgres control-plane transportd backup"
grep -Fq 'https://controller.example.test/api/v1/readyz' "${fixture}/curl.log"
grep -Fq 'https://controller.example.test/api/v1/version' "${fixture}/curl.log"
if grep -Fq -- '-k' "${ROOT}/deploy/production/controller-release-smoke.sh"; then
  echo "release smoke must not disable TLS verification" >&2
  exit 1
fi

cat >"${bin}/compose.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' '{"Service":"postgres","State":"running","Health":"healthy"}'
printf '%s\n' '{"Service":"control-plane","State":"running","Health":"healthy"}'
printf '%s\n' '{"Service":"transportd","State":"running","Health":"unhealthy"}'
printf '%s\n' '{"Service":"backup","State":"running","Health":"healthy"}'
EOF
chmod 0755 "${bin}/compose.sh"
if PATH="${bin}:${PATH}" \
  OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
  OCSERV_CONTROLLER_PUBLIC_URL="https://controller.example.test" \
  "${SMOKE}" --release-file "${release_file}" >"${fixture}/unhealthy.log" 2>&1; then
  echo "unhealthy Compose state was accepted" >&2
  exit 1
fi
grep -Fq 'required Compose services are not healthy' "${fixture}/unhealthy.log"

echo "Controller release smoke tests passed"
