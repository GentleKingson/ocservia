#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/deploy/production/bootstrap-host.sh"

# The bootstrap targets Ubuntu and uses GNU stat; skip on hosts without the
# GNU userland (mirrors the flock guard in test-controller-lifecycle.sh).
stat -c '%u' . >/dev/null 2>&1 || {
  echo "Controller host bootstrap tests skipped: GNU stat is unavailable" >&2
  exit 0
}
[[ -x "${BOOTSTRAP}" ]] || {
  echo "Controller host bootstrap tests require an executable bootstrap script" >&2
  exit 1
}

# Keep the fixture under HOME: /tmp is world-writable (mode 1777), which the
# state root ancestry contract must reject.
fixture="$(mktemp -d "${HOME}/.ocservia-bootstrap-test.XXXXXX")"

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
    as_root rm -rf -- "${fixture}"
  else
    rm -rf -- "${fixture}"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM
bin="${fixture}/bin"
work="${fixture}/work"
logs="${fixture}/logs"
dpkg_db="${fixture}/dpkg-packages"
docker_template="${fixture}/docker-template"
docker_unready="${fixture}/docker-unready"
docker_denied="${fixture}/docker-denied"
compose_no_wait="${fixture}/compose-no-wait"
os_release="${fixture}/os/ubuntu-24.04"
apt_log="${logs}/apt.log"
curl_log="${logs}/curl.log"
systemctl_log="${logs}/systemctl.log"
EXTRA_ENV=()
RUN_STATUS=0
RUN_OUTPUT=""

die() {
  echo "Controller host bootstrap tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

mkdir -m 700 -- "${bin}" "${logs}" "${fixture}/os"
install -d -m 0755 -- "${work}"

# The bootstrap child must only see the mocked docker/apt/dpkg-query commands,
# so it runs with a restricted PATH: the mock bin first, then a symlink farm of
# bash plus the real coreutils binaries the bootstrap and the mocks need. A
# real Docker on the host must stay invisible to the docker-absent scenarios.
rootfs_bin="${fixture}/rootfs-bin"
mkdir -p -- "${rootfs_bin}"
link_tool() {
  local tool="$1" path
  path="$(command -v "${tool}" || true)"
  if [[ -z "${path}" && -x "/usr/sbin/${tool}" ]]; then
    path="/usr/sbin/${tool}"
  fi
  [[ -n "${path}" ]] || die "required test tool not found: ${tool}"
  ln -s "${path}" "${rootfs_bin}/${tool}"
}
for tool in bash stat install mkdir chown dirname cat grep id chmod cp rm realpath env runuser; do
  link_tool "${tool}"
done

cat >"${fixture}/os/ubuntu-24.04" <<'EOF'
PRETTY_NAME="Ubuntu 24.04 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
UBUNTU_CODENAME=noble
EOF

cat >"${fixture}/os/debian-12" <<'EOF'
PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION_CODENAME=bookworm
ID=debian
EOF

cat >"${docker_template}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
command="${1:-}"
if [[ "${command}" == info ]]; then
  if [[ -e "${BOOTSTRAP_TEST_DOCKER_UNREADY:-/nonexistent-ocservia-flag}" ]]; then
    echo "docker mock: daemon unavailable" >&2
    exit 1
  fi
  if [[ -e "${BOOTSTRAP_TEST_DOCKER_DENIED:-/nonexistent-ocservia-flag}" ]] && [[ "$(id -u)" != 0 ]]; then
    echo "docker mock: permission denied while trying to connect to the Docker daemon socket" >&2
    exit 1
  fi
  exit 0
fi
case "${command}" in
  info) exit 0 ;;
  version)
    if [[ "${2:-}" == --format ]]; then
      printf '28.0.0-mock\n'
    else
      printf 'Docker version 28.0.0-mock\n'
    fi
    ;;
  compose)
    if [[ "${BOOTSTRAP_TEST_NO_COMPOSE:-}" == 1 ]]; then
      echo "docker mock: compose plugin unavailable" >&2
      exit 1
    fi
    case "${2:-}" in
      version) printf 'Docker Compose version v2.39.1-mock\n' ;;
      up)
        if [[ "${3:-}" == --help ]]; then
          if [[ -e "${BOOTSTRAP_TEST_COMPOSE_NO_WAIT:-/nonexistent-ocservia-flag}" ]]; then
            printf '%s\n' 'Options:' '      --no-wait-at-all'
          else
            printf '%s\n' '      --wait                         Wait for services to be running|healthy'
          fi
        fi
        ;;
    esac
    ;;
esac
exit 0
EOF

cat >"${bin}/uname" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${BOOTSTRAP_TEST_ARCH:-x86_64}"
EOF

cat >"${bin}/dpkg-query" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
package=""
for argument in "$@"; do
  package="${argument}"
done
if grep -Fxq -- "${package}" "${BOOTSTRAP_TEST_DPKG_PACKAGES}" 2>/dev/null; then
  printf 'ii \n'
  exit 0
fi
echo "dpkg-query: no packages found matching ${package}" >&2
exit 1
EOF

cat >"${bin}/apt-get" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${BOOTSTRAP_TEST_APT_LOG}"
if [[ "${1:-}" == install ]]; then
  shift
  for argument in "$@"; do
    case "${argument}" in
      -*) continue ;;
    esac
    case "${argument}" in
      docker-ce|docker-ce-cli|containerd.io|docker-buildx-plugin|docker-compose-plugin)
        if [[ ! -x "${BOOTSTRAP_TEST_BIN}/docker" ]]; then
          cp -- "${BOOTSTRAP_TEST_DOCKER_TEMPLATE}" "${BOOTSTRAP_TEST_BIN}/docker"
          chmod 0755 -- "${BOOTSTRAP_TEST_BIN}/docker"
        fi
        ;;
    esac
    if ! grep -Fxq -- "${argument}" "${BOOTSTRAP_TEST_DPKG_PACKAGES}" 2>/dev/null; then
      printf '%s\n' "${argument}" >>"${BOOTSTRAP_TEST_DPKG_PACKAGES}"
    fi
  done
fi
exit 0
EOF

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${BOOTSTRAP_TEST_CURL_LOG}"
output=""
while (($# > 0)); do
  if [[ "$1" == -o ]]; then
    shift
    output="$1"
    break
  fi
  shift
done
if [[ -n "${output}" ]]; then
  : >"${output}"
fi
exit 0
EOF

cat >"${bin}/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${BOOTSTRAP_TEST_SYSTEMCTL_LOG}"
if [[ "${1:-}" == enable && "${2:-}" == --now ]]; then
  if [[ -n "${BOOTSTRAP_TEST_DOCKER_UNREADY:-}" ]]; then
    rm -f -- "${BOOTSTRAP_TEST_DOCKER_UNREADY}"
  fi
fi
exit 0
EOF

cat >"${fixture}/docker-compose-stub" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 0755 -- "${bin}/uname" "${bin}/dpkg-query" "${bin}/apt-get" "${bin}/curl" \
  "${bin}/systemctl" "${fixture}/docker-compose-stub" "${docker_template}"

reset_scenario() {
  # Root scenarios leave root-owned files inside the work tree.
  if can_root; then
    as_root rm -rf -- "${work}"
  else
    rm -rf -- "${work}"
  fi
  rm -f -- "${bin}/docker" "${bin}/docker-compose" "${dpkg_db}"
  install -d -m 0755 -- "${work}/apt/sources"
  : >"${apt_log}"
  : >"${curl_log}"
  : >"${systemctl_log}"
  : >"${dpkg_db}"
  rm -f -- "${docker_unready}" "${docker_denied}" "${compose_no_wait}"
  EXTRA_ENV=()
  os_release="${fixture}/os/ubuntu-24.04"
}

install_docker_mock() {
  cp -- "${docker_template}" "${bin}/docker"
  chmod 0755 -- "${bin}/docker"
}

mark_all_tools_installed() {
  cat >"${dpkg_db}" <<'EOF'
jq
util-linux
curl
openssl
ca-certificates
EOF
}

run_bootstrap() {
  local prefix="$1"
  shift
  local pairs=(
    "PATH=${bin}:${rootfs_bin}"
    "OCSERV_BOOTSTRAP_OS_RELEASE=${os_release}"
    "OCSERV_BOOTSTRAP_APT_KEYRING_DIR=${work}/apt/keyrings"
    "OCSERV_BOOTSTRAP_APT_SOURCES_DIR=${work}/apt/sources"
    "OCSERV_CONTROLLER_STATE_ROOT=${work}/state-root"
    "BOOTSTRAP_TEST_BIN=${bin}"
    "BOOTSTRAP_TEST_DPKG_PACKAGES=${dpkg_db}"
    "BOOTSTRAP_TEST_APT_LOG=${apt_log}"
    "BOOTSTRAP_TEST_CURL_LOG=${curl_log}"
    "BOOTSTRAP_TEST_SYSTEMCTL_LOG=${systemctl_log}"
    "BOOTSTRAP_TEST_DOCKER_TEMPLATE=${docker_template}"
    "BOOTSTRAP_TEST_DOCKER_UNREADY=${docker_unready}"
    "BOOTSTRAP_TEST_DOCKER_DENIED=${docker_denied}"
    "BOOTSTRAP_TEST_COMPOSE_NO_WAIT=${compose_no_wait}"
  )
  if [[ "${prefix}" == sudo ]]; then
    if (( EUID == 0 )); then
      env "${pairs[@]}" "${EXTRA_ENV[@]}" "${BOOTSTRAP}" "$@"
    else
      sudo -n env "${pairs[@]}" "${EXTRA_ENV[@]}" "${BOOTSTRAP}" "$@"
    fi
  else
    env "${pairs[@]}" "${EXTRA_ENV[@]}" "${BOOTSTRAP}" "$@"
  fi
}

capture() {
  RUN_STATUS=0
  RUN_OUTPUT="$(run_bootstrap "$@" 2>&1)" || RUN_STATUS=$?
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

# 1. Usage errors.
reset_scenario
capture self
assert_status 2 "no arguments must be a usage error"
capture self frobnicate
assert_status 2 "unknown command must be a usage error"
reset_scenario
capture self check --backup-dir relative/path
assert_status 2 "a relative --backup-dir must be a usage error"
echo "usage errors fail with status 2"

# 2. Unsupported operating system.
reset_scenario
os_release="${fixture}/os/debian-12"
capture self check
assert_status 1 "unsupported OS must fail closed"
assert_output "supported hosts are Ubuntu 24.04"
assert_log_empty "${apt_log}"
echo "unsupported OS fails closed without mutation"

# 3. Unsupported architecture.
reset_scenario
EXTRA_ENV=("BOOTSTRAP_TEST_ARCH=riscv64")
capture self check
assert_status 1 "unsupported architecture must fail closed"
assert_output "unsupported architecture 'riscv64'"
assert_log_empty "${apt_log}"
echo "unsupported architecture fails closed without mutation"

# 4. check reports missing Docker without mutating packages.
reset_scenario
mark_all_tools_installed
capture self check
assert_status 1 "missing Docker must fail the host check"
assert_output "Docker is required but not installed"
assert_log_empty "${apt_log}"
echo "check reports missing Docker without package mutation"

# 5. check reports Compose without --wait support.
reset_scenario
install_docker_mock
mark_all_tools_installed
: >"${compose_no_wait}"
capture self check
assert_status 1 "Compose without --wait must fail the host check"
assert_output "docker compose up --wait"
assert_log_empty "${apt_log}"
echo "check reports Compose builds without --wait support"

# 6. check detects the docker.io conflict without uninstalling anything.
reset_scenario
install_docker_mock
mark_all_tools_installed
printf '%s\n' docker.io >>"${dpkg_db}"
capture self check
assert_status 1 "a conflicting runtime must fail the host check"
assert_output "docker.io"
assert_output "never uninstalls an existing runtime"
assert_log_empty "${apt_log}"
echo "conflicting docker.io package fails closed without uninstall"

# 7. check detects a standalone docker-compose without the v2 plugin.
reset_scenario
install_docker_mock
mark_all_tools_installed
cp -- "${fixture}/docker-compose-stub" "${bin}/docker-compose"
EXTRA_ENV=("BOOTSTRAP_TEST_NO_COMPOSE=1")
capture self check
assert_status 1 "standalone docker-compose without the plugin must fail"
assert_output "docker-compose"
assert_log_empty "${apt_log}"
echo "standalone docker-compose without the plugin fails closed"

# 7a. check detects the containerd conflict before any host mutation.
reset_scenario
install_docker_mock
mark_all_tools_installed
printf '%s\n' containerd >>"${dpkg_db}"
capture self check
assert_status 1 "the containerd conflict must fail the host check"
assert_output "containerd"
assert_log_empty "${apt_log}"
echo "conflicting containerd package fails closed without mutation"

# 7b. check detects Ubuntu's docker-compose-v2 package without the plugin.
reset_scenario
install_docker_mock
mark_all_tools_installed
printf '%s\n' docker-compose-v2 >>"${dpkg_db}"
EXTRA_ENV=("BOOTSTRAP_TEST_NO_COMPOSE=1")
capture self check
assert_status 1 "docker-compose-v2 without the plugin must fail the host check"
assert_output "docker-compose-v2"
assert_log_empty "${apt_log}"
echo "conflicting docker-compose-v2 package without the plugin fails closed"

# 8. check passes on a provisioned host and lists operator prerequisites.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0700 -- "${work}/state-root"
capture self check
assert_status 0 "a provisioned host must pass the read-only check"
assert_output "Controller host prerequisites satisfied"
assert_output "OCSERV_SECRET_DIR is not set"
assert_output "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY is not set"
assert_output "controller-release-amd64.json"
assert_log_empty "${apt_log}"
assert_log_empty "${systemctl_log}"
echo "check passes on a provisioned host without any mutation"

# 9. check rejects a state root with the wrong mode.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0755 -- "${work}/state-root"
capture self check
assert_status 1 "a wrong state root mode must fail the host check"
assert_output "state root"
assert_log_empty "${apt_log}"
echo "check rejects a state root with the wrong mode"

# 10. check rejects a backup directory with the wrong ownership.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0755 -- "${work}/backup-wrong"
capture self check --backup-dir "${work}/backup-wrong"
assert_status 1 "a wrong backup directory must fail the host check"
assert_output "999:999"
assert_log_empty "${apt_log}"
echo "check rejects a backup directory with the wrong ownership"

# 10a. check classifies a Docker socket permission denial for the invoking
# user and fails closed with remediation. The mock denial applies to non-root
# users only, mirroring root's unconditional Docker access.
if (( EUID != 0 )); then
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  install -d -m 0700 -- "${work}/state-root"
  : >"${docker_denied}"
  capture self check
  assert_status 1 "a launcher without Docker daemon access must fail the check"
  assert_output "cannot access the Docker daemon"
  assert_output "never modifies the Docker permission model"
  assert_log_empty "${apt_log}"
  echo "check reports a Docker permission denial for the invoking user"
fi

# 10b. a relative state root is rejected like controller.sh rejects it.
reset_scenario
install_docker_mock
mark_all_tools_installed
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=relative/state-root")
capture self check
assert_status 1 "a relative state root must fail the host check"
assert_output "must be an absolute path"
echo "check rejects a relative state root"

# 10c. a non-canonical state root path is rejected.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0700 -- "${work}/state-root"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/state-root/")
capture self check
assert_status 1 "a non-canonical state root must fail the host check"
assert_output "canonical path without traversal"
echo "check rejects a non-canonical state root path"

# 10d. a group/world-writable state root ancestor is rejected.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0777 -- "${work}/loose"
install -d -m 0700 -- "${work}/loose/state-root"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/loose/state-root")
capture self check
assert_status 1 "a world-writable ancestor must fail the host check"
assert_output "group/world writable"
echo "check rejects a group/world-writable state root ancestor"

# 10e. a symlinked state root ancestor is rejected.
reset_scenario
install_docker_mock
mark_all_tools_installed
install -d -m 0700 -- "${work}/real/state-root"
ln -s -- real "${work}/link"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/link/state-root")
capture self check
assert_status 1 "symlink ancestry must fail the host check"
assert_output "symlink ancestry"
echo "check rejects symlinked state root ancestry"

# 11. install refuses to run without root.
if (( EUID != 0 )); then
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  capture self install
  assert_status 1 "install without root must fail"
  assert_output "must run as root or via sudo"
  assert_log_empty "${apt_log}"
  echo "install refuses to run without root"
fi

if can_root; then
  # 12. install on a bare host installs tools, the Docker repository, Docker,
  # and creates the non-secret directories with the lifecycle contract.
  reset_scenario
  capture sudo install --backup-dir "${work}/backup"
  assert_status 0 "install on a bare host must succeed"
  assert_log_contains "${apt_log}" "install -y jq util-linux curl openssl ca-certificates"
  assert_log_contains "${apt_log}" "install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin"
  assert_log_contains "${curl_log}" "https://download.docker.com/linux/ubuntu/gpg"
  expected_source="deb [arch=amd64 signed-by=${work}/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable"
  [[ "$(cat -- "${work}/apt/sources/docker.list")" == "${expected_source}" ]] ||
    die "unexpected docker.list content: $(cat -- "${work}/apt/sources/docker.list")"
  [[ -f "${work}/apt/keyrings/docker.asc" ]] || die "the Docker repository key was not installed"
  [[ -x "${bin}/docker" ]] || die "the Docker packages did not produce a docker client"
  [[ "$(stat -c '%u:%a' "${work}/state-root")" == "$(id -u):700" ]] ||
    die "state root ownership or mode is wrong: $(stat -c '%u:%a' "${work}/state-root")"
  [[ "$(stat -c '%u:%g:%a' "${work}/backup")" == "999:999:700" ]] ||
    die "backup directory ownership or mode is wrong"
  assert_output "Controller host prerequisites satisfied"
  assert_output "OCSERV_SECRET_DIR is not set"
  if (( EUID != 0 )); then
    assert_output "launcher access"
  fi
  echo "install on a bare host installs prerequisites and creates directories"

  # 13. a repeated install is a no-op (idempotency).
  apt_lines="$(wc -l <"${apt_log}" | tr -d ' ')"
  curl_lines="$(wc -l <"${curl_log}" | tr -d ' ')"
  capture sudo install --backup-dir "${work}/backup"
  assert_status 0 "a repeated install must succeed"
  [[ "$(wc -l <"${apt_log}" | tr -d ' ')" == "${apt_lines}" ]] ||
    die "the repeated install mutated packages again"
  [[ "$(wc -l <"${curl_log}" | tr -d ' ')" == "${curl_lines}" ]] ||
    die "the repeated install reconfigured the Docker repository"
  assert_output "preserving it without reinstall or upgrade"
  echo "a repeated install performs no package mutation"

  # 14. an existing compatible Docker installation is preserved untouched.
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  capture sudo install --backup-dir "${work}/backup"
  assert_status 0 "install with existing Docker must succeed"
  assert_output "preserving it without reinstall or upgrade"
  assert_log_empty "${apt_log}"
  assert_log_empty "${curl_log}"
  echo "an existing compatible Docker installation is preserved"

  # 15. an inactive daemon is started through systemd, not replaced.
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  : >"${docker_unready}"
  capture sudo install
  assert_status 0 "install must recover an inactive daemon"
  assert_log_contains "${systemctl_log}" "enable --now docker"
  [[ ! -e "${docker_unready}" ]] || die "the daemon revive path was not exercised"
  echo "an inactive Docker daemon is started through systemd"

  # 16. install refuses to overwrite an existing wrong backup directory.
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  install -d -m 0755 -- "${work}/backup-wrong"
  capture sudo install --backup-dir "${work}/backup-wrong"
  assert_status 1 "an existing wrong backup directory must fail"
  assert_output "999:999"
  echo "install refuses to overwrite an existing wrong backup directory"

  # 17. install refuses an existing state root owned by another user.
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  as_root install -d -m 0700 -o 999 -g 999 -- "${work}/state-root"
  capture sudo install
  assert_status 1 "an existing wrong state root owner must fail"
  assert_output "state root"
  echo "install refuses an existing state root owned by another user"

  # 17a. install fails closed when the launcher cannot reach the Docker
  # daemon, before any lifecycle directory is created.
  if (( EUID != 0 )); then
    reset_scenario
    install_docker_mock
    mark_all_tools_installed
    : >"${docker_denied}"
    capture sudo install
    assert_status 1 "install must fail when the launcher lacks Docker access"
    assert_output "cannot access the Docker daemon"
    assert_output "never modifies the Docker permission model"
    [[ ! -e "${work}/state-root" ]] ||
      die "install created the state root despite the launcher access failure"
    echo "install fails closed on missing launcher Docker access"

    # 17b. a root-run check also probes the sudo-invoking launcher.
    capture sudo check
    assert_status 1 "a root check must probe the sudo-invoking launcher"
    assert_output "cannot access the Docker daemon"
    echo "root check probes the sudo-invoking launcher Docker access"

    # 17c. a root-owned state root fails a launcher-user check, matching the
    # exact controller.sh ownership contract.
    reset_scenario
    install_docker_mock
    mark_all_tools_installed
    rm -f -- "${docker_denied}"
    as_root install -d -m 0700 -o 0 -g 0 -- "${work}/state-root"
    capture self check
    assert_status 1 "a root-owned state root must fail the launcher check"
    assert_output "launcher user"
    echo "check rejects a root-owned state root for a launcher user"
  else
    echo "launcher-user ownership and access cases skipped: running as root without sudo"
  fi

  # 18. install refuses to create a state root under unsafe ancestry.
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
  install -d -m 0777 -- "${work}/loose"
  EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/loose/state-root")
  capture sudo install
  assert_status 1 "an unsafe ancestor must fail install"
  assert_output "group/world writable"
  [[ ! -e "${work}/loose/state-root" ]] ||
    die "install created a state root under unsafe ancestry"
  echo "install refuses to create a state root under unsafe ancestry"
else
  echo "root-path install cases skipped: no passwordless sudo available" >&2
fi

echo "Controller host bootstrap tests passed"
