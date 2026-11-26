#!/usr/bin/env bash
# Runtime archives and checksums are published together in the immutable Release.
# HTTPS + the Release publication policy establish origin; these checks detect
# corruption and local drift, and do not claim to be an independent signature.
runtime_download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --tlsv1.2 --output "$2" "$1"
  else
    wget --https-only --secure-protocol=TLSv1_2 -q -O "$2" "$1"
  fi
}

runtime_safe_path() {
  [[ "$1" =~ ^[A-Za-z0-9._/-]+$ && "$1" != /* && "$1" != *..* && "$1" != *//* ]] || return 1
  case "$1" in deploy/production/*|deploy/lib/*|install.env.example|RELEASE|SHA256SUMS) return 0 ;; esac
  return 1
}

runtime_valid_tag() {
  [[ "$1" =~ ^v[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?$ ]]
}

runtime_identity() {
  local root="$1" tag="$2" commit="${3:-}" actual_tag actual_commit extra
  runtime_valid_tag "${tag}" || fail "exact runtime release required"
  [[ -d "${root}" && ! -L "${root}" ]] || fail "runtime must be a regular directory: ${root}"
  [[ -f "${root}/RELEASE" && ! -L "${root}/RELEASE" && -f "${root}/SHA256SUMS" && ! -L "${root}/SHA256SUMS" ]] ||
    fail "runtime release metadata is missing"
  read -r actual_tag actual_commit extra <"${root}/RELEASE"
  [[ "${actual_tag}" == "${tag}" && "${actual_commit}" =~ ^[0-9a-f]{40}$ && -z "${extra}" ]] ||
    fail "runtime release identity mismatch"
  [[ -z "${commit}" || "${actual_commit}" == "${commit}" ]] || fail "runtime source_commit mismatch"
}

runtime_validate() (
  local root="$1"
  runtime_identity "$@"
  cd -- "${root}" || fail "cannot enter runtime directory"
  local uid
  uid="$(id -u)"
  [[ -z "$(find . \( -type l -o -perm /022 -o \( ! -uid 0 ! -uid "${uid}" ! -uid "${SUDO_UID:-${uid}}" \) \) -print -quit)" ]] ||
    fail "unsafe runtime file type, ownership or permissions"
  # Compare the complete inventory as well as bytes: extra files are drift too.
  diff -u <(awk '{print $2}' SHA256SUMS | sort) <(find . -type f ! -path ./SHA256SUMS | sed 's|^./||' | sort) >/dev/null ||
    fail "runtime file inventory changed"
  sha256sum --check --strict SHA256SUMS >/dev/null || fail "runtime content changed"
)

runtime_fetch() (
  local target="$1" tag="$2" commit="${3:-}" temporary archive base hash name entry
  runtime_valid_tag "${tag}" || fail "exact runtime release required"
  if [[ -e "${target}" || -L "${target}" ]]; then
    runtime_validate "${target}" "${tag}" "${commit}"
    return
  fi
  temporary="$(mktemp -d "$(dirname -- "${target}")/.runtime.XXXXXX")"
  trap 'rm -rf -- "${temporary}"' EXIT
  archive="controller-runtime-${tag}.tar.gz"
  base="https://github.com/GentleKingson/ocservia/releases/download/${tag}"
  runtime_download "${base}/${archive}" "${temporary}/${archive}"
  runtime_download "${base}/${archive}.sha256" "${temporary}/${archive}.sha256"
  read -r hash name <"${temporary}/${archive}.sha256"
  [[ "${hash}" =~ ^[0-9a-f]{64}$ && "${name}" == "${archive}" ]] || fail "invalid runtime archive checksum"
  [[ "$(sha256sum "${temporary}/${archive}" | awk '{print $1}')" == "${hash}" ]] || fail "runtime archive checksum mismatch"
  # The packager emits regular files only, with no directory/link entries.
  tar -tzf "${temporary}/${archive}" >"${temporary}/entries"
  while IFS= read -r entry; do
    runtime_safe_path "${entry}" || fail "unsafe runtime archive path"
  done <"${temporary}/entries"
  [[ -z "$(sort "${temporary}/entries" | uniq -d)" ]] || fail "duplicate runtime archive path"
  tar -tvzf "${temporary}/${archive}" | awk 'substr($0,1,1) != "-" {exit 1}' || fail "runtime archive contains non-regular files"
  mkdir -m 0700 "${temporary}/payload"
  (umask 022; tar -xzf "${temporary}/${archive}" --no-same-owner --no-same-permissions -C "${temporary}/payload")
  runtime_identity "${temporary}/payload" "${tag}" "${commit}"
  mv -T -- "${temporary}/payload" "${target}"
)
