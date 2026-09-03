#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="${ROOT}/deploy/managed-node/install.sh"
DOWNLOAD_BASE="https://github.com/GentleKingson/ocservia/releases/download"
VERSION="0.2.1"
MOCK_ENDPOINT_ID="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
MOCK_NODE_ID="018f1e11-2222-7333-8444-555555555555"

# The installer fixture asserts file modes with GNU stat and signs release
# fixtures with openssl; skip on hosts without them (mirrors the guard in
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

fixture="$(mktemp -d "${HOME}/.ocservia-managed-node-test.XXXXXX")"

can_root() {
  ((EUID == 0)) || sudo -n true >/dev/null 2>&1
}

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
trusted="${fixture}/trusted"
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
endpoint_id_file="${fixture}/endpoint.id"
node_id_file="${fixture}/node.id"
enroll_exit_file="${fixture}/enroll-exit"
installed_version_file="${fixture}/installed-version"
arch_file="${fixture}/arch"
tamper_after_verify_file="${fixture}/tamper-after-verify"
key_swap_path_file="${fixture}/key-swap-path"

die() {
  echo "Managed node install tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

RUN_STATUS=0
RUN_OUTPUT=""
EXTRA_ENV=()
OS_RELEASE="${os}/ubuntu-24.04"

mkdir -m 700 -- "${bin}" "${logs}" "${serve}" "${trusted}" "${sysroot}"
mkdir -p -- "${repo}/deploy/managed-node" "${os}"

# The out-of-band trust anchor: an Ed25519 release key kept outside the
# release download directory, exactly like an operator-provisioned key.
openssl genpkey -algorithm ed25519 -out "${trusted}/release-signing.key" 2>/dev/null
openssl pkey -in "${trusted}/release-signing.key" -pubout -out "${trusted}/release-signing.pub.pem"
fingerprint="$(openssl pkey -pubin -in "${trusted}/release-signing.pub.pem" -outform DER | sha256sum | awk '{print $1}')"
# A second key the launcher race swaps in for the operator-provisioned one.
openssl genpkey -algorithm ed25519 -out "${fixture}/attacker-signing.key" 2>/dev/null
openssl pkey -in "${fixture}/attacker-signing.key" -pubout \
  -out "${fixture}/attacker-release-signing.pub.pem"
openssl genpkey -algorithm ed25519 -out "${fixture}/command-verification.key" 2>/dev/null
openssl pkey -in "${fixture}/command-verification.key" -pubout -out "${fixture}/controller-command-verification-key.pem"
printf 'mock relay access token bytes\n' >"${fixture}/relay-access-token"
chmod 0600 -- "${trusted}/release-signing.key" "${fixture}/command-verification.key"
controller_id="$(printf 'c%.0s' $(seq 1 64))"

assets=(
  "ocservia-agent-${VERSION}-linux-amd64.tar.gz"
  "ocservia-agent-${VERSION}-linux-arm64.tar.gz"
  "ocservia-agent_${VERSION}_amd64.deb"
  "ocservia-agent_${VERSION}_arm64.deb"
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
  while IFS= read -r name; do
    (cd "${serve}" && sha256sum -- "${name}")
  done < <(printf '%s\n' "${assets[@]}" | LC_ALL=C sort) >"${serve}/SHA256SUMS"
  openssl pkeyutl -sign -rawin -inkey "${trusted}/release-signing.key" \
    -in "${serve}/SHA256SUMS" -out "${serve}/SHA256SUMS.sig"
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
    printf '%s\n' "$(cat -- "${root}/endpoint.id")"
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
expected="$(grep -F "  ${package##*/}" "${root}/serve/SHA256SUMS" | awk '{print $1}')"
[[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "native install mock: no signed checksum for ${package##*/}" >&2
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
printf '[Service]\nEnvironmentFile=/etc/ocservia-agent/relays.env\n' \
  >"${OCSERV_MANAGED_NODE_SYSROOT}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
printf 'RELAY_URL_A=https://relay-a.example.com\nRELAY_URL_B=https://relay-b.example.com\n' >"${conf}/relays.env"
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
  *.deb) version="${version#ocservia-agent_}" ; version="${version%_*}-1" ;;
  *.rpm) version="${version#ocservia-agent-}" ; version="${version%-1.*}" ;;
esac
printf 'installed %s\n' "${version}" >"${OCSERV_MANAGED_NODE_SYSROOT}/.package-state"
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
    -u) shift 2 ;;
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
# Regression seam: downloads run after the release key fingerprint was
# verified against the frozen copy, so this is the moment a racing
# launcher-UID process swaps the launcher-controlled TRUSTED_RELEASE_KEY
# pathname (key-swap-path holds the target, one shot). A correct installer
# verified the frozen key and never reads the original pathname again.
if [[ -s "${root}/key-swap-path" ]]; then
  swap_target="$(cat -- "${root}/key-swap-path")"
  : >"${root}/key-swap-path"
  if [[ -n "${swap_target}" && -f "${swap_target}" ]]; then
    cp -- "${root}/attacker-release-signing.pub.pem" "${swap_target}"
  fi
fi
output="" url=""
while (($# > 0)); do
  case "$1" in
    --output) shift; output="${1:-}" ;;
    http*) url="$1" ;;
  esac
  shift
done
[[ -n "${output}" && -n "${url}" ]] || {
  echo "curl mock: missing --output or url" >&2
  exit 1
}
printf '%s\n' "${url}" >>"${root}/logs/curl.log"
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
if [[ "${1:-}" == "pkeyutl" && -s "${root}/tamper-after-verify" ]]; then
  status=0
  /usr/bin/openssl "$@" || status=$?
  if ((status == 0)); then
    # Regression seam for the signed-manifest TOCTOU: the moment signature
    # verification succeeds, a racing launcher-UID process replaces the
    # launcher-writable manifest and package with internally consistent
    # attacker artifacts (a manifest whose digest matches an attacker
    # package). A correct installer froze its copies before verification and
    # never reads the launcher staging again. The download staging template
    # is ocservia-managed-node. (the privileged staging is -pkg. and is
    # already frozen here).
    for staging in /tmp/ocservia-managed-node.* "${TMPDIR:-/tmp}"/ocservia-managed-node.*; do
      [[ -d "${staging}" ]] || continue
      pkg=""
      for candidate in "${staging}"/*.deb "${staging}"/*.rpm; do
        [[ -f "${candidate}" ]] && pkg="${candidate}"
      done
      [[ -n "${pkg}" ]] || continue
      printf 'attacker package bytes\n' >"${pkg}"
      evil_digest="$(sha256sum -- "${pkg}" | awk '{print $1}')"
      printf '%s  %s\n' "${evil_digest}" "$(basename -- "${pkg}")" >"${staging}/SHA256SUMS"
    done
  fi
  exit "${status}"
fi
exec /usr/bin/openssl "$@"
EOF

chmod 0755 -- "${bin}/agent-stub" "${bin}/native-install" "${bin}/sudo" \
  "${bin}/curl" "${bin}/dpkg" "${bin}/dpkg-query" "${bin}/rpm" \
  "${bin}/runuser" "${bin}/systemctl" "${bin}/uname" "${bin}/id" \
  "${bin}/openssl"

build_serve

cp -- "${INSTALL}" "${repo}/deploy/managed-node/install.sh"
git -C "${repo}" init -q
git -C "${repo}" config user.name test
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" add -A
git -C "${repo}" commit -qm base
git -C "${repo}" tag "v${VERSION}"

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
    "PATH=${TEST_PATH_PREFIX:-${bin}:${PATH}}"
    "OCSERV_MANAGED_NODE_SYSROOT=${sysroot}"
    "OCSERV_MANAGED_NODE_OS_RELEASE=${OS_RELEASE}"
    "CONTROLLER_ENDPOINT_ID=${controller_id}"
    "RELAY_URL_A=https://relay-a.example.test"
    "RELAY_URL_B=https://relay-b.example.test"
    "RELAY_ACCESS_TOKEN_SOURCE=${fixture}/relay-access-token"
    "CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=${fixture}/controller-command-verification-key.pem"
    "TRUSTED_RELEASE_KEY=${trusted}/release-signing.pub.pem"
    "EXPECTED_RELEASE_KEY_SHA256=${fingerprint}"
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
  printf '%s\n' "${MOCK_ENDPOINT_ID}" >"${endpoint_id_file}"
  printf '%s\n' "${MOCK_NODE_ID}" >"${node_id_file}"
  rm -f -- "${enroll_exit_file}" "${installed_version_file}" "${arch_file}" \
    "${tamper_after_verify_file}" "${key_swap_path_file}"
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
}

capture() {
  build_env
  RUN_STATUS=0
  RUN_OUTPUT="$(env "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
    "${repo}/deploy/managed-node/install.sh" "$@" 2>&1)" || RUN_STATUS=$?
}

capture_root() {
  build_env
  RUN_STATUS=0
  if ((EUID == 0)); then
    RUN_OUTPUT="$(env -u SUDO_USER "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
      "${repo}/deploy/managed-node/install.sh" 2>&1)" || RUN_STATUS=$?
  else
    RUN_OUTPUT="$(sudo -n env -u SUDO_USER "${ROOT_ENV[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
      "${repo}/deploy/managed-node/install.sh" 2>&1)" || RUN_STATUS=$?
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
  reset_state
  reset_checkout
}

# 1. usage errors.
scenario
capture unexpected-argument
assert_status 2 "an unexpected argument must be a usage error"
capture --root-lifecycle extra-argument
assert_status 2 "extra arguments alongside --root-lifecycle must be a usage error"
echo "usage errors fail with status 2"

# 2. a non-tag checkout is rejected before any host mutation.
scenario
git -C "${repo}" commit -q --allow-empty -m beyond-tag
capture
assert_status 1 "a checkout past the release tag must fail closed"
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${curl_log}"
assert_log_empty "${dpkg_log}"
assert_log_empty "${sudo_log}"
echo "a non-tag checkout is rejected before any host mutation"

# 2a. a non-SemVer tag is not a release identity.
scenario
git -C "${repo}" commit -q --allow-empty -m rc
git -C "${repo}" tag v0.3.0-rc1
capture
assert_status 1 "an RC tag must not be a production release identity"
assert_output "exactly one exact vX.Y.Z release tag"
assert_log_empty "${dpkg_log}"
echo "an RC tag is rejected"

# 2b. a dirty checkout is rejected before any host mutation.
scenario
printf '\n# local edit\n' >>"${repo}/deploy/managed-node/install.sh"
capture
assert_status 1 "a dirty checkout must fail closed"
assert_output "dirty"
assert_log_empty "${curl_log}"
assert_log_empty "${dpkg_log}"
echo "a dirty checkout is rejected before any host mutation"

# 3. an unsupported architecture is rejected before any host mutation.
scenario
printf 'ppc64le\n' >"${arch_file}"
capture
assert_status 1 "an unsupported architecture must fail closed"
assert_output "unsupported architecture"
assert_log_empty "${curl_log}"
assert_log_empty "${dpkg_log}"
echo "an unsupported architecture is rejected before any host mutation"

# 4. an unsupported distribution is rejected before any host mutation.
# Ubuntu 20.04 and Debian 11 ship OpenSSL 1.1.1, whose pkeyutl cannot verify
# the Ed25519 SHA256SUMS signature, so they are unsupported despite being
# declarable dpkg hosts.
for distro in fedora-41 arch ubuntu-20.04 debian-11; do
  scenario
  EXTRA_ENV=("OCSERV_MANAGED_NODE_OS_RELEASE=${os}/${distro}")
  capture
  assert_status 1 "an unsupported distribution must fail closed"
  assert_output "unsupported OS"
  assert_log_empty "${curl_log}"
  assert_log_empty "${dpkg_log}"
  assert_log_empty "${rpm_log}"
done
echo "unsupported distributions are rejected before any host mutation"

# 5. a missing or mismatched out-of-band trust anchor fails before any
# download and before the package manager.
scenario
EXTRA_ENV=("TRUSTED_RELEASE_KEY=${fixture}/no-such-key.pem")
capture
assert_status 1 "a missing trusted release key must fail closed"
assert_output "trusted release public key"
assert_log_empty "${curl_log}"
assert_log_empty "${dpkg_log}"
scenario
EXTRA_ENV=("EXPECTED_RELEASE_KEY_SHA256=$(printf '0%.0s' $(seq 1 64))")
capture
assert_status 1 "a mismatched trusted key fingerprint must fail closed"
assert_output "does not match the expected"
assert_log_empty "${curl_log}"
assert_log_empty "${dpkg_log}"
echo "a missing or mismatched trust anchor fails before any download"

# 6. a bad release manifest signature fails before the package manager.
scenario
printf '\ntampered\n' >>"${serve}/SHA256SUMS"
capture
assert_status 1 "a tampered checksum manifest must fail closed"
assert_output "signature verification failed"
assert_log_empty "${dpkg_log}"
echo "a bad release signature is rejected before the package manager"

# 7. a selected package digest mismatch fails before the package manager.
scenario
zeros="$(printf '0%.0s' $(seq 1 64))"
sed -i.bak \
  -e "s/^[0-9a-f]\{64\}  \(ocservia-agent_${VERSION}_amd64\.deb\)$/${zeros}  \1/" \
  -e "s/^[0-9a-f]\{64\}  \(ocservia-agent_${VERSION}_arm64\.deb\)$/${zeros}  \1/" \
  "${serve}/SHA256SUMS"
rm -f -- "${serve}/SHA256SUMS.bak"
openssl pkeyutl -sign -rawin -inkey "${trusted}/release-signing.key" \
  -in "${serve}/SHA256SUMS" -out "${serve}/SHA256SUMS.sig"
capture
assert_status 1 "a digest mismatch must fail closed"
assert_output "does not match the signed checksum"
assert_log_empty "${dpkg_log}"
echo "a package digest mismatch is rejected before the package manager"

# 8. package selection follows the platform and architecture: amd64/arm64
# DEB naming on the Debian family and x86_64/aarch64 RPM naming on Rocky 9.
scenario
capture
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/SHA256SUMS"
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/SHA256SUMS.sig"
case "$(uname -m)" in
  x86_64) selected="ocservia-agent_${VERSION}_amd64.deb" ;;
  aarch64 | arm64) selected="ocservia-agent_${VERSION}_arm64.deb" ;;
  *) selected="ocservia-agent_${VERSION}_amd64.deb" ;;
esac
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/${selected}"
[[ "$(wc -l <"${curl_log}" | tr -d ' ')" == 3 ]] ||
  die "expected exactly three downloads, got: $(cat -- "${curl_log}")"
if grep -q "release-signing" "${curl_log}"; then
  die "the installer must never download release trust material: $(cat -- "${curl_log}")"
fi
assert_log_contains "${dpkg_log}" "${selected}"
if ((EUID == 0)); then
  # A suite that itself runs as root performs the real privileged flow.
  assert_status 0 "the root install flow must reach ENROLLMENT_READY"
  assert_output "ENROLLMENT_READY"
else
  # The unprivileged fixture cannot create root-owned node configuration, so
  # the flow must stop at the first privileged metadata check instead of
  # silently continuing with unsafe ownership.
  assert_status 1 "the unprivileged fixture must fail closed at node preparation"
  assert_output "unsafe metadata"
fi
echo "the amd64/arm64 DEB selection installs the matching package"

scenario
printf 'aarch64\n' >"${arch_file}"
capture
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/ocservia-agent_${VERSION}_arm64.deb"
if grep -q "amd64" "${curl_log}"; then
  die "an arm64 host must not download the amd64 package: $(cat -- "${curl_log}")"
fi
echo "arm64 hosts select the arm64 DEB"

scenario
printf 'x86_64\n' >"${arch_file}"
EXTRA_ENV=("OCSERV_MANAGED_NODE_OS_RELEASE=${os}/rocky-9")
capture
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/ocservia-agent-${VERSION}-1.x86_64.rpm"
assert_log_empty "${dpkg_log}"
assert_log_contains "${rpm_log}" "ocservia-agent-${VERSION}-1.x86_64.rpm"
echo "RPM hosts use the x86_64 RPM naming"

scenario
printf 'aarch64\n' >"${arch_file}"
EXTRA_ENV=("OCSERV_MANAGED_NODE_OS_RELEASE=${os}/rocky-9")
capture
assert_log_contains "${curl_log}" "${DOWNLOAD_BASE}/v${VERSION}/ocservia-agent-${VERSION}-1.aarch64.rpm"
assert_log_empty "${dpkg_log}"
echo "RPM hosts use the aarch64 RPM naming"

# 9. an installed package at a different version fails closed: the bootstrap
# neither upgrades nor downgrades.
scenario
printf '9.9.9\n' >"${installed_version_file}"
capture
assert_status 1 "a version mismatch must fail closed"
assert_output "neither upgrades nor downgrades"
# Out-of-band trust verification legitimately crosses the privileged boundary
# before the installed-version check; the package manager must not run.
assert_log_empty "${dpkg_log}"
echo "an installed version mismatch fails closed"

# 10. an installed package without the production relay contract fails
# closed instead of silently reusing a relay-free install.
scenario
# The DEB platform's native version carries the nfpm release component.
printf '%s\n' "${VERSION}-1" >"${installed_version_file}"
capture
assert_status 1 "a relay-free installed package must fail closed"
assert_output "production relay drop-in"
assert_log_empty "${dpkg_log}"
echo "a relay-free installed package is rejected"

# The remaining scenarios perform real privileged node preparation and need
# the root lifecycle.
if ! can_root; then
  echo "root lifecycle scenarios skipped: no passwordless sudo available" >&2
  echo "Managed node install tests passed"
  exit 0
fi

# 11. the full root flow reaches ENROLLMENT_READY and prepares the production
# node without enabling any service. The run uses a hostile operator-owned
# TMPDIR: the privileged package staging must sit under a trusted system
# parent, never under a directory the launcher controls (pathname trust comes
# from the parent, so an operator-owned parent would allow renaming even a
# root-owned staging entry between the digest check and the package manager).
scenario
hostile_tmp="${fixture}/hostile-tmp"
mkdir -m 700 -- "${hostile_tmp}"
EXTRA_ENV=("TMPDIR=${hostile_tmp}")
capture_root
assert_status 0 "the root bootstrap flow must succeed"
assert_output "ENROLLMENT_READY"
assert_output "EndpointID: ${MOCK_ENDPOINT_ID}"
assert_output "next: create a short-lived one-time enrollment token"
conf="${sysroot}/etc/ocservia-agent"
# The configuration directory is mode 0750 root:ocserv-agent, so the
# assertions below must read it through the privileged helper.
[[ "$(as_root stat -c '%U:%G:%a' "${conf}/relays.env")" == "root:ocserv-agent:640" ]] ||
  die "relays.env ownership is wrong: $(as_root stat -c '%U:%G:%a' "${conf}/relays.env")"
as_root grep -qx "RELAY_URL_A=https://relay-a.example.test" "${conf}/relays.env" ||
  die "relays.env does not carry the requested relay URL A"
as_root grep -qx "RELAY_URL_B=https://relay-b.example.test" "${conf}/relays.env" ||
  die "relays.env does not carry the requested relay URL B"
[[ "$(as_root stat -c '%U:%G:%a' "${conf}/relay-access-token")" == "root:ocserv-agent:640" ]] ||
  die "relay-access-token ownership is wrong"
as_root cmp -s -- "${fixture}/relay-access-token" "${conf}/relay-access-token" ||
  die "relay-access-token content does not match the protected source"
[[ "$(as_root stat -c '%U:%G:%a' "${conf}/controller-command-verification-key.pem")" == "root:ocserv-agent:640" ]] ||
  die "command verification key ownership is wrong"
for key in user-password-seal-private.pem p12-password-seal-private.pem; do
  [[ "$(as_root stat -c '%U:%G:%a' "${conf}/${key}")" == "root:root:600" ]] ||
    die "${key} ownership is wrong: $(as_root stat -c '%U:%G:%a' "${conf}/${key}")"
done
as_root grep -qx "NODE_ID=00000000-0000-7000-8000-000000000000" "${conf}/agent.env" ||
  die "agent.env must still carry the placeholder NODE_ID before enrollment"
[[ ! -e "${sysroot}/etc/ocservia/agent-install-production-relays" ]] ||
  die "the production request marker must be consumed by the successful install"
assert_log_contains "${agent_log}" "--prepare-enrollment"
# The package manager must install the frozen root-owned copy, never a path
# inside the launcher-writable staging directory (mktemp template
# ocservia-managed-node-pkg. vs ocservia-managed-node.), and never under the
# hostile operator-controlled TMPDIR.
assert_log_contains "${dpkg_log}" "ocservia-managed-node-pkg."
if grep -q -- "ocservia-managed-node\." "${dpkg_log}"; then
  die "dpkg must install the root-owned package copy, not the launcher staging: $(cat -- "${dpkg_log}")"
fi
if grep -q -- "${hostile_tmp}" "${dpkg_log}"; then
  die "the privileged package staging must not live under the operator-controlled TMPDIR: $(cat -- "${dpkg_log}")"
fi
if grep -q -- "--enrollment-token-file" "${agent_log}"; then
  die "no enrollment may run without a token file"
fi
assert_log_empty "${systemctl_log}"
assert_log_empty "${sudo_log}"
grep -q -- "--controller ${controller_id}" "${agent_log}" ||
  die "identity preparation must pin the Controller EndpointID"
echo "the root bootstrap flow reaches ENROLLMENT_READY"

# 11a. a launcher-UID race cannot swap the signed manifest after signature
# verification succeeds: the manifest parse and the package digest must read
# the frozen root-owned staging, never the launcher-writable download
# directory. The openssl mock rewrites the launcher staging with an
# internally consistent attacker manifest + package the instant verification
# returns success; the installer must still install the frozen signed bytes
# (the native-install simulator rejects any package whose digest does not
# match the signed manifest).
scenario
printf '1\n' >"${tamper_after_verify_file}"
capture_root
assert_status 0 "a post-verification launcher staging swap must not affect the install"
assert_output "ENROLLMENT_READY"
assert_log_contains "${dpkg_log}" "ocservia-managed-node-pkg."
echo "a post-verification manifest swap in the launcher staging is ignored"

# 11b. the release key is frozen before its fingerprint is verified: a
# launcher-controlled TRUSTED_RELEASE_KEY pathname swapped after the check
# must never become the key that verifies the manifest. TRUSTED_RELEASE_KEY
# points at a launcher-writable copy of the real key; the curl mock swaps that
# pathname to an attacker key at the first download (after the fingerprint
# check), and every later openssl read must use the frozen root-owned copy —
# otherwise the real manifest's signature would verify under the attacker key.
scenario
cp -- "${trusted}/release-signing.pub.pem" "${fixture}/operator-release-key.pub.pem"
printf '%s\n' "${fixture}/operator-release-key.pub.pem" >"${key_swap_path_file}"
EXTRA_ENV=("TRUSTED_RELEASE_KEY=${fixture}/operator-release-key.pub.pem")
capture_root
assert_status 0 "a post-verification key path swap must not affect the install"
assert_output "ENROLLMENT_READY"
assert_log_contains "${openssl_log}" "pkeyutl -verify -rawin -pubin -inkey"
assert_log_contains "${openssl_log}" "/var/tmp/ocservia-managed-node-pkg."
if grep -q -- "operator-release-key" "${openssl_log}"; then
  die "openssl must use the frozen key copy, not the launcher-controlled path: $(cat -- "${openssl_log}")"
fi
echo "a post-verification key path swap is ignored"

# 12. a rerun converges: the installed package is reused, not reinstalled.
scenario
capture_root
assert_status 0
dpkg_calls="$(grep -c -- "-i" "${dpkg_log}")"
capture_root
assert_status 0 "the converged rerun must succeed"
assert_output "already installed; skipping package installation"
[[ "$(grep -c -- "-i" "${dpkg_log}")" == "${dpkg_calls}" ]] ||
  die "a rerun must not reinstall the native package"
grep -q "preserved existing user-password sealing key" <<<"${RUN_OUTPUT}" ||
  die "a rerun must preserve the generated sealing keys"
echo "a rerun converges without reinstalling"

# 13. existing valid sealing keys are preserved and bound as descriptors.
scenario
as_root install -d -o root -g root -m 0755 -- "${sysroot}/etc/ocservia-agent"
as_root openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem"
as_root openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem"
as_root chmod 0600 -- "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" \
  "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem"
user_key_digest="$(as_root sha256sum -- "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" | awk '{print $1}')"
p12_key_digest="$(as_root sha256sum -- "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem" | awk '{print $1}')"
printf 'mock one-time enrollment token bytes\n' >"${fixture}/enrollment-token"
as_root install -o root -g ocserv-agent -m 0640 -- "${fixture}/enrollment-token" \
  "${sysroot}/etc/ocservia-agent/enrollment-token"
capture_root
assert_status 0 "enrollment with preserved keys must succeed"
assert_output "PENDING_APPROVAL"
assert_output "NODE_ID: ${MOCK_NODE_ID}"
[[ "$(as_root sha256sum -- "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" | awk '{print $1}')" == "${user_key_digest}" ]] ||
  die "the existing user-password sealing key must be preserved byte for byte"
[[ "$(as_root sha256sum -- "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem" | awk '{print $1}')" == "${p12_key_digest}" ]] ||
  die "the existing p12-password sealing key must be preserved byte for byte"
expected_user_desc="$(as_root openssl rsa -in "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
expected_p12_desc="$(as_root openssl rsa -in "${sysroot}/etc/ocservia-agent/p12-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
as_root grep -qx "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${expected_user_desc}" "${sysroot}/etc/ocservia-agent/agent.env" ||
  die "agent.env must carry the preserved user-password sealing key descriptor"
as_root grep -qx "P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${expected_p12_desc}" "${sysroot}/etc/ocservia-agent/agent.env" ||
  die "agent.env must carry the preserved p12-password sealing key descriptor"
echo "existing valid sealing keys are preserved and bound"

# 14. unsafe existing material fails closed.
scenario
capture_root
assert_status 0
as_root chmod 0644 -- "${sysroot}/etc/ocservia-agent/user-password-seal-private.pem"
capture_root
assert_status 1 "an unsafe sealing key must fail closed"
assert_output "unsafe metadata"
assert_output "never overwrites sealing material"
scenario
capture_root
assert_status 0
as_root sh -c "printf 'RELAY_URL_A=https://other-a.example.test\nRELAY_URL_B=https://other-b.example.test\n' >'${sysroot}/etc/ocservia-agent/relays.env'"
as_root chmod 0640 "${sysroot}/etc/ocservia-agent/relays.env"
as_root chown root:ocserv-agent "${sysroot}/etc/ocservia-agent/relays.env"
capture_root
assert_status 1 "a mismatching relays.env must fail closed"
assert_output "not the configured dedicated relays"
echo "unsafe or mismatched node material fails closed"

# 15. an invalid enrollment token leaves identity and configuration intact.
scenario
capture_root
assert_status 0
printf 'mock one-time enrollment token bytes\n' >"${fixture}/enrollment-token"
as_root install -o root -g ocserv-agent -m 0640 -- "${fixture}/enrollment-token" \
  "${sysroot}/etc/ocservia-agent/enrollment-token"
printf '23\n' >"${enroll_exit_file}"
capture_root
assert_status 1 "a failed enrollment must fail the bootstrap"
assert_output "enrollment failed"
as_root grep -qx "NODE_ID=00000000-0000-7000-8000-000000000000" "${sysroot}/etc/ocservia-agent/agent.env" ||
  die "a failed enrollment must not finalize agent.env"
as_root test -e "${sysroot}/etc/ocservia-agent/enrollment-token" ||
  die "a failed enrollment must leave the token file for the operator"
echo "an invalid enrollment token leaves the node state intact"

# 16. a valid protected token completes enrollment: the exact CLI contract,
# atomic agent.env finalization, token consumption, PENDING_APPROVAL.
scenario
capture_root
assert_status 0
printf 'mock one-time enrollment token bytes\n' >"${fixture}/enrollment-token"
as_root install -o root -g ocserv-agent -m 0640 -- "${fixture}/enrollment-token" \
  "${sysroot}/etc/ocservia-agent/enrollment-token"
capture_root
assert_status 0 "the enrollment rerun must succeed"
assert_output "PENDING_APPROVAL"
assert_output "NODE_ID: ${MOCK_NODE_ID}"
for argument in \
  "--identity-dir ${sysroot}/var/lib/ocservia-agent/identity" \
  "--controller ${controller_id}" \
  "--enrollment-token-file ${sysroot}/etc/ocservia-agent/enrollment-token" \
  "--enrollment-environment production" \
  "--user-password-seal-key-id user-password-v1" \
  "--p12-password-seal-key-id p12-password-v1" \
  "--relay-mode custom" \
  "--relay-url https://relay-a.example.test" \
  "--relay-url https://relay-b.example.test" \
  "--relay-token-file ${sysroot}/etc/ocservia-agent/relay-access-token"; do
  grep -qF -- "${argument}" "${agent_log}" ||
    die "the enrollment CLI invocation must carry '${argument}'"
done
[[ "$(as_root stat -c '%U:%G:%a' "${sysroot}/etc/ocservia-agent/agent.env")" == "root:ocserv-agent:640" ]] ||
  die "the final agent.env ownership is wrong"
as_root grep -qx "CONTROLLER_ENDPOINT_ID=${controller_id}" "${sysroot}/etc/ocservia-agent/agent.env" ||
  die "agent.env must pin the Controller EndpointID"
as_root grep -qx "NODE_ID=${MOCK_NODE_ID}" "${sysroot}/etc/ocservia-agent/agent.env" ||
  die "agent.env must carry the enrolled NODE_ID"
if as_root test -e "${sysroot}/etc/ocservia-agent/enrollment-token"; then
  die "the one-time enrollment token file must be consumed after success"
fi
assert_log_empty "${systemctl_log}"
echo "a valid token completes enrollment to PENDING_APPROVAL"

# 17. a rerun after enrollment does not re-enroll and stays at
# PENDING_APPROVAL; a stale token is reported, never reused.
enrollment_calls="$(grep -c -- "--enrollment-token-file" "${agent_log}")"
capture_root
assert_status 0 "the post-enrollment rerun must succeed"
assert_output "PENDING_APPROVAL"
assert_output "NODE_ID: ${MOCK_NODE_ID}"
[[ "$(grep -c -- "--enrollment-token-file" "${agent_log}")" == "${enrollment_calls}" ]] ||
  die "a rerun after enrollment must not enroll again"
printf 'mock stale enrollment token bytes\n' >"${fixture}/enrollment-token"
as_root install -o root -g ocserv-agent -m 0640 -- "${fixture}/enrollment-token" \
  "${sysroot}/etc/ocservia-agent/enrollment-token"
capture_root
assert_status 0 "a rerun with a stale token must stay at PENDING_APPROVAL"
assert_output "stale token"
[[ "$(grep -c -- "--enrollment-token-file" "${agent_log}")" == "${enrollment_calls}" ]] ||
  die "a stale token must never trigger a second enrollment"
assert_log_empty "${systemctl_log}"
echo "post-enrollment reruns stay at PENDING_APPROVAL"

# 18. whole-script sudo is rejected: the operator environment must not cross
# to root wholesale.
scenario
build_env
RUN_STATUS=0
RUN_OUTPUT="$(sudo -n env "${ROOT_ENV[@]}" SUDO_USER=ocservia-operator \
  "${repo}/deploy/managed-node/install.sh" 2>&1)" || RUN_STATUS=$?
assert_status 1 "whole-script sudo must fail closed"
assert_output "launcher user"
assert_output "--root-lifecycle"
assert_log_empty "${dpkg_log}"
assert_log_empty "${curl_log}"
echo "whole-script sudo is rejected with the launcher mismatch"

# 19. --root-lifecycle deliberately runs as root, forwarding only the
# managed-node configuration across sudo's env_reset. This regression needs a
# real non-root launcher with a real sudo boundary.
if ((EUID != 0)) && can_root; then
  scenario
    root_lifecycle_bin="${fixture}/root-lifecycle-bin"
  mkdir -m 700 -- "${root_lifecycle_bin}"
  for tool in bash git env install stat sed openssl sha256sum awk mktemp cat \
    mv rm touch tail dirname getent cp grep chown chmod basename; do
    ln -s "$(command -v "${tool}")" "${root_lifecycle_bin}/${tool}"
  done
  for mock in curl uname dpkg dpkg-query rpm runuser systemctl id; do
    cp -- "${bin}/${mock}" "${root_lifecycle_bin}/${mock}"
  done
  real_sudo="$(command -v sudo)"
  cat >"${root_lifecycle_bin}/sudo" <<EOF
  #!/usr/bin/env bash
  set -euo pipefail
  root="$(dirname -- "\${OCSERV_MANAGED_NODE_SYSROOT:?}")"
  printf '%s\n' "\$*" >>"${root_sudo_log}"
  exec "${real_sudo}" env PATH="${root_lifecycle_bin}" "\$@"
EOF
  chmod 0755 -- "${root_lifecycle_bin}/sudo"
  RUN_STATUS=0
  RUN_OUTPUT="$(
    export PATH="${root_lifecycle_bin}"
    export UNRELATED_ENV=must-not-cross-sudo
    TEST_PATH_PREFIX="${root_lifecycle_bin}" build_env
    env "${ROOT_ENV[@]}" "${repo}/deploy/managed-node/install.sh" --root-lifecycle 2>&1
  )" || RUN_STATUS=$?
  assert_status 0 "the operator root-lifecycle command must succeed"
  assert_output "ENROLLMENT_READY"
  assert_log_contains "${root_sudo_log}" "CONTROLLER_ENDPOINT_ID=${controller_id}"
  assert_log_contains "${root_sudo_log}" "TRUSTED_RELEASE_KEY=${trusted}/release-signing.pub.pem"
  if grep -q "UNRELATED_ENV" "${root_sudo_log}"; then
    die "the root-lifecycle sudo command must not forward unrelated environment variables: $(cat -- "${root_sudo_log}")"
  fi
  if ((EUID != 0)); then
    as_root chown -R "$(id -u):$(id -g)" "${repo}" 2>/dev/null || true
  fi
  assert_log_empty "${systemctl_log}"
  echo "the operator root-lifecycle command forwards only node configuration"
else
  echo "root-lifecycle forwarding case skipped: running as root" >&2
fi

echo "Managed node install tests passed"
