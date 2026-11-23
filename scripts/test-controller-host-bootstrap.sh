#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/test-installer-common.sh
source "${ROOT}/scripts/test-installer-common.sh"
BOOTSTRAP="${ROOT}/deploy/production/bootstrap-host.sh"

# The bootstrap targets Ubuntu and Debian hosts and uses GNU stat; skip on
# hosts without the GNU userland (mirrors the flock guard in
# test-controller-lifecycle.sh).
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
fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-bootstrap-test.XXXXXX")"

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

os_fixture() {
  local id="$1" version="$2" codename="$3" name="${4:-$1-$2}"
  printf 'ID=%s\nVERSION_ID="%s"\nVERSION_CODENAME=%s\n' "${id}" "${version}" "${codename}" >"${fixture}/os/${name}"
}
while read -r id version codename; do
  os_fixture "${id}" "${version}" "${codename}"
done <<'RELEASES'
ubuntu 20.04 focal
ubuntu 22.04 jammy
ubuntu 24.04 noble
ubuntu 25.10 questing
ubuntu 26.04 resolute
debian 10 buster
debian 11 bullseye
debian 12 bookworm
debian 13 trixie
fedora 41 none
RELEASES
os_fixture ubuntu 24.04 hirsute ubuntu-24.04-mismatch

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
python3
curl
openssl
ca-certificates
EOF
}

provisioned_host() {
  reset_scenario
  install_docker_mock
  mark_all_tools_installed
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

# Usage errors.
reset_scenario
capture self
assert_status 2 "no arguments must be a usage error"
capture self frobnicate
assert_status 2 "unknown command must be a usage error"
reset_scenario
capture self check --backup-dir relative/path
assert_status 2 "a relative --backup-dir must be a usage error"

for unsupported in fedora-41 ubuntu-25.10 debian-10; do
  reset_scenario
  os_release="${fixture}/os/${unsupported}"
  capture self check
  assert_status 1 "unsupported host ${unsupported}"
  assert_log_empty "${apt_log}"
done

# a codename that disagrees with the supported release matrix fails closed.
reset_scenario
os_release="${fixture}/os/ubuntu-24.04-mismatch"
capture self check
assert_status 1 "a mismatched distribution codename must fail closed"
assert_output "does not match the expected 'noble'"
assert_log_empty "${apt_log}"

# Unsupported architecture.
reset_scenario
EXTRA_ENV=("BOOTSTRAP_TEST_ARCH=riscv64")
capture self check
assert_status 1 "unsupported architecture must fail closed"
assert_output "unsupported architecture 'riscv64'"
assert_log_empty "${apt_log}"

# check reports missing Docker without mutating packages.
reset_scenario
mark_all_tools_installed
capture self check
assert_status 1 "missing Docker must fail the host check"
assert_output "Docker is required but not installed"
assert_log_empty "${apt_log}"

# check reports Compose without --wait support.
provisioned_host
: >"${compose_no_wait}"
capture self check
assert_status 1 "Compose without --wait must fail the host check"
assert_output "docker compose up --wait"
assert_log_empty "${apt_log}"

# check detects a standalone docker-compose without the v2 plugin.
provisioned_host
cp -- "${fixture}/docker-compose-stub" "${bin}/docker-compose"
EXTRA_ENV=("BOOTSTRAP_TEST_NO_COMPOSE=1")
capture self check
assert_status 1 "standalone docker-compose without the plugin must fail"
assert_output "docker-compose"
assert_log_empty "${apt_log}"

for conflict in docker.io containerd docker-compose-v2 docker-buildx; do
  provisioned_host
  printf '%s\n' "${conflict}" >>"${dpkg_db}"
  EXTRA_ENV=(BOOTSTRAP_TEST_NO_COMPOSE=1)
  capture self check
  assert_status 1 "package conflict ${conflict}"
  assert_output "${conflict}"
  assert_log_empty "${apt_log}"
done

# Debian conflicts point at the Debian installation guidance, and the
# Ubuntu-only docker-compose-v2 package is not a Debian conflict.
reset_scenario
os_release="${fixture}/os/debian-12"
install_docker_mock
mark_all_tools_installed
printf '%s\n' docker.io >>"${dpkg_db}"
capture self check
assert_status 1 "a conflicting runtime must fail the Debian host check"
assert_output "https://docs.docker.com/engine/install/debian/"
assert_log_empty "${apt_log}"

reset_scenario
os_release="${fixture}/os/debian-12"
install_docker_mock
mark_all_tools_installed
printf '%s\n' docker-compose-v2 >>"${dpkg_db}"
EXTRA_ENV=("BOOTSTRAP_TEST_NO_COMPOSE=1")
capture self check
assert_status 1 "the missing Compose plugin must still fail the Debian host check"
if grep -qF "conflicting package 'docker-compose-v2'" <<<"${RUN_OUTPUT}"; then
  die "docker-compose-v2 must not be reported as a Debian conflict"
fi
assert_log_empty "${apt_log}"

# check passes on a provisioned host and lists operator prerequisites.
provisioned_host
install -d -m 0700 -- "${work}/state-root"
capture self check
assert_status 0 "a provisioned host must pass the read-only check"
assert_output "Controller host prerequisites satisfied"
assert_output "OCSERV_SECRET_DIR is not set"
assert_output "controller-release-amd64.json"
assert_log_empty "${apt_log}"
assert_log_empty "${systemctl_log}"

# check rejects a state root with the wrong mode.
provisioned_host
install -d -m 0755 -- "${work}/state-root"
capture self check
assert_status 1 "a wrong state root mode must fail the host check"
assert_output "state root"
assert_log_empty "${apt_log}"

# check rejects a backup directory with the wrong ownership.
provisioned_host
install -d -m 0755 -- "${work}/backup-wrong"
capture self check --backup-dir "${work}/backup-wrong"
assert_status 1 "a wrong backup directory must fail the host check"
assert_output "999:999"
assert_log_empty "${apt_log}"

# check classifies a Docker socket permission denial for the invoking
# user and fails closed with remediation. The mock denial applies to non-root
# users only, mirroring root's unconditional Docker access.
if (( EUID != 0 )); then
  provisioned_host
  install -d -m 0700 -- "${work}/state-root"
  : >"${docker_denied}"
  capture self check
  assert_status 1 "a launcher without Docker daemon access must fail the check"
  assert_output "cannot access the Docker daemon"
  assert_output "never modifies the Docker permission model"
  assert_log_empty "${apt_log}"
  echo "check reports a Docker permission denial for the invoking user"
fi

# a relative state root is rejected like controller.sh rejects it.
provisioned_host
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=relative/state-root")
capture self check
assert_status 1 "a relative state root must fail the host check"
assert_output "must be an absolute path"

# a non-canonical state root path is rejected.
provisioned_host
install -d -m 0700 -- "${work}/state-root"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/state-root/")
capture self check
assert_status 1 "a non-canonical state root must fail the host check"
assert_output "canonical path without traversal"

# a group/world-writable state root ancestor is rejected.
provisioned_host
install -d -m 0777 -- "${work}/loose"
install -d -m 0700 -- "${work}/loose/state-root"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/loose/state-root")
capture self check
assert_status 1 "a world-writable ancestor must fail the host check"
assert_output "group/world writable"

# a symlinked state root ancestor is rejected.
provisioned_host
install -d -m 0700 -- "${work}/real/state-root"
ln -s -- real "${work}/link"
EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/link/state-root")
capture self check
assert_status 1 "symlink ancestry must fail the host check"
assert_output "symlink ancestry"

# 10g. The backup writer rejects an unprotected backup path before it backs up and
# never reuses an existing staging or output object.
writer="${ROOT}/scripts/postgres-backup.sh"
writer_bin="${fixture}/writer-bin"
writer_log="${fixture}/writer.log"
mkdir -m 700 -- "${writer_bin}"
cat >"${writer_bin}/psql" <<'EOF'
#!/usr/bin/env bash
printf '180006\n'
EOF
cat >"${writer_bin}/pg_basebackup" <<'EOF'
#!/usr/bin/env bash
echo pg_basebackup >>"${WRITER_LOG}"
pgdata=""
for arg in "$@"; do
  case "${arg}" in
    --pgdata=*) pgdata="${arg#--pgdata=}" ;;
  esac
done
mkdir -m 0700 -- "${pgdata}"
printf 'START WAL LOCATION: 0/01000028 (file 000000010000000000000001)\n' >"${pgdata}/backup_label"
printf '{"PostgreSQL-Backup-Manifest-Version":1}\n' >"${pgdata}/backup_manifest"
EOF
cat >"${writer_bin}/pg_verifybackup" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"${writer_bin}/pg_archivecleanup" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"${writer_bin}/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "-u +%Y%m%dT%H%M%SZ" ]]; then echo 20260101T000000Z; else exec "${WRITER_REAL_DATE}" "$@"; fi
EOF
chmod 0755 -- "${writer_bin}/psql" "${writer_bin}/pg_basebackup" "${writer_bin}/pg_verifybackup" "${writer_bin}/pg_archivecleanup" "${writer_bin}/date"
run_writer() {
  RUN_STATUS=0
  : >"${writer_log}"
  RUN_OUTPUT="$(env PATH="${writer_bin}:${PATH}" WRITER_LOG="${writer_log}" WRITER_REAL_DATE="$(command -v date)" BACKUP_ROOT="$1" \
    RUN_ID=writer-test "${writer}" --once 2>&1)" || RUN_STATUS=$?
}
assert_not_dumped() { assert_log_empty "${writer_log}"; }

wroot="${work}/writer"
install -d -m 0700 -- "${wroot}"
install -d -m 0755 -- "${wroot}/loose"
run_writer "${wroot}/loose"
assert_status 1 "a writer root with a loose mode must fail"
assert_output "mode 0700"
assert_not_dumped
install -d -m 0700 -- "${wroot}/loose-base" "${wroot}/loose-base/base"
chmod 0750 -- "${wroot}/loose-base/base"
run_writer "${wroot}/loose-base"
assert_status 1 "a base directory with a loose mode must fail"
assert_not_dumped
install -d -m 0700 -- "${wroot}/link-target" "${wroot}/link-base"
ln -s -- ../link-target "${wroot}/link-base/base"
run_writer "${wroot}/link-base"
assert_status 1 "a symlinked base directory must fail"
assert_not_dumped
ln -s -- link-target "${wroot}/link-root"
run_writer "${wroot}/link-root"
assert_status 1 "a symlinked writer root must fail"
assert_not_dumped
if can_root; then
  install -d -m 0700 -- "${wroot}/foreign"
  as_root chown 999:999 -- "${wroot}/foreign"
  if (( EUID == 0 )); then
    run_writer "${wroot}/foreign"
    assert_status 1 "a writer root owned by another user must fail"
    assert_not_dumped
  fi
fi
echo "the backup writer rejects unprotected paths before backing up"

install -d -m 0700 -- "${wroot}/staging" "${wroot}/staging/base"
mkdir -- "${wroot}/staging/base/.20260101T000000Z-writer-test.tmp"
printf 'keep\n' >"${wroot}/staging/base/.20260101T000000Z-writer-test.tmp/sentinel"
run_writer "${wroot}/staging"
assert_status 1 "an existing staging directory must be refused"
assert_not_dumped
[[ "$(cat -- "${wroot}/staging/base/.20260101T000000Z-writer-test.tmp/sentinel")" == keep ]] ||
  die "an existing staging directory was reused or removed"
flock -n "${wroot}/staging/.backup.lock" true || die "a refused backup held its lock"
install -d -m 0700 -- "${wroot}/staging-link" "${wroot}/staging-link/base"
ln -s -- "${wroot}/link-target" "${wroot}/staging-link/base/.20260101T000000Z-writer-test.tmp"
run_writer "${wroot}/staging-link"
assert_status 1 "a symlinked staging path must be refused"
assert_not_dumped
[[ -L "${wroot}/staging-link/base/.20260101T000000Z-writer-test.tmp" ]] || die "a symlinked staging path was removed"
install -d -m 0700 -- "${wroot}/final" "${wroot}/final/base"
ln -s -- "${wroot}/link-target" "${wroot}/final/base/20260101T000000Z"
run_writer "${wroot}/final"
assert_status 1 "an existing output object must be refused"
assert_not_dumped
install -d -m 0700 -- "${wroot}/latest" "${wroot}/latest/base" "${wroot}/latest/LATEST"
run_writer "${wroot}/latest"
assert_status 1 "a LATEST directory must be refused"
assert_not_dumped

install -d -m 0700 -- "${wroot}/ok"
run_writer "${wroot}/ok"
assert_status 0 "a protected writer root must back up"
assert_output "backup completed id=20260101T000000Z"
[[ "$(stat -c '%a' "${wroot}/ok/base")" == 700 && "$(stat -c '%a' "${wroot}/ok/wal")" == 700 ]] || die "the base and wal directories must be created with mode 0700"
[[ "$(cat -- "${wroot}/ok/LATEST")" == 20260101T000000Z ]] || die "the LATEST marker was not published"
[[ -s "${wroot}/ok/base/20260101T000000Z/backup_manifest" && "$(cat -- "${wroot}/ok/base/20260101T000000Z/OCSERVIA_BACKUP_ID")" == 20260101T000000Z ]] ||
  die "the backup files were not published"
[[ -z "$(find "${wroot}/ok" -name '.LATEST*' -o -name '*.tmp')" ]] || die "the writer left temporary objects"
echo "the backup writer backs up under a protected path"

# 11. install refuses to run without root.
if (( EUID != 0 )); then
  provisioned_host
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

  # 13a. Ubuntu 20.04 install without Docker fails closed without host mutation.
  reset_scenario
  os_release="${fixture}/os/ubuntu-20.04"
  mark_all_tools_installed
  capture sudo install --backup-dir "${work}/backup"
  assert_status 1 "Ubuntu 20.04 without Docker must fail closed"
  assert_output "Ubuntu 20.04 can run ocservia with an existing compatible Docker installation, but automatic Docker bootstrap is unavailable on this legacy host"
  assert_log_empty "${apt_log}"
  assert_log_empty "${curl_log}"
  [[ ! -e "${work}/apt/sources/docker.list" ]] ||
    die "Ubuntu 20.04 install wrote a Docker repository anyway"
  echo "Ubuntu 20.04 without Docker fails closed without repository mutation"

  # 13b. Ubuntu 20.04 install preserves an existing compatible Docker.
  reset_scenario
  os_release="${fixture}/os/ubuntu-20.04"
  install_docker_mock
  mark_all_tools_installed
  capture sudo install --backup-dir "${work}/backup"
  assert_status 0 "Ubuntu 20.04 with existing Docker must install"
  assert_output "preserving it without reinstall or upgrade"
  assert_log_empty "${apt_log}"
  assert_log_empty "${curl_log}"
  [[ ! -e "${work}/apt/sources/docker.list" ]] ||
    die "Ubuntu 20.04 install wrote a Docker repository despite existing Docker"
  echo "Ubuntu 20.04 with existing Docker installs without repository mutation"

  # The bare-host scenario above checks permissions and packages. Vary only the repository selection here.
  while read -r release codename arch; do
    reset_scenario
    os_release="${fixture}/os/${release}"
    EXTRA_ENV=("BOOTSTRAP_TEST_ARCH=${arch}")
    capture sudo install --backup-dir "${work}/backup"
    assert_status 0 "${release} on ${arch}"
    family="${release%%-*}"
    apt_arch=amd64
    [[ "${arch}" != aarch64 ]] || apt_arch=arm64
    assert_log_contains "${curl_log}" "https://download.docker.com/linux/${family}/gpg"
    assert_log_contains "${work}/apt/sources/docker.list" "arch=${apt_arch} signed-by=${work}/apt/keyrings/docker.asc] https://download.docker.com/linux/${family} ${codename} stable"
    if [[ "${release}" == debian-11 ]]; then assert_output "Debian ELTS"; fi
  done <<'PLATFORMS'
ubuntu-22.04 jammy x86_64
ubuntu-26.04 resolute x86_64
debian-12 bookworm aarch64
debian-13 trixie x86_64
debian-11 bullseye x86_64
PLATFORMS

  # 15. an inactive daemon is started through systemd, not replaced.
  provisioned_host
  : >"${docker_unready}"
  capture sudo install
  assert_status 0 "install must recover an inactive daemon"
  assert_log_contains "${systemctl_log}" "enable --now docker"
  [[ ! -e "${docker_unready}" ]] || die "the daemon revive path was not exercised"
  echo "an inactive Docker daemon is started through systemd"

  # 16. install refuses to overwrite an existing wrong backup directory.
  provisioned_host
  install -d -m 0755 -- "${work}/backup-wrong"
  capture sudo install --backup-dir "${work}/backup-wrong"
  assert_status 1 "an existing wrong backup directory must fail"
  assert_output "999:999"
  echo "install refuses to overwrite an existing wrong backup directory"

  # 17. install refuses an existing state root owned by another user.
  provisioned_host
  as_root install -d -m 0700 -o 999 -g 999 -- "${work}/state-root"
  capture sudo install
  assert_status 1 "an existing wrong state root owner must fail"
  assert_output "state root"
  echo "install refuses an existing state root owned by another user"

  # 17a. install fails closed when the launcher cannot reach the Docker
  # daemon, before any lifecycle directory is created.
  if (( EUID != 0 )); then
    provisioned_host
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
    provisioned_host
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
  provisioned_host
  install -d -m 0777 -- "${work}/loose"
  EXTRA_ENV=("OCSERV_CONTROLLER_STATE_ROOT=${work}/loose/state-root")
  capture sudo install
  assert_status 1 "an unsafe ancestor must fail install"
  assert_output "group/world writable"
  [[ ! -e "${work}/loose/state-root" ]] ||
    die "install created a state root under unsafe ancestry"
  echo "install refuses to create a state root under unsafe ancestry"
  # 18a. a correct 999:999 0700 leaf still needs protected ancestry.
  provisioned_host
  install -d -m 0700 -- "${work}/state-root"
  install -d -m 0777 -- "${work}/open-parent"
  as_root install -d -o 999 -g 999 -m 0700 -- "${work}/open-parent/backup"
  capture sudo check --backup-dir "${work}/open-parent/backup"
  assert_status 1 "a writable backup ancestor must fail the check"
  assert_output "backup directory ancestry must not be group/world writable"
  as_root install -d -o 999 -g 999 -m 0755 -- "${work}/owned-parent"
  as_root install -d -o 999 -g 999 -m 0700 -- "${work}/owned-parent/backup"
  capture sudo check --backup-dir "${work}/owned-parent/backup"
  assert_status 1 "a low-trust owned backup ancestor must fail the check"
  assert_output "backup directory ancestry must be root- or launcher-owned"
  install -d -m 0700 -- "${work}/real-parent"
  as_root install -d -o 999 -g 999 -m 0700 -- "${work}/real-parent/backup"
  ln -s -- real-parent "${work}/link-parent"
  capture sudo check --backup-dir "${work}/link-parent/backup"
  assert_status 1 "a symlinked backup ancestor must fail the check"
  assert_output "backup directory ancestry must not contain symlinks"
  capture sudo install --backup-dir "${work}/open-parent/new-backup"
  assert_status 1 "install must not create a backup directory below a writable ancestor"
  [[ ! -e "${work}/open-parent/new-backup" ]] || die "install created a backup directory below a writable ancestor"
  as_root install -d -o 999 -g 999 -m 0700 -- "${work}/good-backup"
  capture sudo check --backup-dir "${work}/good-backup"
  assert_status 0 "a protected backup path must pass the check"
  assert_output "Controller host prerequisites satisfied"
  capture sudo install --backup-dir "${work}/good-backup"
  assert_status 0 "a protected backup path must pass install"
  assert_output "backup directory validated"
  echo "backup directory ancestry is enforced for check and install"
else
  echo "root-path install cases skipped: no passwordless sudo available" >&2
fi

echo "Controller host bootstrap tests passed"
