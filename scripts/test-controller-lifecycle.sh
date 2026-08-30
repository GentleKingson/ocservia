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
  ps)
    if [[ -n "${MOCK_PS_JSON:-}" ]]; then
      printf '%s\n' "${MOCK_PS_JSON}"
    else
      printf '%s\n' \
        '{"Service":"postgres","State":"running","Health":"healthy"}' \
        '{"Service":"backup","State":"running","Health":"healthy"}'
    fi
    exit "${MOCK_PS_EXIT:-0}"
    ;;
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

run_controller_command() {
  local state="$1" selected="$2" command="$3"
  shift 3
  PATH="${bin}:${PATH}" \
    CONTROLLER_TEST_LOG="${state}/compose.log" \
    CONTROLLER_TEST_ENV_LOG="${state}/compose-env.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${state}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    "$@" "${CONTROLLER}" "${command}" --release-file "${selected}"
}

run_controller() {
  run_controller_command "$1" "$2" install "${@:3}"
}

run_controller_upgrade() {
  run_controller_command "$1" "$2" upgrade "${@:3}"
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

seed_upgrade_state() {
  local state="$1" previous="${2:-}"
  mkdir -m 700 -- "${state}"
  cp -- "${release_file}" "${state}/current-release.json"
  chmod 600 "${state}/current-release.json"
  if [[ -n "${previous}" ]]; then
    cp -- "${previous}" "${state}/previous-release.json"
    chmod 600 "${state}/previous-release.json"
  fi
}

expect_upgrade_failure() {
  local state="$1" selected="$2" expected_message="$3"
  shift 3
  if run_controller_upgrade "${state}" "${selected}" "$@" >"${state}/output.log" 2>&1; then
    echo "expected Controller upgrade to fail: ${expected_message}" >&2
    exit 1
  fi
  grep -Fq "${expected_message}" "${state}/output.log"
  test -f "${state}/current-release.json"
  test "$(find "${state}" -maxdepth 1 -name '.current-release.json.*' -print | wc -l)" -eq 0
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
malformed_digest="${fixture}/malformed-digest.json"
jq '.images.control = "ghcr.io/gentlekingson/ocservia/control@sha256:deadbeef"' "${release_file}" >"${malformed_digest}"

if ! run_controller_upgrade "${valid_state}" "${release_file}" env >"${fixture}/same-release.log" 2>&1; then
  echo "same-release upgrade was rejected" >&2
  exit 1
fi
grep -Fq 'is already current; no-op' "${fixture}/same-release.log"
test "$(wc -l <"${valid_state}/compose.log")" -eq 3
test ! -e "${valid_state}/previous-release.json"

next_digest="sha256:$(printf 'c%.0s' {1..64})"
next_release_file="${fixture}/release/controller-release-next.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
  --output "${next_release_file}" \
  --release-version 0.3.0 \
  --release-tag v0.3.0 \
  --source-commit "${commit}" \
  --migration-dir "${ROOT}/control-plane/migrations" \
  --image "gateway=ghcr.io/gentlekingson/ocservia/gateway@${next_digest}" \
  --image "control=ghcr.io/gentlekingson/ocservia/control@${next_digest}" \
  --image "transport=ghcr.io/gentlekingson/ocservia/transport@${next_digest}" \
  --image "backup=ghcr.io/gentlekingson/ocservia/backup@${next_digest}" \
  --image "postgres=docker.io/library/postgres@${next_digest}" \
  --image "otel=docker.io/otel/opentelemetry-collector@${next_digest}"

target_source_mismatch="${fixture}/release/controller-release-source-mismatch.json"
jq --arg source "$(printf 'd%.0s' {1..40})" '.source_commit = $source' \
  "${next_release_file}" >"${target_source_mismatch}"

target_source_state="${fixture}/target-source-mismatch"
seed_upgrade_state "${target_source_state}"
expect_upgrade_failure "${target_source_state}" "${target_source_mismatch}" \
  'checkout HEAD does not match release manifest source_commit' env
test ! -e "${target_source_state}/compose.log"

upgrade_success_state="${fixture}/upgrade-success"
seed_upgrade_state "${upgrade_success_state}"
run_controller_upgrade "${upgrade_success_state}" "${next_release_file}" env
cmp -s "${next_release_file}" "${upgrade_success_state}/current-release.json"
cmp -s "${release_file}" "${upgrade_success_state}/previous-release.json"
test "$(stat -c '%u:%a' "${upgrade_success_state}/current-release.json")" = "$(id -u):600"
test "$(stat -c '%u:%a' "${upgrade_success_state}/previous-release.json")" = "$(id -u):600"
test "$(sed -n '1p' "${upgrade_success_state}/compose.log")" = "ps --format json postgres backup"
test "$(sed -n '2p' "${upgrade_success_state}/compose.log")" = "config --quiet"
test "$(sed -n '3p' "${upgrade_success_state}/compose.log")" = "pull"
test "$(sed -n '4p' "${upgrade_success_state}/compose.log")" = "up -d --wait"
test "$(find "${upgrade_success_state}" -maxdepth 1 -name '.*release.json.*' -print | wc -l)" -eq 0
test "$(wc -l <"${upgrade_success_state}/compose-env.log")" -eq 4
[[ "$(sed -n '1p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '2p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '3p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '4p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]

downgrade_state="${fixture}/downgrade"
seed_upgrade_state "${downgrade_state}"
cp -- "${next_release_file}" "${downgrade_state}/current-release.json"
if run_controller_upgrade "${downgrade_state}" "${release_file}" env >"${downgrade_state}/output.log" 2>&1; then
  echo "downgrade was accepted" >&2
  exit 1
fi
grep -Fq 'upgrade does not perform downgrade' "${downgrade_state}/output.log"
cmp -s "${next_release_file}" "${downgrade_state}/current-release.json"
test ! -e "${downgrade_state}/compose.log"

invalid_upgrade_state="${fixture}/invalid-upgrade"
seed_upgrade_state "${invalid_upgrade_state}"
expect_upgrade_failure "${invalid_upgrade_state}" "${unsupported}" 'release manifest is invalid' env
test ! -e "${invalid_upgrade_state}/compose.log"

missing_digest_upgrade_state="${fixture}/missing-digest-upgrade"
seed_upgrade_state "${missing_digest_upgrade_state}"
expect_upgrade_failure "${missing_digest_upgrade_state}" "${malformed_digest}" 'release manifest is invalid' env
test ! -e "${missing_digest_upgrade_state}/compose.log"

for failure_case in config pull up; do
  failure_state="${fixture}/upgrade-${failure_case}-failure"
  seed_upgrade_state "${failure_state}" "${release_file}"
  case "${failure_case}" in
    config) failure_env=(MOCK_CONFIG_EXIT=1) ;;
    pull) failure_env=(MOCK_PULL_EXIT=1) ;;
    up) failure_env=(MOCK_UP_EXIT=1) ;;
  esac
  if run_controller_upgrade "${failure_state}" "${next_release_file}" env "${failure_env[@]}" >"${failure_state}/output.log" 2>&1; then
    echo "upgrade ${failure_case} failure was accepted" >&2
    exit 1
  fi
  cmp -s "${release_file}" "${failure_state}/current-release.json"
  cmp -s "${release_file}" "${failure_state}/previous-release.json"
  case "${failure_case}" in
    config)
      grep -Fq 'production preflight failed for target release' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 2
      ;;
    pull)
      grep -Fq 'target image pull failed' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 3
      ;;
    up)
      grep -Fq 'activation started but was not confirmed successful' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 4
      ;;
  esac
done

for unhealthy_case in postgres backup; do
  unhealthy_state="${fixture}/unhealthy-${unhealthy_case}"
  seed_upgrade_state "${unhealthy_state}"
  unhealthy_json='{"Service":"postgres","State":"running","Health":"healthy"}
{"Service":"backup","State":"running","Health":"healthy"}'
  if [[ "${unhealthy_case}" == postgres ]]; then
    unhealthy_json='{"Service":"postgres","State":"running","Health":"unhealthy"}
{"Service":"backup","State":"running","Health":"healthy"}'
  else
    unhealthy_json='{"Service":"postgres","State":"running","Health":"healthy"}
{"Service":"backup","State":"running","Health":"unhealthy"}'
  fi
  expect_upgrade_failure "${unhealthy_state}" "${next_release_file}" 'current PostgreSQL and backup services are not healthy' \
    env MOCK_PS_JSON="${unhealthy_json}"
  test "$(wc -l <"${unhealthy_state}/compose.log")" -eq 1
done

expect_failure "${fixture}/unsupported" "${unsupported}" 'release manifest is invalid' false env

missing_image="${fixture}/missing-image.json"
jq 'del(.images.gateway)' "${release_file}" >"${missing_image}"
expect_failure "${fixture}/missing-image" "${missing_image}" 'release manifest is invalid' false env

mutable_image="${fixture}/mutable-image.json"
jq '.images.gateway = "ghcr.io/gentlekingson/ocservia/gateway:latest"' "${release_file}" >"${mutable_image}"
expect_failure "${fixture}/mutable-image" "${mutable_image}" 'release manifest is invalid' false env

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
test "$(find "${retry_state}" -maxdepth 1 -name '.current-release.json.*' -print | wc -l)" -eq 0

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
