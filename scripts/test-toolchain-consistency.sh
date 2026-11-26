#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="${ROOT}/toolchains.lock"

pinned() {
  sed -n "s/^$1=//p" "${LOCK}"
}

declared() {
  sed -n "s/^$2 //p" "${ROOT}/$1" | head -n 1
}

expect_equal() {
  local tool="$1" expected="$2" actual="$3" source="$4" pin_source="${5:-toolchains.lock}"
  if [[ -z "${actual}" ]]; then
    echo "${source} no longer declares a ${tool} version" >&2
    exit 1
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${source} declares ${tool} ${actual} but ${pin_source} pins ${expected}" >&2
    echo "align ${source} with ${pin_source} so local toolchains match CI" >&2
    exit 1
  fi
}

go_pin="$(pinned go)"
rust_pin="$(pinned rust)"
node_pin="$(pinned node)"
npm_pin="$(pinned npm)"

expect_equal Go "${go_pin}" "$(declared .tool-versions golang)" ".tool-versions"
expect_equal Go "${go_pin}" "$(declared go.work go)" "go.work"
expect_equal Go "${go_pin}" "$(declared control-plane/go.mod go)" "control-plane/go.mod"
expect_equal Go "${go_pin}" "$(declared signer/go.mod go)" "signer/go.mod"

expect_equal Rust "${rust_pin}" "$(declared .tool-versions rust)" ".tool-versions"
expect_equal Rust "${rust_pin}" \
  "$(sed -n 's/^channel = "\(.*\)"$/\1/p' "${ROOT}/rust/rust-toolchain.toml")" \
  "rust/rust-toolchain.toml"

expect_equal Node "${node_pin}" "$(declared .tool-versions nodejs)" ".tool-versions"
expect_equal Node "${node_pin}" "$(head -n 1 "${ROOT}/.nvmrc")" ".nvmrc"
expect_equal Node "${node_pin}" "$(head -n 1 "${ROOT}/.node-version")" ".node-version"
expect_equal Node "${node_pin}" "$(jq -r '.engines.node // empty' "${ROOT}/web/package.json")" "web/package.json"
expect_equal npm "${npm_pin}" "$(jq -r '.engines.npm // empty' "${ROOT}/web/package.json")" "web/package.json"

playwright_pin="$(jq -r '.devDependencies["@playwright/test"] // empty' "${ROOT}/web/package.json")"
playwright_pin="${playwright_pin#^}"
if [[ -z "${playwright_pin}" ]]; then
  echo "web/package.json no longer declares a @playwright/test version" >&2
  exit 1
fi
expect_equal Playwright "${playwright_pin}" \
  "$(sed -n 's/^FROM mcr\.microsoft\.com\/playwright:v\([0-9][0-9.]*\)-noble$/\1/p' "${ROOT}/web/playwright.Dockerfile" | head -n 1)" \
  "web/playwright.Dockerfile" \
  "web/package.json"

iroh_pin="$(pinned iroh)"
pg18_pin="$(pinned postgresql_18)"
expect_equal iroh "${iroh_pin}" \
  "$(sed -n 's/^iroh = { version = "=\([^"]*\)".*/\1/p' "${ROOT}/rust/Cargo.toml" | head -n 1)" \
  "rust/Cargo.toml"
expect_equal iroh-relay "${iroh_pin}" \
  "$(sed -n 's/^iroh-relay = { version = "=\([^"]*\)".*/\1/p' "${ROOT}/rust/Cargo.toml" | head -n 1)" \
  "rust/Cargo.toml"
if ! grep -Eq "^[0-9a-f]{64}  iroh-relay-${iroh_pin}\.crate$" "${ROOT}/scripts/checksums.txt"; then
  echo "scripts/checksums.txt is missing iroh-relay-${iroh_pin}.crate" >&2
  exit 1
fi

pg_ref="$(sed -n 's/^readonly OCSERV_POSTGRES_IMAGE="\([^"]*\)"$/\1/p' "${ROOT}/scripts/release-common.sh" | head -n 1)"
pg_digest="${pg_ref##*@}"
otel_ref="$(sed -n 's/^readonly OCSERV_OTEL_IMAGE="\([^"]*\)"$/\1/p' "${ROOT}/scripts/release-common.sh" | head -n 1)"
otel_digest="${otel_ref##*@}"
[[ "${pg_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid OCSERV_POSTGRES_IMAGE digest" >&2; exit 1; }
[[ "${otel_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid OCSERV_OTEL_IMAGE digest" >&2; exit 1; }

expect_equal "PostgreSQL 18" "postgres:${pg18_pin}-bookworm@${pg_digest}" \
  "$(sed -n 's/^FROM \(postgres:[^ ]*\) AS runtime$/\1/p' "${ROOT}/deploy/production/backup.Dockerfile" | head -n 1)" \
  "deploy/production/backup.Dockerfile" "toolchains.lock + scripts/release-common.sh"
expect_equal "PostgreSQL 18" "postgres:${pg18_pin}-bookworm@${pg_digest}" \
  "$(sed -n 's/^    image: \(postgres:[^ ]*\)$/\1/p' "${ROOT}/deploy/compose/compose.yaml" | head -n 1)" \
  "deploy/compose/compose.yaml" "toolchains.lock + scripts/release-common.sh"
expect_equal "PostgreSQL digest" "${pg_ref}" \
  "$(sed -n "s/^[[:space:]]*'postgres': '\([^']*\)',$/\1/p" "${ROOT}/scripts/publish-release.py" | head -n 1)" \
  "scripts/publish-release.py" "scripts/release-common.sh"

pg_test_pin="$(sed -n "s/^readonly POSTGRES_18_IMAGE='\\([^']*\\)'$/\\1/p" "${ROOT}/scripts/postgres-test-common.sh" | head -n 1)"
expect_equal "PostgreSQL 18 test pin" "postgres:${pg18_pin}-bookworm@${pg_digest}" \
  "${pg_test_pin}" "scripts/postgres-test-common.sh" "toolchains.lock + scripts/release-common.sh"

for script in \
  scripts/database-controller-e2e.sh \
  scripts/database-integration.sh \
  scripts/database-postgres-smoke.sh \
  scripts/development-simulation-integration.sh \
  scripts/external-postgres-backup-restore-smoke.sh \
  scripts/postgres-backup-restore-smoke.sh \
  scripts/test-postgres-volume-layout.sh; do
  if ! grep -Fq 'scripts/postgres-test-common.sh' "${ROOT}/${script}"; then
    echo "${script} does not source scripts/postgres-test-common.sh" >&2
    exit 1
  fi
  if grep -Eq 'postgres:18[^[:space:]]*@sha256:' "${ROOT}/${script}"; then
    echo "${script} must reuse POSTGRES_18_IMAGE from scripts/postgres-test-common.sh instead of pinning inline" >&2
    exit 1
  fi
done

expect_equal "OTel digest" "${otel_ref}" \
  "$(sed -n "s/^[[:space:]]*'otel': '\([^']*\)',$/\1/p" "${ROOT}/scripts/publish-release.py" | head -n 1)" \
  "scripts/publish-release.py" "scripts/release-common.sh"
expect_equal "OTel image" "${otel_digest}" \
  "$(sed -n 's/^    image: otel\/opentelemetry-collector:[^@]*@\([^ ]*\)$/\1/p' "${ROOT}/deploy/compose/compose.yaml" | head -n 1)" \
  "deploy/compose/compose.yaml" "scripts/release-common.sh"

caddy_ref="$(sed -n 's/^FROM \(caddy:[^ ]*\) AS runtime-base$/\1/p' "${ROOT}/deploy/production/gateway.Dockerfile" | head -n 1)"
expect_equal "Gateway Caddy" "${caddy_ref}" \
  "$(sed -n 's/^CADDY = "\([^"]*\)"$/\1/p' "${ROOT}/scripts/test-controller-gateway-headers.py" | head -n 1)" \
  "scripts/test-controller-gateway-headers.py" "deploy/production/gateway.Dockerfile"
