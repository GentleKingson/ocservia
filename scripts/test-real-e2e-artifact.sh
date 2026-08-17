#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER="${ROOT}/scripts/real-e2e-artifact.sh"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT

export GITHUB_RUN_ID=424242
export GITHUB_RUN_ATTEMPT=3
export GITHUB_REPOSITORY=ocservia/example
export GITHUB_API_URL=https://api.example.invalid
export GITHUB_TOKEN=test-token
export REAL_E2E_ARTIFACT_RETRIES=0
export REAL_E2E_ARTIFACT_POLL_INTERVAL_SECONDS=1
export REAL_E2E_ARTIFACT_PROPAGATION_GRACE_SECONDS=1
export REAL_E2E_ARTIFACT_MAX_CONSECUTIVE_ERRORS=2

install_curl_shim() {
  local body="${1:?shim body is required}"
  local directory="${TEST_ROOT}/$2"
  mkdir -p "${directory}/bin"
  printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "${body}" \
    >"${directory}/bin/curl"
  chmod +x "${directory}/bin/curl"
  printf '%s\n' "${directory}"
}

run_wait() {
  local directory="${1:?test directory is required}"
  shift
  PATH="${directory}/bin:${PATH}" RUNNER_TEMP="${directory}" \
    "${HELPER}" wait-download \
    "g6-rd-agents-enrolled-fd-b-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" \
    "${directory}/destination" 30 "G6 Readiness Failure Domain B" "$@"
}

# shellcheck disable=SC2016  # the generated shim expands these at execution
peer_failed="$(install_curl_shim '
url="${!#}"
case "${url}" in
  *"/artifacts?"*) printf "%s\\n" '\''{"artifacts":[]}'\'' ;;
  *"/jobs?"*) printf "%s\\n" '\''{"jobs":[{"name":"G6 Readiness Failure Domain B","status":"in_progress","conclusion":null,"steps":[{"name":"Enroll the failure domain B fleet","status":"completed","conclusion":"failure"},{"name":"Collect diagnostics","status":"in_progress","conclusion":null}]}]}'\'' ;;
  *) exit 64 ;;
esac' peer-failed)"
started="${SECONDS}"
if run_wait "${peer_failed}" >"${peer_failed}/stdout" 2>"${peer_failed}/stderr"; then
  echo "artifact wait accepted a failed peer job" >&2
  exit 1
fi
grep -q 'peer job G6 Readiness Failure Domain B failed at step Enroll the failure domain B fleet (failure)' \
  "${peer_failed}/stderr" || {
  echo "artifact wait did not report the peer failure" >&2
  exit 1
}
((SECONDS - started < 5)) || {
  echo "artifact wait did not fail promptly after the peer step failed" >&2
  exit 1
}

# shellcheck disable=SC2016  # the generated shim expands these at execution
peer_succeeded="$(install_curl_shim '
url="${!#}"
case "${url}" in
  *"/artifacts?"*) printf "%s\\n" '\''{"artifacts":[]}'\'' ;;
  *"/jobs?"*) printf "%s\\n" '\''{"jobs":[{"name":"G6 Readiness Failure Domain B","status":"completed","conclusion":"success","steps":[]}]}'\'' ;;
  *) exit 64 ;;
esac' peer-succeeded)"
if run_wait "${peer_succeeded}" >"${peer_succeeded}/stdout" 2>"${peer_succeeded}/stderr"; then
  echo "artifact wait accepted a successful peer without its promised artifact" >&2
  exit 1
fi
grep -q 'did not publish artifact .* within the propagation grace period' \
  "${peer_succeeded}/stderr" || {
  echo "artifact wait did not enforce the post-success propagation grace period" >&2
  exit 1
}

# shellcheck disable=SC2016  # the generated shim expands these at execution
api_failure="$(install_curl_shim '
printf "call\\n" >>"${RUNNER_TEMP}/curl-calls"
exit 7' api-failure)"
started="${SECONDS}"
if run_wait "${api_failure}" >"${api_failure}/stdout" 2>"${api_failure}/stderr"; then
  echo "artifact wait accepted repeated GitHub API failures" >&2
  exit 1
fi
[[ "$(wc -l <"${api_failure}/curl-calls" | tr -d ' ')" == 3 ]] || {
  echo "artifact wait exceeded its consecutive API failure budget" >&2
  exit 1
}
((SECONDS - started < 5)) || {
  echo "artifact API failures were not bounded" >&2
  exit 1
}

invalid_json="$(install_curl_shim 'printf "%s\\n" not-json' invalid-json)"
if run_wait "${invalid_json}" >"${invalid_json}/stdout" 2>"${invalid_json}/stderr"; then
  echo "artifact wait accepted an invalid GitHub API response" >&2
  exit 1
fi
grep -q 'artifact API returned an invalid JSON document' "${invalid_json}/stderr" || {
  echo "artifact wait did not fail closed on invalid JSON" >&2
  exit 1
}

# shellcheck disable=SC2016  # the generated shim expands these at execution
download_success="$(install_curl_shim '
url="${!#}"
case "${url}" in
  *"/artifacts?"*) printf "%s\\n" '\''{"artifacts":[{"id":99,"name":"g6-rd-agents-enrolled-fd-b-424242-3","expired":false}]}'\'' ;;
  *"/jobs?"*) printf "%s\\n" '\''{"jobs":[{"name":"G6 Readiness Failure Domain B","status":"in_progress","conclusion":null,"steps":[]}]}'\'' ;;
  *"/actions/artifacts/99/zip") cat "${RUNNER_TEMP}/artifact.zip" ;;
  *) exit 64 ;;
esac' download-success)"
mkdir -p "${download_success}/payload"
printf '%s\n' expected-candidate >"${download_success}/payload/candidate-sha"
(cd "${download_success}/payload" && zip -q "${download_success}/artifact.zip" candidate-sha)
run_wait "${download_success}"
[[ "$(<"${download_success}/destination/candidate-sha")" == expected-candidate ]] || {
  echo "artifact wait did not safely extract the validated artifact" >&2
  exit 1
}

# Reject symlinks from ZIP metadata before extraction, rather than discovering
# them only after a later member may already have traversed the link.
# shellcheck disable=SC2016  # the generated shim expands these at execution
symlink_archive="$(install_curl_shim '
url="${!#}"
case "${url}" in
  *"/artifacts?"*) printf "%s\\n" '\''{"artifacts":[{"id":99,"name":"g6-rd-agents-enrolled-fd-b-424242-3","expired":false}]}'\'' ;;
  *"/jobs?"*) printf "%s\\n" '\''{"jobs":[{"name":"G6 Readiness Failure Domain B","status":"in_progress","conclusion":null,"steps":[]}]}'\'' ;;
  *"/actions/artifacts/99/zip") cat "${RUNNER_TEMP}/artifact.zip" ;;
  *) exit 64 ;;
esac' symlink-archive)"
mkdir -p "${symlink_archive}/payload"
printf '%s\n' target >"${symlink_archive}/payload/target"
ln -s target "${symlink_archive}/payload/link"
(cd "${symlink_archive}/payload" && zip -qy "${symlink_archive}/artifact.zip" link target)
if run_wait "${symlink_archive}" \
  >"${symlink_archive}/stdout" 2>"${symlink_archive}/stderr"; then
  echo "artifact wait accepted a symbolic link" >&2
  exit 1
fi
grep -q 'artifact contains a symbolic link' "${symlink_archive}/stderr" || {
  echo "artifact wait did not report the symbolic link" >&2
  exit 1
}

# An artifact is not sufficient on its own: accepting it while the peer-status
# request is failing would recreate a fail-open race around a failed producer.
# shellcheck disable=SC2016  # the generated shim expands these at execution
artifact_without_peer_state="$(install_curl_shim '
url="${!#}"
case "${url}" in
  *"/artifacts?"*) printf "%s\\n" '\''{"artifacts":[{"id":99,"name":"g6-rd-agents-enrolled-fd-b-424242-3","expired":false}]}'\'' ;;
  *"/jobs?"*) exit 7 ;;
  *"/actions/artifacts/99/zip") touch "${RUNNER_TEMP}/downloaded"; exit 64 ;;
  *) exit 64 ;;
esac' artifact-without-peer-state)"
if run_wait "${artifact_without_peer_state}" \
  >"${artifact_without_peer_state}/stdout" 2>"${artifact_without_peer_state}/stderr"; then
  echo "artifact wait accepted an artifact without validating its peer job" >&2
  exit 1
fi
[[ ! -e "${artifact_without_peer_state}/downloaded" ]] || {
  echo "artifact download started before peer state was validated" >&2
  exit 1
}
grep -q 'jobs API failed 2 consecutive bounded requests' \
  "${artifact_without_peer_state}/stderr" || {
  echo "artifact wait did not fail closed when peer status stayed unavailable" >&2
  exit 1
}

echo "real E2E artifact wait checks passed"
