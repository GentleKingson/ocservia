#!/usr/bin/env bash
set -euo pipefail
unset OCSERV_OTEL_BACKEND_ENDPOINT

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/test-installer-common.sh
source "${ROOT}/scripts/test-installer-common.sh"
INSTALL="${ROOT}/deploy/production/install.sh"
DOWNLOAD_BASE="https://github.com/GentleKingson/ocservia/releases/download"

# The installer fixture asserts file modes with GNU stat; skip on hosts without
# the GNU userland (mirrors the guard in test-controller-host-bootstrap.sh).
stat -c '%u' . >/dev/null 2>&1 || {
  echo "Controller install tests skipped: GNU stat is unavailable" >&2
  exit 0
}
command -v git >/dev/null 2>&1 || {
  echo "Controller install tests skipped: git is unavailable" >&2
  exit 0
}
[[ -x "${INSTALL}" ]] || {
  echo "Controller install tests require an executable installer" >&2
  exit 1
}
# The install.env scenarios assert metadata (0644 files pass, group- and
# world-writable files fail closed), so fixture creation must not depend on
# the invoking login's umask (Debian-style usergroups logins default to 002).
umask 022

# Keep the fixture under HOME: /tmp is world-writable (mode 1777), which the
# lifecycle state root ancestry contract must reject.
fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-install-test.XXXXXX")"

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  # Root scenarios leave root-owned files inside the fixture tree.
  if can_root; then
    as_root rm -rf -- "${fixture}" || rm -rf -- "${fixture}"
  else
    rm -rf -- "${fixture}"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

repo="${fixture}/repo"
bin="${fixture}/bin"
state_root="${fixture}/state-root"
logs="${fixture}/logs"
curl_log="${logs}/curl.log"
bootstrap_log="${logs}/bootstrap.log"
controller_log="${logs}/controller.log"
sudo_log="${logs}/sudo.log"
root_curl_log="${state_root}.curl.log"
root_bootstrap_log="${state_root}.bootstrap.log"
root_controller_log="${state_root}.controller.log"
root_sudo_log="${state_root}.sudo.log"
EXTRA_ENV=()
RUN_STATUS=0
RUN_OUTPUT=""

die() {
  echo "Controller install tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

mkdir -m 700 -- "${bin}" "${logs}"
mkdir -p -- "${repo}/deploy/production" "${repo}/deploy/lib"

# The installer resolves ROOT from its own location, so the fixture exercises
# the real install.sh against a fixture release checkout with mocked sibling
# entrypoints (bootstrap-host.sh, controller.sh) and mocked PATH tools. The
# shared install.env loader ships beside the installer, and the repository's
# real .gitignore is committed too: the installer must treat a present
# /install.env as ignored, not as a dirty release checkout.
cp -- "${INSTALL}" "${repo}/deploy/production/install.sh"
cp -- "${ROOT}/deploy/lib/install-env.sh" "${repo}/deploy/lib/install-env.sh"
cp -- "${ROOT}/.gitignore" "${repo}/.gitignore"

cat >"${repo}/deploy/production/bootstrap-host.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
bootstrap_log="${INSTALL_TEST_BOOTSTRAP_LOG:-${OCSERV_CONTROLLER_STATE_ROOT:-/nonexistent}.bootstrap.log}"
for name in OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL; do
  printf '%s=%s\n' "${name}" "${!name-<unset>}" >>"${bootstrap_log}"
done
printf 'launcher:%s\n' "${SUDO_USER:-<unset>}" >>"${bootstrap_log}"
printf '%s\n' "$*" >>"${bootstrap_log}"
printf 'OCSERV_PUBLIC_HOST=%s\n' "${OCSERV_PUBLIC_HOST:-<unset>}" >>"${bootstrap_log}"
printf 'recommended:%s\n' "${OCSERV_RECOMMENDED_AGENT_VERSION-<unset>}" >>"${bootstrap_log}"
printf 'proxy-network:%s,%s,%s\n' "${OCSERV_APPLICATION_SUBNET:-}" "${OCSERV_APPLICATION_IP_RANGE:-}" "${OCSERV_GATEWAY_APPLICATION_IP:-}" >>"${bootstrap_log}"
printf 'otel:%s\n' "${OCSERV_OTEL_BACKEND_ENDPOINT:-}" >>"${bootstrap_log}"
printf 'OCSERV_SECRET_DIR=%s\n' "${OCSERV_SECRET_DIR:-<unset>}" >>"${bootstrap_log}"
printf 'OCSERV_BACKUP_DIR=%s\n' "${OCSERV_BACKUP_DIR:-<unset>}" >>"${bootstrap_log}"
printf 'UNRELATED_ENV=%s\n' "${UNRELATED_ENV:-<unset>}" >>"${bootstrap_log}"
printf 'OCSERV_UNRELATED_CONFIG=%s\n' "${OCSERV_UNRELATED_CONFIG:-<unset>}" >>"${bootstrap_log}"
[[ "${MOCK_BOOTSTRAP_EXIT:-0}" == 0 ]] || exit "${MOCK_BOOTSTRAP_EXIT}"
if [[ "${MOCK_BOOTSTRAP_PROVISION_STATE:-1}" == 1 ]]; then
  mkdir -p -m 0700 -- \
    "${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/nonexistent-ocservia}}"
fi
exit 0
EOF

cat >"${repo}/deploy/production/controller.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
controller_log="${INSTALL_TEST_CONTROLLER_LOG:-${OCSERV_CONTROLLER_STATE_ROOT:-/nonexistent}.controller.log}"
printf '%s\n' "$*" >>"${controller_log}"
for name in OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL; do
  printf '%s=%s\n' "${name}" "${!name-<unset>}" >>"${controller_log}"
done
printf 'relay-a:%s\n' "${OCSERV_RELAY_URL_A-<unset>}" >>"${controller_log}"
exit "${MOCK_CONTROLLER_EXIT:-0}"
EOF

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
curl_log="${INSTALL_TEST_CURL_LOG:-${OCSERV_CONTROLLER_STATE_ROOT:-/nonexistent}.curl.log}"
printf '%s\n' "$*" >>"${curl_log}"
output=""
url=""
while (($# > 0)); do
  if [[ "$1" == --output ]]; then
    shift
    output="${1:-}"
  else
    case "$1" in
      http*) url="$1" ;;
    esac
  fi
  shift
done
[[ -n "${output}" ]] || {
  echo "curl mock: no --output target" >&2
  exit 1
}
printf 'mock bundle bytes %s\n' "${url}" >"${output}"
exit 0
EOF

cat >"${bin}/sudo" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${INSTALL_TEST_SUDO_LOG}"
[[ "${1:-}" != -n ]] || shift
exec "$@"
EOF

cat >"${bin}/uname" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${INSTALL_TEST_ARCH:-}" ]]; then
  printf '%s\n' "${INSTALL_TEST_ARCH}"
else
  unset INSTALL_TEST_ARCH
  exec /bin/uname "$@"
fi
EOF

chmod 0755 -- "${repo}/deploy/production/bootstrap-host.sh" \
  "${repo}/deploy/production/controller.sh" \
  "${bin}/curl" "${bin}/sudo" "${bin}/uname"

# A restricted PATH for the fresh-host scenarios: the installer must see no
# docker command anywhere, even when the host running the tests has Docker.
fresh_bin="${fixture}/fresh-bin"
mkdir -m 700 -- "${fresh_bin}"
root_lifecycle_bin="${fixture}/root-lifecycle-bin"
link_tool_into() {
  local destination="$1" tool="$2" path
  path="$(command -v "${tool}" || true)"
  [[ -n "${path}" ]] || die "required test tool not found: ${tool}"
  ln -s "${path}" "${destination}/${tool}"
}
link_tool() {
  link_tool_into "${fresh_bin}" "$1"
}
for tool in bash git env chmod mkdir dirname; do
  link_tool "${tool}"
done
ln -s "${bin}/sudo" "${fresh_bin}/sudo"
ln -s "${bin}/curl" "${fresh_bin}/curl"
ln -s "${bin}/uname" "${fresh_bin}/uname"

# The operator-to-root regression case uses the real sudo boundary, while
# keeping the fixture's mocked tools visible after sudo's environment reset.
# The wrapper logs the exact sudo env arguments before adding this test-only
# PATH, so the production installer still has to forward only its allowlist.
# The stat link feeds the install.env loader's permission check, and the
# poison-install-env seam is the install.env TOCTOU regression: the wrapper
# (still running as the launcher user) swaps $PWD/install.env for attacker
# content at the exact moment the privilege transition starts, so a root
# re-read of the file would pick up attacker-chosen configuration.
if (( EUID != 0 )) && can_root; then
  mkdir -m 700 -- "${root_lifecycle_bin}"
  for tool in bash git env chmod mkdir dirname stat; do
    link_tool_into "${root_lifecycle_bin}" "${tool}"
  done
  ln -s "${bin}/curl" "${root_lifecycle_bin}/curl"
  ln -s "${bin}/uname" "${root_lifecycle_bin}/uname"
  real_sudo="$(command -v sudo)"
  cat >"${root_lifecycle_bin}/sudo" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"${root_sudo_log}"
if [[ -s "${fixture}/poison-install-env" ]]; then
  cp -- "${fixture}/poison-install-env" "${repo}/install.env"
fi
exec "${real_sudo}" env PATH="${root_lifecycle_bin}" "\$@"
EOF
  chmod 0755 -- "${root_lifecycle_bin}/sudo"
fi

# The installer only probes for a docker client with command -v before the
# bootstrap; a stub on PATH is enough for the existing-Docker scenarios.
install_docker_client_stub() {
  cat >"${bin}/docker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod 0755 -- "${bin}/docker"
}

git -C "${repo}" init -q
git -C "${repo}" config user.name test
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" add -A
git -C "${repo}" commit -qm base
git -C "${repo}" tag v0.1.2

reset_logs() {
  : >"${curl_log}"
  : >"${bootstrap_log}"
  : >"${controller_log}"
  : >"${sudo_log}"
  rm -f -- "${bin}/docker"
  if [[ -e "${state_root}" ]]; then
    if can_root; then
      as_root rm -rf -- "${state_root}"
    else
      rm -rf -- "${state_root}"
    fi
  fi
  if can_root; then
    as_root rm -f -- "${root_curl_log}" "${root_bootstrap_log}" \
      "${root_controller_log}" "${root_sudo_log}"
  else
    rm -f -- "${root_curl_log}" "${root_bootstrap_log}" \
      "${root_controller_log}" "${root_sudo_log}"
  fi
  : >"${root_curl_log}"
  : >"${root_bootstrap_log}"
  : >"${root_controller_log}"
  : >"${root_sudo_log}"
  EXTRA_ENV=()
}

reset_checkout() {
  git -C "${repo}" checkout -q -- .
  git -C "${repo}" clean -qfd
  git -C "${repo}" reset -q --hard v0.1.2
  # install.env is git-ignored, so git clean never removes it; the install.env
  # scenarios must not leak into unrelated ones.
  rm -f -- "${repo}/install.env" "${fixture}/poison-install-env"
}

scenario() {
  reset_logs
  reset_checkout
}

run_installer() {
  INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    INSTALL_TEST_ARCH="${INSTALL_TEST_ARCH:-}" \
    PATH="${bin}:${PATH}" \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    env "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" "${repo}/deploy/production/install.sh" "$@"
}

capture() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_installer "$@" 2>&1)" || RUN_STATUS=$?
}

capture_from() {
  local directory="$1"
  shift
  RUN_STATUS=0
  RUN_OUTPUT="$(cd -- "${directory}" && run_installer "$@" 2>&1)" || RUN_STATUS=$?
}

# The installer selects the manifest from uname -m, so assertions follow the
# real host architecture; scenario 4a pins aarch64 explicitly.
case "$(uname -m)" in
  x86_64) native_arch=amd64 ;;
  aarch64|arm64) native_arch=arm64 ;;
  *) native_arch=amd64 ;;
esac

# 1. usage errors.
scenario
capture unexpected-argument
assert_status 2 "an unexpected argument must be a usage error"
capture --root-lifecycle extra-argument
assert_status 2 "extra arguments alongside --root-lifecycle must be a usage error"
echo "usage errors fail with status 2"

# A checkout between releases must fail before the first host mutation.
scenario
git -C "${repo}" commit -q --allow-empty -m beyond-tag
capture
assert_status 1
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${bootstrap_log}" "${curl_log}" "${controller_log}"

# 3b. a dirty checkout is rejected early.
scenario
printf '\n# local edit\n' >>"${repo}/deploy/production/install.sh"
capture
assert_status 1 "a dirty checkout must fail closed"
assert_output "dirty"
assert_log_empty "${bootstrap_log}" "${curl_log}"
echo "a dirty checkout is rejected before any host mutation"

# 4. the full happy path as the launcher user on a host with Docker.
scenario
install_docker_client_stub
EXTRA_ENV=("OCSERV_BACKUP_DIR=${fixture}/backup")
capture
assert_status 0 "the launcher-user install flow must succeed"
grep -Fxq 'otel:' "${bootstrap_log}" || die "OTEL must be disabled for the base install"
assert_output "release identity: v0.1.2"
if (( EUID == 0 )); then
  # A root test shell exercises the root lifecycle: no whole-script sudo.
  assert_log_empty "${sudo_log}"
  assert_log_contains "${bootstrap_log}" "install --backup-dir ${fixture}/backup"
else
  assert_log_contains "${sudo_log}" "env OCSERV_CONTROLLER_STATE_ROOT=${state_root} ${repo}/deploy/production/bootstrap-host.sh install --backup-dir ${fixture}/backup"
  assert_log_contains "${bootstrap_log}" "install --backup-dir ${fixture}/backup"
fi
[[ "$(wc -l <"${curl_log}" | tr -d ' ')" == 1 ]] ||
  die "expected exactly one configuration download, got: $(cat -- "${curl_log}")"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-${native_arch}.json"
if grep -q "release-signing" "${curl_log}"; then
  die "the installer must never download release trust material: $(cat -- "${curl_log}")"
fi
bundle_dir="${state_root}/release-bundles/v0.1.2"
[[ "$(stat -c '%a' "${bundle_dir}")" == 700 ]] ||
  die "bundle directory mode is wrong: $(stat -c '%a' "${bundle_dir}")"
mapfile -d '' -t bundle_files < <(find "${bundle_dir}" -mindepth 1 -maxdepth 1 -printf '%f\0')
bundle_file="controller-release-${native_arch}.json"
[[ "${#bundle_files[@]}" == 1 && "${bundle_files[0]}" == "${bundle_file}" ]] ||
  die "unexpected bundle contents: $(printf '%q ' "${bundle_files[@]}")"
[[ "$(stat -c '%a' "${bundle_dir}/${bundle_file}")" == 600 ]] ||
  die "bundle file ${bundle_file} mode is wrong"
assert_log_contains "${controller_log}" "install --release-file ${bundle_dir}/controller-release-${native_arch}.json"
echo "the launcher install flow bootstraps, downloads, and activates"

# 4a. arm64 hosts select the arm64 manifest automatically.
scenario
install_docker_client_stub
EXTRA_ENV=("INSTALL_TEST_ARCH=aarch64")
capture
assert_status 0 "the arm64 install flow must succeed"
if grep -q "controller-release-amd64" "${curl_log}"; then
  die "an arm64 host must not download the amd64 manifest"
fi
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-arm64.json"
assert_log_contains "${controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-arm64.json"
echo "arm64 hosts select the arm64 release manifest"

# 5. a bootstrap failure stops before download and activation.
scenario
install_docker_client_stub
EXTRA_ENV=("MOCK_BOOTSTRAP_EXIT=1")
capture
assert_status 1 "a bootstrap failure must fail the installer"
assert_log_empty "${curl_log}" "${controller_log}"
echo "a bootstrap failure stops before download and activation"

# 5a. a state root the bootstrap did not provision is rejected.
scenario
install_docker_client_stub
EXTRA_ENV=("MOCK_BOOTSTRAP_PROVISION_STATE=0")
capture
assert_status 1 "a missing state root must fail closed"
assert_output "was not provisioned"
assert_log_empty "${controller_log}"
echo "a missing state root is rejected before activation"

# 6. a whole-script sudo invocation mismatches the launcher contract.
if can_root; then
  scenario
  RUN_STATUS=0
  RUN_OUTPUT="$(sudo -n env \
    INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    PATH="${bin}:${PATH}" \
    SUDO_USER=ocservia-operator \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    "${repo}/deploy/production/install.sh" 2>&1)" || RUN_STATUS=$?
  assert_status 1 "whole-script sudo must fail closed"
  assert_output "run install.sh as the lifecycle launcher user"
  assert_output "--root-lifecycle"
  assert_log_empty "${bootstrap_log}" "${curl_log}" "${controller_log}"
  echo "whole-script sudo is rejected with the launcher mismatch"

  # 6a. a plain root shell is an allowed root lifecycle and is the supported
  # fresh-host path: with no Docker client anywhere on PATH, root installs
  # Docker through the bootstrap and activates without a permission mismatch.
  scenario
  RUN_STATUS=0
  RUN_OUTPUT="$(sudo -n env \
    -u SUDO_USER \
    INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    PATH="${fresh_bin}" \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    "${repo}/deploy/production/install.sh" 2>&1)" || RUN_STATUS=$?
  assert_status 0 "the root lifecycle install flow must succeed"
  assert_log_empty "${sudo_log}"
  assert_log_contains "${bootstrap_log}" "install"
  assert_log_contains "${controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-${native_arch}.json"
  if (( EUID != 0 )); then
    # The root lifecycle refreshes the fixture checkout's git index as root;
    # return ownership so later launcher scenarios can mutate the checkout.
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
  fi
  echo "a root shell runs the fresh-host lifecycle with no Docker present"

  # 6b. --root-lifecycle deliberately runs the whole lifecycle as root even
  # when the sudo environment still carries the invoking user's identity
  # (which sudo -i retains): the installer strips SUDO_USER so the bootstrap
  # provisions for root, while SUDO_UID/SUDO_GID stay untouched — git trusts
  # the operator-owned checkout through exactly those, as real sudo sets
  # them to the checkout owner's ids.
  scenario
  RUN_STATUS=0
  RUN_OUTPUT="$(sudo -n env \
    INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    PATH="${fresh_bin}" \
    SUDO_USER=ocservia-operator \
    SUDO_UID="$(id -u)" \
    SUDO_GID="$(id -g)" \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    "${repo}/deploy/production/install.sh" --root-lifecycle 2>&1)" || RUN_STATUS=$?
  assert_status 0 "--root-lifecycle must succeed with a retained sudo identity"
  assert_output "release identity: v0.1.2"
  assert_log_empty "${sudo_log}"
  assert_log_contains "${bootstrap_log}" "launcher:<unset>"
  assert_log_contains "${controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-${native_arch}.json"
  if (( EUID != 0 )); then
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
  fi
  echo "--root-lifecycle runs the root lifecycle despite a retained sudo identity"

  # 6c. The documented operator command must survive sudo's default
  # env_reset: the operator exports production settings, invokes the script
  # directly, and the installer performs the controlled sudo env re-exec.
  if (( EUID != 0 )); then
    scenario
    RUN_STATUS=0
    # Each fixture intentionally isolates its PATH and exported settings.
    # shellcheck disable=SC2030,SC2031
    RUN_OUTPUT="$(
      export INSTALL_TEST_CURL_LOG="${curl_log}"
      export INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}"
      export INSTALL_TEST_CONTROLLER_LOG="${controller_log}"
      export INSTALL_TEST_SUDO_LOG="${sudo_log}"
      export PATH="${root_lifecycle_bin}"
      export OCSERV_CONTROLLER_STATE_ROOT="${state_root}"
      export OCSERV_SECRET_DIR="${fixture}/secrets"
      export OCSERV_BACKUP_DIR="${fixture}/backup"
      export OCSERV_PUBLIC_HOST=controller.example.test
      export OCSERV_LOCAL_AUTH_ENABLED=true
      export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test/v1
      export OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1
      export OCSERV_CONTROLLER_ENDPOINT_ID=0000000000000000000000000000000000000000000000000000000000000000
      export OCSERV_RELAY_URL_A=https://relay-a.example.test
      export OCSERV_HTTPS_ADDRESS=127.0.0.1
      export OCSERV_APPLICATION_SUBNET=198.18.80.0/24
      export OCSERV_APPLICATION_IP_RANGE=198.18.80.128/25
      export OCSERV_GATEWAY_APPLICATION_IP=198.18.80.2
      export OCSERV_BACKUP_INTERVAL_SECONDS=300
      export OCSERV_BACKUP_RETENTION_COUNT=4
      export UNRELATED_ENV=must-not-cross-sudo
      export OCSERV_UNRELATED_CONFIG=must-not-cross-sudo
      "${repo}/deploy/production/install.sh" --root-lifecycle 2>&1
    )" || RUN_STATUS=$?
    assert_status 0 "the operator root-lifecycle command must succeed through sudo env_reset"
    assert_output "release identity: v0.1.2"
    assert_log_contains "${root_sudo_log}" "OCSERV_PUBLIC_HOST=controller.example.test"
    if grep -q "UNRELATED_ENV\|OCSERV_UNRELATED_CONFIG" "${root_sudo_log}"; then
      die "the root-lifecycle sudo command must not forward unrelated environment variables: $(cat -- "${root_sudo_log}")"
    fi
    assert_log_contains "${root_bootstrap_log}" "launcher:<unset>"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_PUBLIC_HOST=controller.example.test"
    assert_log_contains "${root_bootstrap_log}" "proxy-network:198.18.80.0/24,198.18.80.128/25,198.18.80.2"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_SECRET_DIR=${fixture}/secrets"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_BACKUP_DIR=${fixture}/backup"
    assert_log_contains "${root_bootstrap_log}" "UNRELATED_ENV=<unset>"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_UNRELATED_CONFIG=<unset>"
    [[ "$(wc -l <"${root_curl_log}" | tr -d ' ')" == 1 ]] ||
      die "expected exactly one root-lifecycle configuration download, got: $(cat -- "${root_curl_log}")"
    assert_log_contains "${root_controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-${native_arch}.json"
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
    echo "the operator root-lifecycle command forwards only production configuration across sudo env_reset"
    reset_logs
    reset_checkout
    printf 'OCSERV_RELAY_URL_A=https://relay-file.example.test\n' >"${repo}/install.env"
    RUN_STATUS=0
    # Each fixture intentionally isolates its PATH and exported settings.
    # shellcheck disable=SC2030,SC2031
    RUN_OUTPUT="$(
      export PATH="${root_lifecycle_bin}"
      export OCSERV_CONTROLLER_STATE_ROOT="${state_root}"
      unset OCSERV_RELAY_URL_A
      cd -- "${repo}"
      "${repo}/deploy/production/install.sh" --root-lifecycle 2>&1
    )" || RUN_STATUS=$?
    assert_status 0
    assert_log_contains "${root_controller_log}" 'relay-a:https://relay-file.example.test'
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
  fi

  # 6d. --root-lifecycle resolves install.env as the launcher user and
  # forwards only the effective allowlist across the boundary. The sudo
  # wrapper swaps $PWD/install.env for poisoned content the instant the
  # privilege transition starts, so a root re-read would hand root
  # attacker-chosen configuration; the re-exec marker must prevent it.
  if (( EUID != 0 )); then
    scenario
    cat >"${repo}/install.env" <<EOF
OCSERV_PUBLIC_HOST=controller-root-file.example.test
OCSERV_RECOMMENDED_AGENT_VERSION=0.2.0
OCSERV_SECRET_DIR=${fixture}/root-file-secrets
OCSERV_BACKUP_DIR=${fixture}/root-file-backup
OCSERV_CONTROLLER_STATE_ROOT=${state_root}
EOF
    printf 'OCSERV_PUBLIC_HOST=controller-poisoned.example.test\nOCSERV_SECRET_DIR=%s\nOCSERV_BACKUP_DIR=%s\n' \
      "${fixture}/poisoned-secrets" "${fixture}/poisoned-backup" >"${fixture}/poison-install-env"
    RUN_STATUS=0
    # Each fixture intentionally isolates its PATH and exported settings.
    # shellcheck disable=SC2030,SC2031
    RUN_OUTPUT="$(
      export INSTALL_TEST_CURL_LOG="${root_curl_log}"
      export INSTALL_TEST_BOOTSTRAP_LOG="${root_bootstrap_log}"
      export INSTALL_TEST_CONTROLLER_LOG="${root_controller_log}"
      export INSTALL_TEST_SUDO_LOG="${root_sudo_log}"
      export PATH="${root_lifecycle_bin}"
      cd -- "${repo}"
      "${repo}/deploy/production/install.sh" --root-lifecycle 2>&1
    )" || RUN_STATUS=$?
    assert_status 0 "the root-lifecycle with install.env must succeed"
    assert_output "release identity: v0.1.2"
    assert_log_contains "${root_sudo_log}" "OCSERV_INSTALL_ENV_RESOLVED=1"
    assert_log_contains "${root_sudo_log}" "OCSERV_PUBLIC_HOST=controller-root-file.example.test"
    assert_log_contains "${root_sudo_log}" "OCSERV_CONTROLLER_STATE_ROOT=${state_root}"
    assert_log_contains "${root_sudo_log}" "OCSERV_RECOMMENDED_AGENT_VERSION=0.2.0"
    assert_log_contains "${root_bootstrap_log}" "recommended:0.2.0"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_PUBLIC_HOST=controller-root-file.example.test"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_SECRET_DIR=${fixture}/root-file-secrets"
    assert_log_contains "${root_bootstrap_log}" "install --backup-dir ${fixture}/root-file-backup"
    if grep -q "poisoned" "${root_sudo_log}" || grep -q "poisoned" "${root_bootstrap_log}"; then
      die "root must never re-read the swapped install.env: $(cat -- "${root_sudo_log}")"
    fi
    assert_log_contains "${root_controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-${native_arch}.json"
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
    echo "the root lifecycle forwards resolved install.env values and never re-reads the file as root"
  fi
else
  echo "sudo contract cases skipped: no passwordless sudo available" >&2
fi

# 7. a non-root launcher on a fresh host without Docker fails closed before
# any host mutation: a fresh Docker installation grants no non-root daemon
# access and the installer never modifies the Docker permission model.
if (( EUID != 0 )); then
  scenario
  EXTRA_ENV=("PATH=${fresh_bin}")
  capture
  assert_status 1 "a non-root launcher on a Docker-less host must fail closed"
  assert_output "--root-lifecycle"
  assert_output "post-install"
  assert_output "never modifies the Docker permission model"
  assert_log_empty "${bootstrap_log}" "${curl_log}" "${controller_log}"
  echo "a non-root launcher on a fresh Docker-less host fails closed before mutation"
else
  echo "fresh-host launcher-path case skipped: running as root"
fi

# The launcher loads static file values and honors shell overrides, including empty ones.
scenario
install_docker_client_stub
cat >"${repo}/install.env" <<EOF
OCSERV_BACKUP_DIR=${fixture}/file-backup
OCSERV_PUBLIC_HOST='controller-file.example.test'
OCSERV_RECOMMENDED_AGENT_VERSION=0.2.0
OCSERV_APPLICATION_SUBNET=198.18.80.0/24
OCSERV_APPLICATION_IP_RANGE=198.18.80.128/25
OCSERV_GATEWAY_APPLICATION_IP=198.18.80.2
OCSERV_OTEL_BACKEND_ENDPOINT=otel.example.test:4317
EOF
EXTRA_ENV=("OCSERV_RECOMMENDED_AGENT_VERSION=0.3.0" "OCSERV_BACKUP_DIR=")
capture_from "${repo}"
assert_status 0 "install.env must keep the checkout clean"
assert_log_contains "${bootstrap_log}" "OCSERV_PUBLIC_HOST=controller-file.example.test"
assert_log_contains "${bootstrap_log}" "recommended:0.3.0"
assert_log_contains "${bootstrap_log}" "proxy-network:198.18.80.0/24,198.18.80.128/25,198.18.80.2"
assert_log_contains "${bootstrap_log}" "otel:otel.example.test:4317"
if grep -q -- "--backup-dir" "${bootstrap_log}"; then
  die "install.env overrode the deliberately empty backup directory"
fi

# Run the documented Local auth configuration through the real installer and, on the CI
# launcher account, its real sudo env_reset transition. No source allowlist is read.
awk -v dir="${fixture}" '
  /^```dotenv$/ { block++; active=1; next }
  /^```$/ { active=0 }
  active { print > (dir "/auth-" block ".env") }
' "${ROOT}/docs/operations/authentication.md"
for lifecycle in launcher root; do
  if [[ "${lifecycle}" == root ]] && ! can_root; then
    die "auth forwarding requires passwordless sudo"
  fi
  reset_logs
  reset_checkout
  install_docker_client_stub
  cp "${fixture}/auth-1.env" "${repo}/install.env"
  RUN_STATUS=0
  RUN_OUTPUT="$( (
    unset OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL
    unset OCSERV_OIDC_ISSUER OCSERV_OIDC_CLIENT_ID OCSERV_OIDC_REDIRECT_URL
    if [[ "${lifecycle}" == root ]] && (( EUID != 0 )); then
      cd "${repo}"
      PATH="${root_lifecycle_bin}" OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
        "${repo}/deploy/production/install.sh" --root-lifecycle
    else
      cd -- "${repo}" && run_installer
    fi
  ) 2>&1)" || RUN_STATUS=$?
  assert_status 0
  expected="${fixture}/auth-1.env"
  actual_bootstrap="${bootstrap_log}"
  actual_controller="${controller_log}"
  if [[ "${lifecycle}" == root ]] && (( EUID != 0 )); then
    actual_bootstrap="${root_bootstrap_log}"
    actual_controller="${root_controller_log}"
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
  fi
  while IFS= read -r setting; do
    grep -Fxq "${setting}" "${actual_bootstrap}" || die "bootstrap lost Local auth configuration: ${setting}"
    grep -Fxq "${setting}" "${actual_controller}" || die "activation lost Local auth configuration: ${setting}"
  done <"${expected}"
done
echo "documented Local auth configuration reaches bootstrap and activation across the sudo boundary"

assert_env_refused() {
  capture_from "${repo}"
  assert_status 1
  assert_output "$1"
  assert_log_empty "${bootstrap_log}" "${curl_log}" "${controller_log}" "${sudo_log}"
}

scenario
while IFS='|' read -r line reason; do
  printf '%s\n' "${line}" >"${repo}/install.env"
  assert_env_refused "${reason}"
done <<'INVALID_ENV'
OCSERV_CONTROLLER_COMPOSE_SH=/tmp/override|unknown configuration variable
OCSERV_PUBLIC_HOST example.test|expected KEY=VALUE
OCSERV_PUBLIC_HOST=$(touch pwned-marker)|must be a literal value
INVALID_ENV
[[ ! -e "${repo}/pwned-marker" ]] || die "install.env was executed"
printf 'OCSERV_PUBLIC_HOST=controller.example.test\n' >"${repo}/install.env"
chmod 0664 "${repo}/install.env"
assert_env_refused "group/world-writable"
mv "${repo}/install.env" "${fixture}/unsafe-install-env"
ln -s "${fixture}/unsafe-install-env" "${repo}/install.env"
assert_env_refused "symlink"

# 8i. unsupported non-PostgreSQL backend configurations fail closed before host bootstrap or download.
reset_logs
reset_checkout
install_docker_client_stub
EXTRA_ENV=(OCSERV_DATABASE_BACKEND=mysql OCSERV_DATABASE_DEPLOYMENT=external)
capture
EXTRA_ENV=()
assert_status 1 "OCSERV_DATABASE_BACKEND=mysql must fail closed"
assert_output "unsupported database configuration"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
assert_log_empty "${controller_log}"
echo "unsupported database configurations fail closed without host mutation"

reset_logs
reset_checkout
install_docker_client_stub
git -C "${repo}" commit -q --allow-empty -m rc
git -C "${repo}" tag v0.2.0-rc.1
capture
assert_status 0 "an exact RC checkout must install"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.2.0-rc.1/controller-release-${native_arch}.json"
git -C "${repo}" tag v0.2.0
reset_logs
capture
assert_status 1 "RC and final at the same HEAD must fail closed"
assert_log_empty "${curl_log}"
# The backend orchestrator also consumes runtime metadata without any Git.
scenario
install_docker_client_stub
runtime_commit="$(git -C "${repo}" rev-parse HEAD)"
mv "${repo}/.git" "${fixture}/legacy-git"
mv "${repo}/.gitignore" "${fixture}/legacy-ignore"
cp "${ROOT}/deploy/lib/controller-runtime.sh" "${repo}/deploy/lib/"
printf 'v0.1.2 %s\n' "${runtime_commit}" >"${repo}/RELEASE"
(cd "${repo}" && find deploy RELEASE -type f | sort | xargs sha256sum >SHA256SUMS)
printf '#!/usr/bin/env bash\nexit 97\n' >"${bin}/git"
chmod 755 "${bin}/git"
capture --non-interactive
assert_status 0 "the runtime installer must not need Git"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-${native_arch}.json"
assert_log_contains "${controller_log}" 'install --release-file'
reset_logs
printf '\n# local drift\n' >>"${repo}/deploy/production/controller.sh"
capture --non-interactive
assert_status 1 "runtime drift must fail before host bootstrap"
assert_output 'runtime content changed'
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
