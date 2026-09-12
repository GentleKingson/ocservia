#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
container="${1:?disposable database container name required}"
gate="$(mktemp -d "${TMPDIR:-/tmp}/enrollment-restart-XXXXXX")"
pid=""
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "${pid}" ]]; then
    kill -TERM "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
  rm -rf "${gate}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "${ROOT}/control-plane"
OCSERV_TEST_RESTART_GATE="${gate}" bash "${ROOT}/scripts/required-go-tests.sh" backend-enrollment-restart --select -race -timeout=5m &
pid=$!
for ((i=0; i<1800; i++)); do
  [[ ! -f "${gate}/ready" ]] || break
  kill -0 "${pid}" 2>/dev/null || { wait "${pid}"; exit 1; }
  sleep 0.1
done
[[ -f "${gate}/ready" ]] || { echo 'restart fixture did not become ready' >&2; exit 1; }
docker restart "${container}" >/dev/null
touch "${gate}/restarted"
wait "${pid}"
pid=""
