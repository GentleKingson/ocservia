#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTION="${ROOT}/.github/actions/g6-cache-credentials/index.js"
test_dir="$(mktemp -d)"
cleanup() {
  rm -rf -- "${test_dir}"
}
trap cleanup EXIT

without_credentials="${test_dir}/without-credentials.env"
without_log="${test_dir}/without-credentials.log"
GITHUB_ENV="${without_credentials}" \
  env -u ACTIONS_RUNTIME_TOKEN -u ACTIONS_CACHE_URL -u ACTIONS_RESULTS_URL \
  node "${ACTION}" >"${without_log}" 2>&1
grep -Fxq 'G6_CACHE_AVAILABLE<<G6_CACHE_CREDENTIALS_EOF' "${without_credentials}"
grep -Fxq 'false' "${without_credentials}"

with_credentials="${test_dir}/with-credentials.env"
with_log="${test_dir}/with-credentials.log"
GITHUB_ENV="${with_credentials}" \
  ACTIONS_RUNTIME_TOKEN='test-cache-token' \
  ACTIONS_RESULTS_URL='https://example.invalid/cache-results' \
  node "${ACTION}" >"${with_log}" 2>&1
grep -Fxq 'G6_CACHE_AVAILABLE<<G6_CACHE_CREDENTIALS_EOF' "${with_credentials}"
grep -Fxq 'true' "${with_credentials}"
grep -Fq 'ACTIONS_RUNTIME_TOKEN<<G6_CACHE_CREDENTIALS_EOF' "${with_credentials}"
if grep -Fq 'test-cache-token' "${with_log}"; then
  echo "cache credential relay logged a credential value" >&2
  exit 1
fi

token_without_url="${test_dir}/token-without-url.env"
GITHUB_ENV="${token_without_url}" \
  ACTIONS_RUNTIME_TOKEN='test-cache-token' \
  env -u ACTIONS_CACHE_URL -u ACTIONS_RESULTS_URL \
  node "${ACTION}" >/dev/null 2>&1
grep -Fxq 'false' "${token_without_url}"

echo "G6 cache credential optionality checks passed"
