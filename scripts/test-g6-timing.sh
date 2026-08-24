#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIMING="${ROOT}/scripts/g6-timing.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
timing_file="${fixture}/nested/timing.json"
rendezvous_dir="${fixture}/rendezvous"
candidate="$(printf 'a%.0s' {1..40})"

"${TIMING}" init "${timing_file}" test smoke "${candidate}" 123 1
"${TIMING}" start "${timing_file}" runner_preparation
"${TIMING}" end "${timing_file}" runner_preparation
mkdir -p "${rendezvous_dir}"
cat >"${rendezvous_dir}/first.result.json" <<'JSON'
{"started_at":"2026-08-23T15:00:00.000Z","completed_at":"2026-08-23T15:00:01.250Z"}
JSON
cat >"${rendezvous_dir}/second.result.json" <<'JSON'
{"started_at":"2026-08-23T15:00:02.000Z","completed_at":"2026-08-23T15:00:04.750Z"}
JSON
"${TIMING}" rendezvous-dir "${timing_file}" "${rendezvous_dir}"
jq -e '.rendezvous == {count: 2, cumulative_wait_ms: 4000}' "${timing_file}" >/dev/null

mkdir -p "${fixture}/bad-rendezvous"
printf '%s\n' '{"started_at":"invalid","completed_at":"2026-08-23T15:00:04.750Z"}' >"${fixture}/bad-rendezvous/invalid.result.json"
if G6_TIMING_REQUIRED=true "${TIMING}" rendezvous-dir "${timing_file}" "${fixture}/bad-rendezvous" >/dev/null 2>&1; then
  echo "expected malformed rendezvous result to fail" >&2
  exit 1
fi
