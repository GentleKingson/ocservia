#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS="${ROOT}/.tools"
CACHE="${ROOT}/.cache/downloads"
CHECKSUMS="${ROOT}/scripts/checksums.txt"
PROFILE="${1:-}"

if (($# > 1)); then
  echo "usage: $0 [all|ci-quality|contracts|go-integration|rust-validation|native|web|security]" >&2
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
  all | ci-quality | contracts | go-integration | rust-validation | native | web | security) ;;
  *)
    echo "unsupported bootstrap profile: ${PROFILE}" >&2
    exit 2
    ;;
esac

mkdir -p "${TOOLS}/bin" "${CACHE}"

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
  local url="$1" artifact="$2" destination="${CACHE}/$2"
  if [[ ! -f "${destination}" ]]; then
    curl --fail --location --retry 3 --output "${destination}.tmp" "${url}"
    mv "${destination}.tmp" "${destination}"
  fi
  verify_sha256 "${destination}" "$(checksum "${artifact}")"
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

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)
    go_platform="darwin-arm64"
    node_platform="darwin-arm64"
    rust_platform="aarch64-apple-darwin"
    buf_platform="Darwin-arm64"
    protoc_platform="osx-aarch_64"
    gitleaks_platform="darwin_arm64"
    oasdiff_platform="darwin_all"
    staticcheck_platform="darwin_arm64"
    cargo_audit_platform="aarch64-apple-darwin"
    cargo_deny_platform="aarch64-apple-darwin"
    sccache_platform="aarch64-apple-darwin"
    ;;
  Linux-x86_64)
    go_platform="linux-amd64"
    node_platform="linux-x64"
    rust_platform="x86_64-unknown-linux-gnu"
    buf_platform="Linux-x86_64"
    protoc_platform="linux-x86_64"
    gitleaks_platform="linux_x64"
    oasdiff_platform="linux_amd64"
    staticcheck_platform="linux_amd64"
    cargo_audit_platform="x86_64-unknown-linux-gnu"
    cargo_deny_platform="x86_64-unknown-linux-musl"
    sccache_platform="x86_64-unknown-linux-musl"
    ;;
  *)
    echo "unsupported bootstrap platform: $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

install_go() {
  local go_version go_artifact archive
  go_version="$(version go)"
  go_artifact="go${go_version}.${go_platform}.tar.gz"
  if ! version_output_contains "go version go${go_version} " "${TOOLS}/go/bin/go" \
    "${TOOLS}/go/bin/go" version; then
    archive="$(download "https://go.dev/dl/${go_artifact}" "${go_artifact}")"
    rm -rf "${TOOLS}/go"
    tar -xzf "${archive}" -C "${TOOLS}"
  fi
  [[ "$("${TOOLS}/go/bin/go" env GOVERSION)" == "go${go_version}" ]]
}

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
  [[ "$(rustc --version | awk '{print $2}')" == "$(version rust)" ]]
  cargo --version >/dev/null
}

install_rust_validation_components() {
  if ! cargo clippy --version >/dev/null 2>&1 || ! rustfmt --version >/dev/null 2>&1; then
    rustup component add --toolchain "$(version rust)" clippy rustfmt
  fi
  cargo clippy --version >/dev/null
  rustfmt --version >/dev/null
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

install_protoc() {
  local artifact archive
  artifact="protoc-$(version protobuf)-${protoc_platform}.zip"
  if ! version_output_equals "libprotoc $(version protobuf)" "${TOOLS}/protoc/bin/protoc" \
    "${TOOLS}/protoc/bin/protoc" --version; then
    archive="$(download "https://github.com/protocolbuffers/protobuf/releases/download/v$(version protobuf)/${artifact}" "${artifact}")"
    rm -rf "${TOOLS}/protoc"
    mkdir -p "${TOOLS}/protoc"
    unzip -q "${archive}" -d "${TOOLS}/protoc"
  fi
  [[ "$(protoc --version)" == "libprotoc $(version protobuf)" ]]
}

install_openapi_generator() {
  local artifact jar
  artifact="openapi-generator-cli-$(version openapi_generator).jar"
  if [[ ! -f "${TOOLS}/${artifact}" ]]; then
    jar="$(download "https://github.com/OpenAPITools/openapi-generator/releases/download/v$(version openapi_generator)/${artifact}" "${artifact}")"
    install -m 0644 "${jar}" "${TOOLS}/${artifact}"
  fi
  verify_sha256 "${TOOLS}/${artifact}" "$(checksum "${artifact}")"
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

install_staticcheck() {
  local artifact archive
  artifact="staticcheck_${staticcheck_platform}.tar.gz"
  if ! version_output_contains "$(version staticcheck)" "${TOOLS}/bin/staticcheck" \
    "${TOOLS}/bin/staticcheck" -version; then
    archive="$(download "https://github.com/dominikh/go-tools/releases/download/$(version staticcheck)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/bin" staticcheck/staticcheck
    chmod 0755 "${TOOLS}/bin/staticcheck"
  fi
  [[ "$(staticcheck -version)" == *"$(version staticcheck)"* ]]
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

install_sccache() {
  local artifact archive directory
  artifact="sccache-v$(version sccache)-${sccache_platform}.tar.gz"
  directory="${artifact%.tar.gz}"
  if ! version_output_contains "$(version sccache)" "${TOOLS}/bin/sccache" \
    "${TOOLS}/bin/sccache" --version; then
    archive="$(download "https://github.com/mozilla/sccache/releases/download/v$(version sccache)/${artifact}" "${artifact}")"
    tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/bin" "${directory}/sccache"
    chmod 0755 "${TOOLS}/bin/sccache"
  fi
  [[ "$(sccache --version)" == *"$(version sccache)"* ]]
}

install_contract_tools() {
  install_buf
  install_openapi_generator
  install_oasdiff
}

install_go_quality_tools() {
  install_staticcheck
  install_govulncheck
}

install_rust_quality_tools() {
  install_rust_validation_components
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

verify_java() {
  verify_host_command java
  java -version >/dev/null 2>&1 || {
    echo "Java 17 or newer is required to run OpenAPI Generator" >&2
    exit 1
  }
}

# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "${ROOT}/scripts/env.sh"

case "${PROFILE}" in
  all)
    install_go
    install_node
    install_npm
    install_rust
    install_contract_tools
    install_protoc
    install_gitleaks
    install_go_quality_tools
    install_rust_quality_tools
    install_sccache
    verify_java
    verify_host_command jq
    verify_host_command shellcheck
    install_web_dependencies
    ;;
  ci-quality)
    install_go
    install_node
    install_npm
    install_rust
    install_contract_tools
    install_gitleaks
    install_rust_quality_tools
    install_sccache
    verify_java
    verify_host_command jq
    verify_host_command shellcheck
    install_web_dependencies
    ;;
  contracts)
    install_node
    install_npm
    install_contract_tools
    verify_java
    install_web_dependencies
    ;;
  go-integration)
    install_go
    install_rust
    install_go_quality_tools
    install_sccache
    verify_host_command jq
    ;;
  rust-validation)
    install_rust
    install_rust_quality_tools
    install_sccache
    ;;
  native)
    install_rust
    install_sccache
    ;;
  web)
    install_node
    install_npm
    install_web_dependencies
    ;;
  security)
    install_go
    install_node
    install_npm
    install_rust
    install_gitleaks
    install_cargo_deny
    install_sccache
    install_web_dependencies
    ;;
esac
