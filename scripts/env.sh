#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "source scripts/env.sh instead of executing it" >&2
  exit 2
fi

OCSERVIA_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export OCSERVIA_ROOT
export GOTOOLCHAIN=local
export GOCACHE="${OCSERVIA_ROOT}/.cache/go-build"
export GOPATH="${OCSERVIA_ROOT}/.cache/gopath"
export GOMODCACHE="${OCSERVIA_ROOT}/.cache/go-mod"
export RUSTUP_HOME="${OCSERVIA_ROOT}/.tools/rustup"
export CARGO_HOME="${OCSERVIA_ROOT}/.tools/cargo"
export npm_config_cache="${OCSERVIA_ROOT}/.cache/npm"
export XDG_CACHE_HOME="${OCSERVIA_ROOT}/.cache/xdg"
export PATH="${OCSERVIA_ROOT}/.tools/go/bin:${OCSERVIA_ROOT}/.tools/node/bin:${CARGO_HOME}/bin:${OCSERVIA_ROOT}/.tools/bin:${OCSERVIA_ROOT}/.tools/protoc/bin:${PATH}"
