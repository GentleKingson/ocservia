#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS="${ROOT}/.tools"
CACHE="${ROOT}/.cache/downloads"
CHECKSUMS="${ROOT}/scripts/checksums.txt"
PROFILE="${1:-}"

if (($# > 1)); then
  echo "usage: $0 [all|source-style|contracts|secret-scan|go-test|go-security|native-packages|image-security|rust-basic|rust-security|node|web|npm-security]" >&2
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
  all | source-style | contracts | secret-scan | go-test | go-security | native-packages | image-security | rust-basic | rust-security | node | web | npm-security) ;;
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

version_output_contains() {
  local expected="$1"
  shift
  local executable="$1" output
  shift
  [[ -x "${executable}" ]] || return 1
  output="$("$@" 2>&1)" || return 1
  [[ "${output}" == *"${expected}"* ]]
}

version_output_equals() {
  local expected="$1"
  shift
  local executable="$1" output
  shift
  [[ -x "${executable}" ]] || return 1
  output="$("$@" 2>&1)" || return 1
  [[ "${output}" == "${expected}" ]]
}

platform="$(uname -s)-$(uname -m)"
case "${platform}" in
  Darwin-arm64)
    go_platform="darwin-arm64"
    node_platform="darwin-arm64"
    rust_platform="aarch64-apple-darwin"
    ruff_platform="aarch64-apple-darwin"
    buf_platform="Darwin-arm64"
    gitleaks_platform="darwin_arm64"
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
    gitleaks_platform="linux_x64"
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
    gitleaks_platform="linux_arm64"
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
  local artifact archive
  artifact="ruff-$(version ruff)-${ruff_platform}.tar.gz"
  if ! version_output_equals "ruff $(version ruff)" "${TOOLS}/bin/ruff" \
    "${TOOLS}/bin/ruff" --version; then
    archive="$(download "https://github.com/astral-sh/ruff/releases/download/$(version ruff)/ruff-${ruff_platform}.tar.gz" "${artifact}")"
    tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/bin" "ruff-${ruff_platform}/ruff"
    chmod 0755 "${TOOLS}/bin/ruff"
  fi
  [[ "$(ruff --version)" == "ruff $(version ruff)" ]]
}

install_gitleaks() {
  local artifact archive
  artifact="gitleaks_$(version gitleaks)_${gitleaks_platform}.tar.gz"
  if ! version_output_equals "$(version gitleaks)" "${TOOLS}/bin/gitleaks" \
    "${TOOLS}/bin/gitleaks" version; then
    archive="$(download "https://github.com/gitleaks/gitleaks/releases/download/v$(version gitleaks)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" -C "${TOOLS}/bin" gitleaks
    chmod 0755 "${TOOLS}/bin/gitleaks"
  fi
  [[ "$(gitleaks version)" == "$(version gitleaks)" ]]
}

install_syft() {
  local artifact archive
  artifact="syft_$(version syft)_${syft_platform}.tar.gz"
  if ! version_output_contains "$(version syft)" "${TOOLS}/bin/syft" \
    "${TOOLS}/bin/syft" --version; then
    archive="$(download "https://github.com/anchore/syft/releases/download/v$(version syft)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" -C "${TOOLS}/bin" syft
    chmod 0755 "${TOOLS}/bin/syft"
  fi
  [[ "$("${TOOLS}/bin/syft" --version)" == *"$(version syft)"* ]]
}

install_grype() {
  local artifact archive
  artifact="grype_$(version grype)_${grype_platform}.tar.gz"
  if ! version_output_contains "$(version grype)" "${TOOLS}/bin/grype" \
    "${TOOLS}/bin/grype" --version; then
    archive="$(download "https://github.com/anchore/grype/releases/download/v$(version grype)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" -C "${TOOLS}/bin" grype
    chmod 0755 "${TOOLS}/bin/grype"
  fi
  [[ "$("${TOOLS}/bin/grype" --version)" == *"$(version grype)"* ]]
}

install_oasdiff() {
  local artifact archive
  artifact="oasdiff_$(version oasdiff)_${oasdiff_platform}.tar.gz"
  if ! version_output_contains "$(version oasdiff)" "${TOOLS}/bin/oasdiff" \
    "${TOOLS}/bin/oasdiff" --version; then
    archive="$(download "https://github.com/Tufin/oasdiff/releases/download/v$(version oasdiff)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" -C "${TOOLS}/bin" oasdiff
    chmod 0755 "${TOOLS}/bin/oasdiff"
  fi
  [[ "$(oasdiff --version)" == *"$(version oasdiff)"* ]]
}

install_govulncheck() {
  if ! version_output_contains "v$(version govulncheck)" "${TOOLS}/bin/govulncheck" \
    "${TOOLS}/bin/govulncheck" -version; then
    GOBIN="${TOOLS}/bin" go install \
      "golang.org/x/vuln/cmd/govulncheck@v$(version govulncheck)"
  fi
  [[ "$(govulncheck -version 2>&1)" == *"v$(version govulncheck)"* ]]
}

install_go_licenses() {
  local expected actual
  expected="v$(version go_licenses)"
  actual="$(go version -m "${TOOLS}/bin/go-licenses" 2>/dev/null | awk '$1 == "mod" { print $3 }' || true)"
  if [[ "${actual}" != "${expected}" ]]; then
    GOBIN="${TOOLS}/bin" go install \
      "github.com/google/go-licenses/v2@${expected}"
  fi
  actual="$(go version -m "${TOOLS}/bin/go-licenses" 2>/dev/null | awk '$1 == "mod" { print $3 }' || true)"
  [[ "${actual}" == "${expected}" ]]
}

install_cargo_audit() {
  local artifact archive
  artifact="cargo-audit-${cargo_audit_platform}-v$(version cargo_audit).tgz"
  if ! version_output_contains "$(version cargo_audit)" "${TOOLS}/cargo/bin/cargo-audit" \
    "${TOOLS}/cargo/bin/cargo-audit" --version; then
    archive="$(download "https://github.com/rustsec/rustsec/releases/download/cargo-audit%2Fv$(version cargo_audit)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/cargo/bin" \
      "cargo-audit-${cargo_audit_platform}-v$(version cargo_audit)/cargo-audit"
    chmod 0755 "${TOOLS}/cargo/bin/cargo-audit"
  fi
  [[ "$(cargo audit --version)" == *"$(version cargo_audit)"* ]]
}

install_cargo_deny() {
  local artifact archive
  artifact="cargo-deny-$(version cargo_deny)-${cargo_deny_platform}.tar.gz"
  if ! version_output_contains "$(version cargo_deny)" "${TOOLS}/cargo/bin/cargo-deny" \
    "${TOOLS}/cargo/bin/cargo-deny" --version; then
    archive="$(download "https://github.com/EmbarkStudios/cargo-deny/releases/download/$(version cargo_deny)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/cargo/bin" \
      "cargo-deny-$(version cargo_deny)-${cargo_deny_platform}/cargo-deny"
    chmod 0755 "${TOOLS}/cargo/bin/cargo-deny"
  fi
  [[ "$(cargo deny --version)" == *"$(version cargo_deny)"* ]]
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

install_go_quality_tools() {
  install_govulncheck
  install_go_licenses
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
    install_gitleaks
    install_go_quality_tools
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
  secret-scan)
    install_gitleaks
    verify_host_command jq
    verify_host_command openssl
    ;;
  go-test)
    install_go
    verify_host_command jq
    ;;
  go-security)
    install_go
    install_govulncheck
    install_go_licenses
    ;;
  rust-basic)
    install_rust
    ;;
  rust-security)
    install_rust
    install_rust_quality_tools
    install_grype
    verify_host_command jq
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
