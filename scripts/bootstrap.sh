#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS="${ROOT}/.tools"
CACHE="${ROOT}/.cache/downloads"
CHECKSUMS="${ROOT}/scripts/checksums.txt"
PROFILE="${1:-}"

if (($# > 1)); then
  echo "usage: $0 [all|source-style|contracts|go-test|go-security|native-packages|image-security|rust-basic|rust-security|node|web|npm-security]" >&2
  exit 2
fi

if [[ -z "${PROFILE}" ]]; then
  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    echo "bootstrap profile must be explicit in GitHub Actions" >&2
    exit 2
  fi
  PROFILE="all"
fi

case "${PROFILE}" in
  all | source-style | contracts | go-test | go-security | native-packages | image-security | rust-basic | rust-security | node | web | npm-security) ;;
  *)
    echo "unsupported bootstrap profile: ${PROFILE}" >&2
    exit 2
    ;;
esac

version() {
  sed -n "s/^$1=//p" "${ROOT}/toolchains.lock"
}

checksum() {
  awk -v artifact="$1" '$2 == artifact { print $1 }' "${CHECKSUMS}"
}

verify_sha256() {
  local file="$1" expected="$2" actual
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "${file}" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "${file}" | awk '{print $1}')"
  fi
  [[ -n "${expected}" && "${actual}" == "${expected}" ]] || {
    echo "checksum mismatch for ${file}" >&2
    return 1
  }
}

download() {
  local url="$1" artifact="$2" destination="${CACHE}/$2" expected
  expected="$(checksum "${artifact}")"
  [[ "${expected}" =~ ^[a-fA-F0-9]{64}$ ]] || {
    echo "missing or invalid SHA-256 checksum for ${artifact}" >&2
    return 1
  }
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo 'required host command is missing: sha256sum or shasum' >&2
    return 1
  fi
  if [[ ! -f "${destination}" ]]; then
    verify_host_command curl
    curl --fail --location --retry 3 --output "${destination}.tmp" "${url}" || return 1
    verify_sha256 "${destination}.tmp" "${expected}" || return 1
    mv "${destination}.tmp" "${destination}" || return 1
  else
    # Command substitutions do not reliably inherit errexit. Never return a path
    # after failed verification, even when the caller assigns download's output.
    verify_sha256 "${destination}" "${expected}" || return 1
  fi
  printf '%s\n' "${destination}"
}

version_output_matches() {
  local mode="$1" expected="$2" executable="$3" output
  shift 3
  [[ -x "${executable}" ]] || return 1
  output="$("$@" 2>&1)" || return 1
  case "${mode}" in
    contains) [[ "${output}" == *"${expected}"* ]] ;;
    equals) [[ "${output}" == "${expected}" ]] ;;
    *) return 2 ;;
  esac
}

version_output_contains() {
  local expected="$1"
  shift
  version_output_matches contains "${expected}" "$@"
}

version_output_equals() {
  local expected="$1"
  shift
  version_output_matches equals "${expected}" "$@"
}

install_tar_binary() {
  local match_mode="$1" expected="$2" dest="$3" version_arg="$4" url="$5" artifact="$6" member="$7" strip="${8:-0}"
  local archive
  local -a tar_args=(-xzf)
  if ! version_output_matches "${match_mode}" "${expected}" "${dest}" "${dest}" "${version_arg}"; then
    archive="$(download "${url}" "${artifact}")"
    tar_args+=("${archive}")
    (( strip == 0 )) || tar_args+=("--strip-components=${strip}")
    tar "${tar_args[@]}" -C "$(dirname -- "${dest}")" "${member}"
    chmod 0755 "${dest}"
  fi
  version_output_matches "${match_mode}" "${expected}" "${dest}" "${dest}" "${version_arg}"
}

platform="$(uname -s)-$(uname -m)"
case "${platform}" in
  Darwin-arm64)
    go_platform="darwin-arm64"
    node_platform="darwin-arm64"
    rust_platform="aarch64-apple-darwin"
    ruff_platform="aarch64-apple-darwin"
    buf_platform="Darwin-arm64"
    oasdiff_platform="darwin_all"
    cargo_audit_platform="aarch64-apple-darwin"
    cargo_deny_platform="aarch64-apple-darwin"
    nfpm_platform="Darwin_arm64"
    syft_platform="darwin_arm64"
    grype_platform="darwin_arm64"
    ;;
  Linux-x86_64)
    go_platform="linux-amd64"
    node_platform="linux-x64"
    rust_platform="x86_64-unknown-linux-gnu"
    ruff_platform="x86_64-unknown-linux-gnu"
    buf_platform="Linux-x86_64"
    oasdiff_platform="linux_amd64"
    cargo_audit_platform="x86_64-unknown-linux-gnu"
    cargo_deny_platform="x86_64-unknown-linux-musl"
    nfpm_platform="Linux_x86_64"
    syft_platform="linux_amd64"
    grype_platform="linux_amd64"
    ;;
  Linux-aarch64)
    go_platform="linux-arm64"
    node_platform="linux-arm64"
    rust_platform="aarch64-unknown-linux-gnu"
    ruff_platform="aarch64-unknown-linux-gnu"
    buf_platform="Linux-aarch64"
    oasdiff_platform="linux_arm64"
    cargo_audit_platform="aarch64-unknown-linux-gnu"
    cargo_deny_platform="aarch64-unknown-linux-musl"
    nfpm_platform="Linux_arm64"
    syft_platform="linux_arm64"
    grype_platform="linux_arm64"
    ;;
  *)
    echo "unsupported bootstrap platform/profile: ${platform}/${PROFILE}; no artifact mappings" >&2
    exit 1
    ;;
esac

go_toolchain_matches() {
  local executable="$1" expected_version="$2" identity
  [[ -x "${executable}" ]] || return 1
  identity="$("${executable}" env GOVERSION GOHOSTOS GOHOSTARCH)" || return 1
  [[ "${identity}" == "$(printf 'go%s\n%s\n%s' "${expected_version}" "${go_platform%-*}" "${go_platform#*-}")" ]]
}

install_go() (
  local go_version go_artifact archive staging
  go_version="$(version go)"
  go_artifact="go${go_version}.${go_platform}.tar.gz"
  if go_toolchain_matches "${TOOLS}/go/bin/go" "${go_version}"; then
    return 0
  fi
  verify_host_command tar
  verify_host_command gzip
  archive="$(download "https://go.dev/dl/${go_artifact}" "${go_artifact}")"
  staging="$(mktemp -d "${TOOLS}/.go-install-XXXXXX")"
  trap 'rm -rf "${staging}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  tar -xzf "${archive}" -C "${staging}"
  go_toolchain_matches "${staging}/go/bin/go" "${go_version}" || {
    echo "invalid Go toolchain: expected go${go_version} host ${go_platform}" >&2
    exit 1
  }
  rm -rf "${TOOLS}/go"
  mv "${staging}/go" "${TOOLS}/go"
)

install_node() {
  local node_version node_artifact archive
  node_version="$(version node)"
  node_artifact="node-v${node_version}-${node_platform}.tar.xz"
  if ! version_output_equals "v${node_version}" "${TOOLS}/node/bin/node" \
    "${TOOLS}/node/bin/node" --version; then
    archive="$(download "https://nodejs.org/dist/v${node_version}/${node_artifact}" "${node_artifact}")"
    rm -rf "${TOOLS}/node"
    mkdir -p "${TOOLS}/node"
    tar -xJf "${archive}" --strip-components=1 -C "${TOOLS}/node"
  fi
  [[ "$("${TOOLS}/node/bin/node" --version)" == "v${node_version}" ]]
}

install_npm() {
  if [[ "$(npm --version)" != "$(version npm)" ]]; then
    npm install --global --ignore-scripts "npm@$(version npm)"
  fi
  [[ "$(npm --version)" == "$(version npm)" ]]
}

install_rust() {
  local rustup_artifact rustup_init
  rustup_artifact="rustup-init-${rust_platform}"
  if ! RUSTUP_HOME="${TOOLS}/rustup" CARGO_HOME="${TOOLS}/cargo" \
    version_output_contains "rustc $(version rust) " "${TOOLS}/cargo/bin/rustc" \
    "${TOOLS}/cargo/bin/rustc" --version || \
    [[ ! -x "${TOOLS}/cargo/bin/cargo" || ! -x "${TOOLS}/cargo/bin/rustup" ]]; then
    rustup_init="$(download "https://static.rust-lang.org/rustup/archive/1.29.0/${rust_platform}/rustup-init" "${rustup_artifact}")"
    chmod +x "${rustup_init}"
    rm -rf "${TOOLS}/rustup" "${TOOLS}/cargo"
    RUSTUP_HOME="${TOOLS}/rustup" CARGO_HOME="${TOOLS}/cargo" \
      "${rustup_init}" -y --no-modify-path --profile minimal \
      --default-toolchain "$(version rust)"
  fi
  # Resolve the workspace manifest once before parallel Cargo consumers start.
  (
    cd "${ROOT}/rust"
    rustup toolchain install --no-update --no-self-update
    [[ "$(rustc --version | awk '{print $2}')" == "$(version rust)" ]]
    cargo --version >/dev/null
  )
}

install_buf() {
  local artifact binary
  artifact="buf-${buf_platform}"
  if ! version_output_equals "$(version buf)" "${TOOLS}/bin/buf" \
    "${TOOLS}/bin/buf" --version; then
    binary="$(download "https://github.com/bufbuild/buf/releases/download/v$(version buf)/${artifact}" "${artifact}")"
    install -m 0755 "${binary}" "${TOOLS}/bin/buf"
  fi
  [[ "$(buf --version)" == "$(version buf)" ]]
}

install_ruff() {
  install_tar_binary equals "ruff $(version ruff)" "${TOOLS}/bin/ruff" --version \
    "https://github.com/astral-sh/ruff/releases/download/$(version ruff)/ruff-${ruff_platform}.tar.gz" \
    "ruff-$(version ruff)-${ruff_platform}.tar.gz" \
    "ruff-${ruff_platform}/ruff" 1
}

install_syft() {
  install_tar_binary contains "$(version syft)" "${TOOLS}/bin/syft" --version \
    "https://github.com/anchore/syft/releases/download/v$(version syft)/syft_$(version syft)_${syft_platform}.tar.gz" \
    "syft_$(version syft)_${syft_platform}.tar.gz" \
    syft
}

install_grype() {
  install_tar_binary contains "$(version grype)" "${TOOLS}/bin/grype" --version \
    "https://github.com/anchore/grype/releases/download/v$(version grype)/grype_$(version grype)_${grype_platform}.tar.gz" \
    "grype_$(version grype)_${grype_platform}.tar.gz" \
    grype
}

install_oasdiff() {
  install_tar_binary contains "$(version oasdiff)" "${TOOLS}/bin/oasdiff" --version \
    "https://github.com/Tufin/oasdiff/releases/download/v$(version oasdiff)/oasdiff_$(version oasdiff)_${oasdiff_platform}.tar.gz" \
    "oasdiff_$(version oasdiff)_${oasdiff_platform}.tar.gz" \
    oasdiff
}

install_govulncheck() {
  if ! version_output_contains "v$(version govulncheck)" "${TOOLS}/bin/govulncheck" \
    "${TOOLS}/bin/govulncheck" -version; then
    GOBIN="${TOOLS}/bin" go install \
      "golang.org/x/vuln/cmd/govulncheck@v$(version govulncheck)"
  fi
  [[ "$(govulncheck -version 2>&1)" == *"v$(version govulncheck)"* ]]
}

install_cargo_audit() {
  local v
  v="$(version cargo_audit)"
  install_tar_binary contains "${v}" "${TOOLS}/cargo/bin/cargo-audit" --version \
    "https://github.com/rustsec/rustsec/releases/download/cargo-audit%2Fv${v}/cargo-audit-${cargo_audit_platform}-v${v}.tgz" \
    "cargo-audit-${cargo_audit_platform}-v${v}.tgz" \
    "cargo-audit-${cargo_audit_platform}-v${v}/cargo-audit" 1
}

install_cargo_deny() {
  local v
  v="$(version cargo_deny)"
  install_tar_binary contains "${v}" "${TOOLS}/cargo/bin/cargo-deny" --version \
    "https://github.com/EmbarkStudios/cargo-deny/releases/download/${v}/cargo-deny-${v}-${cargo_deny_platform}.tar.gz" \
    "cargo-deny-${v}-${cargo_deny_platform}.tar.gz" \
    "cargo-deny-${v}-${cargo_deny_platform}/cargo-deny" 1
}

install_nfpm() {
  local artifact archive
  artifact="nfpm_$(version nfpm)_${nfpm_platform}.tar.gz"
  # nfpm --version prints a multi-line banner; the pinned version is the
  # GitVersion field.
  if ! nfpm --version 2>/dev/null | awk '$1 == "GitVersion:" { print $2 }' \
    | grep -Fxq "$(version nfpm)" || [[ ! -x "${TOOLS}/bin/nfpm" ]]; then
    archive="$(download "https://github.com/goreleaser/nfpm/releases/download/v$(version nfpm)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" -C "${TOOLS}/bin" nfpm
    chmod 0755 "${TOOLS}/bin/nfpm"
  fi
  [[ "$(nfpm --version 2>/dev/null | awk '$1 == "GitVersion:" { print $2 }')" == "$(version nfpm)" ]]
}

install_contract_tools() {
  install_buf
  install_oasdiff
}

install_rust_quality_tools() {
  install_cargo_audit
  install_cargo_deny
}

install_web_dependencies() {
  (cd "${ROOT}/web" && npm ci)
}

verify_host_command() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "required host command is missing: $1" >&2
    exit 1
  }
}

# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "${ROOT}/scripts/env.sh"

mkdir -p "${TOOLS}/bin" "${CACHE}"

case "${PROFILE}" in
  all)
    install_go
    install_node
    install_npm
    install_rust
    install_contract_tools
    install_govulncheck
    install_rust_quality_tools
    verify_host_command jq
    verify_host_command shellcheck
    install_ruff
    install_web_dependencies
    ;;
  source-style)
    verify_host_command shellcheck
    verify_host_command python3
    install_ruff
    ;;
  contracts)
    install_node
    install_npm
    install_contract_tools
    install_web_dependencies
    ;;
  go-test)
    install_go
    verify_host_command jq
    ;;
  go-security)
    install_go
    install_govulncheck
    ;;
  rust-basic)
    install_rust
    ;;
  rust-security)
    install_rust
    install_rust_quality_tools
    ;;
  native-packages)
    install_rust
    install_nfpm
    ;;
  image-security)
    install_syft
    install_grype
    verify_host_command jq
    ;;
  node)
    install_node
    ;;
  web)
    install_node
    install_npm
    install_web_dependencies
    ;;
  npm-security)
    install_node
    install_npm
    ;;
esac
