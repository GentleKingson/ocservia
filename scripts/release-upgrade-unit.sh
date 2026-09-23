#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
component="${1:?agent or controller required}"
arch="${2:?architecture required}"
: "${FROZEN_FILE:?}" "${FROZEN_SHA256:?}" "${ARTIFACT_DIR:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
[[ "${FROZEN_SHA256}" =~ ^[0-9a-f]{64}$ ]]
[[ "$(sha256sum "${FROZEN_FILE}" | cut -d' ' -f1)" == "${FROZEN_SHA256}" ]]
mkdir -p "${ARTIFACT_DIR}"
started="$(date -u +%FT%TZ)"
stage=identity
export UPGRADE_SCENARIOS_FILE="${ARTIFACT_DIR}/scenarios.txt"
: >"${UPGRADE_SCENARIOS_FILE}"
printf '[]\n' >"${ARTIFACT_DIR}/artifacts.json"
printf '{}\n' >"${ARTIFACT_DIR}/native.json"
work="$(mktemp -d "${RUNNER_TEMP}/upgrade-${component}.XXXXXX")"
finish() {
  local status=$? result=fail
  trap - EXIT
  [[ "${status}" != 0 ]] || result=pass
  jq -n --slurpfile frozen "${FROZEN_FILE}" --slurpfile native "${ARTIFACT_DIR}/native.json" \
    --slurpfile artifacts "${ARTIFACT_DIR}/artifacts.json" --rawfile scenarios "${UPGRADE_SCENARIOS_FILE}" \
    --arg component "${component}" --arg arch "${arch}" --arg started "${started}" --arg finished "$(date -u +%FT%TZ)" \
    --arg result "${result}" --arg stage "${stage}" --argjson code "${status}" \
    --arg run "${GITHUB_RUN_ID}" --arg attempt "${GITHUB_RUN_ATTEMPT}" '
      $frozen[0] | {candidate_sha,candidate_version,baseline_tag,baseline_commit,baseline_lock_sha256} +
      {component:$component,arch:$arch,run_id:$run,run_attempt:$attempt,started_at:$started,finished_at:$finished,
       native:$native[0],artifacts:$artifacts[0],status:$result,
       failure:(if $code == 0 then null else {stage:$stage,exit_code:$code,reason:($stage + " exited " + ($code|tostring) + "; see unit.log")} end),
       scenarios:($scenarios | split("\n") | map(select(length>0) | {key:.,value:"pass"}) | from_entries)}' \
    >"${ARTIFACT_DIR}/result.json"
  if [[ "${status}" == 0 ]]; then
    node scripts/release-upgrade-contract.mjs validate-unit "${ARTIFACT_DIR}/result.json" || status=$?
  fi
  docker buildx rm "upgrade-${component}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" >/dev/null 2>&1 || true
  rm -rf -- "${work}" || true
  exit "${status}"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
exec > >(tee "${ARTIFACT_DIR}/unit.log") 2>&1
[[ "$(git rev-parse HEAD)" == "${GITHUB_SHA}" ]]
jq -e --arg sha "${GITHUB_SHA}" --arg run "${GITHUB_RUN_ID}" \
  --arg lock "$(sha256sum scripts/release-upgrade-baselines.json | awk '{print $1}')" \
  '.candidate_sha == $sha and .run_id == $run and .baseline_lock_sha256 == $lock' "${FROZEN_FILE}" >/dev/null
bash "${ROOT}/scripts/release-upgrade-native.sh" "${arch}" >"${ARTIFACT_DIR}/native.json"
VERSION="$(jq -er '.candidate_version' "${FROZEN_FILE}")"
BASELINE_RELEASE="$(jq -er '.baseline_tag' "${FROZEN_FILE}")"
SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
export VERSION BASELINE_RELEASE SOURCE_DATE_EPOCH SOURCE_COMMIT="${GITHUB_SHA}"
export RUN_ID="upgrade-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-${arch}"
export OUTPUT_DIR="${CANDIDATE_PRODUCTS:-${work}/products}" PACKAGE_ARCH="${arch}" CONTROLLER_ARCH="${arch}"
stage=build
if [[ -n "${CANDIDATE_PRODUCTS:-}" ]]; then
  node scripts/release-artifacts.mjs verify "${OUTPUT_DIR}" "${component}" "${arch}" "${VERSION}" "${CANDIDATE_MANIFEST_SHA256:?}"
elif [[ "${component}" == agent ]]; then
  bash scripts/bootstrap.sh native-packages
  export AGENT_SIGNING_KEY="${work}/candidate.key"
  (umask 077; openssl genpkey -algorithm ED25519 -out "${AGENT_SIGNING_KEY}")
  bash scripts/build-release-agent.sh
elif [[ "${component}" == controller ]]; then
  export BUILDX_BUILDER="upgrade-${component}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
  bash scripts/build-release-controller.sh
else exit 2; fi
find "${OUTPUT_DIR}" -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum | \
  jq -Rs 'split("\n") | map(select(length>0) | capture("^(?<sha256>[0-9a-f]{64})  (?<name>.*)$") | .name |= split("/")[-1])' \
  >"${ARTIFACT_DIR}/artifacts.json"
stage=upgrade
if [[ "${component}" == agent ]]; then
  case "${arch}" in amd64) rpm_arch=x86_64 ;; arm64) rpm_arch=aarch64 ;; esac
  export CANDIDATE_DEB="${OUTPUT_DIR}/ocservia-agent_${VERSION}-1_${arch}.deb"
  export CANDIDATE_RPM="${OUTPUT_DIR}/ocservia-agent-${VERSION}-1.${rpm_arch}.rpm"
  bash scripts/release-baseline-upgrade-smoke.sh
else
  IMAGES_DIR="${OUTPUT_DIR}" bash scripts/release-controller-upgrade-smoke.sh
fi
stage=scenarios
# Fail this job too, not just the aggregate, for any incomplete scenario set.
node --input-type=module - "${component}" "${UPGRADE_SCENARIOS_FILE}" <<'JS'
import fs from "node:fs";
import { scenarios } from "./scripts/release-upgrade-contract.mjs";
const done = fs.readFileSync(process.argv[3], "utf8").trim().split("\n");
if (new Set(done).size !== done.length || !scenarios[process.argv[2]].every(s => done.includes(s))) process.exit(1);
JS
stage=complete
