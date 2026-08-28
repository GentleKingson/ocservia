#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_ROOT=""

cleanup() {
  if [[ -n "${TMP_ROOT}" ]]; then
    rm -rf -- "${TMP_ROOT}"
  fi
}
trap cleanup EXIT INT TERM

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

validate_commit() {
  [[ "$1" =~ ^[0-9a-f]{40}$ ]] || {
    echo "candidate commit must be a full lowercase Git SHA" >&2
    exit 2
  }
}

runtime_build_go() {
  local output_dir=$1
  (cd "${ROOT}/control-plane" && \
    go build -trimpath -o "${output_dir}/ocserv-control" ./cmd/ocserv-control)
}

runtime_build_stub() {
  local output_dir=$1
  (cd "${ROOT}/rust" && cargo build --locked --package ocservia-transportd-stub)
  install -m 0755 "${ROOT}/rust/target/debug/ocservia-transportd-stub" \
    "${output_dir}/ocservia-transportd-stub"
}

build_artifact() {
  local output_dir=$1 candidate_sha=$2 build_status=0 go_pid stub_pid
  validate_commit "${candidate_sha}"
  mkdir -p "${output_dir}"
  TMP_ROOT="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ocservia-runtime-build-XXXXXX")"

  # shellcheck source=scripts/env.sh
  source "${ROOT}/scripts/env.sh"
  runtime_build_go "${TMP_ROOT}" &
  go_pid=$!
  runtime_build_stub "${TMP_ROOT}" &
  stub_pid=$!
  # Both waits must run so a late failure cannot be masked by set -e exiting
  # before the other build is reaped.
  wait "${go_pid}" || build_status=1
  wait "${stub_pid}" || build_status=1
  if [[ "${build_status}" -ne 0 ]]; then
    echo "runtime artifact build failed" >&2
    return 1
  fi

  printf 'candidate_sha\t%s\n' "${candidate_sha}" >"${TMP_ROOT}/manifest.tsv"
  printf 'ocserv-control\t%s\n' "$(sha256_file "${TMP_ROOT}/ocserv-control")" \
    >>"${TMP_ROOT}/manifest.tsv"
  printf 'ocservia-transportd-stub\t%s\n' \
    "$(sha256_file "${TMP_ROOT}/ocservia-transportd-stub")" >>"${TMP_ROOT}/manifest.tsv"
  tar -czf "${output_dir}/runtime-artifacts.tar.gz" -C "${TMP_ROOT}" \
    manifest.tsv ocserv-control ocservia-transportd-stub
}

extract_artifact() {
  local archive=$1 output_dir=$2 expected_sha=$3 manifest expected actual entries
  validate_commit "${expected_sha}"
  [[ -f "${archive}" ]] || {
    echo "runtime artifact archive does not exist: ${archive}" >&2
    exit 2
  }
  entries="$(tar -tzf "${archive}" | LC_ALL=C sort)"
  [[ "${entries}" == $'manifest.tsv\nocserv-control\nocservia-transportd-stub' ]] || {
    echo "runtime artifact contains an unexpected archive entry" >&2
    exit 1
  }
  rm -rf -- "${output_dir}"
  mkdir -p "${output_dir}"
  tar -xzf "${archive}" -C "${output_dir}"

  manifest="${output_dir}/manifest.tsv"
  [[ -f "${manifest}" && ! -L "${manifest}" ]] || {
    echo "runtime artifact manifest is missing or unsafe" >&2
    exit 1
  }
  [[ "$(wc -l <"${manifest}" | tr -d '[:space:]')" == "3" ]] || {
    echo "runtime artifact manifest has an unexpected schema" >&2
    exit 1
  }
  for expected in ocserv-control ocservia-transportd-stub; do
    [[ -f "${output_dir}/${expected}" && ! -L "${output_dir}/${expected}" ]] || {
      echo "runtime artifact is missing ${expected}" >&2
      exit 1
    }
    actual="$(sha256_file "${output_dir}/${expected}")"
    [[ "$(awk -F '\t' -v name="${expected}" '$1 == name { print $2 }' "${manifest}")" == "${actual}" ]] || {
      echo "runtime artifact checksum mismatch for ${expected}" >&2
      exit 1
    }
    chmod 0755 "${output_dir}/${expected}"
  done
  [[ "$(awk -F '\t' '$1 == "candidate_sha" { print $2 }' "${manifest}")" == "${expected_sha}" ]] || {
    echo "runtime artifact was not built from ${expected_sha}" >&2
    exit 1
  }
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-}" in
    build)
      (($# == 3)) || {
        echo "usage: $0 build OUTPUT_DIR CANDIDATE_SHA" >&2
        exit 2
      }
      build_artifact "$2" "$3"
      ;;
    extract)
      (($# == 4)) || {
        echo "usage: $0 extract ARCHIVE OUTPUT_DIR EXPECTED_SHA" >&2
        exit 2
      }
      extract_artifact "$2" "$3" "$4"
      ;;
    *)
      echo "usage: $0 {build OUTPUT_DIR CANDIDATE_SHA|extract ARCHIVE OUTPUT_DIR EXPECTED_SHA}" >&2
      exit 2
      ;;
  esac
fi
