#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
group="${1:?required test group}"
shift
if [[ "${1:-}" == --select ]]; then
  shift
  selection="$(jq -nr --arg group "${group}" --arg mode select \
    --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
    -f "${ROOT}/scripts/check-required-go-tests.jq")"
  IFS=$'\t' read -r package pattern <<<"${selection}"
  [[ -n "${package}" && -n "${pattern}" ]] || { echo 'empty test selection' >&2; exit 1; }
  echo "Selected ${group}: ${package} -run ${pattern}"
  set -- "$@" "${package}" -run "${pattern}"
fi
if [[ "${group}" == database-* ]]; then
  : "${OCSERV_TEST_DATABASE_URL:?database acceptance requires a runtime connection}"
  : "${OCSERV_TEST_OWNER_DATABASE_URL:?database acceptance requires an owner connection}"
fi
tmp="$(mktemp -d "${TMPDIR:-/tmp}/required-go-tests-XXXXXX")"
result="${tmp}/results.json"
export RESULT_FILE="${result}"
# Include testing.T.TempDir fixtures (notably bootstrap Secret files) in cleanup
# even when Go's timeout/panic prevents their own Cleanup callbacks from running.
export TMPDIR="${tmp}"
pid=""
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "${pid}" ]]; then
    if kill -TERM -- "-${pid}" 2>/dev/null; then
      sleep 1
      kill -KILL -- "-${pid}" 2>/dev/null || true
    fi
    wait "${pid}" 2>/dev/null || true
  fi
  rm -rf "${tmp}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# A private process group also stops compiler/test children on interruption.
setsid bash -o pipefail -c 'go test -json -count=1 -timeout=10m "$@" | tee "$RESULT_FILE"' \
  required-go-tests "$@" &
pid=$!
status=0
wait "${pid}" || status=$?
if ((status != 0)); then exit "${status}"; fi
jq -se --arg group "${group}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
  -f "${ROOT}/scripts/check-required-go-tests.jq" "${result}"
