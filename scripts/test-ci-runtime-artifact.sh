#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Source the script under test so the build path can be exercised with the
# build steps overridden; its case dispatch is guarded against sourcing.
# shellcheck source=scripts/ci-runtime-artifact.sh
source "${ROOT}/scripts/ci-runtime-artifact.sh"

TMP_ROOT="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ocservia-runtime-artifact-test-XXXXXX")"
COMMIT=1111111111111111111111111111111111111111

cleanup() {
  rm -rf -- "${TMP_ROOT}"
}
trap cleanup EXIT INT TERM

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

make_archive() {
  local archive=$1
  rm -rf -- "${TMP_ROOT}/payload"
  mkdir -p "${TMP_ROOT}/payload"
  printf '#!/usr/bin/env sh\nexit 0\n' >"${TMP_ROOT}/payload/ocserv-control"
  printf '#!/usr/bin/env sh\nexit 0\n' >"${TMP_ROOT}/payload/ocservia-transportd-stub"
  chmod 0755 "${TMP_ROOT}/payload/ocserv-control" "${TMP_ROOT}/payload/ocservia-transportd-stub"
  printf 'candidate_sha\t%s\n' "${COMMIT}" >"${TMP_ROOT}/payload/manifest.tsv"
  printf 'ocserv-control\t%s\n' "$(sha256_file "${TMP_ROOT}/payload/ocserv-control")" \
    >>"${TMP_ROOT}/payload/manifest.tsv"
  printf 'ocservia-transportd-stub\t%s\n' \
    "$(sha256_file "${TMP_ROOT}/payload/ocservia-transportd-stub")" \
    >>"${TMP_ROOT}/payload/manifest.tsv"
  tar -czf "${archive}" -C "${TMP_ROOT}/payload" \
    manifest.tsv ocserv-control ocservia-transportd-stub
}

make_archive "${TMP_ROOT}/valid.tar.gz"
"${ROOT}/scripts/ci-runtime-artifact.sh" extract \
  "${TMP_ROOT}/valid.tar.gz" "${TMP_ROOT}/valid" "${COMMIT}"
"${TMP_ROOT}/valid/ocserv-control"
"${TMP_ROOT}/valid/ocservia-transportd-stub"

if "${ROOT}/scripts/ci-runtime-artifact.sh" extract \
  "${TMP_ROOT}/valid.tar.gz" "${TMP_ROOT}/wrong-commit" \
  2222222222222222222222222222222222222222 >/dev/null 2>&1; then
  echo "runtime artifact accepted the wrong candidate commit" >&2
  exit 1
fi

printf 'tampered\n' >>"${TMP_ROOT}/payload/ocserv-control"
tar -czf "${TMP_ROOT}/tampered.tar.gz" -C "${TMP_ROOT}/payload" \
  manifest.tsv ocserv-control ocservia-transportd-stub
if "${ROOT}/scripts/ci-runtime-artifact.sh" extract \
  "${TMP_ROOT}/tampered.tar.gz" "${TMP_ROOT}/tampered" "${COMMIT}" >/dev/null 2>&1; then
  echo "runtime artifact accepted a checksum mismatch" >&2
  exit 1
fi

make_archive "${TMP_ROOT}/unexpected.tar.gz"
printf 'unexpected\n' >"${TMP_ROOT}/payload/unexpected"
tar -czf "${TMP_ROOT}/unexpected.tar.gz" -C "${TMP_ROOT}/payload" \
  manifest.tsv ocserv-control ocservia-transportd-stub unexpected
if "${ROOT}/scripts/ci-runtime-artifact.sh" extract \
  "${TMP_ROOT}/unexpected.tar.gz" "${TMP_ROOT}/unexpected" "${COMMIT}" >/dev/null 2>&1; then
  echo "runtime artifact accepted an unexpected archive member" >&2
  exit 1
fi

# Build-path scenarios run in subshells so the TMP_ROOT global reassigned by
# build_artifact cannot clobber this script's workspace; output paths are
# captured first, and the trap removes each build scratch directory on exit.
runtime_build_go() {
  printf '#!/usr/bin/env sh\nexit 0\n' >"$1/ocserv-control"
}
runtime_build_stub() {
  printf '#!/usr/bin/env sh\nexit 0\n' >"$1/ocservia-transportd-stub"
}
(
  trap cleanup EXIT INT TERM
  out="${TMP_ROOT}/build-ok"
  build_artifact "${out}" "${COMMIT}"
)
extract_artifact "${TMP_ROOT}/build-ok/runtime-artifacts.tar.gz" \
  "${TMP_ROOT}/build-ok-extracted" "${COMMIT}"
"${TMP_ROOT}/build-ok-extracted/ocserv-control"
"${TMP_ROOT}/build-ok-extracted/ocservia-transportd-stub"

runtime_build_go() {
  return 1
}
(
  trap cleanup EXIT INT TERM
  out="${TMP_ROOT}/build-go-failed"
  if build_artifact "${out}" "${COMMIT}"; then
    echo "build succeeded despite a failed Go build" >&2
    exit 1
  fi
  [[ ! -e "${out}/runtime-artifacts.tar.gz" ]] || {
    echo "build produced an archive despite a failed Go build" >&2
    exit 1
  }
)

runtime_build_go() {
  printf '#!/usr/bin/env sh\nexit 0\n' >"$1/ocserv-control"
}
runtime_build_stub() {
  return 1
}
(
  trap cleanup EXIT INT TERM
  out="${TMP_ROOT}/build-stub-failed"
  if build_artifact "${out}" "${COMMIT}"; then
    echo "build succeeded despite a failed stub build" >&2
    exit 1
  fi
  [[ ! -e "${out}/runtime-artifacts.tar.gz" ]] || {
    echo "build produced an archive despite a failed stub build" >&2
    exit 1
  }
)

echo "CI runtime artifact tests passed"
