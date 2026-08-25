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
  local tool="$1" expected="$2" actual="$3" source="$4"
  if [[ -z "${actual}" ]]; then
    echo "${source} no longer declares a ${tool} version" >&2
    exit 1
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${source} declares ${tool} ${actual} but toolchains.lock pins ${expected}" >&2
    echo "align ${source} with toolchains.lock so local toolchains match CI" >&2
    exit 1
  fi
}

go_pin="$(pinned go)"
rust_pin="$(pinned rust)"
node_pin="$(pinned node)"

expect_equal Go "${go_pin}" "$(declared .tool-versions golang)" ".tool-versions"
expect_equal Go "${go_pin}" "$(declared go.work go)" "go.work"
expect_equal Go "${go_pin}" "$(declared control-plane/go.mod go)" "control-plane/go.mod"
expect_equal Go "${go_pin}" "$(declared tools/g6-harness/go.mod go)" "tools/g6-harness/go.mod"

expect_equal Rust "${rust_pin}" "$(declared .tool-versions rust)" ".tool-versions"
expect_equal Rust "${rust_pin}" \
  "$(sed -n 's/^channel = "\(.*\)"$/\1/p' "${ROOT}/rust/rust-toolchain.toml")" \
  "rust/rust-toolchain.toml"

expect_equal Node "${node_pin}" "$(declared .tool-versions nodejs)" ".tool-versions"
expect_equal Node "${node_pin}" "$(head -n 1 "${ROOT}/.nvmrc")" ".nvmrc"
expect_equal Node "${node_pin}" "$(head -n 1 "${ROOT}/.node-version")" ".node-version"
