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

# A measured command records its duration and always propagates the wrapped
# exit status; timing collection must never mask a build failure.
"${TIMING}" measure "${timing_file}" control_plane_build -- /bin/sh -c 'exit 0'
if "${TIMING}" measure "${timing_file}" relay_build -- /bin/sh -c 'exit 3'; then
  echo "expected a failing measured command to propagate its status" >&2
  exit 1
fi
status_capture=0
"${TIMING}" measure "${timing_file}" g6_probe_build -- /bin/sh -c 'exit 5' || status_capture=$?
[[ "${status_capture}" -eq 5 ]] || {
  echo "measure must preserve the exact wrapped exit code (got ${status_capture})" >&2
  exit 1
}
# measure only appends raw rows; the parent renders after every measured
# command has been waited on.
"${TIMING}" render "${timing_file}"
jq -e '.stages[] | select(.name == "control_plane_build" and .duration_ms >= 0)' \
  "${timing_file}" >/dev/null
jq -e '.stages[] | select(.name == "relay_build" and .duration_ms >= 0)' \
  "${timing_file}" >/dev/null
jq -e '.stages[] | select(.name == "g6_probe_build" and .duration_ms >= 0)' \
  "${timing_file}" >/dev/null
if G6_TIMING_REQUIRED=true "${TIMING}" measure "${timing_file}" malformed -- /bin/sh -c 'exit 9' >/dev/null 2>&1; then
  echo "measure must stay authoritative under G6_TIMING_REQUIRED" >&2
  exit 1
fi
status_capture=0
G6_TIMING_REQUIRED=true "${TIMING}" measure "${timing_file}" required_probe -- /bin/sh -c 'exit 9' \
  || status_capture=$?
[[ "${status_capture}" -eq 9 ]] || {
  echo "authoritative measure must still propagate the wrapped status" >&2
  exit 1
}

# A broken clock must never satisfy the non-authoritative ERR trap and skip
# the wrapped authoritative command: measure disarms the trap before any
# timing work, always runs the wrapped command, and records no rows from
# invalid timestamps. Fake both clock backends (awk for /proc/uptime, node
# for the fallback) so the fixture fails on every host.
broken_clock_bin="${fixture}/broken-clock-bin"
mkdir -p "${broken_clock_bin}"
for clock_tool in awk node; do
  printf '#!/usr/bin/env bash\necho "broken clock: %s failed" >&2\nexit 1\n' \
    "${clock_tool}" >"${broken_clock_bin}/${clock_tool}"
  chmod 0755 "${broken_clock_bin}/${clock_tool}"
done
broken_clock_marker="${fixture}/broken-clock-ran"
wrapped_command="${fixture}/broken-clock-wrapped.sh"
printf '#!/usr/bin/env bash\nprintf ran >"%s"\nexit 7\n' \
  "${broken_clock_marker}" >"${wrapped_command}"
chmod 0755 "${wrapped_command}"
status_capture=0
PATH="${broken_clock_bin}:${PATH}" "${TIMING}" measure "${timing_file}" broken_clock \
  -- "${wrapped_command}" || status_capture=$?
[[ "${status_capture}" -eq 7 ]] || {
  echo "a broken clock must not stop the wrapped command from returning its own status (got ${status_capture})" >&2
  exit 1
}
[[ -f "${broken_clock_marker}" ]] || {
  echo "the wrapped command must execute even when the timing clock is broken" >&2
  exit 1
}
if grep -q $'^start\tbroken_clock\t' "${timing_file}.tsv"; then
  echo "a broken clock must not record a bogus start timestamp" >&2
  exit 1
fi
if grep -q $'^duration\tbroken_clock\t' "${timing_file}.tsv"; then
  echo "a broken clock must not record a bogus duration" >&2
  exit 1
fi

digest="$(printf 'b%.0s' {1..64})"
"${TIMING}" image "${timing_file}" transportd 1048576 "sha256:${digest}"
jq -e '.images == {transportd: {bytes: 1048576, image_id: "sha256:'"${digest}"'"}}' \
  "${timing_file}" >/dev/null
if G6_TIMING_REQUIRED=true "${TIMING}" image "${timing_file}" bad_size not-an-integer "sha256:${digest}" >/dev/null 2>&1; then
  echo "expected malformed image bytes to fail" >&2
  exit 1
fi
if G6_TIMING_REQUIRED=true "${TIMING}" image "${timing_file}" bad_id 1024 not-a-digest >/dev/null 2>&1; then
  echo "expected malformed image id to fail" >&2
  exit 1
fi
summary="${fixture}/step-summary.md"
GITHUB_STEP_SUMMARY="${summary}" "${TIMING}" summary "${timing_file}"
grep -q '| transportd | 1048576 | sha256:'"${digest}"' |' "${summary}"
