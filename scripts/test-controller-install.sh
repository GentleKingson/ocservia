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
EXTRA_ENV=()
RUN_STATUS=0
RUN_OUTPUT=""

die() {
  echo "Controller install tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

mkdir -m 700 -- "${bin}" "${logs}"
mkdir -p -- "${repo}/deploy/production"

# The installer resolves ROOT from its own location, so the fixture exercises
# the real install.sh against a fixture release checkout with mocked sibling
# entrypoints (bootstrap-host.sh, controller.sh) and mocked PATH tools.
cp -- "${INSTALL}" "${repo}/deploy/production/install.sh"

cat >"${repo}/deploy/production/bootstrap-host.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'launcher:%s\n' "${SUDO_USER:-<unset>}" >>"${INSTALL_TEST_BOOTSTRAP_LOG}"
printf '%s\n' "$*" >>"${INSTALL_TEST_BOOTSTRAP_LOG}"
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
printf '%s\n' "$*" >>"${INSTALL_TEST_CONTROLLER_LOG}"
exit "${MOCK_CONTROLLER_EXIT:-0}"
EOF

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${INSTALL_TEST_CURL_LOG}"
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
link_tool() {
  local tool="$1" path
  path="$(command -v "${tool}" || true)"
  [[ -n "${path}" ]] || die "required test tool not found: ${tool}"
  ln -s "${path}" "${fresh_bin}/${tool}"
}
for tool in bash git env chmod mkdir dirname; do
  link_tool "${tool}"
done
ln -s "${bin}/sudo" "${fresh_bin}/sudo"
ln -s "${bin}/curl" "${fresh_bin}/curl"
ln -s "${bin}/uname" "${fresh_bin}/uname"

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
  EXTRA_ENV=()
}

reset_checkout() {
  git -C "${repo}" checkout -q -- .
  git -C "${repo}" clean -qfd
  git -C "${repo}" reset -q --hard v0.1.2
  git -C "${repo}" tag -d v0.2.0-rc1 v0.1.3 v0.1.4 >/dev/null 2>&1 || true
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

capture() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_installer "$@" 2>&1)" || RUN_STATUS=$?
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

# 1a. --root-lifecycle without root is rejected before any host mutation.
if (( EUID != 0 )); then
  reset_logs
  reset_checkout
  capture --root-lifecycle
  assert_status 1 "--root-lifecycle without root must fail closed"
  assert_output "--root-lifecycle must run as root"
  assert_log_empty "${bootstrap_log}"
  assert_log_empty "${curl_log}"
  assert_log_empty "${controller_log}"
  echo "--root-lifecycle without root is rejected"
fi

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

echo "Controller install tests passed"
