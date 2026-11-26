#!/usr/bin/env bash
set -Eeuo pipefail

report_error_line() {
  local status=$?
  printf 'controller image smoke failed at line %s\n' "${BASH_LINENO[0]:-unknown}" >&2
  return "${status}"
}
trap report_error_line ERR

fail() {
  echo "controller image smoke: $1" >&2
  exit 1
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/release-common.sh
source "${ROOT}/scripts/release-common.sh"

IMAGES_DIR="${IMAGES_DIR:?IMAGES_DIR is required}"
CONTROLLER_ARCH="${CONTROLLER_ARCH:?CONTROLLER_ARCH is required}"
VERSION="${VERSION:?VERSION is required}"
CONTROLLER_IMAGE_PREFIX="${CONTROLLER_IMAGE_PREFIX:-ghcr.io/gentlekingson/ocservia}"

case "${CONTROLLER_ARCH}" in
  amd64 | arm64) ;;
  *)
    fail "CONTROLLER_ARCH must be amd64 or arm64, got ${CONTROLLER_ARCH}"
    ;;
esac
case "$(uname -m)" in
  x86_64) host_arch=amd64 ;;
  aarch64) host_arch=arm64 ;;
  *)
    fail "controller image smoke requires a supported native host architecture, got $(uname -m)"
    ;;
esac
[[ "${host_arch}" == "${CONTROLLER_ARCH}" ]] ||
  fail "controller images must be smoked on their native runner: host ${host_arch} != ${CONTROLLER_ARCH}"
for tool in docker curl; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

image_ref() {
  printf '%s/%s:%s-linux-%s' "${CONTROLLER_IMAGE_PREFIX}" "$1" "${VERSION}" "${CONTROLLER_ARCH}"
}

release_load_controller_images "${IMAGES_DIR}" "${VERSION}" "${CONTROLLER_ARCH}" "${CONTROLLER_IMAGE_PREFIX}" ||
  fail "failed loading or verifying Controller image archives"

# Gateway must actually start: its USER directive once referenced an
# account the upstream image never created (#139), so boot it for real,
# serve a request through Caddy, and assert the process is not root.
gateway_container="ocservia-gateway-release-smoke-$$"
gateway_port=18053
fixture="$(mktemp -d)"
trap 'docker rm -f "${gateway_container}" >/dev/null 2>&1 || true; rm -rf -- "${fixture}"' EXIT INT TERM
cat >"${fixture}/Caddyfile" <<'EOF'
{
	admin off
	auto_https off
}

:8443 {
	respond "controller-image-smoke-ok" 200
}
EOF
docker run --detach --name "${gateway_container}" \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --publish "127.0.0.1:${gateway_port}:8443" \
  --volume "${fixture}/Caddyfile:/etc/caddy/Caddyfile:ro" \
  "$(image_ref gateway)" >/dev/null

# Exercise the additional native runtimes in parallel while gateway boots.
check_edge() {
  docker run --rm --entrypoint nginx "$(image_ref edge)" -V 2>&1 | grep -Fq -- --with-stream_ssl_preread_module
}
check_relay() {
  docker run --rm --entrypoint /usr/local/bin/iroh-relay "$(image_ref relay)" --version | grep -Fq iroh-relay
}
check_signer() {
  local signer_output
  if signer_output="$(docker run --rm "$(image_ref signer)" invalid-command 2>&1)"; then
    fail "signer accepted an unknown command"
  fi
  grep -Fq 'unknown command' <<<"${signer_output}" || fail "signer did not reach command validation"
}
check_control() {
  local control_output
  if control_output="$(docker run --rm "$(image_ref control)" --role=bogus 2>&1)"; then
    fail "control image accepted an invalid role"
  fi
  grep -Eq 'invalid (role|command-line flags)' <<<"${control_output}" ||
    fail "control image did not reach flag validation: ${control_output}"
}
check_transport() {
  local transport_output
  if transport_output="$(docker run --rm "$(image_ref transport)" --socket /run/ocserv-platform/transportd.sock 2>&1)"; then
    fail "transport image started without a key file"
  fi
  grep -Fq -- '--key-file is required' <<<"${transport_output}" ||
    fail "transport image did not reach argument validation: ${transport_output}"
}
check_backup() {
  local psql_version backup_output
  docker run --rm --entrypoint pg_basebackup "$(image_ref backup)" --version | grep -Fq 'pg_basebackup (PostgreSQL) 18.'
  psql_version="$(docker run --rm --entrypoint psql "$(image_ref backup)" --version)"
  grep -Eq 'psql \(PostgreSQL\) 18\.' <<<"${psql_version}" ||
    fail "backup image psql runtime did not report PostgreSQL 18: ${psql_version}"
  if backup_output="$(docker run --rm "$(image_ref backup)" 2>&1)"; then
    fail "backup entrypoint ran without its pgpass source"
  fi
  grep -Fq 'PostgreSQL passfile source must be a regular file' <<<"${backup_output}" ||
    fail "backup image did not reach the entrypoint guard: ${backup_output}"
}

check_pids=()
check_logs=()
for check_fn in check_edge check_relay check_signer check_control check_transport check_backup; do
  log_file="${fixture}/${check_fn}.log"
  check_logs+=("${log_file}")
  "${check_fn}" >"${log_file}" 2>&1 &
  check_pids+=("$!")
done

check_errors=0
for idx in "${!check_pids[@]}"; do
  if ! wait "${check_pids[idx]}"; then
    check_errors=1
  fi
  if [[ -s "${check_logs[idx]}" ]]; then
    cat "${check_logs[idx]}" >&2
  fi
done
((check_errors == 0)) || fail "one or more Controller image runtime checks failed"

ready=0
for _ in $(seq 1 150); do
  if [[ "$(curl --silent --max-time 2 "http://127.0.0.1:${gateway_port}/" 2>/dev/null || true)" == "controller-image-smoke-ok" ]]; then
    ready=1
    break
  fi
  sleep 0.2
done
if [[ "${ready}" != "1" ]]; then
  docker logs "${gateway_container}" >&2 || true
  fail "gateway image did not serve the expected response on ${CONTROLLER_ARCH}"
fi
gateway_uid="$(docker exec "${gateway_container}" id -u)"
[[ "${gateway_uid}" != "0" ]] || fail "gateway container must not run as root"

echo "Controller ${CONTROLLER_ARCH} image smoke passed (gateway uid ${gateway_uid})"
