#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS="${ROOT}/.tools"
CACHE="${ROOT}/.cache/downloads"
CHECKSUMS="${ROOT}/scripts/checksums.txt"

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
  [[ "${actual}" == "${expected}" ]] || {
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
  [[ -x "${executable}" ]] || return 1
  output="$("$@" 2>&1)" || return 1
  [[ "${output}" == *"${expected}"* ]]
}

version_output_equals() {
  local expected="$1"
  shift
  local executable="$1" output
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
    ;;
  *)
    echo "unsupported bootstrap platform: $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

go_version="$(version go)"
go_artifact="go${go_version}.${go_platform}.tar.gz"
if ! version_output_contains "go version go${go_version} " "${TOOLS}/go/bin/go" "${TOOLS}/go/bin/go" version; then
  archive="$(download "https://go.dev/dl/${go_artifact}" "${go_artifact}")"
  rm -rf "${TOOLS}/go"
  tar -xzf "${archive}" -C "${TOOLS}"
fi

node_version="$(version node)"
node_artifact="node-v${node_version}-${node_platform}.tar.xz"
if ! version_output_equals "v${node_version}" "${TOOLS}/node/bin/node" "${TOOLS}/node/bin/node" --version; then
  archive="$(download "https://nodejs.org/dist/v${node_version}/${node_artifact}" "${node_artifact}")"
  rm -rf "${TOOLS}/node"
  mkdir -p "${TOOLS}/node"
  tar -xJf "${archive}" --strip-components=1 -C "${TOOLS}/node"
fi

rustup_artifact="rustup-init-${rust_platform}"
if ! RUSTUP_HOME="${TOOLS}/rustup" CARGO_HOME="${TOOLS}/cargo" \
  version_output_contains "rustc $(version rust) " "${TOOLS}/cargo/bin/rustc" \
  "${TOOLS}/cargo/bin/rustc" --version; then
  rustup_init="$(download "https://static.rust-lang.org/rustup/archive/1.29.0/${rust_platform}/rustup-init" "${rustup_artifact}")"
  chmod +x "${rustup_init}"
  rm -rf "${TOOLS}/rustup" "${TOOLS}/cargo"
  RUSTUP_HOME="${TOOLS}/rustup" CARGO_HOME="${TOOLS}/cargo" \
    "${rustup_init}" -y --no-modify-path --profile minimal \
    --default-toolchain "$(version rust)" --component clippy,rustfmt
fi

buf_artifact="buf-${buf_platform}"
if ! version_output_equals "$(version buf)" "${TOOLS}/bin/buf" "${TOOLS}/bin/buf" --version; then
  buf_binary="$(download "https://github.com/bufbuild/buf/releases/download/v$(version buf)/${buf_artifact}" "${buf_artifact}")"
  install -m 0755 "${buf_binary}" "${TOOLS}/bin/buf"
fi

protoc_artifact="protoc-$(version protobuf)-${protoc_platform}.zip"
if ! version_output_equals "libprotoc $(version protobuf)" "${TOOLS}/protoc/bin/protoc" \
  "${TOOLS}/protoc/bin/protoc" --version; then
  archive="$(download "https://github.com/protocolbuffers/protobuf/releases/download/v$(version protobuf)/${protoc_artifact}" "${protoc_artifact}")"
  rm -rf "${TOOLS}/protoc"
  mkdir -p "${TOOLS}/protoc"
  unzip -q "${archive}" -d "${TOOLS}/protoc"
fi

openapi_artifact="openapi-generator-cli-$(version openapi_generator).jar"
if [[ ! -f "${TOOLS}/${openapi_artifact}" ]]; then
  jar="$(download "https://github.com/OpenAPITools/openapi-generator/releases/download/v$(version openapi_generator)/${openapi_artifact}" "${openapi_artifact}")"
  install -m 0644 "${jar}" "${TOOLS}/${openapi_artifact}"
fi

gitleaks_artifact="gitleaks_$(version gitleaks)_${gitleaks_platform}.tar.gz"
if ! version_output_equals "$(version gitleaks)" "${TOOLS}/bin/gitleaks" \
  "${TOOLS}/bin/gitleaks" version; then
  archive="$(download "https://github.com/gitleaks/gitleaks/releases/download/v$(version gitleaks)/${gitleaks_artifact}" "${gitleaks_artifact}")"
  tar -xzf "${archive}" -C "${TOOLS}/bin" gitleaks
  chmod 0755 "${TOOLS}/bin/gitleaks"
fi

oasdiff_artifact="oasdiff_$(version oasdiff)_${oasdiff_platform}.tar.gz"
if ! version_output_contains "$(version oasdiff)" "${TOOLS}/bin/oasdiff" \
  "${TOOLS}/bin/oasdiff" --version; then
  archive="$(download "https://github.com/Tufin/oasdiff/releases/download/v$(version oasdiff)/${oasdiff_artifact}" "${oasdiff_artifact}")"
  tar -xzf "${archive}" -C "${TOOLS}/bin" oasdiff
  chmod 0755 "${TOOLS}/bin/oasdiff"
fi

staticcheck_artifact="staticcheck_${staticcheck_platform}.tar.gz"
if ! version_output_contains "$(version staticcheck)" "${TOOLS}/bin/staticcheck" \
  "${TOOLS}/bin/staticcheck" -version; then
  archive="$(download "https://github.com/dominikh/go-tools/releases/download/$(version staticcheck)/${staticcheck_artifact}" "${staticcheck_artifact}")"
  tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/bin" staticcheck/staticcheck
  chmod 0755 "${TOOLS}/bin/staticcheck"
fi

cargo_audit_artifact="cargo-audit-${cargo_audit_platform}-v$(version cargo_audit).tgz"
if ! version_output_contains "$(version cargo_audit)" "${TOOLS}/cargo/bin/cargo-audit" \
  "${TOOLS}/cargo/bin/cargo-audit" --version; then
  archive="$(download "https://github.com/rustsec/rustsec/releases/download/cargo-audit%2Fv$(version cargo_audit)/${cargo_audit_artifact}" "${cargo_audit_artifact}")"
  tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/cargo/bin" \
    "cargo-audit-${cargo_audit_platform}-v$(version cargo_audit)/cargo-audit"
  chmod 0755 "${TOOLS}/cargo/bin/cargo-audit"
fi

cargo_deny_artifact="cargo-deny-$(version cargo_deny)-${cargo_deny_platform}.tar.gz"
if ! version_output_contains "$(version cargo_deny)" "${TOOLS}/cargo/bin/cargo-deny" \
  "${TOOLS}/cargo/bin/cargo-deny" --version; then
  archive="$(download "https://github.com/EmbarkStudios/cargo-deny/releases/download/$(version cargo_deny)/${cargo_deny_artifact}" "${cargo_deny_artifact}")"
  tar -xzf "${archive}" --strip-components=1 -C "${TOOLS}/cargo/bin" \
    "cargo-deny-$(version cargo_deny)-${cargo_deny_platform}/cargo-deny"
  chmod 0755 "${TOOLS}/cargo/bin/cargo-deny"
fi

# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
if [[ "$(npm --version)" != "$(version npm)" ]]; then
  npm install --global --ignore-scripts "npm@$(version npm)"
fi
[[ "$(go env GOVERSION)" == "go${go_version}" ]]
[[ "$(node --version)" == "v${node_version}" ]]
[[ "$(npm --version)" == "$(version npm)" ]]
[[ "$(rustc --version | awk '{print $2}')" == "$(version rust)" ]]
[[ "$(buf --version)" == "$(version buf)" ]]
[[ "$(protoc --version)" == "libprotoc $(version protobuf)" ]]
[[ "$(gitleaks version)" == "$(version gitleaks)" ]]
[[ "$(oasdiff --version)" == *"$(version oasdiff)"* ]]
[[ "$(staticcheck -version)" == *"$(version staticcheck)"* ]]
[[ "$(cargo audit --version)" == *"$(version cargo_audit)"* ]]
[[ "$(cargo deny --version)" == *"$(version cargo_deny)"* ]]

if ! version_output_contains "v$(version govulncheck)" "${TOOLS}/bin/govulncheck" \
  "${TOOLS}/bin/govulncheck" -version; then
  GOBIN="${TOOLS}/bin" go install \
    "golang.org/x/vuln/cmd/govulncheck@v$(version govulncheck)"
fi
[[ "$(govulncheck -version 2>&1)" == *"v$(version govulncheck)"* ]]

for command_name in java jq shellcheck; do
  command -v "${command_name}" >/dev/null 2>&1 || {
    echo "required host command is missing: ${command_name}" >&2
    exit 1
  }
done
java -version >/dev/null 2>&1 || {
  echo "Java 17 or newer is required to run OpenAPI Generator" >&2
  exit 1
}

(cd "${ROOT}/web" && npm ci)
