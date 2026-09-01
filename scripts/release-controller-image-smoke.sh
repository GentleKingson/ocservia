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
for tool in docker jq curl; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

image_ref() {
  printf '%s/%s:%s-linux-%s' "${CONTROLLER_IMAGE_PREFIX}" "$1" "${VERSION}" "${CONTROLLER_ARCH}"
}

# The build step loaded the exact images its OCI archives carry into the
# runner's Docker daemon. Assert each one really targets the matrix
# architecture: an amd64-only build smuggled into the arm64 leg must fail
# here, on the native runner, instead of at publish time.
for name in gateway control transport backup; do
  archive="${IMAGES_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  [[ -s "${archive}" ]] || fail "Controller image archive is missing or empty: ${archive}"
  docker image inspect "$(image_ref "${name}")" >/dev/null ||
    fail "Controller image ${name} was not loaded into the daemon"
  platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$(image_ref "${name}")")"
  [[ "${platform}" == "linux/${CONTROLLER_ARCH}" ]] ||
    fail "loaded ${name} image is ${platform}, expected linux/${CONTROLLER_ARCH}"
done

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
  --publish "127.0.0.1:${gateway_port}:8443" \
  --volume "${fixture}/Caddyfile:/etc/caddy/Caddyfile:ro" \
  "$(image_ref gateway)" >/dev/null
ready=0
for _ in $(seq 1 30); do
  if [[ "$(curl --silent --max-time 2 "http://127.0.0.1:${gateway_port}/" 2>/dev/null || true)" == "controller-image-smoke-ok" ]]; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "${ready}" != "1" ]]; then
  docker logs "${gateway_container}" >&2 || true
  fail "gateway image did not serve the expected response on ${CONTROLLER_ARCH}"
fi
gateway_uid="$(docker exec "${gateway_container}" id -u)"
[[ "${gateway_uid}" != "0" ]] || fail "gateway container must not run as root"

# control: prove the binary executes on this architecture up to its
# configuration boundary — it must reject an invalid role, not crash.
if control_output="$(docker run --rm "$(image_ref control)" --role=bogus 2>&1)"; then
  fail "control image accepted an invalid role"
fi
grep -Fq 'invalid role' <<<"${control_output}" ||
  fail "control image did not reach role validation: ${control_output}"

# transport: same boundary — the daemon must refuse to start without its
# required key file argument.
if transport_output="$(docker run --rm "$(image_ref transport)" --socket /run/ocserv-platform/transportd.sock 2>&1)"; then
  fail "transport image started without a key file"
fi
grep -Fq -- '--key-file is required' <<<"${transport_output}" ||
  fail "transport image did not reach argument validation: ${transport_output}"

# backup: the PostgreSQL client runtime must execute, and the real
# entrypoint must reach its own guard when the pgpass source is absent.
psql_version="$(docker run --rm --entrypoint psql "$(image_ref backup)" --version)"
grep -Eq 'psql \(PostgreSQL\) 17\.' <<<"${psql_version}" ||
  fail "backup image psql runtime did not report PostgreSQL 17: ${psql_version}"
if backup_output="$(docker run --rm "$(image_ref backup)" 2>&1)"; then
  fail "backup entrypoint ran without its pgpass source"
fi
grep -Fq 'PostgreSQL passfile source must be a regular file' <<<"${backup_output}" ||
  fail "backup image did not reach the entrypoint guard: ${backup_output}"

echo "Controller ${CONTROLLER_ARCH} image smoke passed (gateway uid ${gateway_uid})"
