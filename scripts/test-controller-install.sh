#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
fixture="$(mktemp -d "${HOME}/.ocservia-install-test.XXXXXX")"

can_root() {
  (( EUID == 0 )) || sudo -n true >/dev/null 2>&1
}

as_root() {
  if (( EUID == 0 )); then
    "$@"
  else
    sudo -n "$@"
  fi
}

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
printf 'launcher:%s\n' "${SUDO_USER:-<unset>}" >>"${bootstrap_log}"
printf '%s\n' "$*" >>"${bootstrap_log}"
printf 'OCSERV_PUBLIC_HOST=%s\n' "${OCSERV_PUBLIC_HOST:-<unset>}" >>"${bootstrap_log}"
printf 'OCSERV_SECRET_DIR=%s\n' "${OCSERV_SECRET_DIR:-<unset>}" >>"${bootstrap_log}"
printf 'OCSERV_BACKUP_DIR=%s\n' "${OCSERV_BACKUP_DIR:-<unset>}" >>"${bootstrap_log}"
printf 'OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=%s\n' "${OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY:-<unset>}" >>"${bootstrap_log}"
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
  git -C "${repo}" tag -d v0.2.0-rc1 v0.1.3 v0.1.4 >/dev/null 2>&1 || true
  # install.env is git-ignored, so git clean never removes it; the install.env
  # scenarios must not leak into unrelated ones.
  rm -f -- "${repo}/install.env" "${fixture}/poison-install-env"
}

run_installer() {
  INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    INSTALL_TEST_ARCH="${INSTALL_TEST_ARCH:-}" \
    PATH="${bin}:${PATH}" \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem" \
    env "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" "${repo}/deploy/production/install.sh" "$@"
}

# The install.env contract is bound to the invocation directory, so its
# scenarios run the installer with the fixture checkout as $PWD.
run_installer_from() {
  local directory="$1"
  shift
  (
    cd -- "${directory}"
    INSTALL_TEST_CURL_LOG="${curl_log}" \
      INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
      INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
      INSTALL_TEST_SUDO_LOG="${sudo_log}" \
      INSTALL_TEST_ARCH="${INSTALL_TEST_ARCH:-}" \
      PATH="${bin}:${PATH}" \
      OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
      OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem" \
      env "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" "${repo}/deploy/production/install.sh" "$@"
  )
}

capture() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_installer "$@" 2>&1)" || RUN_STATUS=$?
}

capture_from() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_installer_from "$@" 2>&1)" || RUN_STATUS=$?
}

assert_status() {
  (( RUN_STATUS == "$1" )) ||
    die "expected exit status $1 but got ${RUN_STATUS}"
}

assert_output() {
  grep -q -- "$1" <<<"${RUN_OUTPUT}" ||
    die "expected output to contain '$1'"
}

assert_log_contains() {
  grep -qF -- "$2" "$1" || die "expected $1 to contain '$2'"
}

assert_log_empty() {
  [[ ! -s "$1" ]] || die "expected $1 to stay empty, got: $(cat -- "$1")"
}

# The installer selects the manifest from uname -m, so assertions follow the
# real host architecture; scenario 4a pins aarch64 explicitly.
case "$(uname -m)" in
  x86_64) native_arch=amd64 ;;
  aarch64|arm64) native_arch=arm64 ;;
  *) native_arch=amd64 ;;
esac

# 1. usage errors.
reset_logs
reset_checkout
capture unexpected-argument
assert_status 2 "an unexpected argument must be a usage error"
capture --root-lifecycle extra-argument
assert_status 2 "extra arguments alongside --root-lifecycle must be a usage error"
echo "usage errors fail with status 2"

# 2. a non-tag checkout is rejected before any host mutation.
reset_logs
reset_checkout
git -C "${repo}" commit -q --allow-empty -m beyond-tag
capture
assert_status 1 "a checkout past the release tag must fail closed"
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
assert_log_empty "${controller_log}"
echo "a non-tag checkout is rejected before any host mutation"

# 3. a non-SemVer tag is not a release identity.
reset_logs
reset_checkout
git -C "${repo}" commit -q --allow-empty -m rc
git -C "${repo}" tag v0.2.0-rc1
capture
assert_status 1 "a non-SemVer tag must fail closed"
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${bootstrap_log}"
echo "a non-SemVer tag is rejected"

# 3a. ambiguous release tags fail closed.
reset_logs
reset_checkout
git -C "${repo}" commit -q --allow-empty -m double-tag
git -C "${repo}" tag v0.1.3
git -C "${repo}" tag v0.1.4
capture
assert_status 1 "multiple SemVer tags at HEAD must fail closed"
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${bootstrap_log}"
echo "multiple SemVer tags at HEAD are rejected"

# 3b. a dirty checkout is rejected early.
reset_logs
reset_checkout
printf '\n# local edit\n' >>"${repo}/deploy/production/install.sh"
capture
assert_status 1 "a dirty checkout must fail closed"
assert_output "dirty"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "a dirty checkout is rejected before any host mutation"

# 3c. a missing release trust key is rejected before any host mutation.
reset_logs
reset_checkout
EXTRA_ENV=("OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=")
capture
assert_status 1 "a missing release public key must fail closed"
assert_output "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "a missing release trust key is rejected"

# 4. the full happy path as the launcher user on a host with Docker.
reset_logs
reset_checkout
install_docker_client_stub
EXTRA_ENV=("OCSERV_BACKUP_DIR=${fixture}/backup")
capture
assert_status 0 "the launcher-user install flow must succeed"
assert_output "release identity: v0.1.2"
if (( EUID == 0 )); then
  # A root test shell exercises the root lifecycle: no whole-script sudo.
  assert_log_empty "${sudo_log}"
  assert_log_contains "${bootstrap_log}" "install --backup-dir ${fixture}/backup"
else
  assert_log_contains "${sudo_log}" "env OCSERV_CONTROLLER_STATE_ROOT=${state_root} ${repo}/deploy/production/bootstrap-host.sh install --backup-dir ${fixture}/backup"
  assert_log_contains "${bootstrap_log}" "install --backup-dir ${fixture}/backup"
fi
[[ "$(wc -l <"${curl_log}" | tr -d ' ')" == 4 ]] ||
  die "expected exactly four bundle downloads, got: $(cat -- "${curl_log}")"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-${native_arch}.json"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-${native_arch}.json.sha256"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/SHA256SUMS"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/SHA256SUMS.sig"
if grep -q "release-signing" "${curl_log}"; then
  die "the installer must never download release trust material: $(cat -- "${curl_log}")"
fi
bundle_dir="${state_root}/release-bundles/v0.1.2"
[[ "$(stat -c '%a' "${bundle_dir}")" == 700 ]] ||
  die "bundle directory mode is wrong: $(stat -c '%a' "${bundle_dir}")"
[[ "$(ls -A -- "${bundle_dir}" | sort | tr '\n' ' ')" == \
  "SHA256SUMS SHA256SUMS.sig controller-release-${native_arch}.json controller-release-${native_arch}.json.sha256 " ]] ||
  die "unexpected bundle contents: $(ls -A -- "${bundle_dir}")"
for bundle_file in \
  "controller-release-${native_arch}.json" \
  "controller-release-${native_arch}.json.sha256" \
  SHA256SUMS \
  SHA256SUMS.sig; do
  [[ "$(stat -c '%a' "${bundle_dir}/${bundle_file}")" == 600 ]] ||
    die "bundle file ${bundle_file} mode is wrong"
done
assert_log_contains "${controller_log}" "install --release-file ${bundle_dir}/controller-release-${native_arch}.json"
echo "the launcher install flow bootstraps, downloads, and activates"

# 4a. arm64 hosts select the arm64 manifest automatically.
reset_logs
reset_checkout
install_docker_client_stub
EXTRA_ENV=("INSTALL_TEST_ARCH=aarch64")
capture
assert_status 0 "the arm64 install flow must succeed"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v0.1.2/controller-release-arm64.json.sha256"
if grep -q "controller-release-amd64" "${curl_log}"; then
  die "an arm64 host must not download the amd64 manifest"
fi
assert_log_contains "${controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-arm64.json"
echo "arm64 hosts select the arm64 release manifest"

# 5. a bootstrap failure stops before download and activation.
reset_logs
reset_checkout
install_docker_client_stub
EXTRA_ENV=("MOCK_BOOTSTRAP_EXIT=1")
capture
assert_status 1 "a bootstrap failure must fail the installer"
assert_log_empty "${curl_log}"
assert_log_empty "${controller_log}"
echo "a bootstrap failure stops before download and activation"

# 5a. a state root the bootstrap did not provision is rejected.
reset_logs
reset_checkout
install_docker_client_stub
EXTRA_ENV=("MOCK_BOOTSTRAP_PROVISION_STATE=0")
capture
assert_status 1 "a missing state root must fail closed"
assert_output "was not provisioned"
assert_log_empty "${controller_log}"
echo "a missing state root is rejected before activation"

# 6. a whole-script sudo invocation mismatches the launcher contract.
if can_root; then
  reset_logs
  reset_checkout
  RUN_STATUS=0
  RUN_OUTPUT="$(sudo -n env \
    INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    PATH="${bin}:${PATH}" \
    SUDO_USER=ocservia-operator \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem" \
    "${repo}/deploy/production/install.sh" 2>&1)" || RUN_STATUS=$?
  assert_status 1 "whole-script sudo must fail closed"
  assert_output "run install.sh as the lifecycle launcher user"
  assert_output "--root-lifecycle"
  assert_log_empty "${bootstrap_log}"
  assert_log_empty "${curl_log}"
  assert_log_empty "${controller_log}"
  echo "whole-script sudo is rejected with the launcher mismatch"

  # 6a. a plain root shell is an allowed root lifecycle and is the supported
  # fresh-host path: with no Docker client anywhere on PATH, root installs
  # Docker through the bootstrap and activates without a permission mismatch.
  reset_logs
  reset_checkout
  RUN_STATUS=0
  RUN_OUTPUT="$(sudo -n env \
    -u SUDO_USER \
    INSTALL_TEST_CURL_LOG="${curl_log}" \
    INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}" \
    INSTALL_TEST_CONTROLLER_LOG="${controller_log}" \
    INSTALL_TEST_SUDO_LOG="${sudo_log}" \
    PATH="${fresh_bin}" \
    OCSERV_CONTROLLER_STATE_ROOT="${state_root}" \
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem" \
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
  reset_logs
  reset_checkout
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
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem" \
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
    reset_logs
    reset_checkout
    RUN_STATUS=0
    RUN_OUTPUT="$(
      export INSTALL_TEST_CURL_LOG="${curl_log}"
      export INSTALL_TEST_BOOTSTRAP_LOG="${bootstrap_log}"
      export INSTALL_TEST_CONTROLLER_LOG="${controller_log}"
      export INSTALL_TEST_SUDO_LOG="${sudo_log}"
      export PATH="${root_lifecycle_bin}"
      export OCSERV_CONTROLLER_STATE_ROOT="${state_root}"
      export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/controller-release-signing.pub.pem"
      export OCSERV_SECRET_DIR="${fixture}/secrets"
      export OCSERV_BACKUP_DIR="${fixture}/backup"
      export OCSERV_PUBLIC_HOST=controller.example.test
      export OCSERV_OIDC_ISSUER=https://id.example.test
      export OCSERV_OIDC_CLIENT_ID=ocservia
      export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test/v1
      export OCSERV_OTEL_BACKEND_ENDPOINT=otel.example.test:4317
      export OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1
      export OCSERV_CONTROLLER_ENDPOINT_ID=0000000000000000000000000000000000000000000000000000000000000000
      export OCSERV_RELAY_URL_A=https://relay-a.example.test
      export OCSERV_RELAY_URL_B=https://relay-b.example.test
      export OCSERV_HTTPS_ADDRESS=127.0.0.1
      export OCSERV_BACKUP_INTERVAL_SECONDS=300
      export OCSERV_BACKUP_RETENTION_COUNT=4
      export UNRELATED_ENV=must-not-cross-sudo
      export OCSERV_UNRELATED_CONFIG=must-not-cross-sudo
      "${repo}/deploy/production/install.sh" --root-lifecycle 2>&1
    )" || RUN_STATUS=$?
    assert_status 0 "the operator root-lifecycle command must succeed through sudo env_reset"
    assert_output "release identity: v0.1.2"
    assert_log_contains "${root_sudo_log}" "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${fixture}/controller-release-signing.pub.pem"
    assert_log_contains "${root_sudo_log}" "OCSERV_PUBLIC_HOST=controller.example.test"
    if grep -q "UNRELATED_ENV\|OCSERV_UNRELATED_CONFIG" "${root_sudo_log}"; then
      die "the root-lifecycle sudo command must not forward unrelated environment variables: $(cat -- "${root_sudo_log}")"
    fi
    assert_log_contains "${root_bootstrap_log}" "launcher:<unset>"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_PUBLIC_HOST=controller.example.test"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_SECRET_DIR=${fixture}/secrets"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_BACKUP_DIR=${fixture}/backup"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${fixture}/controller-release-signing.pub.pem"
    assert_log_contains "${root_bootstrap_log}" "UNRELATED_ENV=<unset>"
    assert_log_contains "${root_bootstrap_log}" "OCSERV_UNRELATED_CONFIG=<unset>"
    [[ "$(wc -l <"${root_curl_log}" | tr -d ' ')" == 4 ]] ||
      die "expected exactly four root-lifecycle bundle downloads, got: $(cat -- "${root_curl_log}")"
    assert_log_contains "${root_controller_log}" "install --release-file ${state_root}/release-bundles/v0.1.2/controller-release-${native_arch}.json"
    as_root chown -R "$(id -u):$(id -g)" "${repo}"
    echo "the operator root-lifecycle command forwards only production configuration across sudo env_reset"
  fi

  # 6d. --root-lifecycle resolves install.env as the launcher user and
  # forwards only the effective allowlist across the boundary. The sudo
  # wrapper swaps $PWD/install.env for poisoned content the instant the
  # privilege transition starts, so a root re-read would hand root
  # attacker-chosen configuration; the re-exec marker must prevent it.
  if (( EUID != 0 )); then
    reset_logs
    reset_checkout
    cat >"${repo}/install.env" <<EOF
OCSERV_PUBLIC_HOST=controller-root-file.example.test
OCSERV_SECRET_DIR=${fixture}/root-file-secrets
OCSERV_BACKUP_DIR=${fixture}/root-file-backup
OCSERV_CONTROLLER_STATE_ROOT=${state_root}
OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${fixture}/controller-release-signing.pub.pem
EOF
    printf 'OCSERV_PUBLIC_HOST=controller-poisoned.example.test\nOCSERV_SECRET_DIR=%s\nOCSERV_BACKUP_DIR=%s\n' \
      "${fixture}/poisoned-secrets" "${fixture}/poisoned-backup" >"${fixture}/poison-install-env"
    RUN_STATUS=0
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
  reset_logs
  reset_checkout
  EXTRA_ENV=("PATH=${fresh_bin}")
  capture
  assert_status 1 "a non-root launcher on a Docker-less host must fail closed"
  assert_output "--root-lifecycle"
  assert_output "post-install"
  assert_output "never modifies the Docker permission model"
  assert_log_empty "${bootstrap_log}"
  assert_log_empty "${curl_log}"
  assert_log_empty "${controller_log}"
  echo "a non-root launcher on a fresh Docker-less host fails closed before mutation"
else
  echo "fresh-host launcher-path case skipped: running as root"
fi

# 8. install.env supplies allowlisted configuration from the invocation
# directory. The fixture repo carries the repository's real .gitignore, so a
# present install.env must also stay invisible to the clean-release-checkout
# contract, and every value below must reach the bootstrap.
reset_logs
reset_checkout
install_docker_client_stub
cat >"${repo}/install.env" <<EOF
OCSERV_BACKUP_DIR=${fixture}/file-backup
OCSERV_PUBLIC_HOST=controller-file.example.test
OCSERV_HTTPS_ADDRESS=10.0.0.9
EOF
capture_from "${repo}"
assert_status 0 "install.env values must load and keep the checkout clean"
assert_output "release identity: v0.1.2"
assert_log_contains "${bootstrap_log}" "install --backup-dir ${fixture}/file-backup"
assert_log_contains "${bootstrap_log}" "OCSERV_PUBLIC_HOST=controller-file.example.test"
echo "install.env values load without dirtying the release checkout"

# 8a. an explicit shell variable wins over install.env.
reset_logs
reset_checkout
install_docker_client_stub
cat >"${repo}/install.env" <<EOF
OCSERV_PUBLIC_HOST=controller-file.example.test
OCSERV_BACKUP_DIR=${fixture}/file-backup
EOF
EXTRA_ENV=("OCSERV_PUBLIC_HOST=controller-env.example.test")
capture_from "${repo}"
assert_status 0 "an explicit shell variable must win over install.env"
assert_log_contains "${bootstrap_log}" "OCSERV_PUBLIC_HOST=controller-env.example.test"
if grep -q "controller-file.example.test" "${bootstrap_log}"; then
  die "the file value must not override the explicit shell variable: $(cat -- "${bootstrap_log}")"
fi
echo "an explicit shell variable overrides install.env"

# 8a-2. an explicitly empty shell variable also wins over install.env: the
# file must not fill a variable the operator deliberately unset in the
# session, so the bootstrap runs without the file's --backup-dir.
reset_logs
reset_checkout
install_docker_client_stub
cat >"${repo}/install.env" <<EOF
OCSERV_BACKUP_DIR=${fixture}/file-backup
EOF
EXTRA_ENV=("OCSERV_BACKUP_DIR=")
capture_from "${repo}"
assert_status 0 "an explicitly empty shell variable must win over install.env"
assert_log_contains "${bootstrap_log}" "install"
if grep -q -- "--backup-dir" "${bootstrap_log}"; then
  die "install.env must not fill the deliberately emptied OCSERV_BACKUP_DIR: $(cat -- "${bootstrap_log}")"
fi
echo "an explicitly empty shell variable overrides install.env"

# 8b. an unknown key fails closed before any host mutation.
reset_logs
reset_checkout
printf 'OCSERV_NOT_A_REAL_SETTING=x\n' >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "an unknown install.env key must fail closed"
assert_output "unknown configuration variable OCSERV_NOT_A_REAL_SETTING"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
assert_log_empty "${controller_log}"
assert_log_empty "${sudo_log}"
echo "an unknown install.env key fails closed before any host mutation"

# 8b-2. internal test seams gain no install.env entry: the compose override
# seam is production-reachable configuration and must stay environment-only.
reset_logs
reset_checkout
printf 'OCSERV_CONTROLLER_COMPOSE_SH=%s\n' "${fixture}/evil-compose.sh" >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a test seam must not be settable through install.env"
assert_output "unknown configuration variable OCSERV_CONTROLLER_COMPOSE_SH"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "internal seams are not install.env keys"

# 8c. malformed lines fail closed.
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST controller-file.example.test\n' >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a line without = must fail closed"
assert_output "expected KEY=VALUE"
reset_logs
reset_checkout
printf 'oCsErV_PUBLIC_HOST=controller-file.example.test\n' >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a lowercase key must fail closed"
assert_output "expected KEY=VALUE"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "malformed install.env lines fail closed"

# 8d. nothing in install.env is ever executed: command substitution,
# backticks, and source lines are rejected, never evaluated.
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST=$(touch %s)\n' "${fixture}/pwned-marker" >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "command substitution syntax must fail closed"
assert_output "must be a literal value"
[[ ! -e "${fixture}/pwned-marker" ]] ||
  die "command substitution in install.env must never execute"
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST=`touch %s`\n' "${fixture}/pwned-marker" >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "backtick syntax must fail closed"
[[ ! -e "${fixture}/pwned-marker" ]] ||
  die "backticks in install.env must never execute"
reset_logs
reset_checkout
printf 'source %s\n' "${fixture}/evil.sh" >"${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a source line must fail closed as malformed"
assert_output "expected KEY=VALUE"
[[ ! -e "${fixture}/evil.sh" ]] || die "the test must not create evil.sh"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "install.env content is never executed"

# 8e. an install.env symlink is rejected.
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST=controller-symlink.example.test\n' >"${fixture}/install-env-real"
ln -s "${fixture}/install-env-real" "${repo}/install.env"
capture_from "${repo}"
assert_status 1 "an install.env symlink must fail closed"
assert_output "symlink"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
rm -f -- "${repo}/install.env" "${fixture}/install-env-real"
echo "an install.env symlink is rejected"

# 8f. a group-writable install.env is rejected.
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST=controller-file.example.test\n' >"${repo}/install.env"
chmod 0664 -- "${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a group-writable install.env must fail closed"
assert_output "group/world-writable"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "a group-writable install.env is rejected"

# 8g. a world-writable install.env is rejected.
reset_logs
reset_checkout
printf 'OCSERV_PUBLIC_HOST=controller-file.example.test\n' >"${repo}/install.env"
chmod 0666 -- "${repo}/install.env"
capture_from "${repo}"
assert_status 1 "a world-writable install.env must fail closed"
assert_output "group/world-writable"
assert_log_empty "${bootstrap_log}"
assert_log_empty "${curl_log}"
echo "a world-writable install.env is rejected"

# 8h. the quoted-value form strips exactly one static quote layer.
reset_logs
reset_checkout
install_docker_client_stub
cat >"${repo}/install.env" <<EOF
OCSERV_PUBLIC_HOST='controller-quoted.example.test'
OCSERV_HTTPS_ADDRESS="10.0.0.9"
EOF
capture_from "${repo}"
assert_status 0 "statically quoted install.env values must load"
assert_log_contains "${bootstrap_log}" "OCSERV_PUBLIC_HOST=controller-quoted.example.test"
echo "one static quote layer is stripped without expansion"

echo "Controller install tests passed"
