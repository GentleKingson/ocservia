#!/usr/bin/env bash
# Finite published-node -> candidate Controller matrix. No baseline rebuilds.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-}"
case "${mode}" in fetch|verify|run) ;; *) echo 'usage: release-session-compatibility.sh fetch|verify|run' >&2; exit 2 ;; esac
: "${BASELINE_RELEASE:?}" "${PACKAGE_ARCH:?}" "${RELEASE_ASSET_DIR:?}"
case "${BASELINE_RELEASE}" in v0.6.0|v0.6.1) ;; *) echo 'unregistered session compatibility baseline' >&2; exit 2 ;; esac
case "${PACKAGE_ARCH}" in amd64|arm64) ;; *) echo 'unsupported package architecture' >&2; exit 2 ;; esac
baseline="$(jq -ce --arg tag "${BASELINE_RELEASE}" '.[$tag]' "${ROOT}/scripts/release-upgrade-baselines.json")"
archive="ocservia-agent-${BASELINE_RELEASE#v}-linux-${PACKAGE_ARCH}.tar.gz"
assets=(SHA256SUMS SHA256SUMS.sig release-signing.pub.pem "${archive}" "${archive}.sha256" "${archive}.sha256.sig")
if [[ "${mode}" == fetch ]]; then
  # Refuse reuse rather than overwrite another run's evidence or downloads.
  mkdir -m 0700 -- "${RELEASE_ASSET_DIR}"
  for asset in "${assets[@]}"; do
    curl --fail --silent --show-error --location --retry 3 \
      -o "${RELEASE_ASSET_DIR}/${asset}" \
      "https://github.com/GentleKingson/ocservia/releases/download/${BASELINE_RELEASE}/${asset}"
  done
fi
[[ -d "${RELEASE_ASSET_DIR}" && ! -L "${RELEASE_ASSET_DIR}" ]]
RELEASE_ASSET_DIR="$(cd "${RELEASE_ASSET_DIR}" && pwd -P)"
for asset in "${assets[@]}"; do
  [[ -s "${RELEASE_ASSET_DIR}/${asset}" && ! -L "${RELEASE_ASSET_DIR}/${asset}" ]]
done
(
  cd "${RELEASE_ASSET_DIR}"
  printf '%s  SHA256SUMS\n' "$(jq -er '.sums_sha256' <<<"${baseline}")" | sha256sum -c --strict -
  fingerprint="$(openssl pkey -pubin -in release-signing.pub.pem -outform DER | sha256sum | awk '{print $1}')"
  [[ "${fingerprint}" == "$(jq -er '.key_der_sha256' <<<"${baseline}")" ]]
  openssl pkeyutl -verify -rawin -pubin -inkey release-signing.pub.pem -in SHA256SUMS -sigfile SHA256SUMS.sig
  # Historical SHA256SUMS lists payloads, not their sidecars or the public key.
  expected="$(awk -v name="${archive}" '$2 == name && NF == 2 {n++; hash=$1} END {if(n != 1) exit 1; print hash}' SHA256SUMS)"
  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]]
  printf '%s  %s\n' "${expected}" "${archive}" | sha256sum -c --strict -
  [[ "$(cat "${archive}.sha256")" == "$(sha256sum "${archive}")" ]]
  openssl pkeyutl -verify -rawin -pubin -inkey release-signing.pub.pem \
    -in "${archive}.sha256" -sigfile "${archive}.sha256.sig"
) >&2
identity="$(jq -n --arg tag "${BASELINE_RELEASE}" --arg arch "${PACKAGE_ARCH}" \
  --arg sha "$(sha256sum "${RELEASE_ASSET_DIR}/${archive}" | awk '{print $1}')" \
  --argjson baseline "${baseline}" \
  '{baseline_tag:$tag,baseline_commit:$baseline.commit,arch:$arch,archive_sha256:$sha,
    sums_sha256:$baseline.sums_sha256,key_der_sha256:$baseline.key_der_sha256}')"
if [[ "${mode}" != run ]]; then
  jq '. + {status:"artifact-verified",runtime_tested:false}' <<<"${identity}"
  exit 0
fi

: "${CANDIDATE_SHA:?}" "${RUN_ID:?}" "${RUNNER_TEMP:?}" "${ARTIFACT_DIR:?}"
: "${G6RD_CONTROL_PLANE_IMAGE:?}" "${G6RD_TRANSPORTD_IMAGE:?}" "${G6RD_RELAY_IMAGE:?}" "${G6RD_PROBE_IMAGE:?}" "${SINGLE_NODE_IMAGE:?}"
[[ "${CANDIDATE_SHA}" =~ ^[0-9a-f]{40}$ && "$(git -C "${ROOT}" rev-parse HEAD)" == "${CANDIDATE_SHA}" ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" ]]
[[ "${RUN_ID}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,39}$ ]]
[[ "${RUNNER_TEMP}" == /* && -d "${RUNNER_TEMP}" && ! -L "${RUNNER_TEMP}" ]]
[[ ! -e "${ARTIFACT_DIR}" && ! -L "${ARTIFACT_DIR}" ]]
# This audit refuses emulation and remote Docker; it does not change binfmt.
native="$(bash "${ROOT}/scripts/release-upgrade-native.sh" "${PACKAGE_ARCH}")"
images='[]'
for variable in G6RD_CONTROL_PLANE_IMAGE G6RD_TRANSPORTD_IMAGE G6RD_RELAY_IMAGE G6RD_PROBE_IMAGE SINGLE_NODE_IMAGE; do
  ref="${!variable}"
  [[ "${ref}" =~ ^sha256:[0-9a-f]{64}$ ]]
  inspected="$(docker image inspect "${ref}")"
  jq -e --arg arch "${PACKAGE_ARCH}" --arg ref "${ref}" \
    'length == 1 and .[0].Id == $ref and .[0].Os == "linux" and .[0].Architecture == $arch' <<<"${inspected}" >/dev/null
  case "${variable}" in
    G6RD_CONTROL_PLANE_IMAGE|G6RD_TRANSPORTD_IMAGE|G6RD_PROBE_IMAGE)
      jq -e --arg sha "${CANDIDATE_SHA}" '.[0].Config.Labels["org.opencontainers.image.revision"] == $sha' <<<"${inspected}" >/dev/null ;;
  esac
  images="$(jq --arg role "${variable}" --arg id "${ref}" '. + [{role:$role,image_id:$id}]' <<<"${images}")"
done
mkdir -m 0700 -- "${ARTIFACT_DIR}"
started="$(date -u +%FT%TZ)"
finish() {
  local code=$?
  trap - EXIT
  jq --arg sha "${CANDIDATE_SHA}" --arg started "${started}" --arg finished "$(date -u +%FT%TZ)" \
    --arg run "${RUN_ID}" --arg ci_run "${GITHUB_RUN_ID:-}" --arg attempt "${GITHUB_RUN_ATTEMPT:-}" \
    --argjson code "${code}" --argjson images "${images}" --argjson native "${native}" \
    '. + {candidate_sha:$sha,started_at:$started,finished_at:$finished,run_id:$run,
      ci_run_id:$ci_run,ci_run_attempt:$attempt,images:$images,native:$native,
      topology:{hosts:1,relays:(if .baseline_tag == "v0.6.0" then 2 else 1 end)},
      scope:"published-node-session-reload-recovery",exit_code:$code,
      status:(if $code == 0 then "pass" else "fail" end)}' <<<"${identity}" >"${ARTIFACT_DIR}/compatibility-result.json"
  exit "${code}"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
exec > >(tee "${ARTIFACT_DIR}/unit.log") 2>&1
export SINGLE_AGENT_ARCHIVE="${RELEASE_ASSET_DIR}/${archive}"
export SINGLE_AGENT_PUBLIC_KEY="${RELEASE_ASSET_DIR}/release-signing.pub.pem"
SINGLE_AGENT_KEY_SHA256="$(jq -er '.key_der_sha256' <<<"${baseline}")"
export SINGLE_AGENT_KEY_SHA256
export SINGLE_EXPECTED_AGENT_VERSION="${BASELINE_RELEASE#v}"
export SINGLE_LEGACY_SECOND_RELAY=false
[[ "${BASELINE_RELEASE}" != v0.6.0 ]] || export SINGLE_LEGACY_SECOND_RELAY=true
# Install only inside the existing dedicated, disposable systemd node fixture.
bash "${ROOT}/scripts/single-relay-integration.sh"
bash "${ROOT}/scripts/release-upgrade-native.sh" "${PACKAGE_ARCH}" >"${ARTIFACT_DIR}/native-after.json"
jq -e --arg version "${SINGLE_EXPECTED_AGENT_VERSION}" '.agent_version == $version' "${ARTIFACT_DIR}/final-node-read.json" >/dev/null
