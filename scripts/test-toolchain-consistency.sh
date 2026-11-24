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
