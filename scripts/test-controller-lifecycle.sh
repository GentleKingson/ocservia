#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROLLER="${ROOT}/deploy/production/controller.sh"
unset OCSERV_OTEL_BACKEND_ENDPOINT

if ! command -v flock >/dev/null 2>&1; then
  echo "Controller lifecycle tests skipped: flock is unavailable"
  exit 0
fi

fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-controller-test.XXXXXX")"
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
if [[ "${1:-}" == network ]]; then
  echo 'lifecycle must not inspect or convert legacy networks' >&2
  exit 97
fi
if [[ "${1:-}" == version ]]; then
  printf '%s\n' "${MOCK_DOCKER_ARCH:-amd64}"
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
  config)
    [[ "$*" == 'config --quiet' ]]
    exit "${MOCK_CONFIG_EXIT:-0}"
    ;;
  pull) exit "${MOCK_PULL_EXIT:-0}" ;;
  stop) exit 0 ;;
  up)
    exit "${MOCK_UP_EXIT:-0}"
    ;;
  create|start) [[ "$2" == backup ]] ;;
  down)
    [[ "${COMPOSE_PROJECT_NAME:-}" == ocservia-production ]]
    exit "${MOCK_DOWN_EXIT:-0}"
    ;;
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
  run)
    if [[ "${4:-}" == signer ]]; then
      case "${5:-}" in
        init) exit "${MOCK_SIGNER_INIT_EXIT:-0}" ;;
        inspect)
          [[ "${MOCK_SIGNER_INSPECT_EXIT:-0}" == 0 ]] || exit 1
          printf '{"state_version":1,"issuer_sha256":"%064d","policy":"rsa-client-v1","revision":%s}\n' 1 "${MOCK_SIGNER_REVISION:-4}"
          exit 0
          ;;
        *) exit 97 ;;
      esac
    fi
    exit 97
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

make_manifest() {
  local output="$1" version="$2" digest="$3" arch="${4:-amd64}"
  local images=() role
  for role in gateway control transport backup; do
    images+=(--image "${role}=ghcr.io/gentlekingson/ocservia/${role}@${digest}")
  done
  node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
    --output "${output}" --release-version "${version}" --release-tag "v${version}" \
    --source-commit "${commit}" --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${arch}" \
    "${images[@]}" --image "postgres=docker.io/library/postgres@${digest}" --image "otel=docker.io/otel/opentelemetry-collector@${digest}"
}
release_file="${fixture}/release/controller-release.json"
arm64_release_file="${fixture}/release/controller-release-arm64.json"
make_manifest "${release_file}" 0.2.0 "${digest}"
make_manifest "${arm64_release_file}" 0.2.0 "${digest}" arm64

with_controller_env() {
  local state="$1"
  shift
  PATH="${bin}:${PATH}" \
    CONTROLLER_TEST_LOG="${state}/compose.log" \
    CONTROLLER_TEST_ENV_LOG="${state}/compose-env.log" \
    CONTROLLER_TEST_SMOKE_LOG="${state}/smoke.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${state}" \
    OCSERV_CONTROLLER_COMPOSE_SH="${bin}/compose.sh" \
    OCSERV_CONTROLLER_SMOKE_SH="${bin}/controller-release-smoke.sh" \
    "$@"
}

run_controller() {
  with_controller_env "$1" "${@:3}" "${CONTROLLER}" install --release-file "$2"
}
run_controller_upgrade() {
  with_controller_env "$1" "${@:3}" "${CONTROLLER}" upgrade --release-file "$2"
}
run_controller_rollback() {
  with_controller_env "$1" "${@:2}" "${CONTROLLER}" rollback
}
run_controller_start() {
  with_controller_env "$1" "${CONTROLLER}" start "${@:2}"
}
run_controller_uninstall() {
  with_controller_env "$1" "${CONTROLLER}" uninstall "${@:2}"
}

expect_rejection() {
  local log="$1" message="$2"
  shift 2
  if "$@" >"${log}" 2>&1; then
    echo "expected rejection: ${message}" >&2
    exit 1
  fi
  grep -Fq -- "${message}" "${log}" || { cat "${log}" >&2; exit 1; }
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

# The same generated RC manifest must pass the runtime validator and install.
rc_release="${fixture}/release/rc.json"
rc_images=()
for role in gateway control transport backup postgres otel; do
  rc_images+=(--image "${role}=registry.test/${role}@${digest}")
done
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" \
  --output "${rc_release}" --release-version 0.2.0-rc.1 --release-tag v0.2.0-rc.1 \
  --source-commit "${commit}" --platform linux/amd64 "${rc_images[@]}"
mkdir -m 700 "${fixture}/rc-state"
run_controller "${fixture}/rc-state" "${rc_release}" env
jq -e '.release_version == "0.2.0-rc.1"' "${fixture}/rc-state/current-release.json" >/dev/null
for invalid in 0.2.0-rc.0 0.2.0-rc.01 0.2.0-beta.1 0.2.0-rc.1+build; do
  jq --arg version "${invalid}" '.release_version=$version | .release_tag=("v"+$version)' \
    "${rc_release}" >"${fixture}/release/invalid-rc.json"
  expect_failure "${fixture}/${invalid}" "${fixture}/release/invalid-rc.json" 'invalid' false env
done

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

seed_rollback_state() {
  local state="$1" current="${2:-${next_release_file}}" previous="${3:-${release_file}}"
  mkdir -m 700 -- "${state}"
  install -m 600 "${current}" "${state}/current-release.json"
  install -m 600 "${previous}" "${state}/previous-release.json"
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

platform_mismatch_state="${fixture}/platform-mismatch"
expect_failure "${platform_mismatch_state}" "${arm64_release_file}" \
  'release manifest platform linux/arm64 does not match the Docker host platform linux/amd64' false env
test ! -e "${platform_mismatch_state}/compose.log"

unsupported_host_state="${fixture}/unsupported-host-arch"
expect_failure "${unsupported_host_state}" "${release_file}" \
  'Docker server architecture is not a supported Controller platform: riscv64' false \
  env MOCK_DOCKER_ARCH=riscv64
test ! -e "${unsupported_host_state}/compose.log"

arm64_host_state="${fixture}/arm64-host"
mkdir -m 700 -- "${arm64_host_state}"
run_controller "${arm64_host_state}" "${arm64_release_file}" env MOCK_DOCKER_ARCH=arm64
test -f "${arm64_host_state}/current-release.json"
cmp -s "${arm64_release_file}" "${arm64_host_state}/current-release.json"
test ! -e "${arm64_host_state}/pending-release.json"

relative_state="${fixture}/relative"
mkdir -m 700 -- "${relative_state}"
(cd "${fixture}/release" && run_controller "${relative_state}" controller-release.json env)
cmp -s "${release_file}" "${relative_state}/current-release.json"
test ! -e "${relative_state}/pending-release.json"

expect_rejection "${fixture}/already-installed.log" 'Controller is already installed; use upgrade' \
  run_controller "${valid_state}" "${release_file}" env
test "$(wc -l <"${valid_state}/compose.log")" -eq 3

completed_install_state="${fixture}/completed-install"
seed_upgrade_state "${completed_install_state}"
seed_pending_state "${completed_install_state}" "${release_file}"
expect_rejection "${completed_install_state}/output.log" 'Controller is already installed; use upgrade' \
  run_controller "${completed_install_state}" "${release_file}" env
test ! -e "${completed_install_state}/pending-release.json"
test ! -e "${completed_install_state}/compose.log"

unsupported="${fixture}/release/unsupported.json"
jq '.manifest_version = 2' "${release_file}" >"${unsupported}"
malformed_digest="${fixture}/release/malformed-digest.json"
jq '.images.control = "ghcr.io/gentlekingson/ocservia/control@sha256:deadbeef"' "${release_file}" >"${malformed_digest}"

run_controller_upgrade "${valid_state}" "${release_file}" env >"${fixture}/same-release.log" 2>&1
grep -Fq 'is already current; no-op' "${fixture}/same-release.log"
test "$(wc -l <"${valid_state}/compose.log")" -eq 3
test ! -e "${valid_state}/previous-release.json"

next_digest="sha256:$(printf 'c%.0s' {1..64})"
next_release_file="${fixture}/release/controller-release-next.json"
make_manifest "${next_release_file}" 0.3.0 "${next_digest}"

third_digest="sha256:$(printf 'd%.0s' {1..64})"
third_release_file="${fixture}/release/controller-release-third.json"
make_manifest "${third_release_file}" 0.4.0 "${third_digest}"

stale_digest="sha256:$(printf 'a%.0s' {1..64})"
stale_release_file="${fixture}/release/controller-release-stale.json"
make_manifest "${stale_release_file}" 0.1.0 "${stale_digest}"

target_source_mismatch="${fixture}/release/controller-release-source-mismatch.json"
jq --arg source "$(printf 'd%.0s' {1..40})" '.source_commit = $source' \
  "${next_release_file}" >"${target_source_mismatch}"

target_source_state="${fixture}/target-source-mismatch"
seed_upgrade_state "${target_source_state}"
expect_upgrade_failure "${target_source_state}" "${target_source_mismatch}" \
  'release source_commit cannot be resolved locally' env
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
seed_rollback_state "${rollback_success_state}"
run_controller_rollback "${rollback_success_state}" env MOCK_PS_JSON='{"Service":"postgres","State":"running","Health":"healthy"}
{"Service":"backup","State":"running","Health":"unhealthy"}'
cmp -s "${release_file}" "${rollback_success_state}/current-release.json"
cmp -s "${next_release_file}" "${rollback_success_state}/previous-release.json"
test ! -e "${rollback_success_state}/pending-release.json"
test "$(sed -n '1p' "${rollback_success_state}/compose.log")" = "ps --format json postgres"
test "$(sed -n '2p' "${rollback_success_state}/compose.log")" = "config --quiet"
test "$(sed -n '3p' "${rollback_success_state}/compose.log")" = "pull"
test "$(sed -n '4p' "${rollback_success_state}/compose.log")" = "up -d --wait gateway control-plane transportd"
test "$(sed -n '5p' "${rollback_success_state}/compose.log")" = "create backup"
test "$(sed -n '6p' "${rollback_success_state}/compose.log")" = "start backup"
test "$(wc -l <"${rollback_success_state}/smoke.log")" -eq 1
grep -Fq -- ' --skip-backup' "${rollback_success_state}/smoke.log"
[[ "$(sed -n '1p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${next_digest}"$'\t'* ]]
[[ "$(sed -n '2p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '3p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
[[ "$(sed -n '4p' "${rollback_success_state}/compose-env.log")" == "ghcr.io/gentlekingson/ocservia/gateway@${digest}"$'\t'* ]]
test "$(wc -l <"${rollback_success_state}/compose-env.log")" -eq 6

rollback_unhealthy_database_state="${fixture}/rollback-unhealthy-database"
seed_rollback_state "${rollback_unhealthy_database_state}"
expect_rejection "${rollback_unhealthy_database_state}/output.log" 'current PostgreSQL services are not healthy' \
  run_controller_rollback "${rollback_unhealthy_database_state}" env \
  MOCK_PS_JSON='{"Service":"postgres","State":"running","Health":"unhealthy"}'
cmp -s "${next_release_file}" "${rollback_unhealthy_database_state}/current-release.json"

missing_previous_state="${fixture}/rollback-missing-previous"
seed_upgrade_state "${missing_previous_state}"
expect_rejection "${missing_previous_state}/output.log" 'previous release state is missing' \
  run_controller_rollback "${missing_previous_state}" env
test ! -e "${missing_previous_state}/compose.log"

malformed_previous_state="${fixture}/rollback-malformed-previous"
seed_upgrade_state "${malformed_previous_state}"
printf '%s\n' '{' >"${malformed_previous_state}/previous-release.json"
chmod 600 "${malformed_previous_state}/previous-release.json"
expect_rejection "${malformed_previous_state}/output.log" 'previous release state is invalid' \
  run_controller_rollback "${malformed_previous_state}" env
test ! -e "${malformed_previous_state}/compose.log"

symlink_previous_state="${fixture}/rollback-symlink-previous"
seed_upgrade_state "${symlink_previous_state}"
ln -s "${release_file}" "${symlink_previous_state}/previous-release.json"
expect_rejection "${symlink_previous_state}/output.log" 'previous release state must not be a symlink' \
  run_controller_rollback "${symlink_previous_state}" env
test ! -e "${symlink_previous_state}/compose.log"

same_release_state="${fixture}/rollback-same-release"
seed_upgrade_state "${same_release_state}"
cp -- "${release_file}" "${same_release_state}/previous-release.json"
chmod 600 "${same_release_state}/previous-release.json"
run_controller_rollback "${same_release_state}" env
cmp -s "${release_file}" "${same_release_state}/current-release.json"
test ! -e "${same_release_state}/compose.log"

not_older_state="${fixture}/rollback-not-older"
seed_upgrade_state "${not_older_state}"
cp -- "${next_release_file}" "${not_older_state}/previous-release.json"
chmod 600 "${not_older_state}/previous-release.json"
run_controller_rollback "${not_older_state}" env
cmp -s "${next_release_file}" "${not_older_state}/current-release.json"
cmp -s "${release_file}" "${not_older_state}/previous-release.json"
test -s "${not_older_state}/compose.log"

different_schema_previous="${fixture}/release/controller-release-schema-different.json"
cross_schema_current="${fixture}/release/controller-release-schema-current.json"
jq '.database_migration.revision += 1' "${next_release_file}" >"${cross_schema_current}"
cp -- "${release_file}" "${different_schema_previous}"
for schema in 1 100; do
  schema_state="${fixture}/rollback-schema-${schema}"
  seed_upgrade_state "${schema_state}"
  cp -- "${next_release_file}" "${schema_state}/current-release.json"
  jq --argjson schema "${schema}" '.database_migration=$schema' "${release_file}" >"${schema_state}/previous-release.json"
  chmod 600 "${schema_state}/current-release.json" "${schema_state}/previous-release.json"
  run_controller_rollback "${schema_state}" env
  test "$(jq -r '.database_migration' "${schema_state}/current-release.json")" = "${schema}"
  test "$(sed -n '4p' "${schema_state}/compose.log")" = 'up -d --wait gateway control-plane transportd'
  test "$(wc -l <"${schema_state}/compose.log")" -eq 6
done
incompatible_pending_state="${fixture}/rollback-incompatible-pending"
seed_rollback_state "${incompatible_pending_state}"
seed_pending_state "${incompatible_pending_state}" "${release_file}"
expect_rejection "${incompatible_pending_state}/output.log" 'pending release transaction is not compatible with rollback' \
  run_controller_rollback "${incompatible_pending_state}" env
test ! -e "${incompatible_pending_state}/compose.log"

# The lifecycle implementation stays current, but target deployment files come
# from the manifest commit, even when descriptors differ. No historical release.
source_repo="${fixture}/source-repo"
mkdir -p "${source_repo}/deploy/production" "${source_repo}/scripts"
cp "${CONTROLLER}" "${source_repo}/deploy/production/controller.sh"
cp "${bin}/controller-release-smoke.sh" "${source_repo}/deploy/production/"
cat >"${source_repo}/deploy/production/compose.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo target >>"${CONTROLLER_TEST_SOURCE_LOG}"
exec "${SOURCE_TEST_COMPOSE}" "$@"
EOF
chmod 755 "${source_repo}/deploy/production/compose.sh"
git -C "${source_repo}" init -q
git -C "${source_repo}" add .
git -C "${source_repo}" -c user.name=Test -c user.email=test@example.invalid commit -qm target
source_target="$(git -C "${source_repo}" rev-parse HEAD)"
sed -i 's/echo target/echo current/' "${source_repo}/deploy/production/compose.sh"
git -C "${source_repo}" add .
git -C "${source_repo}" -c user.name=Test -c user.email=test@example.invalid commit -qm current
source_current="$(git -C "${source_repo}" rev-parse HEAD)"
source_state="${fixture}/source-state"
seed_upgrade_state "${source_state}"
jq --arg source "${source_current}" '.source_commit=$source' "${next_release_file}" >"${source_state}/current-release.json"
jq --arg source "${source_target}" '.source_commit=$source' "${release_file}" >"${source_state}/previous-release.json"
chmod 600 "${source_state}/current-release.json" "${source_state}/previous-release.json"
saved_controller="${CONTROLLER}"
CONTROLLER="${source_repo}/deploy/production/controller.sh"
source_env=(env OCSERV_CONTROLLER_COMPOSE_SH= OCSERV_CONTROLLER_SMOKE_SH=
  "CONTROLLER_TEST_SOURCE_LOG=${source_state}/sources.log" "SOURCE_TEST_COMPOSE=${bin}/compose.sh")
run_controller_rollback "${source_state}" "${source_env[@]}"
test "$(cat "${source_state}/sources.log")" = "$(printf 'current\ntarget\ntarget\ntarget\ntarget\ntarget')"
test "$(git -C "${source_state}/source-${source_target}" rev-parse HEAD)" = "${source_target}"
test "$(jq -r .source_commit "${source_state}/current-release.json")" = "${source_target}"
: >"${source_state}/sources.log"
run_source_start() {
  PATH="${bin}:${PATH}" CONTROLLER_TEST_LOG="${source_state}/compose.log" \
    CONTROLLER_TEST_ENV_LOG="${source_state}/compose-env.log" \
    CONTROLLER_TEST_SMOKE_LOG="${source_state}/smoke.log" \
    OCSERV_CONTROLLER_STATE_ROOT="${source_state}" \
    "${source_env[@]}" "${CONTROLLER}" start
}
run_source_start
test "$(cat "${source_state}/sources.log")" = "$(printf 'target\ntarget\ntarget')"
printf '\n# tampered\n' >>"${source_state}/source-${source_target}/deploy/production/compose.sh"
: >"${source_state}/sources.log"
expect_rejection "${source_state}/dirty.log" 'checkout has unstaged changes' \
  run_source_start
test ! -s "${source_state}/sources.log"
CONTROLLER="${saved_controller}"

unresolvable_previous="${fixture}/release/controller-release-unresolvable-previous.json"
jq --arg source "$(printf 'e%.0s' {1..40})" '.source_commit = $source' "${release_file}" >"${unresolvable_previous}"
unresolvable_state="${fixture}/rollback-unresolvable-previous"
seed_rollback_state "${unresolvable_state}" "${next_release_file}" "${unresolvable_previous}"
expect_rejection "${unresolvable_state}/output.log" 'release source_commit cannot be resolved locally' \
  run_controller_rollback "${unresolvable_state}" env
test ! -e "${unresolvable_state}/compose.log"

rollback_cross_schema_smoke_failure_state="${fixture}/rollback-cross-schema-smoke-failure"
seed_rollback_state "${rollback_cross_schema_smoke_failure_state}" "${cross_schema_current}" "${different_schema_previous}"
expect_rejection "${rollback_cross_schema_smoke_failure_state}/output.log" 'release smoke failed; confirmed release state remains unchanged' \
  run_controller_rollback "${rollback_cross_schema_smoke_failure_state}" env \
  MOCK_REQUIRE_CROSS_SCHEMA_ACTIVATION=1 MOCK_SMOKE_EXIT=1
cmp -s "${cross_schema_current}" "${rollback_cross_schema_smoke_failure_state}/current-release.json"
cmp -s "${different_schema_previous}" "${rollback_cross_schema_smoke_failure_state}/previous-release.json"
assert_pending_release "${different_schema_previous}" "${rollback_cross_schema_smoke_failure_state}/pending-release.json"
assert_pending_previous_release "${cross_schema_current}" "${rollback_cross_schema_smoke_failure_state}/pending-release.json"
test "$(jq -r '.phase' "${rollback_cross_schema_smoke_failure_state}/pending-release.json")" = failed

rollback_retry_state="${fixture}/rollback-retry"
seed_rollback_state "${rollback_retry_state}"
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
seed_rollback_state "${completed_upgrade_state}"
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
run_controller_upgrade "${downgrade_state}" "${release_file}" env
cmp -s "${release_file}" "${downgrade_state}/current-release.json"
cmp -s "${next_release_file}" "${downgrade_state}/previous-release.json"
test -s "${downgrade_state}/compose.log"

same_version_target="${fixture}/release/same-version-different-artifact.json"
jq '.release_version="0.2.0" | .release_tag="v0.2.0"' "${next_release_file}" >"${same_version_target}"
same_version_state="${fixture}/same-version-different-artifact"
seed_upgrade_state "${same_version_state}"
run_controller_upgrade "${same_version_state}" "${same_version_target}" env
cmp -s "${same_version_target}" "${same_version_state}/current-release.json"
cmp -s "${release_file}" "${same_version_state}/previous-release.json"
test -s "${same_version_state}/compose.log"
: >"${same_version_state}/compose.log"
run_controller_upgrade "${same_version_state}" "${same_version_target}" env
test ! -s "${same_version_state}/compose.log"

smoke_failure_state="${fixture}/upgrade-smoke-failure"
seed_upgrade_state "${smoke_failure_state}" "${release_file}"
expect_rejection "${smoke_failure_state}/output.log" 'release smoke failed; confirmed release state remains unchanged' \
  run_controller_upgrade "${smoke_failure_state}" "${next_release_file}" env MOCK_SMOKE_EXIT=1
cmp -s "${release_file}" "${smoke_failure_state}/current-release.json"
cmp -s "${release_file}" "${smoke_failure_state}/previous-release.json"
assert_pending_release "${next_release_file}" "${smoke_failure_state}/pending-release.json"
assert_pending_previous_release "${release_file}" "${smoke_failure_state}/pending-release.json"
test "$(jq -r '.phase' "${smoke_failure_state}/pending-release.json")" = failed
test "$(jq -r '.failure.message' "${smoke_failure_state}/pending-release.json")" = \
  'release smoke failed; confirmed release state remains unchanged'

unhealthy_state="${fixture}/unhealthy-backup"
seed_upgrade_state "${unhealthy_state}"
expect_upgrade_failure "${unhealthy_state}" "${next_release_file}" 'current PostgreSQL and backup services are not healthy' \
  env MOCK_PS_JSON='{"Service":"postgres","State":"running","Health":"healthy"}
{"Service":"backup","State":"running","Health":"unhealthy"}'
test "$(wc -l <"${unhealthy_state}/compose.log")" -eq 1

expect_failure "${fixture}/unsupported" "${unsupported}" 'release manifest is invalid' false env

missing_image="${fixture}/release/missing-image.json"
jq 'del(.images.gateway)' "${release_file}" >"${missing_image}"
expect_failure "${fixture}/missing-image" "${missing_image}" 'release manifest is invalid' false env

mutable_image="${fixture}/release/mutable-image.json"
jq '.images.gateway = "ghcr.io/gentlekingson/ocservia/gateway:latest"' "${release_file}" >"${mutable_image}"
expect_failure "${fixture}/mutable-image" "${mutable_image}" 'release manifest is invalid' false env

expect_failure "${fixture}/malformed-digest" "${malformed_digest}" 'release manifest is invalid' false env

malformed_json="${fixture}/release/malformed-json.json"
printf '%s\n' '{' >"${malformed_json}"
expect_failure "${fixture}/malformed-json" "${malformed_json}" 'release manifest is invalid' false env

symlink_release="${fixture}/release-symlink.json"
ln -s "${release_file}" "${symlink_release}"
expect_failure "${fixture}/symlink-release" "${symlink_release}" 'must not contain symlink ancestry' false env

source_mismatch="${fixture}/release/source-mismatch.json"
jq --arg source "$(printf 'c%.0s' {1..40})" '.source_commit = $source' "${release_file}" >"${source_mismatch}"
expect_failure "${fixture}/source-mismatch" "${source_mismatch}" \
  'release source_commit cannot be resolved locally' false env

# Failed phases retain the manifest pair for retry and leave the confirmed release intact.
for action in install upgrade rollback; do
  for phase in config pull up; do
    state="${fixture}/${action}-${phase}"
    fault="MOCK_${phase^^}_EXIT=1"
    case "${action}" in
      install)
        mkdir -m 700 "${state}"
        target="${release_file}"
        command=(run_controller "${state}" "${target}" env "${fault}")
        ;;
      upgrade)
        seed_upgrade_state "${state}" "${release_file}"
        target="${next_release_file}"
        previous="${release_file}"
        command=(run_controller_upgrade "${state}" "${target}" env "${fault}")
        ;;
      rollback)
        seed_rollback_state "${state}"
        target="${release_file}"
        previous="${next_release_file}"
        command=(run_controller_rollback "${state}" env "${fault}")
        ;;
    esac
    if "${command[@]}" >"${state}/output.log" 2>&1; then
      echo "${action} accepted ${phase} failure" >&2
      exit 1
    fi
    assert_pending_release "${target}" "${state}/pending-release.json"
    if [[ "${action}" == install ]]; then
      test ! -e "${state}/current-release.json"
    else
      cmp "${previous}" "${state}/current-release.json"
      assert_pending_previous_release "${previous}" "${state}/pending-release.json"
    fi
  done
done

install_smoke_failure_state="${fixture}/install-smoke-failure"
mkdir -m 700 -- "${install_smoke_failure_state}"
expect_rejection "${install_smoke_failure_state}/output.log" 'release smoke failed; confirmed release state remains unchanged' \
  run_controller "${install_smoke_failure_state}" "${release_file}" env MOCK_SMOKE_EXIT=1
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

different_pending="${fixture}/release/different-pending.json"
other_digest="sha256:$(printf 'c%.0s' {1..64})"
jq --arg image "ghcr.io/gentlekingson/ocservia/gateway@${other_digest}" \
  '.images.gateway = $image' "${release_file}" >"${different_pending}"
expect_rejection "${retry_state}/different.log" 'a different pending release exists; only the same manifest may be retried' \
  run_controller "${retry_state}" "${different_pending}" env
test "$(wc -l <"${retry_state}/compose.log")" -eq 3

run_controller "${retry_state}" "${release_file}" env
cmp -s "${release_file}" "${retry_state}/current-release.json"
test ! -e "${retry_state}/pending-release.json"
test "$(find "${retry_state}" -maxdepth 1 -name '.current-release.json.*' -print | wc -l)" -eq 0

state="${fixture}/lock_state"
mkdir -m 700 "${state}"
(
  exec 9>"${state}/lifecycle.lock"
  chmod 600 "${state}/lifecycle.lock"
  flock -n 9
  expect_rejection "${state}/output.log" 'another Controller lifecycle command is already running' run_controller "${state}" "${release_file}" env
)
test ! -e "${state}/compose.log"

unsafe_state="${fixture}/unsafe-state"
mkdir -m 750 -- "${unsafe_state}"
expect_rejection "${fixture}/unsafe-state.log" 'state root must be owned by the launcher user with mode 0700' \
  run_controller "${unsafe_state}" "${release_file}" env

symlink_state="${fixture}/symlink-state"
ln -s "${valid_state}" "${symlink_state}"
expect_rejection "${fixture}/symlink-state.log" 'state root must not be a symlink' \
  run_controller "${symlink_state}" "${release_file}" env

uninstall_source_mismatch_state="${fixture}/uninstall-source-mismatch"
mkdir -m 700 -- "${uninstall_source_mismatch_state}"
cp -- "${source_mismatch}" "${uninstall_source_mismatch_state}/current-release.json"
chmod 600 "${uninstall_source_mismatch_state}/current-release.json"
expect_rejection "${uninstall_source_mismatch_state}/output.log" 'release source_commit cannot be resolved locally' \
  run_controller_uninstall "${uninstall_source_mismatch_state}"
test ! -e "${uninstall_source_mismatch_state}/compose.log"

uninstall_state="${fixture}/uninstall-default"
seed_upgrade_state "${uninstall_state}" "${next_release_file}"
uninstall_backup="${uninstall_state}/operator-backups"
uninstall_secret="${uninstall_state}/operator-secrets"
mkdir -m 700 -- "${uninstall_backup}" "${uninstall_secret}"
printf '%s\n' backup >"${uninstall_backup}/sentinel"
printf '%s\n' secret >"${uninstall_secret}/sentinel"
OCSERV_BACKUP_DIR="${uninstall_backup}" OCSERV_SECRET_DIR="${uninstall_secret}" \
  run_controller_uninstall "${uninstall_state}"
test "$(sed -n '1p' "${uninstall_state}/compose.log")" = "down"
test "$(wc -l <"${uninstall_state}/compose.log")" -eq 1
cmp -s "${release_file}" "${uninstall_state}/current-release.json"
cmp -s "${next_release_file}" "${uninstall_state}/previous-release.json"
test -f "${uninstall_state}/lifecycle.lock"
test "$(cat "${uninstall_backup}/sentinel")" = backup
test "$(cat "${uninstall_secret}/sentinel")" = secret
OCSERV_BACKUP_DIR="${uninstall_backup}" OCSERV_SECRET_DIR="${uninstall_secret}" \
  run_controller_uninstall "${uninstall_state}"
test "$(sed -n '2p' "${uninstall_state}/compose.log")" = "down"

start_state="${fixture}/start"
seed_upgrade_state "${start_state}"
run_controller_start "${start_state}"
test "$(sed -n '1p' "${start_state}/compose.log")" = "config --quiet"
test "$(sed -n '2p' "${start_state}/compose.log")" = "pull"
test "$(sed -n '3p' "${start_state}/compose.log")" = "up -d --wait"
test "$(wc -l <"${start_state}/smoke.log")" -eq 1
grep -Fq -- '--release-file ' "${start_state}/smoke.log"
IFS=$'\t' read -r start_gateway start_control start_transport start_backup start_postgres start_otel \
  <"${start_state}/compose-env.log"
test "${start_gateway}" = "ghcr.io/gentlekingson/ocservia/gateway@${digest}"
test "${start_control}" = "ghcr.io/gentlekingson/ocservia/control@${digest}"
test "${start_transport}" = "ghcr.io/gentlekingson/ocservia/transport@${digest}"
test "${start_backup}" = "ghcr.io/gentlekingson/ocservia/backup@${digest}"
test "${start_postgres}" = "docker.io/library/postgres@${digest}"
test "${start_otel}" = "docker.io/otel/opentelemetry-collector@${digest}"

uninstall_failure_state="${fixture}/uninstall-failure"
seed_upgrade_state "${uninstall_failure_state}" "${next_release_file}"
export MOCK_DOWN_EXIT=1
if run_controller_uninstall "${uninstall_failure_state}" \
  >"${uninstall_failure_state}/output.log" 2>&1; then
  unset MOCK_DOWN_EXIT
  echo "uninstall failure was accepted" >&2
  exit 1
fi
unset MOCK_DOWN_EXIT
grep -Fq 'Controller Compose down failed; lifecycle state and persistent data were retained' \
  "${uninstall_failure_state}/output.log"
test -f "${uninstall_failure_state}/current-release.json"
test -f "${uninstall_failure_state}/previous-release.json"

pending_uninstall_state="${fixture}/uninstall-pending"
seed_upgrade_state "${pending_uninstall_state}" "${next_release_file}"
seed_pending_state "${pending_uninstall_state}" "${next_release_file}" "${release_file}"
expect_rejection "${pending_uninstall_state}/output.log" 'pending release transaction exists; refusing to uninstall' \
  run_controller_uninstall "${pending_uninstall_state}"
test ! -e "${pending_uninstall_state}/compose.log"

purge_state="${fixture}/uninstall-purge"
seed_upgrade_state "${purge_state}" "${next_release_file}"
purge_backup="${purge_state}/operator-backups"
purge_secret="${purge_state}/operator-secrets"
mkdir -m 700 -- "${purge_backup}" "${purge_secret}"
printf '%s\n' backup >"${purge_backup}/sentinel"
printf '%s\n' secret >"${purge_secret}/sentinel"
OCSERV_BACKUP_DIR="${purge_backup}" OCSERV_SECRET_DIR="${purge_secret}" \
  run_controller_uninstall "${purge_state}" --purge-data
test "$(sed -n '1p' "${purge_state}/compose.log")" = "down --volumes"
test ! -e "${purge_state}/current-release.json"
test ! -e "${purge_state}/previous-release.json"
test ! -e "${purge_state}/pending-release.json"
test -f "${purge_state}/lifecycle.lock"
test "$(cat "${purge_backup}/sentinel")" = backup
test "$(cat "${purge_secret}/sentinel")" = secret

purge_failure_state="${fixture}/uninstall-purge-failure"
seed_upgrade_state "${purge_failure_state}" "${next_release_file}"
export MOCK_DOWN_EXIT=1
if run_controller_uninstall "${purge_failure_state}" --purge-data \
  >"${purge_failure_state}/output.log" 2>&1; then
  unset MOCK_DOWN_EXIT
  echo "purge failure was accepted" >&2
  exit 1
fi
unset MOCK_DOWN_EXIT
grep -Fq 'local data purge is partial and lifecycle state was retained' \
  "${purge_failure_state}/output.log"
test "$(sed -n '1p' "${purge_failure_state}/compose.log")" = "down --volumes"
test -f "${purge_failure_state}/current-release.json"
test -f "${purge_failure_state}/previous-release.json"

unsafe_uninstall_state="${fixture}/uninstall-unsafe-state"
mkdir -m 750 -- "${unsafe_uninstall_state}"
expect_rejection "${fixture}/uninstall-unsafe-state.log" 'state root must be owned by the launcher user with mode 0700' \
  run_controller_uninstall "${unsafe_uninstall_state}"

symlink_uninstall_state="${fixture}/uninstall-symlink-state"
mkdir -m 700 -- "${symlink_uninstall_state}"
ln -s "${release_file}" "${symlink_uninstall_state}/current-release.json"
expect_rejection "${fixture}/uninstall-symlink-state.log" 'current release state is a symlink; refusing to uninstall' \
  run_controller_uninstall "${symlink_uninstall_state}"

state="${fixture}/lock_uninstall_state"
seed_upgrade_state "${state}"
(
  exec 9>"${state}/lifecycle.lock"
  chmod 600 "${state}/lifecycle.lock"
  flock -n 9
  expect_rejection "${state}/output.log" 'another Controller lifecycle command is already running' run_controller_uninstall "${state}"
)
test ! -e "${state}/compose.log"

after_unknown_uninstall="${fixture}/uninstall-unknown-flag.log"
expect_rejection "${after_unknown_uninstall}" 'usage:' \
  run_controller_uninstall "${fixture}/uninstall-unknown-flag" --unexpected

version_release="${fixture}/release/version-images.json"
jq '.images |= with_entries(.value |= sub("@sha256:[0-9a-f]{64}$"; ":v0.2.0"))' "${release_file}" >"${version_release}"
expect_failure "${fixture}/version-images" "${version_release}" 'release manifest is invalid' false env

integrated_release="${fixture}/release/integrated.json"
jq --arg ref "registry.test/image@${digest}" '.manifest_version = 2 | .signer_state_version = 1 |
  .images += {edge:$ref, relay:$ref, signer:$ref}' \
  "${release_file}" >"${integrated_release}"
integrated_state="${fixture}/integrated"
expect_failure "${fixture}/integrated-v1" "${release_file}" "requires a v2 release manifest" false env OCSERV_DEPLOYMENT_MODE=integrated
for filter in '.signer_state_version = 2' '.extra = true' '.images.extra = .images.signer' 'del(.images.signer)' '.images.mysql_backup = .images.signer' '.images.mariadb_backup = .images.signer' '.database_migration.epoch = 0' '.database_migration.revision = -1' '.database_migration.revision = "0"' '.database_migration.extra = true'; do
  rejected="${fixture}/release/integrated-rejected.json"
  jq "${filter}" "${integrated_release}" >"${rejected}"
  rejection_state="$(mktemp -d "${fixture}/v2-rejection.XXXXXX")"
  if run_controller "${rejection_state}" "${rejected}" env >"${rejection_state}/output.log" 2>&1; then
    echo "invalid v2 manifest accepted: ${filter}" >&2; exit 1
  fi
  grep -Fq 'release manifest is invalid' "${rejection_state}/output.log"
  test ! -e "${rejection_state}/compose.log"
done
# Invalid deployment profiles fail closed on every lifecycle action
# before any Compose invocation, pending-state write, or data/state purge.
retired_mysql_state="${fixture}/retired-mysql-profile"
seed_upgrade_state "${retired_mysql_state}" "${integrated_release}"
cp "${release_file}" "${retired_mysql_state}/previous-release.json"
chmod 600 "${retired_mysql_state}/previous-release.json"
printf '%s\n' '{"database_backend":"mysql","database_deployment":"external","deployment_mode":"standalone"}' \
  >"${retired_mysql_state}/deployment-profile.json"
chmod 600 "${retired_mysql_state}/deployment-profile.json"
for action in install upgrade rollback start uninstall uninstall-purge; do
  output_log="${retired_mysql_state}/${action}.log"
  status=0
  case "${action}" in
    install) run_controller "${retired_mysql_state}" "${integrated_release}" env >"${output_log}" 2>&1 || status=$? ;;
    upgrade) run_controller_upgrade "${retired_mysql_state}" "${integrated_release}" env >"${output_log}" 2>&1 || status=$? ;;
    rollback) run_controller_rollback "${retired_mysql_state}" env >"${output_log}" 2>&1 || status=$? ;;
    start) run_controller_start "${retired_mysql_state}" >"${output_log}" 2>&1 || status=$? ;;
    uninstall) run_controller_uninstall "${retired_mysql_state}" >"${output_log}" 2>&1 || status=$? ;;
    uninstall-purge) run_controller_uninstall "${retired_mysql_state}" --purge-data >"${output_log}" 2>&1 || status=$? ;;
  esac
  (( status != 0 )) || { echo "invalid deployment profile accepted on ${action}" >&2; exit 1; }
  grep -Fq 'invalid deployment profile' "${output_log}"
  test ! -e "${retired_mysql_state}/compose.log"
  test ! -e "${retired_mysql_state}/pending-release.json"
  cmp -s "${release_file}" "${retired_mysql_state}/current-release.json"
  cmp -s "${release_file}" "${retired_mysql_state}/previous-release.json"
  test -f "${retired_mysql_state}/deployment-profile.json"
done

mkdir -m 700 "${integrated_state}"
if run_controller "${integrated_state}" "${integrated_release}" env OCSERV_DEPLOYMENT_MODE=integrated \
  OCSERV_SIGNER_STATE_DIR="${integrated_state}" MOCK_SMOKE_EXIT=1 >"${integrated_state}/failure.log" 2>&1; then
  echo "Integrated smoke failure was accepted" >&2; exit 1
fi
test "$(sed -n '1p' "${integrated_state}/compose.log")" = "config --quiet"
test "$(sed -n '2p' "${integrated_state}/compose.log")" = pull
test -f "${integrated_state}/signer-init-attempted"
test -f "${integrated_state}/signer-checkpoint.json"
run_controller "${integrated_state}" "${integrated_release}" env OCSERV_SIGNER_STATE_DIR="${integrated_state}"
test "$(grep -Fc 'run --rm --no-deps signer init' "${integrated_state}/compose.log")" = 1
cmp -s "${integrated_release}" "${integrated_state}/current-release.json"
if OCSERV_DEPLOYMENT_MODE=standalone run_controller_start "${integrated_state}" >"${integrated_state}/failure.log" 2>&1; then
  echo "Integrated mode change was accepted" >&2; exit 1
fi
grep -Fq 'deployment mode cannot change' "${integrated_state}/failure.log"
if OCSERV_DATABASE_BACKEND=postgres OCSERV_DATABASE_DEPLOYMENT=external run_controller_start "${integrated_state}" >"${integrated_state}/failure.log" 2>&1; then
  echo "Integrated database switch was accepted" >&2; exit 1
fi
grep -Fq 'database deployment cannot change' "${integrated_state}/failure.log"
if OCSERV_DATABASE_BACKEND=mysql OCSERV_DATABASE_DEPLOYMENT=external run_controller_start "${integrated_state}" >"${integrated_state}/failure.log" 2>&1; then
  echo "Integrated retired MySQL switch was accepted" >&2; exit 1
fi
grep -Fq 'database deployment cannot change' "${integrated_state}/failure.log"
jq -e 'has("tls_mode") | not' "${integrated_state}/deployment-profile.json" >/dev/null
OCSERV_TLS_MODE=acme expect_rejection "${integrated_state}/failure.log" 'TLS mode cannot change' \
  run_controller_start "${integrated_state}"
acme_state="${fixture}/integrated-acme"
mkdir -m 700 "${acme_state}"
run_controller "${acme_state}" "${integrated_release}" env OCSERV_DEPLOYMENT_MODE=integrated OCSERV_TLS_MODE=acme \
  OCSERV_SIGNER_STATE_DIR="${acme_state}"
jq -e '.tls_mode == "acme"' "${acme_state}/deployment-profile.json" >/dev/null
OCSERV_TLS_MODE=manual expect_rejection "${acme_state}/failure.log" 'TLS mode cannot change' \
  run_controller_start "${acme_state}"
if MOCK_SIGNER_INSPECT_EXIT=1 run_controller_start "${integrated_state}" >"${integrated_state}/failure.log" 2>&1; then
  echo "missing Signer ledger was accepted on start" >&2; exit 1
fi
MOCK_SIGNER_REVISION=3 expect_rejection "${integrated_state}/failure.log" 'minimum revision changed' \
  run_controller_start "${integrated_state}"
jq -e '.revision == 4' "${integrated_state}/signer-checkpoint.json" >/dev/null
test "$(grep -Fc 'run --rm --no-deps signer init' "${integrated_state}/compose.log")" = 1
run_controller_start "${integrated_state}"
run_controller_uninstall "${integrated_state}"
test -f "${integrated_state}/signer-checkpoint.json"
test -f "${integrated_state}/signer-init-attempted"
if run_controller_uninstall "${integrated_state}" --purge-data >"${integrated_state}/failure.log" 2>&1; then
  echo "Integrated identity purge was accepted" >&2; exit 1
fi
init_failure="${fixture}/integrated-init-failure"
mkdir -m 700 "${init_failure}"
if run_controller "${init_failure}" "${integrated_release}" env OCSERV_DEPLOYMENT_MODE=integrated \
  OCSERV_SIGNER_STATE_DIR="${init_failure}" MOCK_SIGNER_INIT_EXIT=1 >"${init_failure}/failure.log" 2>&1; then
  echo "Signer init failure was accepted" >&2; exit 1
fi
if run_controller "${init_failure}" "${integrated_release}" env OCSERV_SIGNER_STATE_DIR="${init_failure}" \
  MOCK_SIGNER_INSPECT_EXIT=1 >"${init_failure}/failure.log" 2>&1; then
  echo "uncertain Signer init was repeated" >&2; exit 1
fi
test "$(grep -Fc 'run --rm --no-deps signer init' "${init_failure}/compose.log")" = 1
# Runtime lifecycle uses only fixed Release assets; retain both versions for
# offline recovery. An executable Git sentinel fails if any dependency remains.
runtime_root="${fixture}/runtime"
runtime_assets="${fixture}/runtime-assets"
mkdir -m 700 "${runtime_root}" "${runtime_assets}"
for tag in v0.2.0 v0.3.0; do
  payload="${runtime_root}/${tag}"
  mkdir -p "${payload}/deploy/production" "${payload}/deploy/lib"
  cp "${ROOT}/deploy/production/controller.sh" "${payload}/deploy/production/"
  cp "${ROOT}/deploy/lib/controller-runtime.sh" "${payload}/deploy/lib/"
  cp "${bin}/compose.sh" "${bin}/controller-release-smoke.sh" "${payload}/deploy/production/"
  printf '%s %s\n' "${tag}" "${commit}" >"${payload}/RELEASE"
  (cd "${payload}" && find deploy RELEASE -type f | sort | xargs sha256sum >SHA256SUMS)
  (cd "${payload}" && find deploy RELEASE SHA256SUMS -type f | sort | tar -czf "${runtime_assets}/controller-runtime-${tag}.tar.gz" -T -)
  (cd "${runtime_assets}" && sha256sum "controller-runtime-${tag}.tar.gz" >"controller-runtime-${tag}.tar.gz.sha256")
done
cat >"${bin}/git" <<'EOF_GIT'
#!/usr/bin/env bash
echo 'Git must not run in runtime lifecycle' >&2
exit 97
EOF_GIT
cat >"${bin}/curl" <<'EOF_CURL'
#!/usr/bin/env bash
set -euo pipefail
output="" url=""
while (($#)); do
  case "$1" in --output) output="$2"; shift ;; https://*) url="$1" ;; esac
  shift
done
[[ "${url}" == https://github.com/GentleKingson/ocservia/releases/download/v0.3.0/* ]] || exit 22
cp "${RUNTIME_TEST_ASSETS}/${url##*/}" "${output}"
EOF_CURL
chmod 755 "${bin}/git" "${bin}/curl"
export RUNTIME_TEST_ASSETS="${runtime_assets}"
CONTROLLER="${runtime_root}/v0.2.0/deploy/production/controller.sh"
runtime_state="${fixture}/runtime-state"
mkdir -m 700 "${runtime_state}"
# Empty overrides ensure lifecycle chooses the runtime's own executable files.
run_controller "${runtime_state}" "${release_file}" env OCSERV_CONTROLLER_COMPOSE_SH= OCSERV_CONTROLLER_SMOKE_SH=
if run_controller_upgrade "${runtime_state}" "${next_release_file}" env \
  OCSERV_CONTROLLER_COMPOSE_SH= OCSERV_CONTROLLER_SMOKE_SH= MOCK_SMOKE_EXIT=1 >"${runtime_state}/failure.log" 2>&1; then
  echo 'runtime smoke failure accepted' >&2; exit 1
fi
cmp "${release_file}" "${runtime_state}/current-release.json"
test -f "${runtime_state}/pending-release.json"
run_controller_upgrade "${runtime_state}" "${next_release_file}" env OCSERV_CONTROLLER_COMPOSE_SH= OCSERV_CONTROLLER_SMOKE_SH=
cmp "${next_release_file}" "${runtime_state}/current-release.json"
# Disable network fetches after both runtimes have been retained.
printf '#!/usr/bin/env bash\nexit 97\n' >"${bin}/curl"
run_controller_rollback "${runtime_state}" env OCSERV_CONTROLLER_COMPOSE_SH= OCSERV_CONTROLLER_SMOKE_SH=
cmp "${release_file}" "${runtime_state}/current-release.json"
run_controller_uninstall "${runtime_state}"
test -f "${runtime_state}/current-release.json"
run_controller_start "${runtime_state}"
printf '\n# local drift\n' >>"${runtime_state}/runtime-v0.2.0/deploy/production/compose.sh"
expect_rejection "${runtime_state}/drift.log" 'runtime content changed' \
  run_controller_start "${runtime_state}"
echo "Controller lifecycle tests passed (including Git-free runtime upgrade, rollback and recovery)"
