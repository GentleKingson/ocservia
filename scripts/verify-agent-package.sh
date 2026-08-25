#!/usr/bin/env bash
set -euo pipefail

archive="${1:?archive required}"
checksum="${2:?checksum required}"
signature="${3:?signature required}"
public_key="${4:?public key required}"
trusted_fingerprint="${AGENT_TRUSTED_KEY_SHA256:?AGENT_TRUSTED_KEY_SHA256 is required}"
DESTDIR="${DESTDIR:-}"
trusted_root="${DESTDIR}/var/lib/ocservia-upgrade/package-staging"

if [[ ${EUID} -ne 0 ]]; then
  echo "verify-agent-package.sh must run as root so verification and extraction use trusted staging" >&2
  exit 1
fi
if [[ ! "${trusted_fingerprint}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "trusted signing-key fingerprint must be 64 lowercase hexadecimal characters" >&2
  exit 2
fi
if [[ -n "${DESTDIR}" ]]; then
  if [[ "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ || ! -d "${DESTDIR}" || -L "${DESTDIR}" || \
        "$(stat -c '%u:%g:%a' -- "${DESTDIR}")" != "0:0:700" ]]; then
    echo "DESTDIR must be an absolute root:root mode 0700 staging root" >&2
    exit 2
  fi
fi

for file in "${archive}" "${checksum}" "${signature}" "${public_key}"; do
  name="$(basename -- "${file}")"
  if [[ "${name}" == -* || ! -f "${file}" || -L "${file}" ]]; then
    echo "package input must be a regular file with a non-option basename: ${file}" >&2
    exit 1
  fi
done

archive_name="$(basename -- "${archive}")"
if [[ ! "${archive_name}" =~ ^ocservia-agent-([0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?)-linux-(amd64|arm64)\.tar\.gz$ ]]; then
  echo "archive basename is not a supported Agent package name" >&2
  exit 1
fi
version="${BASH_REMATCH[1]}"
package_arch="${BASH_REMATCH[3]}"
package_name="ocservia-agent-${version}"

# Without DESTDIR this verifier feeds a real host installation, so reject a
# package built for another architecture before anything is staged or installed.
if [[ -z "${DESTDIR}" ]]; then
  case "$(uname -m)" in
    x86_64) host_arch=amd64 ;;
    aarch64) host_arch=arm64 ;;
    *)
      echo "unsupported host architecture for Agent package installation: $(uname -m)" >&2
      exit 1
      ;;
  esac
  if [[ "${package_arch}" != "${host_arch}" ]]; then
    echo "package architecture ${package_arch} does not match host architecture ${host_arch}; refusing to install a foreign-architecture package" >&2
    exit 1
  fi
fi

validate_trusted_ancestry() {
  local path="$1" current="" relative component uid mode
  local -a components=()
  if [[ -n "${DESTDIR}" ]]; then
    case "${path}" in
      "${DESTDIR}"|"${DESTDIR}"/*) ;;
      *) echo "trusted package staging path escapes DESTDIR" >&2; exit 1 ;;
    esac
    current="${DESTDIR}"
    relative="${path#"${DESTDIR}"}"
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      echo "DESTDIR must remain root-owned and not group/world writable" >&2
      exit 1
    fi
  else
    relative="${path}"
  fi
  IFS='/' read -r -a components <<<"${relative#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    current="${current}/${component}"
    if [[ ! -d "${current}" || -L "${current}" ]]; then
      echo "trusted package staging ancestry must contain only real directories: ${current}" >&2
      exit 1
    fi
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      echo "trusted package staging ancestry must be root-owned and not group/world writable: ${current}" >&2
      exit 1
    fi
  done
}

for directory in "${DESTDIR}/var" "${DESTDIR}/var/lib"; do
  if [[ -e "${directory}" || -L "${directory}" ]]; then
    validate_trusted_ancestry "${directory}"
  else
    validate_trusted_ancestry "$(dirname -- "${directory}")"
    install -d -o root -g root -m 0755 -- "${directory}"
  fi
done
for directory in "${DESTDIR}/var/lib/ocservia-upgrade" "${trusted_root}"; do
  if [[ -e "${directory}" || -L "${directory}" ]]; then
    if [[ ! -d "${directory}" || -L "${directory}" ]]; then
      echo "trusted package staging path must be a real directory: ${directory}" >&2
      exit 1
    fi
    validate_trusted_ancestry "${directory}"
    chown root:root -- "${directory}"
    chmod 0700 -- "${directory}"
  else
    validate_trusted_ancestry "$(dirname -- "${directory}")"
    install -d -o root -g root -m 0700 -- "${directory}"
  fi
done
validate_trusted_ancestry "${trusted_root}"
if [[ "$(stat -c '%u:%g:%a' -- "${trusted_root}")" != "0:0:700" ]]; then
  echo "trusted package staging root must be root:root mode 0700" >&2
  exit 1
fi

staging="$(mktemp -d "${trusted_root}/ocservia-agent-package.XXXXXX")"
cleanup() {
  local status=$?
  if [[ -n "${staging:-}" ]]; then
    rm -rf -- "${staging}"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM
chown root:root -- "${staging}"
chmod 0700 -- "${staging}"

trusted_archive="${staging}/package.tar.gz"
trusted_checksum="${staging}/package.sha256"
trusted_signature="${staging}/package.sha256.sig"
trusted_public_key="${staging}/release-signing.pub.pem"
install -o root -g root -m 0600 -- "${archive}" "${trusted_archive}"
install -o root -g root -m 0600 -- "${checksum}" "${trusted_checksum}"
install -o root -g root -m 0600 -- "${signature}" "${trusted_signature}"
install -o root -g root -m 0600 -- "${public_key}" "${trusted_public_key}"

for file in "${trusted_archive}" "${trusted_checksum}" "${trusted_signature}" "${trusted_public_key}"; do
  if [[ "$(stat -c '%u:%g:%a:%h' -- "${file}")" != "0:0:600:1" ]]; then
    echo "trusted package input staging has unsafe metadata" >&2
    exit 1
  fi
done

public_der="${staging}/release-signing.der"
openssl pkey -pubin -in "${trusted_public_key}" -outform DER -out "${public_der}"
actual_fingerprint="$(sha256sum -- "${public_der}" | awk '{print $1}')"
if [[ "${actual_fingerprint}" != "${trusted_fingerprint}" ]]; then
  echo "package signing key is not the trusted release key" >&2
  exit 1
fi

if [[ "$(wc -l <"${trusted_checksum}")" -ne 1 ]]; then
  echo "checksum manifest must contain exactly one entry" >&2
  exit 1
fi
manifest="$(cat -- "${trusted_checksum}")"
expected_digest="${manifest%%  *}"
if [[ ! "${expected_digest}" =~ ^[0-9a-f]{64}$ || "${manifest}" != "${expected_digest}  ${archive_name}" ]]; then
  echo "checksum manifest must canonically name exactly the supplied archive" >&2
  exit 1
fi

openssl pkeyutl -verify -rawin -pubin -inkey "${trusted_public_key}" \
  -sigfile "${trusted_signature}" -in "${trusted_checksum}" >/dev/null
actual_digest="$(sha256sum -- "${trusted_archive}" | awk '{print $1}')"
if [[ "${actual_digest}" != "${expected_digest}" ]]; then
  echo "Agent package archive digest does not match the signed checksum" >&2
  exit 1
fi

archive_listing="${staging}/archive.list"
archive_verbose="${staging}/archive.verbose"
LC_ALL=C tar --list --gzip --file="${trusted_archive}" >"${archive_listing}"
LC_ALL=C tar --list --verbose --gzip --file="${trusted_archive}" >"${archive_verbose}"
if [[ "$(wc -l <"${archive_listing}")" -lt 1 || "$(wc -l <"${archive_listing}")" -gt 128 ]] || \
  awk 'length($0) > 255 || $0 !~ /^[A-Za-z0-9._+\/-]+$/ || $0 ~ /^\// || $0 ~ /(^|\/)\.\.?($|\/)/ || $0 ~ /\/\// || $0 ~ /(^|\/)-/ { bad=1 } END { exit bad ? 0 : 1 }' "${archive_listing}"; then
  echo "package contains an unsafe member path" >&2
  exit 1
fi
if [[ -n "$(LC_ALL=C sort "${archive_listing}" | uniq -d)" ]]; then
  echo "package contains duplicate archive members" >&2
  exit 1
fi
if awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" { bad=1 } END { exit bad ? 0 : 1 }' "${archive_verbose}"; then
  echo "package contains a link, device, FIFO, or other unsupported member type" >&2
  exit 1
fi
if awk -v root="${package_name}" 'index($0, root "/") != 1 && $0 != root "/" { bad=1 } END { exit bad ? 0 : 1 }' "${archive_listing}"; then
  echo "package contains a member outside its versioned root" >&2
  exit 1
fi
for required in MANIFEST scripts/install-agent.sh scripts/upgrade-agent.sh scripts/rollback-agent.sh scripts/uninstall-agent.sh rust/target/release/ocservia-agent rust/target/release/ocservia-privd; do
  if ! grep -Fxq -- "${package_name}/${required}" "${archive_listing}"; then
    echo "package is missing ${required}" >&2
    exit 1
  fi
done
if grep -Fxq -- "${package_name}/.ocservia-package-verified" "${archive_listing}"; then
  echo "package archive must not provide its own verification marker" >&2
  exit 1
fi

extract_root="${staging}/extracted"
install -d -o root -g root -m 0700 -- "${extract_root}"
tar --extract --gzip --file="${trusted_archive}" --directory="${extract_root}" \
  --no-same-owner --no-same-permissions
package_root="${extract_root}/${package_name}"
if [[ ! -d "${package_root}" || -L "${package_root}" ]]; then
  echo "verified package root is missing or unsafe" >&2
  exit 1
fi
if find "${package_root}" -xdev \! -type f \! -type d -print -quit | grep -q -- .; then
  echo "extracted package contains an unsupported filesystem object" >&2
  exit 1
fi
chown -R root:root -- "${package_root}"
chmod 0700 -- "${package_root}"
marker="${package_root}/.ocservia-package-verified"
install -o root -g root -m 0600 -- /dev/null "${marker}"
printf 'version=1\narchive_sha256=%s\npackage=%s\n' "${actual_digest}" "${package_name}" >"${marker}"
sync -f "${marker}"
sync -f "${package_root}"

printf '%s\n' "${package_root}"
staging=""
