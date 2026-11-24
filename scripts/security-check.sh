#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

gitleaks git --no-banner --redact --no-color --config "${ROOT}/scripts/secret-scan.toml" "${ROOT}" \
  >"${work}/git.log" 2>&1 &
git_pid=$!
"${ROOT}/scripts/test-secret-scan-config-runtime.sh" >"${work}/runtime.log" 2>&1 &
runtime_pid=$!

git_status=0
runtime_status=0
wait "${git_pid}" || git_status=$?
wait "${runtime_pid}" || runtime_status=$?
cat "${work}/git.log" "${work}/runtime.log"
if ((git_status != 0 || runtime_status != 0)); then
  exit 1
fi
