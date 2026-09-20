#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
mkdir -p "${fixture}/repo/scripts" "${fixture}/assets" "${fixture}/bin"
cp "${ROOT}/scripts/release-session-compatibility.sh" "${fixture}/repo/scripts/"
openssl genpkey -algorithm ED25519 -out "${fixture}/key.pem" 2>/dev/null
openssl pkey -in "${fixture}/key.pem" -pubout -out "${fixture}/assets/release-signing.pub.pem" 2>/dev/null
openssl genpkey -algorithm ED25519 -out "${fixture}/wrong-key.pem" 2>/dev/null
openssl pkey -in "${fixture}/wrong-key.pem" -pubout -out "${fixture}/wrong-public.pem" 2>/dev/null
archive=ocservia-agent-0.6.1-linux-arm64.tar.gz
(
  cd "${fixture}/assets"
  # Integrity fixtures only, never installed or claimed as released binaries.
  printf 'not a runtime package\n' >"${archive}"
  sha256sum "${archive}" >"${archive}.sha256"
  openssl pkeyutl -sign -rawin -inkey "${fixture}/key.pem" -in "${archive}.sha256" -out "${archive}.sha256.sig"
  sha256sum "${archive}" >SHA256SUMS
  openssl pkeyutl -sign -rawin -inkey "${fixture}/key.pem" -in SHA256SUMS -out SHA256SUMS.sig
)
pin="$(sha256sum "${fixture}/assets/SHA256SUMS" | awk '{print $1}')"
key="$(openssl pkey -pubin -in "${fixture}/assets/release-signing.pub.pem" -outform DER | sha256sum | awk '{print $1}')"
jq -n --arg pin "${pin}" --arg key "${key}" \
  '{"v0.6.1":{sums_sha256:$pin,key_der_sha256:$key,commit:"1805962fe1a98a22955b3105bfa8ebce7f2ea1eb"}}' \
  >"${fixture}/repo/scripts/release-upgrade-baselines.json"
export BASELINE_RELEASE=v0.6.1 PACKAGE_ARCH=arm64 RELEASE_ASSET_DIR="${fixture}/assets"
runner="${fixture}/repo/scripts/release-session-compatibility.sh"
bash "${runner}" verify >"${fixture}/verified.json"
jq -e '.status == "artifact-verified" and .runtime_tested == false and .arch == "arm64" and .baseline_tag == "v0.6.1"' "${fixture}/verified.json" >/dev/null
reject() {
  if "$@" >"${fixture}/rejected.log" 2>&1; then
    echo "unexpected acceptance: $*" >&2
    exit 1
  fi
}
reject env BASELINE_RELEASE=v1.0.0 bash "${runner}" verify
reject env BASELINE_RELEASE=v0.5.2 bash "${runner}" verify
reject env PACKAGE_ARCH=amd64 bash "${runner}" verify
reject env PACKAGE_ARCH=386 bash "${runner}" verify
reject bash "${runner}" fetch
for file in "${archive}" "${archive}.sha256" "${archive}.sha256.sig" SHA256SUMS SHA256SUMS.sig release-signing.pub.pem; do
  cp "${fixture}/assets/${file}" "${fixture}/saved"
  if [[ "${file}" == release-signing.pub.pem ]]; then
    cp "${fixture}/wrong-public.pem" "${fixture}/assets/${file}"
  else
    printf 'corrupt\n' >>"${fixture}/assets/${file}"
  fi
  reject bash "${runner}" verify
  mv "${fixture}/saved" "${fixture}/assets/${file}"
  mv "${fixture}/assets/${file}" "${fixture}/saved"
  ln -s "${fixture}/saved" "${fixture}/assets/${file}"
  reject bash "${runner}" verify
  rm "${fixture}/assets/${file}"
  mv "${fixture}/saved" "${fixture}/assets/${file}"
done

# Exercise run admission and result status without Docker, networking or install.
printf '#!/usr/bin/env bash\nprintf "{\\"fixture\\":true}\\n"\n' >"${fixture}/repo/scripts/release-upgrade-native.sh"
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nexit "${FIXTURE_WORKFLOW_EXIT:-0}"\n' >"${fixture}/repo/scripts/database-controller-e2e.sh"
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nif [[ "${FIXTURE_WRITE_RESULT:-false}" == true ]]; then\n  jq -n --arg version "${FIXTURE_VERSION:-$SINGLE_EXPECTED_AGENT_VERSION}" '\''{agent_version:$version}'\'' >"$ARTIFACT_DIR/final-node-read.json"\nfi\nexit "${FIXTURE_CHAIN_EXIT:-0}"\n' >"${fixture}/repo/scripts/single-relay-integration.sh"
git -C "${fixture}/repo" init -q
git -C "${fixture}/repo" add scripts
git -C "${fixture}/repo" -c user.name=fixture -c user.email=fixture@example.invalid commit -qm fixture
CANDIDATE_SHA="$(git -C "${fixture}/repo" rev-parse HEAD)"
export CANDIDATE_SHA
export RUN_ID=fixture RUNNER_TEMP="${fixture}" ARTIFACT_DIR="${fixture}/run"
G6RD_CONTROL_PLANE_IMAGE="sha256:$(printf '%064d' 1)"
export G6RD_CONTROL_PLANE_IMAGE
export G6RD_TRANSPORTD_IMAGE="${G6RD_CONTROL_PLANE_IMAGE}" G6RD_RELAY_IMAGE="${G6RD_CONTROL_PLANE_IMAGE}"
export G6RD_PROBE_IMAGE="${G6RD_CONTROL_PLANE_IMAGE}" SINGLE_NODE_IMAGE="${G6RD_CONTROL_PLANE_IMAGE}"
export RELEASE_WORKFLOW_IMAGE="${G6RD_CONTROL_PLANE_IMAGE}"
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\njq -n --arg ref "$3" --arg sha "${FIXTURE_IMAGE_SHA:-$CANDIDATE_SHA}" '\''[{Id:$ref,Os:"linux",Architecture:"arm64",Config:{Labels:{"org.opencontainers.image.revision":$sha}}}]'\''\n' >"${fixture}/bin/docker"
chmod +x "${fixture}/bin/docker"
export PATH="${fixture}/bin:${PATH}"
reject env CANDIDATE_SHA=invalid bash "${runner}" run
reject env G6RD_RELAY_IMAGE=relay:latest bash "${runner}" run
reject env FIXTURE_IMAGE_SHA=wrong bash "${runner}" run
reject env RUN_ID=../escape bash "${runner}" run
printf 'dirty\n' >"${fixture}/repo/dirty"
reject bash "${runner}" run
rm "${fixture}/repo/dirty"
reject env FIXTURE_CHAIN_EXIT=7 bash "${runner}" run
jq -e '.status == "fail" and .exit_code == 7 and .phases.session_exit_code == 7 and .phases.workflow_exit_code == 0' "${fixture}/run/compatibility-result.json" >/dev/null
export ARTIFACT_DIR="${fixture}/missing-result"
reject bash "${runner}" run
jq -e '.status == "fail" and .exit_code != 0' "${ARTIFACT_DIR}/compatibility-result.json" >/dev/null
export ARTIFACT_DIR="${fixture}/wrong-version"
reject env FIXTURE_WRITE_RESULT=true FIXTURE_VERSION=0.6.2 bash "${runner}" run
jq -e '.status == "fail" and .exit_code != 0' "${ARTIFACT_DIR}/compatibility-result.json" >/dev/null
export ARTIFACT_DIR="${fixture}/passed"
FIXTURE_WRITE_RESULT=true bash "${runner}" run
jq -e --arg sha "${CANDIDATE_SHA}" '.status == "pass" and .exit_code == 0 and .candidate_sha == $sha and (.images | length) == 6' "${ARTIFACT_DIR}/compatibility-result.json" >/dev/null
reject bash "${runner}" run
export ARTIFACT_DIR="${fixture}/workflow-failed"
reject env FIXTURE_WRITE_RESULT=true FIXTURE_WORKFLOW_EXIT=8 bash "${runner}" run
jq -e '.status == "fail" and .exit_code == 8 and .phases.session_exit_code == 0 and .phases.workflow_exit_code == 8' "${ARTIFACT_DIR}/compatibility-result.json" >/dev/null
# shellcheck disable=SC2329 # Invoked by the dynamically sourced production helper.
(
  # Reuse the real HTTP client helper; only exact pre-effect revision failures
  # may retry, never another 409, a server error or an uncertain network write.
  # shellcheck disable=SC1090
  source <(sed -n '/^enqueue_reload() {/,/^}/p' "${ROOT}/scripts/single-relay-integration.sh")
  export node=fixture approval=fixture
  revision=0
  sleep() { :; }
  g6rd_node_revision() { printf '%s\n' "$(($(cat "${fixture}/requests") + 1))"; }
  g6rd_api_session_curl() {
    local output count
    while (( $# )); do
      if [[ "$1" == --output ]]; then output="$2"; shift; fi
      shift
    done
    count="$(($(cat "${fixture}/requests") + 1))"
    printf '%s\n' "$count" >"${fixture}/requests"
    [[ "$response_case" != network ]] || return 7
    if [[ "$response_case" == recover && "$count" == 2 ]]; then
      printf '{}\n' >"$output"; printf 202
    else
      jq -n --arg type "$response_type" '{type:$type}' >"$output"
      printf '%s' "$response_status"
    fi
  }
  response_type=https://ocservia.dev/problems/stale-revision response_status=409
  response_case=recover
  printf '0\n' >"${fixture}/requests"
  enqueue_reload fixed-key reason "${fixture}/enqueue.json"
  [[ "$revision" == 2 && -f "${fixture}/enqueue.json.attempt-1" ]]
  for response_case in stale forbidden server network; do
    response_status=409 response_type=https://ocservia.dev/problems/stale-revision expected=1
    case "$response_case" in
      stale) expected=3 ;;
      forbidden) response_type=https://ocservia.dev/problems/approval-required ;;
      server) response_status=500 ;;
    esac
    printf '0\n' >"${fixture}/requests"
    reject enqueue_reload fixed-key reason "${fixture}/enqueue.json"
    [[ "$(cat "${fixture}/requests")" == "$expected" ]]
  done
)
echo 'Published session matrix integrity and admission contracts passed (fixtures, not runtime acceptance)'
