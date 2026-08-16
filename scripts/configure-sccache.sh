#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -n "${ACTIONS_RUNTIME_TOKEN:-}" && \
  ( -n "${ACTIONS_CACHE_URL:-}" || -n "${ACTIONS_RESULTS_URL:-}" ) ]]; then
  backend=true
  message="GitHub Actions"
else
  backend=false
  message="local fallback"
fi

if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'SCCACHE_GHA_ENABLED=%s\n' "${backend}" >>"${GITHUB_ENV}"
  if [[ "${backend}" == "false" ]]; then
    printf 'SCCACHE_DIR=%s\n' "${ROOT}/.cache/sccache" >>"${GITHUB_ENV}"
  fi
elif [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  echo "GITHUB_ENV is required to configure sccache in GitHub Actions" >&2
  exit 1
fi

printf 'sccache backend: %s\n' "${message}"
