#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 "${ROOT}/scripts/test-node-bootstrap.py"
INSTALL="${INSTALLER_UNDER_TEST:-${ROOT}/deploy/managed-node/install.sh}"
VERSION="${TEST_RELEASE_VERSION:-0.2.1}"
scope="${1:-full}"
case "${scope}" in
  full|release-contracts) [[ $# -le 1 ]] ;;
  *) echo 'usage: test-managed-node-install.sh [full|release-contracts]' >&2; exit 2 ;;
esac
MOCK_NODE_ID="018f1e11-2222-7333-8444-555555555555"

# The installer fixture asserts file modes with GNU stat and creates runtime command
# key fixtures with openssl; skip on hosts without them (mirrors the guard in
# test-controller-install.sh).
stat -c '%u' . >/dev/null 2>&1 || {
  echo "Managed node install tests skipped: GNU stat is unavailable" >&2
  exit 0
}
for tool in git openssl sha256sum awk sed; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "Managed node install tests skipped: ${tool} is unavailable" >&2
    exit 0
  }
done
[[ -x "${INSTALL}" ]] || {
  echo "Managed node install tests require an executable installer" >&2
  exit 1
}
umask 022

fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-managed-node-test.XXXXXX")"

can_root() {
  ((EUID == 0)) || sudo -n true >/dev/null 2>&1
}

# The reduced release run must exercise native version convergence and digest
# checks, not report success after skipping the privileged scenarios.
if [[ "${scope}" == release-contracts ]] && ! can_root; then
  echo 'Release contracts require root or passwordless sudo' >&2
  exit 1
fi

as_root() {
  if ((EUID == 0)); then
    "$@"
  else
    sudo -n "$@"
  fi
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if can_root; then
    as_root userdel ocserv-agent >/dev/null 2>&1 || true
    as_root groupdel ocserv-agent >/dev/null 2>&1 || true
    as_root rm -rf -- "${fixture}" || rm -rf -- "${fixture}"
  else
    rm -rf -- "${fixture}"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

repo="${fixture}/repo"
sysroot="${fixture}/sysroot"
serve="${fixture}/serve"
bin="${fixture}/bin"
os="${fixture}/os"
logs="${fixture}/logs"
curl_log="${logs}/curl.log"
sudo_log="${logs}/sudo.log"
dpkg_log="${logs}/dpkg.log"
rpm_log="${logs}/rpm.log"
agent_log="${logs}/agent.log"
systemctl_log="${logs}/systemctl.log"
runuser_log="${logs}/runuser.log"
root_sudo_log="${logs}/root-sudo.log"
openssl_log="${logs}/openssl.log"

# Mock configuration travels through files below the fixture root (derived
# from OCSERV_MANAGED_NODE_SYSROOT, which the installer forwards across the
# --root-lifecycle boundary), never through environment variables that sudo
# env_reset would drop.
node_id_file="${fixture}/node.id"
enroll_exit_file="${fixture}/enroll-exit"
installed_version_file="${fixture}/installed-version"
arch_file="${fixture}/arch"
tamper_after_verify_file="${fixture}/tamper-after-verify"
services_state_file="${fixture}/services-state"

die() {
  echo "Managed node install tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

RUN_STATUS=0
RUN_OUTPUT=""
# The installer under test: the fixture checkout by default, the standalone
# single-file copy for the package-first --version scenarios.
SCRIPT_UNDER_TEST="${repo}/deploy/managed-node/install.sh"
EXTRA_ENV=()
OS_RELEASE="${os}/ubuntu-24.04"

mkdir -m 700 -- "${bin}" "${logs}" "${serve}" "${sysroot}"
mkdir -p -- "${repo}/deploy/managed-node" "${repo}/deploy/lib" "${os}"

openssl genpkey -algorithm ed25519 -out "${fixture}/command-verification.key" 2>/dev/null
openssl pkey -in "${fixture}/command-verification.key" -pubout -out "${fixture}/controller-command-verification-key.pem"
printf 'mock relay access token bytes\n' >"${fixture}/relay-access-token"
chmod 0600 -- "${fixture}/command-verification.key"
controller_id="$(printf 'c%.0s' $(seq 1 64))"

assets=(
  "ocservia-agent-${VERSION}-linux-amd64.tar.gz"
  "ocservia-agent-${VERSION}-linux-arm64.tar.gz"
  "ocservia-agent_${VERSION}-1_amd64.deb"
  "ocservia-agent_${VERSION}-1_arm64.deb"
  "ocservia-agent-${VERSION}-1.x86_64.rpm"
  "ocservia-agent-${VERSION}-1.aarch64.rpm"
)

build_serve() {
  local name
  rm -rf -- "${serve}"
  mkdir -m 700 -- "${serve}"
  for name in "${assets[@]}"; do
    printf 'mock release asset %s\n' "${name}" >"${serve}/${name}"
  done
}

for name in ubuntu-24.04 ubuntu-22.04 ubuntu-20.04 debian-12 debian-11 rocky-9 fedora-41 arch; do
  case "${name}" in
    rocky-9) printf 'ID=rocky\nVERSION_ID="9.5"\n' ;;
    fedora-41) printf 'ID=fedora\nVERSION_ID="41"\n' ;;
    arch) printf 'ID=arch\nVERSION_ID="rolling"\n' ;;
    ubuntu-*) printf 'ID=ubuntu\nVERSION_ID="%s"\n' "${name#ubuntu-}" ;;
    debian-*) printf 'ID=debian\nVERSION_ID="%s"\n' "${name#debian-}" ;;
  esac >"${os}/${name}"
done

cat >"${bin}/agent-stub" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/agent.log"
case "$*" in
  *--prepare-enrollment*)
    identity="${OCSERV_MANAGED_NODE_SYSROOT}/var/lib/ocservia-agent/identity"
    key="${identity}/endpoint.key"
    pin="${identity}/controller.endpoint"
    controller=""
    args=("$@")
    for ((i = 0; i < $#; i++)); do
      [[ "${args[$i]}" == "--controller" ]] && controller="${args[$((i + 1))]:-}"
    done
    if [[ ! -f "${key}" && ! -f "${pin}" ]]; then
      # Fresh provisioning, mirroring the real formats the loader enforces:
      # a 32-byte endpoint secret key and the configured 64-hex controller
      # pin, both owner-only.
      install -d -m 0700 -- "${identity}"
      printf '%032d' 7 >"${key}"
      printf '%s' "${controller}" >"${pin}"
      chmod 0600 -- "${key}" "${pin}"
    fi
    # With material present this stub behaves like Identity::provision: it
    # loads and validates instead of provisioning — owner-only permissions,
    # the exact 32-byte endpoint key, and the controller pin matching the
    # configured Controller EndpointID.
    for file in "${key}" "${pin}"; do
      if [[ ! -f "${file}" || -L "${file}" ]]; then
        echo "identity loader: partial or missing identity material (${file})" >&2
        exit 1
      fi
      mode="$(stat -c %a -- "${file}")"
      if (( (8#${mode} & 63) != 0 )); then
        echo "identity loader: ${file} is group/world accessible (${mode})" >&2
        exit 1
      fi
    done
    if [[ "$(stat -c %s -- "${key}")" -ne 32 ]]; then
      echo "identity loader: endpoint key must be exactly 32 bytes" >&2
      exit 1
    fi
    pin_value="$(cat -- "${pin}")"
    if [[ ! "${pin_value}" =~ ^[0-9a-f]{64}$ ]] || [[ "${pin_value}" != "${controller}" ]]; then
      echo "identity loader: controller pin does not match the configured Controller" >&2
      exit 1
    fi
    # The printed EndpointID is derived from the loaded key bytes like
    # key.public(): a different endpoint key always presents a different
    # EndpointID, which the installer compares against the enrollment
    # binding pinned in agent.env.
    printf '%s\n' "$(sha256sum -- "${key}" | awk '{print $1}')"
    exit 0
    ;;
  *--enrollment-token-file*)
    status=0
    [[ ! -s "${root}/enroll-exit" ]] || status="$(cat -- "${root}/enroll-exit")"
    [[ "${status}" == 0 ]] || exit "${status}"
    printf '%s\n' "$(cat -- "${root}/node.id")"
    exit 0
    ;;
esac
exit 0
EOF

cat >"${bin}/privd-stub" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
if [[ "${1:-}" == attestation-registration ]]; then
  [[ ! -L "$2" ]] || exit 2
  if [[ ! -f "$2" ]]; then
    mkdir -p -- "$(dirname -- "$2")"
    printf 'original root key' >"$2"
    chmod 0600 "$2"
  fi
  printf '{"key_id":"root-key"}\n'
  exit 0
fi
[[ "$*" == --host-preflight ]] || exit 2
printf '%s\n' "$*" >>"${root}/logs/preflight.log"
if [[ -s "${root}/preflight-failure" ]]; then
  echo 'resource=ocpasswd reason=mode actual=0644 expected=0600' >&2
  exit 1
fi
echo 'HOST_PREFLIGHT_OK (read-only)'
EOF
chmod 0755 "${bin}/privd-stub"

cat >"${bin}/native-install" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
package="$1"
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
conf="${OCSERV_MANAGED_NODE_SYSROOT}/etc/ocservia-agent"
marker="${OCSERV_MANAGED_NODE_SYSROOT}/etc/ocservia/agent-install-production-relays"
[[ -f "${marker}" && ! -L "${marker}" ]] || {
  echo "native install mock: the production request marker must exist before the package manager runs" >&2
  exit 1
}
expected="$(sha256sum -- "${root}/serve/${package##*/}" | awk '{print $1}')"
[[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "native install mock: no package digest for ${package##*/}" >&2
  exit 1
}
[[ "$(sha256sum -- "${package}" | awk '{print $1}')" == "${expected}" ]] || {
  echo "native install mock: package digest is not the externally verified one" >&2
  exit 1
}
install -d -m 0755 -- "${conf}" \
  "${OCSERV_MANAGED_NODE_SYSROOT}/usr/libexec/ocservia" \
  "${OCSERV_MANAGED_NODE_SYSROOT}/var/lib/ocservia-agent" \
  "${OCSERV_MANAGED_NODE_SYSROOT}/usr/lib/systemd/system/ocservia-agent.service.d"
install -m 0755 -- "${root}/bin/agent-stub" "${OCSERV_MANAGED_NODE_SYSROOT}/usr/libexec/ocservia/ocservia-agent"
install -m 0755 -- "${root}/bin/privd-stub" "${OCSERV_MANAGED_NODE_SYSROOT}/usr/libexec/ocservia/ocservia-privd"
printf '[Service]\nEnvironmentFile=/etc/ocservia-agent/relays.env\n' \
  >"${OCSERV_MANAGED_NODE_SYSROOT}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
printf 'RELAY_URL_A=https://relay-a.example.com\n' >"${conf}/relays.env"
printf 'CONTROLLER_ENDPOINT_ID=replace-with-approved-controller-endpoint-id\nNODE_ID=00000000-0000-7000-8000-000000000000\nCONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem\nUSER_PASSWORD_SEAL_KEY_ID=user-key-v1\nUSER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=replace-with-64-lowercase-hex-sha256\nP12_PASSWORD_SEAL_KEY_ID=p12-key-v1\nP12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=replace-with-distinct-64-lowercase-hex-sha256\n' >"${conf}/agent.env"
# The real package lifecycle runs as root; an unprivileged fixture run cannot
# chown to root:ocserv-agent and deliberately leaves the unsafe ownership for
# the installer to reject.
if ((EUID == 0)) && getent group ocserv-agent >/dev/null 2>&1; then
  chown root:ocserv-agent -- "${conf}/relays.env" "${conf}/agent.env"
fi
chmod 0640 -- "${conf}/relays.env" "${conf}/agent.env"
rm -f -- "${marker}"
version="$(basename -- "${package}")"
case "${version}" in
  # dpkg reports the nfpm release component (X.Y.Z-1); rpm -q --qf '%{VERSION}'
  # reports the bare version because rpm keeps the release in %{RELEASE}.
  *.deb)
    version="${version#ocservia-agent_}"
    version="${version%_*}"
    [[ "${version}" == *-1 ]] || version="${version}-1"
    ;;
  *.rpm) version="${version#ocservia-agent-}" ; version="${version%-1.*}" ;;
esac
printf 'installed %s\n' "${version/-rc./~rc.}" >"${OCSERV_MANAGED_NODE_SYSROOT}/.package-state"
exit 0
EOF

cat >"${bin}/sudo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/sudo.log"
# The unprivileged test user cannot chown to root:ocserv-agent, so the exact
# privileged arguments are logged above and only the ownership flags are
# dropped before execution.
args=()
while (($# > 0)); do
  case "$1" in
    -n) shift ;;
    -u) shift 2 ;;
    mktemp)
      args+=(mktemp); shift
      while (($#)); do args+=("${1/\/var\/tmp\//${root}/}"); shift; done
      break ;;
    install)
      args+=(install)
      shift
      while (($# > 0)); do
        case "$1" in
          -o) shift 2 ;;
          -g) shift 2 ;;
          *) args+=("$1"); shift ;;
        esac
      done
      break
      ;;
    *) args+=("$1"); shift ;;
  esac
done
exec "${args[@]}"
EOF

cat >"${bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
output="" url="" request_path=""
while (($# > 0)); do
  case "$1" in
    --output) shift; output="${1:-}" ;;
    --data-binary) shift; request_path="${1#@}" ;;
    http*) url="$1" ;;
  esac
  shift
done
[[ -n "${output}" && -n "${url}" ]] || {
  echo "curl mock: missing --output or url" >&2
  exit 1
}
printf '%s\n' "${url}" >>"${root}/logs/curl.log"
if [[ "${url}" == */api/v1/node-bootstrap/* ]]; then
  operation="${url##*/}"
  case "$operation" in
    config)
      [[ ! -e "${root}/http-failure" ]] || exit 22
      cp -- "${root}/bootstrap-config.json" "$output" ;;
    attestation)
      registered=false
      [[ ! -f "${root}/registered" ]] || registered=true
      printf '{"controller_nonce_hex":"%064d","credential_context_sha256_hex":"%064d","registered":%s}' 4 5 "$registered" >"$output" ;;
    register)
      if [[ -f "${root}/registration-body.json" ]]; then
        cmp -- "$request_path" "${root}/registration-body.json" || exit 22
      else
        cp -- "$request_path" "${root}/registration-body.json"
      fi
      touch "${root}/registered"
      key=root-key
      [[ ! -e "${root}/bad-registration" ]] || key=wrong-key
      printf '{"key_id":"%s","state":"active"}' "$key" >"$output" ;;
    status)
      freshness=fresh
      [[ ! -e "${root}/stale-status" ]] || freshness=stale
      printf '{"trust_status":"active","connection_state":"online","freshness":"%s","attestation_registered":true}' "$freshness" >"$output" ;;
    *) exit 2 ;;
  esac
  exit 0
fi
cp -- "${root}/serve/${url##*/}" "${output}"
EOF

cat >"${bin}/dpkg" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/dpkg.log"
if [[ "${1:-}" == "-i" ]]; then
  exec "${root}/bin/native-install" "$2"
fi
exit 0
EOF

cat >"${bin}/dpkg-query" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
state="${OCSERV_MANAGED_NODE_SYSROOT}/.package-state"
version=""
if [[ -s "${root}/installed-version" ]]; then
  version="$(cat -- "${root}/installed-version")"
elif [[ -f "${state}" ]]; then
  read -r _ version <"${state}"
fi
if [[ -z "${version}" || "${version}" == "none" ]]; then
  case "$*" in *Status*) echo "unknown ok not-installed" ;; esac
  exit 1
fi
case "$*" in
  *Status*) echo "install ok installed" ;;
  *) echo "${version}" ;;
esac
exit 0
EOF

cat >"${bin}/rpm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/rpm.log"
state="${OCSERV_MANAGED_NODE_SYSROOT}/.package-state"
version=""
if [[ -s "${root}/installed-version" ]]; then
  version="$(cat -- "${root}/installed-version")"
elif [[ -f "${state}" ]]; then
  read -r _ version <"${state}"
fi
case "${1:-}" in
  -q)
    if [[ -n "${version}" && "${version}" != "none" ]]; then
      echo "${version}"
      exit 0
    fi
    echo "package ocservia-agent is not installed" >&2
    exit 1
    ;;
  -ivh) exec "${root}/bin/native-install" "$2" ;;
esac
exit 0
EOF

cat >"${bin}/runuser" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/runuser.log"
# runuser -u ocserv-agent -- <command...>: the agent stub keeps logging
# working for the unprivileged test user, which cannot write as ocserv-agent.
shift 3
exec "$@"
EOF

cat >"${bin}/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/systemctl.log"
case "${1:-}" in
  enable) [[ -f "${OCSERV_MANAGED_NODE_SYSROOT}/var/lib/ocservia-bootstrap/registered.json" ]] || exit 2 ;;
  # The activation-state fixture: "active" makes both managed-node units
  # report enabled+active (the operator's post-approval enable step); any
  # other content leaves them disabled+inactive.
  is-enabled | is-active)
    if [[ "$(cat -- "${root}/services-state" 2>/dev/null || true)" == active ]]; then
      case "$1" in
        is-enabled) echo enabled ;;
        is-active) echo active ;;
      esac
      exit 0
    fi
    case "$1" in
      is-enabled) echo disabled ;;
      is-active) echo inactive ;;
    esac
    exit 1
    ;;
esac
exit 0
EOF

cat >"${bin}/uname" <<'EOF'
#!/usr/bin/env bash
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
if [[ -s "${root}/arch" ]]; then
  printf '%s\n' "$(cat -- "${root}/arch")"
  exit 0
fi
exec /bin/uname "$@"
EOF

cat >"${bin}/id" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# /usr/bin/id is pinned because this mock shadows `id` on PATH.
case "$*" in
  "-g ocserv-agent" | "-u ocserv-agent") ;;
  *) exec /usr/bin/id "$@" ;;
esac
if /usr/bin/id ocserv-agent >/dev/null 2>&1; then
  exec /usr/bin/id "$@"
fi
case "$*" in
  "-g ocserv-agent") echo 997 ;;
  "-u ocserv-agent") echo 996 ;;
esac
EOF

cat >"${bin}/openssl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
printf '%s\n' "$*" >>"${root}/logs/openssl.log"
exec /usr/bin/openssl "$@"
EOF

# Change launcher bytes at a selected checksum boundary. Root staging must
# refuse a pre-copy swap and ignore a later launcher-only replacement.
cat >"${bin}/sha256sum" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(dirname -- "${OCSERV_MANAGED_NODE_SYSROOT:?}")"
result="$(/usr/bin/sha256sum "$@")"
if [[ -s "${root}/tamper-after-verify" ]]; then
  phase="$(cat "${root}/tamper-after-verify")"
  if [[ ( "$phase" == before-freeze && "$*" == *ocservia-managed-node.* ) ||
        ( "$phase" == after-freeze && "$*" == *ocservia-managed-node-pkg.* ) ]]; then
    for staging in /tmp/ocservia-managed-node.* "${TMPDIR:-/tmp}"/ocservia-managed-node.*; do
      for package in "$staging"/*.deb "$staging"/*.rpm; do
        [[ -f "$package" ]] || continue
        printf 'attacker package bytes\n' >"$package"
      done
    done
    printf 'package swapped\n' >"${root}/tamper-after-verify"
  fi
fi
printf '%s\n' "$result"
EOF

chmod 0755 -- "${bin}/agent-stub" "${bin}/native-install" "${bin}/sudo" \
  "${bin}/curl" "${bin}/dpkg" "${bin}/dpkg-query" "${bin}/rpm" \
  "${bin}/runuser" "${bin}/systemctl" "${bin}/uname" "${bin}/id" \
  "${bin}/openssl" "${bin}/sha256sum"

build_serve

# The shared install.env loader ships beside the installer, and the
# repository's real .gitignore is committed too: the installer must treat a
# present /install.env as ignored, not as a dirty release checkout.
cp -- "${INSTALL}" "${repo}/deploy/managed-node/install.sh"
# Legacy checkout identity cases clear only the Release default marker. Keep
# the downloaded installer's entire implementation under test; the flat copy
# below retains its actual stamp and exercises the normal Release path.
sed -i '/^readonly DEFAULT_VERSION=/c\readonly DEFAULT_VERSION=""' "${repo}/deploy/managed-node/install.sh"
cp -- "${ROOT}/deploy/lib/node-bootstrap.sh" "${repo}/deploy/lib/node-bootstrap.sh"
cp -- "${ROOT}/.gitignore" "${repo}/.gitignore"
git -C "${repo}" init -q
git -C "${repo}" config user.name test
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" add -A
git -C "${repo}" commit -qm base
git -C "${repo}" tag "v${VERSION}"

# The versioned installer self-containment fixture: the installer alone in a plain
# directory, with no repository siblings and no .git anywhere it could
# consult. The package-first --version mode must run from exactly this.
standalone="${fixture}/standalone"
mkdir -m 700 -- "${standalone}"
if [[ -n "${INSTALLER_UNDER_TEST:-}" ]]; then
  cp -- "${INSTALL}" "${standalone}/install.sh"
else
  "${ROOT}/scripts/prepare-bootstrap-release-assets.sh" "${standalone}"
  mv "${standalone}/managed-node-bootstrap.sh" "${standalone}/install.sh"
fi

# Root scenarios perform real privileged file operations; they need the
# package's system group to exist exactly like a real host.
if can_root; then
  as_root userdel ocserv-agent >/dev/null 2>&1 || true
  as_root groupdel ocserv-agent >/dev/null 2>&1 || true
  as_root groupadd --system ocserv-agent
  as_root useradd --system --gid ocserv-agent --home-dir /var/lib/ocservia-agent --shell /usr/sbin/nologin ocserv-agent
fi

build_env() {
  ROOT_ENV=(
    "PATH=${bin}:${PATH}"
    "OCSERV_MANAGED_NODE_SYSROOT=${sysroot}"
    "OCSERV_MANAGED_NODE_OS_RELEASE=${OS_RELEASE}"
  )
}

reset_state() {
  build_serve
  if can_root; then
    as_root rm -rf -- "${sysroot}"
  else
    rm -rf -- "${sysroot}"
  fi
  mkdir -m 700 -- "${sysroot}"
  for log in "${curl_log}" "${sudo_log}" "${dpkg_log}" "${rpm_log}" \
    "${agent_log}" "${systemctl_log}" "${runuser_log}" "${root_sudo_log}" \
    "${openssl_log}"; do
    : >"${log}"
  done
  printf '%s\n' "${MOCK_NODE_ID}" >"${node_id_file}"
  rm -f -- "${enroll_exit_file}" "${installed_version_file}" "${arch_file}" \
    "${tamper_after_verify_file}" "${services_state_file}" \
    "${fixture}/poison-install-env" "${fixture}/registered" "${fixture}/bad-registration" \
    "${fixture}/stale-status" "${fixture}/http-failure" "${fixture}/registration-body.json"
  EXTRA_ENV=()
}

reset_checkout() {
  # Root scenarios refresh the fixture checkout's git index as root; return
  # ownership so the unprivileged scenarios can keep mutating the checkout.
  if ((EUID != 0)) && can_root; then
    as_root chown -R "$(id -u):$(id -g)" "${repo}" 2>/dev/null || true
  fi
  git -C "${repo}" checkout -q -- .
  git -C "${repo}" clean -qfd
  git -C "${repo}" reset -q --hard "v${VERSION}"
  git -C "${repo}" tag -d v0.3.0-rc1 v0.2.2 v0.2.3 >/dev/null 2>&1 || true
  # install.env is git-ignored, so git clean never removes it; the install.env
  # scenarios must not leak into unrelated ones.
  rm -f -- "${repo}/install.env"
}

capture() {
  build_env
  RUN_STATUS=0
  RUN_OUTPUT="$(env "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
    "${SCRIPT_UNDER_TEST}" --non-interactive "$@" 2>&1)" || RUN_STATUS=$?
}

capture_root() {
  build_env
  RUN_STATUS=0
  if ((EUID == 0)); then
    RUN_OUTPUT="$(env -u SUDO_USER "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
      "${SCRIPT_UNDER_TEST}" --non-interactive "$@" 2>&1)" || RUN_STATUS=$?
  else
    RUN_OUTPUT="$(sudo -n env -u SUDO_USER "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
      "${SCRIPT_UNDER_TEST}" --non-interactive "$@" 2>&1)" || RUN_STATUS=$?
  fi
}

assert_status() {
  ((RUN_STATUS == "$1")) || die "expected exit status $1 but got ${RUN_STATUS}"
}

assert_output() {
  grep -q -- "$1" <<<"${RUN_OUTPUT}" || die "expected output to contain '$1'"
}

assert_log_contains() {
  grep -qF -- "$2" "$1" || die "expected $1 to contain '$2'"
}

assert_log_empty() {
  [[ ! -s "$1" ]] || die "expected $1 to stay empty, got: $(cat -- "$1")"
}

scenario() {
  rm -f "${fixture}/preflight-failure" "${logs}/preflight.log"
  reset_state
  reset_checkout
  SCRIPT_UNDER_TEST="${repo}/deploy/managed-node/install.sh"
  rm -f -- "${standalone}/install.env"
}

# The old manual installation scenarios are retired with that entrypoint.
# Exercise the complete token flow against bounded HTTP/package/process stubs.
scenario
capture --config missing.env
assert_status 1
assert_output "manual configuration installation is retired"
capture --join-token --resume-install
assert_status 1
assert_output "choose one installation token source"
capture --join-token
assert_status 1
assert_output "unattended token install requires"
capture --join-token-file missing-token
assert_status 1
assert_output "requires a regular file"

if ! can_root; then
  echo "Token CLI and helper tests passed; native root scenarios skipped: no passwordless sudo" >&2
  exit 0
fi

# Only ownership is simulated in the bootstrap parser: the fixture ancestors
# belong to the test runner. Modes, links, contents and all host writes remain
# real, and production code has no ownership bypass.
cat >"${bin}/python3" <<'PYTHON'
#!/usr/bin/python3
import os
import sys
from unittest.mock import patch
source = sys.stdin.read()
sys.argv.pop(1)
a, b, c = os.stat, os.lstat, os.fstat
def root(value):
    fields = list(value)
    fields[4] = fields[5] = 0
    return os.stat_result(fields)
with patch('os.stat', side_effect=lambda *x, **kw: root(a(*x, **kw))), patch('os.lstat', side_effect=lambda *x, **kw: root(b(*x, **kw))), patch('os.fstat', side_effect=lambda *x, **kw: root(c(*x, **kw))):
    exec(compile(source, 'bootstrap-fixture', 'exec'), {})
PYTHON
chmod 0755 "${bin}/python3"
printf '#!/bin/sh\nexit 0\n' >"${bin}/sleep"
chmod 0755 "${bin}/sleep"
# A valid installation credential and the corresponding bounded config.
/usr/bin/python3 - "${fixture}" "${controller_id}" <<'CONFIG'
import base64
import json
from pathlib import Path
import sys
root = Path(sys.argv[1])
origin = base64.urlsafe_b64encode(b'https://controller.example.test').decode().rstrip('=')
(root / 'join-token').write_text('oit1_' + origin + '.' + 'd' * 42 + 'c')
(root / 'join-token').chmod(0o600)
(root / 'bootstrap-config.json').write_text(json.dumps(dict(
    controller_endpoint_id=sys.argv[2], environment='production',
    enrollment_token='obt2_' + 'A' * 43, relay_url='https://relay-a.example.test',
    relay_access_token='test-relay-access-token', command_verification_key_hex='2' * 64)))
CONFIG

install_token() { capture_root --join-token-file "${fixture}/join-token" --version "v${VERSION}"; }
resume_token() { capture_root --resume-install --version "v${VERSION}"; }
identity_digest() {
  as_root sha256sum "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key" \
    "${sysroot}/var/lib/ocservia-agent/identity/controller.endpoint" \
    "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" \
    "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem" \
    "${sysroot}/var/lib/ocservia-privd/attestation.key"
}
scenario
# A stale install.env no longer chooses configuration or runs any shell code.
# shellcheck disable=SC2016 # Deliberately inert legacy configuration.
printf 'INVALID=$(touch should-not-exist)\n' >"${repo}/install.env"
EXTRA_ENV=(USER_PASSWORD_SEAL_KEY_ID=original-user-key P12_PASSWORD_SEAL_KEY_ID=original-p12-key)
install_token
assert_status 0
assert_output '^ONLINE$'
assert_output "NODE_ID: ${MOCK_NODE_ID}"
assert_log_contains "${systemctl_log}" 'enable --now ocservia-privd.service ocservia-agent.service'
initial_identity="$(identity_digest)"
enrollment_calls="$(grep -c -- --enrollment-token-file "${agent_log}")"
EXTRA_ENV=()
resume_token
assert_status 0
assert_output '^ONLINE$'
[[ "$(identity_digest)" == "$initial_identity" ]] || die "resume changed identity or keys"
[[ "$(grep -c -- --enrollment-token-file "${agent_log}")" == "$enrollment_calls" ]] || die "resume enrolled twice"
install_token
assert_status 0
[[ "$(identity_digest)" == "$initial_identity" ]] || die "same-token replay changed keys"

# A registered root key must be restored, never regenerated.
as_root mv "${sysroot}/var/lib/ocservia-privd/attestation.key" "${fixture}/original-attestation"
resume_token
assert_status 1
assert_output "restore the original root key"
as_root test ! -e "${sysroot}/var/lib/ocservia-privd/attestation.key"
as_root mv "${fixture}/original-attestation" "${sysroot}/var/lib/ocservia-privd/attestation.key"
# Enrolled identity/sealing corruption fails before any replacement can occur.
as_root mv "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" "${fixture}/original-seal"
resume_token
assert_status 1
assert_output "missing its user-password sealing private key"
as_root test ! -e "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem"
as_root mv "${fixture}/original-seal" "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem"

for protected in relay-access-token controller-command-verification-key.pem; do
  as_root mv "${sysroot}/etc/ocservia-agent/$protected" "${fixture}/original-$protected"
  resume_token
  assert_status 1
  as_root test ! -e "${sysroot}/etc/ocservia-agent/$protected"
  as_root mv "${fixture}/original-$protected" "${sysroot}/etc/ocservia-agent/$protected"
done
as_root cp "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key" "${fixture}/original-endpoint"
# shellcheck disable=SC2016 # The child shell receives the fixture path as $1.
as_root sh -c 'printf "%032d" 8 >"$1"' sh "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key"
resume_token
assert_status 1
assert_output "replaced endpoint secret key"
as_root cp "${fixture}/original-endpoint" "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key"
[[ "$(identity_digest)" == "$initial_identity" ]] || die "failed recovery changed retained material"

scenario
rm -f "${fixture}/registered"
printf 1 >"${enroll_exit_file}"
install_token
assert_status 1
assert_output "response is unknown"
original_endpoint="$(as_root sha256sum "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key")"
rm -f "${enroll_exit_file}"
resume_token
assert_status 0
assert_output '^ONLINE$'
[[ "$(as_root sha256sum "${sysroot}/var/lib/ocservia-agent/identity/endpoint.key")" == "$original_endpoint" ]] || die "unknown enrollment regenerated identity"

scenario
rm -f "${fixture}/registered"
touch "${fixture}/bad-registration"
install_token
assert_status 1
assert_log_empty "${systemctl_log}"
rm -f "${fixture}/bad-registration"
resume_token
assert_status 0
assert_output '^ONLINE$'

scenario
rm -f "${fixture}/registered"
touch "${fixture}/stale-status"
install_token
assert_status 1
assert_output "Controller has not confirmed online/fresh state"
if grep -qx ONLINE <<<"$RUN_OUTPUT"; then die 'local service startup claimed ONLINE'; fi
rm -f "${fixture}/stale-status"
resume_token
assert_status 0
assert_output '^ONLINE$'

scenario
touch "${fixture}/http-failure"
install_token
assert_status 1
assert_log_empty "${dpkg_log}"
rm -f "${fixture}/http-failure"
resume_token
assert_status 0
assert_output '^ONLINE$'

for architecture in amd64 arm64; do
  scenario
  if [[ "$architecture" == amd64 ]]; then printf x86_64 >"${arch_file}"; else printf aarch64 >"${arch_file}"; fi
  install_token
  assert_status 0
  assert_log_contains "${dpkg_log}" "ocservia-agent_${VERSION}-1_${architecture}.deb"
done
scenario
EXTRA_ENV=("OCSERV_MANAGED_NODE_OS_RELEASE=${os}/rocky-9")
install_token
assert_status 0
assert_log_contains "${rpm_log}" "ocservia-agent-${VERSION}-1."

for phase in before-freeze after-freeze; do
  scenario
  printf '%s\n' "$phase" >"${tamper_after_verify_file}"
  install_token
  assert_status 1
  assert_log_empty "${dpkg_log}"
done
scenario
printf 'resource=ocpasswd reason=mode\n' >"${fixture}/preflight-failure"
install_token
assert_status 1
assert_log_empty "${agent_log}"
as_root test ! -e "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem"
echo "Token native installation, protected recovery, package convergence and Controller readiness tests passed"
