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

cat >"${bin}/controller-release-smoke.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CONTROLLER_TEST_SMOKE_LOG}"
[[ "${1:-}" == "--release-file" && -f "${2:-}" ]]
exit "${MOCK_SMOKE_EXIT:-0}"
EOF
chmod 0755 "${bin}/controller-release-smoke.sh"

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
    CONTROLLER_TEST_SMOKE_LOG="${state}/smoke.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${state}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    OCSERV_CONTROLLER_SMOKE_SH="${bin}/controller-release-smoke.sh" \
    "$@" "${CONTROLLER}" "${command}" --release-file "${selected}"
}

run_controller() {
  run_controller_command "$1" "$2" install "${@:3}"
}

run_controller_upgrade() {
  run_controller_command "$1" "$2" upgrade "${@:3}"
}

run_controller_rollback() {
  local state="$1"
  shift
  PATH="${bin}:${PATH}" \
    CONTROLLER_TEST_LOG="${state}/compose.log" \
    CONTROLLER_TEST_ENV_LOG="${state}/compose-env.log" \
    CONTROLLER_TEST_SMOKE_LOG="${state}/smoke.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${state}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    OCSERV_CONTROLLER_SMOKE_SH="${bin}/controller-release-smoke.sh" \
    "$@" "${CONTROLLER}" rollback
}

assert_pending_release() {
  local expected="$1" pending="$2"
  jq -e --slurpfile expected "${expected}" \
    '(.manifest == $expected[0])' "${pending}" >/dev/null
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
    assert_pending_release "${release_file}" "${state}/pending-release.json"
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

seed_pending_state() {
  local state="$1" manifest="$2" previous="${3:-}"
  if [[ -n "${previous}" ]]; then
    jq -s --slurpfile previous "${previous}" \
      '{manifest: .[0], previous_manifest: $previous[0], phase: "smoke", failure: null}' \
      "${manifest}" >"${state}/pending-release.json"
  else
    jq -s '{manifest: .[0], phase: "smoke", failure: null}' "${manifest}" \
      >"${state}/pending-release.json"
  fi
  chmod 600 "${state}/pending-release.json"
}

assert_pending_previous_release() {
  local expected="$1" pending="$2"
  jq -e --slurpfile expected "${expected}" \
    '(.previous_manifest == $expected[0])' "${pending}" >/dev/null
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
grep -Fq -- '--release-file ' "${valid_state}/smoke.log"
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

completed_install_state="${fixture}/completed-install"
seed_upgrade_state "${completed_install_state}"
seed_pending_state "${completed_install_state}" "${release_file}"
if run_controller "${completed_install_state}" "${release_file}" env \
  >"${completed_install_state}/output.log" 2>&1; then
  echo "completed install reconciliation was rejected" >&2
  exit 1
fi
grep -Fq 'Controller is already installed; use upgrade' "${completed_install_state}/output.log"
test ! -e "${completed_install_state}/pending-release.json"
test ! -e "${completed_install_state}/compose.log"

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

third_digest="sha256:$(printf 'd%.0s' {1..64})"
third_release_file="${fixture}/release/controller-release-third.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
  --output "${third_release_file}" \
  --release-version 0.4.0 \
  --release-tag v0.4.0 \
  --source-commit "${commit}" \
  --migration-dir "${ROOT}/control-plane/migrations" \
  --image "gateway=ghcr.io/gentlekingson/ocservia/gateway@${third_digest}" \
  --image "control=ghcr.io/gentlekingson/ocservia/control@${third_digest}" \
  --image "transport=ghcr.io/gentlekingson/ocservia/transport@${third_digest}" \
  --image "backup=ghcr.io/gentlekingson/ocservia/backup@${third_digest}" \
  --image "postgres=docker.io/library/postgres@${third_digest}" \
  --image "otel=docker.io/otel/opentelemetry-collector@${third_digest}"

stale_digest="sha256:$(printf 'a%.0s' {1..64})"
stale_release_file="${fixture}/release/controller-release-stale.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
  --output "${stale_release_file}" \
  --release-version 0.1.0 \
  --release-tag v0.1.0 \
  --source-commit "${commit}" \
  --migration-dir "${ROOT}/control-plane/migrations" \
  --image "gateway=ghcr.io/gentlekingson/ocservia/gateway@${stale_digest}" \
  --image "control=ghcr.io/gentlekingson/ocservia/control@${stale_digest}" \
  --image "transport=ghcr.io/gentlekingson/ocservia/transport@${stale_digest}" \
  --image "backup=ghcr.io/gentlekingson/ocservia/backup@${stale_digest}" \
  --image "postgres=docker.io/library/postgres@${stale_digest}" \
  --image "otel=docker.io/otel/opentelemetry-collector@${stale_digest}"

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
test "$(wc -l <"${upgrade_success_state}/smoke.log")" -eq 1
grep -Fq -- '--release-file ' "${upgrade_success_state}/smoke.log"
test "$(find "${upgrade_success_state}" -maxdepth 1 -name '.*release.json.*' -print | wc -l)" -eq 0
test "$(wc -l <"${upgrade_success_state}/compose-env.log")" -eq 4
[[ "$(sed -n '1p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '2p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '3p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '4p' "${upgrade_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]

rollback_success_state="${fixture}/rollback-success"
seed_upgrade_state "${rollback_success_state}"
cp -- "${next_release_file}" "${rollback_success_state}/current-release.json"
cp -- "${release_file}" "${rollback_success_state}/previous-release.json"
chmod 600 "${rollback_success_state}/current-release.json" "${rollback_success_state}/previous-release.json"
run_controller_rollback "${rollback_success_state}" env
cmp -s "${release_file}" "${rollback_success_state}/current-release.json"
cmp -s "${next_release_file}" "${rollback_success_state}/previous-release.json"
test ! -e "${rollback_success_state}/pending-release.json"
test "$(sed -n '1p' "${rollback_success_state}/compose.log")" = "ps --format json postgres backup"
test "$(sed -n '2p' "${rollback_success_state}/compose.log")" = "config --quiet"
test "$(sed -n '3p' "${rollback_success_state}/compose.log")" = "pull"
test "$(sed -n '4p' "${rollback_success_state}/compose.log")" = "up -d --wait"
test "$(wc -l <"${rollback_success_state}/smoke.log")" -eq 1
[[ "$(sed -n '1p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '2p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '3p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '4p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
test "$(wc -l <"${rollback_success_state}/compose-env.log")" -eq 4

missing_previous_state="${fixture}/rollback-missing-previous"
seed_upgrade_state "${missing_previous_state}"
if run_controller_rollback "${missing_previous_state}" env >"${missing_previous_state}/output.log" 2>&1; then
  echo "rollback without previous state was accepted" >&2
  exit 1
fi
grep -Fq 'previous release state is missing' "${missing_previous_state}/output.log"
test ! -e "${missing_previous_state}/compose.log"

malformed_previous_state="${fixture}/rollback-malformed-previous"
seed_upgrade_state "${malformed_previous_state}"
printf '%s\n' '{' >"${malformed_previous_state}/previous-release.json"
chmod 600 "${malformed_previous_state}/previous-release.json"
if run_controller_rollback "${malformed_previous_state}" env >"${malformed_previous_state}/output.log" 2>&1; then
  echo "malformed rollback previous state was accepted" >&2
  exit 1
fi
grep -Fq 'previous release state is invalid' "${malformed_previous_state}/output.log"
test ! -e "${malformed_previous_state}/compose.log"

symlink_previous_state="${fixture}/rollback-symlink-previous"
seed_upgrade_state "${symlink_previous_state}"
ln -s "${release_file}" "${symlink_previous_state}/previous-release.json"
if run_controller_rollback "${symlink_previous_state}" env >"${symlink_previous_state}/output.log" 2>&1; then
  echo "symlink rollback previous state was accepted" >&2
  exit 1
fi
grep -Fq 'previous release state must not be a symlink' "${symlink_previous_state}/output.log"
test ! -e "${symlink_previous_state}/compose.log"

same_release_state="${fixture}/rollback-same-release"
seed_upgrade_state "${same_release_state}"
cp -- "${release_file}" "${same_release_state}/previous-release.json"
chmod 600 "${same_release_state}/previous-release.json"
if run_controller_rollback "${same_release_state}" env >"${same_release_state}/output.log" 2>&1; then
  echo "rollback of the same release was accepted" >&2
  exit 1
fi
grep -Fq 'current and previous release states must not be identical' "${same_release_state}/output.log"
test ! -e "${same_release_state}/compose.log"

not_older_state="${fixture}/rollback-not-older"
seed_upgrade_state "${not_older_state}"
cp -- "${next_release_file}" "${not_older_state}/previous-release.json"
chmod 600 "${not_older_state}/previous-release.json"
if run_controller_rollback "${not_older_state}" env >"${not_older_state}/output.log" 2>&1; then
  echo "rollback to a newer release was accepted" >&2
  exit 1
fi
grep -Fq 'previous release version must be lower' "${not_older_state}/output.log"
test ! -e "${not_older_state}/compose.log"

different_schema_previous="${fixture}/release/controller-release-schema-different.json"
jq '.database_migration += 1' "${release_file}" >"${different_schema_previous}"
schema_state="${fixture}/rollback-schema-different"
seed_upgrade_state "${schema_state}"
cp -- "${next_release_file}" "${schema_state}/current-release.json"
cp -- "${different_schema_previous}" "${schema_state}/previous-release.json"
chmod 600 "${schema_state}/current-release.json" "${schema_state}/previous-release.json"
if run_controller_rollback "${schema_state}" env >"${schema_state}/output.log" 2>&1; then
  echo "cross-schema rollback was accepted" >&2
  exit 1
fi
grep -Fq 'cross-schema rollback is not supported' "${schema_state}/output.log"
test ! -e "${schema_state}/compose.log"

incompatible_pending_state="${fixture}/rollback-incompatible-pending"
seed_upgrade_state "${incompatible_pending_state}"
cp -- "${next_release_file}" "${incompatible_pending_state}/current-release.json"
cp -- "${release_file}" "${incompatible_pending_state}/previous-release.json"
chmod 600 "${incompatible_pending_state}/current-release.json" "${incompatible_pending_state}/previous-release.json"
seed_pending_state "${incompatible_pending_state}" "${release_file}"
if run_controller_rollback "${incompatible_pending_state}" env >"${incompatible_pending_state}/output.log" 2>&1; then
  echo "incompatible pending rollback transaction was accepted" >&2
  exit 1
fi
grep -Fq 'pending release transaction is not compatible with rollback' "${incompatible_pending_state}/output.log"
test ! -e "${incompatible_pending_state}/compose.log"

descriptor_change_commit="$(git -C "${ROOT}" log -1 --format='%H' -- deploy/production/compose.yaml)"
descriptor_previous_commit="$(git -C "${ROOT}" rev-parse "${descriptor_change_commit}^")"
descriptor_previous="${fixture}/release/controller-release-descriptor-previous.json"
jq --arg source "${descriptor_previous_commit}" '.source_commit = $source' "${release_file}" >"${descriptor_previous}"
descriptor_state="${fixture}/rollback-descriptor-change"
seed_upgrade_state "${descriptor_state}"
cp -- "${next_release_file}" "${descriptor_state}/current-release.json"
cp -- "${descriptor_previous}" "${descriptor_state}/previous-release.json"
chmod 600 "${descriptor_state}/current-release.json" "${descriptor_state}/previous-release.json"
if run_controller_rollback "${descriptor_state}" env >"${descriptor_state}/output.log" 2>&1; then
  echo "changed production deployment descriptor was accepted" >&2
  exit 1
fi
grep -Fq 'production deployment descriptor changed since previous release' "${descriptor_state}/output.log"
test ! -e "${descriptor_state}/compose.log"

unresolvable_previous="${fixture}/release/controller-release-unresolvable-previous.json"
jq --arg source "$(printf 'e%.0s' {1..40})" '.source_commit = $source' "${release_file}" >"${unresolvable_previous}"
unresolvable_state="${fixture}/rollback-unresolvable-previous"
seed_upgrade_state "${unresolvable_state}"
cp -- "${next_release_file}" "${unresolvable_state}/current-release.json"
cp -- "${unresolvable_previous}" "${unresolvable_state}/previous-release.json"
chmod 600 "${unresolvable_state}/current-release.json" "${unresolvable_state}/previous-release.json"
if run_controller_rollback "${unresolvable_state}" env >"${unresolvable_state}/output.log" 2>&1; then
  echo "unresolvable rollback source commit was accepted" >&2
  exit 1
fi
grep -Fq 'previous release source_commit cannot be resolved locally' "${unresolvable_state}/output.log"
test ! -e "${unresolvable_state}/compose.log"

for rollback_failure in config pull up; do
  failure_state="${fixture}/rollback-${rollback_failure}-failure"
  seed_upgrade_state "${failure_state}"
  cp -- "${next_release_file}" "${failure_state}/current-release.json"
  cp -- "${release_file}" "${failure_state}/previous-release.json"
  chmod 600 "${failure_state}/current-release.json" "${failure_state}/previous-release.json"
  case "${rollback_failure}" in
    config) failure_env=(MOCK_CONFIG_EXIT=1) ;;
    pull) failure_env=(MOCK_PULL_EXIT=1) ;;
    up) failure_env=(MOCK_UP_EXIT=1) ;;
  esac
  if run_controller_rollback "${failure_state}" env "${failure_env[@]}" >"${failure_state}/output.log" 2>&1; then
    echo "rollback ${rollback_failure} failure was accepted" >&2
    exit 1
  fi
  cmp -s "${next_release_file}" "${failure_state}/current-release.json"
  cmp -s "${release_file}" "${failure_state}/previous-release.json"
  assert_pending_release "${release_file}" "${failure_state}/pending-release.json"
  assert_pending_previous_release "${next_release_file}" "${failure_state}/pending-release.json"
  case "${rollback_failure}" in
    config)
      grep -Fq 'production preflight failed for rollback target' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 2
      ;;
    pull)
      grep -Fq 'rollback target image pull failed' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 3
      ;;
    up)
      grep -Fq 'rollback activation started but was not confirmed successful' "${failure_state}/output.log"
      test "$(wc -l <"${failure_state}/compose.log")" -eq 4
      ;;
  esac
done

rollback_smoke_failure_state="${fixture}/rollback-smoke-failure"
seed_upgrade_state "${rollback_smoke_failure_state}"
cp -- "${next_release_file}" "${rollback_smoke_failure_state}/current-release.json"
cp -- "${release_file}" "${rollback_smoke_failure_state}/previous-release.json"
chmod 600 "${rollback_smoke_failure_state}/current-release.json" "${rollback_smoke_failure_state}/previous-release.json"
if run_controller_rollback "${rollback_smoke_failure_state}" env MOCK_SMOKE_EXIT=1 \
  >"${rollback_smoke_failure_state}/output.log" 2>&1; then
  echo "rollback smoke failure was accepted" >&2
  exit 1
fi
grep -Fq 'release smoke failed; confirmed release state remains unchanged' "${rollback_smoke_failure_state}/output.log"
cmp -s "${next_release_file}" "${rollback_smoke_failure_state}/current-release.json"
cmp -s "${release_file}" "${rollback_smoke_failure_state}/previous-release.json"
assert_pending_release "${release_file}" "${rollback_smoke_failure_state}/pending-release.json"
assert_pending_previous_release "${next_release_file}" "${rollback_smoke_failure_state}/pending-release.json"
test "$(jq -r '.phase' "${rollback_smoke_failure_state}/pending-release.json")" = failed

rollback_retry_state="${fixture}/rollback-retry"
seed_upgrade_state "${rollback_retry_state}"
cp -- "${next_release_file}" "${rollback_retry_state}/current-release.json"
cp -- "${release_file}" "${rollback_retry_state}/previous-release.json"
chmod 600 "${rollback_retry_state}/current-release.json" "${rollback_retry_state}/previous-release.json"
if run_controller_rollback "${rollback_retry_state}" env MOCK_UP_EXIT=1 >"${rollback_retry_state}/first.log" 2>&1; then
  echo "initial rollback retry fixture unexpectedly succeeded" >&2
  exit 1
fi
test -f "${rollback_retry_state}/pending-release.json"
assert_pending_release "${release_file}" "${rollback_retry_state}/pending-release.json"
run_controller_rollback "${rollback_retry_state}" env
cmp -s "${release_file}" "${rollback_retry_state}/current-release.json"
cmp -s "${next_release_file}" "${rollback_retry_state}/previous-release.json"
test ! -e "${rollback_retry_state}/pending-release.json"

completed_upgrade_state="${fixture}/completed-upgrade"
seed_upgrade_state "${completed_upgrade_state}"
cp -- "${next_release_file}" "${completed_upgrade_state}/current-release.json"
cp -- "${release_file}" "${completed_upgrade_state}/previous-release.json"
chmod 600 "${completed_upgrade_state}/current-release.json" "${completed_upgrade_state}/previous-release.json"
seed_pending_state "${completed_upgrade_state}" "${next_release_file}" "${release_file}"
run_controller_upgrade "${completed_upgrade_state}" "${third_release_file}" env
cmp -s "${third_release_file}" "${completed_upgrade_state}/current-release.json"
cmp -s "${next_release_file}" "${completed_upgrade_state}/previous-release.json"
test ! -e "${completed_upgrade_state}/pending-release.json"

partial_upgrade_state="${fixture}/partial-upgrade"
seed_upgrade_state "${partial_upgrade_state}" "${stale_release_file}"
cp -- "${next_release_file}" "${partial_upgrade_state}/current-release.json"
seed_pending_state "${partial_upgrade_state}" "${next_release_file}" "${release_file}"
run_controller_upgrade "${partial_upgrade_state}" "${next_release_file}" env
cmp -s "${next_release_file}" "${partial_upgrade_state}/current-release.json"
cmp -s "${release_file}" "${partial_upgrade_state}/previous-release.json"
test ! -e "${partial_upgrade_state}/pending-release.json"
test ! -e "${partial_upgrade_state}/compose.log"

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
  assert_pending_release "${next_release_file}" "${failure_state}/pending-release.json"
  assert_pending_previous_release "${release_file}" "${failure_state}/pending-release.json"
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

smoke_failure_state="${fixture}/upgrade-smoke-failure"
seed_upgrade_state "${smoke_failure_state}" "${release_file}"
if run_controller_upgrade "${smoke_failure_state}" "${next_release_file}" env MOCK_SMOKE_EXIT=1 \
  >"${smoke_failure_state}/output.log" 2>&1; then
  echo "upgrade smoke failure was accepted" >&2
  exit 1
fi
grep -Fq 'release smoke failed; confirmed release state remains unchanged' "${smoke_failure_state}/output.log"
cmp -s "${release_file}" "${smoke_failure_state}/current-release.json"
cmp -s "${release_file}" "${smoke_failure_state}/previous-release.json"
assert_pending_release "${next_release_file}" "${smoke_failure_state}/pending-release.json"
assert_pending_previous_release "${release_file}" "${smoke_failure_state}/pending-release.json"
test "$(jq -r '.phase' "${smoke_failure_state}/pending-release.json")" = failed
test "$(jq -r '.failure.message' "${smoke_failure_state}/pending-release.json")" = \
  'release smoke failed; confirmed release state remains unchanged'

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
assert_pending_release "${release_file}" "${config_state}/pending-release.json"
test "$(wc -l <"${config_state}/compose.log")" -eq 1

pull_state="${fixture}/pull-failure"
mkdir -m 700 -- "${pull_state}"
if run_controller "${pull_state}" "${release_file}" env MOCK_PULL_EXIT=1 >"${pull_state}/output.log" 2>&1; then
  echo "pull failure was accepted" >&2
  exit 1
fi
test ! -e "${pull_state}/current-release.json"
test -f "${pull_state}/pending-release.json"
assert_pending_release "${release_file}" "${pull_state}/pending-release.json"
test "$(wc -l <"${pull_state}/compose.log")" -eq 2

up_state="${fixture}/up-failure"
mkdir -m 700 -- "${up_state}"
if run_controller "${up_state}" "${release_file}" env MOCK_UP_EXIT=1 >"${up_state}/output.log" 2>&1; then
  echo "up failure was accepted" >&2
  exit 1
fi
test ! -e "${up_state}/current-release.json"
test -f "${up_state}/pending-release.json"
assert_pending_release "${release_file}" "${up_state}/pending-release.json"
test "$(wc -l <"${up_state}/compose.log")" -eq 3

install_smoke_failure_state="${fixture}/install-smoke-failure"
mkdir -m 700 -- "${install_smoke_failure_state}"
if run_controller "${install_smoke_failure_state}" "${release_file}" env MOCK_SMOKE_EXIT=1 \
  >"${install_smoke_failure_state}/output.log" 2>&1; then
  echo "install smoke failure was accepted" >&2
  exit 1
fi
grep -Fq 'release smoke failed; confirmed release state remains unchanged' "${install_smoke_failure_state}/output.log"
test ! -e "${install_smoke_failure_state}/current-release.json"
assert_pending_release "${release_file}" "${install_smoke_failure_state}/pending-release.json"
test "$(jq -r '.phase' "${install_smoke_failure_state}/pending-release.json")" = failed

retry_state="${fixture}/retry"
mkdir -m 700 -- "${retry_state}"
if run_controller "${retry_state}" "${release_file}" env MOCK_UP_EXIT=1 >"${retry_state}/first.log" 2>&1; then
  echo "initial retry fixture unexpectedly succeeded" >&2
  exit 1
fi
test -f "${retry_state}/pending-release.json"
assert_pending_release "${release_file}" "${retry_state}/pending-release.json"

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
