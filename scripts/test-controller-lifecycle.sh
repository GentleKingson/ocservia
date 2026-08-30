#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROLLER="${ROOT}/deploy/production/controller.sh"

if ! command -v flock >/dev/null 2>&1; then
  echo "Controller lifecycle tests skipped: flock is unavailable"
  exit 0
fi

fixture="$(mktemp -d "${HOME}/.ocservia-controller-test.XXXXXX")"
trap 'rm -rf -- "${fixture}"' EXIT
bin="${fixture}/bin"
mkdir -m 700 -- "${bin}" "${fixture}/release"

cat >"${bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == compose && "${2:-}" == up && "${3:-}" == --help ]]; then
  printf '%s\n' '      --wait                         Wait for services to be running|healthy'
  exit 0
fi
exit 0
EOF
chmod 0755 "${bin}/docker"

cat >"${bin}/compose.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CONTROLLER_TEST_LOG}"
printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
  "${OCSERV_GATEWAY_IMAGE:-}" "${OCSERV_CONTROL_IMAGE:-}" \
  "${OCSERV_TRANSPORT_IMAGE:-}" "${OCSERV_BACKUP_IMAGE:-}" \
  "${OCSERV_POSTGRES_IMAGE:-}" "${OCSERV_OTEL_IMAGE:-}" >>"${CONTROLLER_TEST_ENV_LOG}"
case "${1:-}" in
  config) exit "${MOCK_CONFIG_EXIT:-0}" ;;
  pull) exit "${MOCK_PULL_EXIT:-0}" ;;
  up) exit "${MOCK_UP_EXIT:-0}" ;;
  *) exit 97 ;;
esac
EOF
chmod 0755 "${bin}/compose.sh"

digest="sha256:$(printf 'b%.0s' {1..64})"
commit="$(git -C "${ROOT}" rev-parse HEAD)"
release_file="${fixture}/release/controller-release.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
  --output "${release_file}" \
  --release-version 0.2.0 \
  --release-tag v0.2.0 \
  --source-commit "${commit}" \
  --migration-dir "${ROOT}/control-plane/migrations" \
  --image "gateway=ghcr.io/gentlekingson/ocservia/gateway@${digest}" \
  --image "control=ghcr.io/gentlekingson/ocservia/control@${digest}" \
  --image "transport=ghcr.io/gentlekingson/ocservia/transport@${digest}" \
  --image "backup=ghcr.io/gentlekingson/ocservia/backup@${digest}" \
  --image "postgres=docker.io/library/postgres@${digest}" \
  --image "otel=docker.io/otel/opentelemetry-collector@${digest}"

run_controller() {
  local state="$1" selected="$2"
  shift 2
  PATH="${bin}:${PATH}" \
    CONTROLLER_TEST_LOG="${state}/compose.log" \
    CONTROLLER_TEST_ENV_LOG="${state}/compose-env.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${state}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    "$@" "${CONTROLLER}" install --release-file "${selected}"
}

expect_failure() {
  local state="$1" selected="$2" expected_message="$3" pending_expected="$4"
  shift 4
  mkdir -m 700 -- "${state}"
  if run_controller "${state}" "${selected}" "$@" >"${state}/output.log" 2>&1; then
    echo "expected controller install to fail: ${expected_message}" >&2
    exit 1
  fi
  grep -Fq "${expected_message}" "${state}/output.log"
  test ! -e "${state}/current-release.json"
  if [[ "${pending_expected}" == true ]]; then
    test -f "${state}/pending-release.json"
    cmp -s "${release_file}" "${state}/pending-release.json"
  else
    test ! -e "${state}/pending-release.json"
  fi
}

valid_state="${fixture}/valid"
mkdir -m 700 -- "${valid_state}"
run_controller "${valid_state}" "${release_file}" env
test -f "${valid_state}/current-release.json"
test ! -e "${valid_state}/pending-release.json"
cmp -s "${release_file}" "${valid_state}/current-release.json"
test "$(stat -c '%u:%a' "${valid_state}")" = "$(id -u):700"
test "$(stat -c '%u:%a' "${valid_state}/current-release.json")" = "$(id -u):600"
test "$(find "${valid_state}" -maxdepth 1 -name '.current-release.json.*' -print | wc -l)" -eq 0
test "$(sed -n '1p' "${valid_state}/compose.log")" = "config --quiet"
test "$(sed -n '2p' "${valid_state}/compose.log")" = "pull"
test "$(sed -n '3p' "${valid_state}/compose.log")" = "up -d --wait"
IFS=$'\t' read -r gateway control transport backup postgres otel <"${valid_state}/compose-env.log"
test "${gateway}" = "ghcr.io/gentlekingson/ocservia/gateway@${digest}"
test "${control}" = "ghcr.io/gentlekingson/ocservia/control@${digest}"
test "${transport}" = "ghcr.io/gentlekingson/ocservia/transport@${digest}"
test "${backup}" = "ghcr.io/gentlekingson/ocservia/backup@${digest}"
test "${postgres}" = "docker.io/library/postgres@${digest}"
test "${otel}" = "docker.io/otel/opentelemetry-collector@${digest}"

relative_state="${fixture}/relative"
mkdir -m 700 -- "${relative_state}"
(cd "${fixture}/release" && run_controller "${relative_state}" controller-release.json env)
cmp -s "${release_file}" "${relative_state}/current-release.json"
test ! -e "${relative_state}/pending-release.json"

if run_controller "${valid_state}" "${release_file}" env >"${fixture}/already-installed.log" 2>&1; then
  echo "already-installed Controller was accepted" >&2
  exit 1
fi
grep -Fq 'Controller is already installed; use upgrade' "${fixture}/already-installed.log"
test "$(wc -l <"${valid_state}/compose.log")" -eq 3

unsupported="${fixture}/unsupported.json"
jq '.manifest_version = 2' "${release_file}" >"${unsupported}"
expect_failure "${fixture}/unsupported" "${unsupported}" 'release manifest is invalid' false env

missing_image="${fixture}/missing-image.json"
jq 'del(.images.gateway)' "${release_file}" >"${missing_image}"
expect_failure "${fixture}/missing-image" "${missing_image}" 'release manifest is invalid' false env

mutable_image="${fixture}/mutable-image.json"
jq '.images.gateway = "ghcr.io/gentlekingson/ocservia/gateway:latest"' "${release_file}" >"${mutable_image}"
expect_failure "${fixture}/mutable-image" "${mutable_image}" 'release manifest is invalid' false env

malformed_digest="${fixture}/malformed-digest.json"
jq '.images.control = "ghcr.io/gentlekingson/ocservia/control@sha256:deadbeef"' "${release_file}" >"${malformed_digest}"
expect_failure "${fixture}/malformed-digest" "${malformed_digest}" 'release manifest is invalid' false env

malformed_json="${fixture}/malformed-json.json"
printf '%s\n' '{' >"${malformed_json}"
expect_failure "${fixture}/malformed-json" "${malformed_json}" 'release manifest is invalid' false env

symlink_release="${fixture}/release-symlink.json"
ln -s "${release_file}" "${symlink_release}"
expect_failure "${fixture}/symlink-release" "${symlink_release}" 'must not contain symlink ancestry' false env

source_mismatch="${fixture}/source-mismatch.json"
jq --arg source "$(printf 'c%.0s' {1..40})" '.source_commit = $source' "${release_file}" >"${source_mismatch}"
expect_failure "${fixture}/source-mismatch" "${source_mismatch}" \
  'checkout HEAD does not match release manifest source_commit' false env

config_state="${fixture}/config-failure"
mkdir -m 700 -- "${config_state}"
if run_controller "${config_state}" "${release_file}" env MOCK_CONFIG_EXIT=1 >"${config_state}/output.log" 2>&1; then
  echo "config failure was accepted" >&2
  exit 1
fi
test ! -e "${config_state}/current-release.json"
test -f "${config_state}/pending-release.json"
cmp -s "${release_file}" "${config_state}/pending-release.json"
test "$(wc -l <"${config_state}/compose.log")" -eq 1

pull_state="${fixture}/pull-failure"
mkdir -m 700 -- "${pull_state}"
if run_controller "${pull_state}" "${release_file}" env MOCK_PULL_EXIT=1 >"${pull_state}/output.log" 2>&1; then
  echo "pull failure was accepted" >&2
  exit 1
fi
test ! -e "${pull_state}/current-release.json"
test -f "${pull_state}/pending-release.json"
cmp -s "${release_file}" "${pull_state}/pending-release.json"
test "$(wc -l <"${pull_state}/compose.log")" -eq 2

up_state="${fixture}/up-failure"
mkdir -m 700 -- "${up_state}"
if run_controller "${up_state}" "${release_file}" env MOCK_UP_EXIT=1 >"${up_state}/output.log" 2>&1; then
  echo "up failure was accepted" >&2
  exit 1
fi
test ! -e "${up_state}/current-release.json"
test -f "${up_state}/pending-release.json"
cmp -s "${release_file}" "${up_state}/pending-release.json"
test "$(wc -l <"${up_state}/compose.log")" -eq 3

retry_state="${fixture}/retry"
mkdir -m 700 -- "${retry_state}"
if run_controller "${retry_state}" "${release_file}" env MOCK_UP_EXIT=1 >"${retry_state}/first.log" 2>&1; then
  echo "initial retry fixture unexpectedly succeeded" >&2
  exit 1
fi
test -f "${retry_state}/pending-release.json"

different_pending="${fixture}/different-pending.json"
other_digest="sha256:$(printf 'c%.0s' {1..64})"
jq --arg image "ghcr.io/gentlekingson/ocservia/gateway@${other_digest}" \
  '.images.gateway = $image' "${release_file}" >"${different_pending}"
if run_controller "${retry_state}" "${different_pending}" env >"${retry_state}/different.log" 2>&1; then
  echo "different pending release was accepted" >&2
  exit 1
fi
grep -Fq 'a different pending release exists; only the same manifest may be retried' "${retry_state}/different.log"
test "$(wc -l <"${retry_state}/compose.log")" -eq 3

run_controller "${retry_state}" "${release_file}" env
cmp -s "${release_file}" "${retry_state}/current-release.json"
test ! -e "${retry_state}/pending-release.json"

lock_state="${fixture}/lock"
mkdir -m 700 -- "${lock_state}"
: >"${lock_state}/lifecycle.lock"
chmod 600 "${lock_state}/lifecycle.lock"
(
  exec 9>>"${lock_state}/lifecycle.lock"
  flock -n 9
  printf '%s\n' ready >"${lock_state}/ready"
  sleep 10
) &
lock_holder=$!
for _ in $(seq 1 100); do
  [[ -f "${lock_state}/ready" ]] && break
  sleep 0.01
done
test -f "${lock_state}/ready"
if run_controller "${lock_state}" "${release_file}" env >"${lock_state}/output.log" 2>&1; then
  kill "${lock_holder}" 2>/dev/null || true
  wait "${lock_holder}" 2>/dev/null || true
  echo "concurrent Controller install was accepted" >&2
  exit 1
fi
test ! -e "${lock_state}/current-release.json"
grep -Fq 'another Controller lifecycle command is already running' "${lock_state}/output.log"
kill "${lock_holder}" 2>/dev/null || true
wait "${lock_holder}" 2>/dev/null || true

unsafe_state="${fixture}/unsafe-state"
mkdir -m 750 -- "${unsafe_state}"
if run_controller "${unsafe_state}" "${release_file}" env >"${fixture}/unsafe-state.log" 2>&1; then
  echo "unsafe state root was accepted" >&2
  exit 1
fi
grep -Fq 'state root must be owned by the launcher user with mode 0700' "${fixture}/unsafe-state.log"

symlink_state="${fixture}/symlink-state"
ln -s "${valid_state}" "${symlink_state}"
if run_controller "${symlink_state}" "${release_file}" env >"${fixture}/symlink-state.log" 2>&1; then
  echo "symlink state root was accepted" >&2
  exit 1
fi
grep -Fq 'state root must not be a symlink' "${fixture}/symlink-state.log"

echo "Controller lifecycle tests passed"
