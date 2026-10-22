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
  [[ "${!variable}" =~ ^[^[:space:]@]+(@sha256:[0-9a-f]{64}|:v[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?)$ ]]
done
EOF
chmod 0755 "${bin}/compose.sh"

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${SMOKE_TEST_CURL_LOG}"
url="${*: -1}"
case "${url}" in
  */api/v1/readyz)
    if [[ -n "${SMOKE_TEST_ATTEMPTS:-}" ]]; then
      count="$(cat "${SMOKE_TEST_ATTEMPTS}")"
      echo "$((count + 1))" >"${SMOKE_TEST_ATTEMPTS}"
      if ((count < SMOKE_TEST_FAILURES)); then
        printf 'partial response'
        exit "${SMOKE_TEST_CURL_ERROR:-35}"
      fi
    fi
    printf '%s\n' '{"status":"ok"}' ;;
  */api/v1/version) printf '%s\n' '{"version":"0.3.0","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}' ;;
  *) exit 22 ;;
esac
EOF
chmod 0755 "${bin}/curl"

release_file="${fixture}/release/controller-release.json"
jq -n '{release_version:"0.3.0", source_commit:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", images:{
  gateway:"registry.example/gateway:v0.3.0",
  control:"registry.example/control:v0.3.0",
  transport:"registry.example/transport:v0.3.0",
  backup:"registry.example/backup:v0.3.0",
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

# ACME can still be issuing the first certificate after Compose is healthy.
# Replace only the delay so the real smoke control flow is tested without waits.
printf '#!/bin/sh\nexit 0\n' >"${bin}/sleep"
chmod 0755 "${bin}/sleep"
acme_smoke() {
  echo 0 >"${fixture}/attempts"
  PATH="${bin}:${PATH}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    OCSERV_CONTROLLER_PUBLIC_URL="https://controller.example.test" \
    OCSERV_TLS_MODE="$1" \
    SMOKE_TEST_COMPOSE_LOG="${fixture}/compose.log" \
    SMOKE_TEST_CURL_LOG="${fixture}/curl.log" \
    SMOKE_TEST_ATTEMPTS="${fixture}/attempts" \
    SMOKE_TEST_FAILURES="$2" SMOKE_TEST_CURL_ERROR="$3" \
    "${SMOKE}" --release-file "${release_file}" >"${fixture}/acme.log" 2>&1
}
acme_smoke acme 2 35
test "$(cat "${fixture}/attempts")" = 3
for scenario in 'acme 30 35 30' 'acme 1 60 1' 'manual 1 35 1'; do
  read -r mode failures error attempts <<<"${scenario}"
  if acme_smoke "${mode}" "${failures}" "${error}"; then
    echo "failed TLS readiness was accepted: ${scenario}" >&2
    exit 1
  fi
  test "$(cat "${fixture}/attempts")" = "${attempts}"
  grep -Fq 'public Controller readiness probe failed' "${fixture}/acme.log"
done

# The runtime smoke consumes exact RC refs and checks the RC binary identity.
sed -i 's/0.3.0/0.3.0-rc.1/g' "${release_file}" "${bin}/curl"
PATH="${bin}:${PATH}" \
  OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
  OCSERV_CONTROLLER_PUBLIC_URL="https://controller.example.test" \
  SMOKE_TEST_COMPOSE_LOG="${fixture}/compose.log" \
  SMOKE_TEST_CURL_LOG="${fixture}/curl.log" \
  "${SMOKE}" --release-file "${release_file}"

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
