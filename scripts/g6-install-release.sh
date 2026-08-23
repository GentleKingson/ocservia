#!/usr/bin/env bash
# Verify a frozen formal/smoke G6 release before it is executable on a
# failure-domain runner.  This is intentionally shared by the action and its
# local negative fixture tests.
set -euo pipefail

fail() { echo "g6 release verification: $*" >&2; exit 1; }
require_file() { [[ -f "$1" ]] || fail "missing $1"; }
timing_start() { [[ -n "${G6_TIMING_FILE:-}" ]] && scripts/g6-timing.sh start "${G6_TIMING_FILE}" "$1" || true; }
timing_end() { [[ -n "${G6_TIMING_FILE:-}" ]] && scripts/g6-timing.sh end "${G6_TIMING_FILE}" "$1" || true; }
single_tsv_value() {
  local file="$1" key="$2" value
  value="$(awk -F '\t' -v key="${key}" '$1 == key { if (++count == 1) value=$2 } END { if (count == 1) print value }' "${file}")"
  [[ -n "${value}" ]] || fail "invalid ${file} entry ${key}"
  printf '%s\n' "${value}"
}
assert_manifest() {
  local file="$1" expected_lines="$2"
  [[ "$(wc -l <"${file}" | tr -d '[:space:]')" == "${expected_lines}" ]] || fail "unexpected ${file} entry count"
  awk -F '\t' 'NF != 2 || $1 == "" || $2 == "" { exit 1 } { if (seen[$1]++) exit 1 }' "${file}" || fail "malformed ${file}"
}

archive_dir="${1:?release archive directory is required}"
candidate_sha="${2:?candidate SHA is required}"
expected_go="${3:?expected Go version is required}"
timing_start checksum_provenance_verification
for file in release-artifacts.sha256 runtime-images.tar.gz image-ids.tsv \
  ocservia-g6-tunnel tunnel-manifest.tsv ocservia-g6-harness harness-manifest.tsv; do
  require_file "${archive_dir}/${file}"
done
(
  cd "${archive_dir}"
  sha256sum --check --strict release-artifacts.sha256
) || fail "release aggregate checksum mismatch"

# The aggregate must bind precisely the six frozen payload entries; accepting
# extra or missing lines would make the frozen release's trust surface vague.
[[ "$(wc -l <"${archive_dir}/release-artifacts.sha256" | tr -d '[:space:]')" == "6" ]] || fail "unexpected release aggregate entry count"
for file in runtime-images.tar.gz image-ids.tsv ocservia-g6-tunnel tunnel-manifest.tsv ocservia-g6-harness harness-manifest.tsv; do
  grep -Eq "^[0-9a-f]{64}  \*?${file}$" "${archive_dir}/release-artifacts.sha256" || fail "aggregate missing ${file}"
done

assert_manifest "${archive_dir}/tunnel-manifest.tsv" 2
[[ "$(single_tsv_value "${archive_dir}/tunnel-manifest.tsv" candidate_sha)" == "${candidate_sha}" ]] || fail "tunnel candidate SHA mismatch"
tunnel_sha="$(single_tsv_value "${archive_dir}/tunnel-manifest.tsv" ocservia-g6-tunnel)"
[[ "${tunnel_sha}" =~ ^[0-9a-f]{64}$ ]] || fail "invalid tunnel SHA"
[[ "$(sha256sum "${archive_dir}/ocservia-g6-tunnel" | awk '{print $1}')" == "${tunnel_sha}" ]] || fail "tunnel SHA mismatch"

assert_manifest "${archive_dir}/harness-manifest.tsv" 3
[[ "$(single_tsv_value "${archive_dir}/harness-manifest.tsv" candidate_sha)" == "${candidate_sha}" ]] || fail "harness candidate SHA mismatch"
[[ "$(single_tsv_value "${archive_dir}/harness-manifest.tsv" go_version)" == "${expected_go}" ]] || fail "harness Go version mismatch"
harness_sha="$(single_tsv_value "${archive_dir}/harness-manifest.tsv" ocservia-g6-harness)"
[[ "${harness_sha}" =~ ^[0-9a-f]{64}$ ]] || fail "invalid harness SHA"
[[ "$(sha256sum "${archive_dir}/ocservia-g6-harness" | awk '{print $1}')" == "${harness_sha}" ]] || fail "harness SHA mismatch"

release_images=(postgres:17.10-bookworm "${G6RD_CONTROL_PLANE_IMAGE:?}" "${G6RD_TRANSPORTD_IMAGE:?}" "${G6RD_RELAY_IMAGE:?}" "${G6RD_PROBE_IMAGE:?}" "${G6RD_AGENT_IMAGE:?}")
[[ "$(wc -l <"${archive_dir}/image-ids.tsv" | tr -d '[:space:]')" == "${#release_images[@]}" ]] || fail "unexpected image ID count"
awk -F '\t' 'NF != 2 || $1 == "" || $2 !~ /^sha256:[0-9a-f]{64}$/ || seen[$1]++ { exit 1 }' "${archive_dir}/image-ids.tsv" || fail "malformed image IDs"
for image in "${release_images[@]}"; do
  expected_id="$(single_tsv_value "${archive_dir}/image-ids.tsv" "${image}")"
  [[ "${expected_id}" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "invalid image ID for ${image}"
done
timing_end checksum_provenance_verification

if [[ "${G6_INSTALL_RELEASE_VERIFY_ONLY:-false}" == true ]]; then
  exit 0
fi

harness_bin="${RUNNER_TEMP:?}/ocservia-g6-harness"
tunnel_bin="${RUNNER_TEMP}/g6-readiness-${RUN_ID:?}/bin/ocservia-g6-tunnel"
install -m 0755 "${archive_dir}/ocservia-g6-harness" "${harness_bin}"
install -D -m 0755 "${archive_dir}/ocservia-g6-tunnel" "${tunnel_bin}"
install -m 0444 "${archive_dir}/tunnel-manifest.tsv" "$(dirname "${tunnel_bin}")/tunnel-manifest.tsv"
test -x "${tunnel_bin}"
diagnostics_dir="${RUNNER_TEMP}/artifacts/g6-readiness-${FD_ID:?}"
mkdir -p "${diagnostics_dir}"
install -m 0444 "${archive_dir}/harness-manifest.tsv" "${diagnostics_dir}/g6-harness-manifest.tsv"
printf 'G6_HARNESS_BIN=%s\n' "${harness_bin}" >>"${GITHUB_ENV:?}"
timing_start docker_load
gzip -dc "${archive_dir}/runtime-images.tar.gz" | docker load
timing_end docker_load
for image in "${release_images[@]}"; do
  expected_id="$(single_tsv_value "${archive_dir}/image-ids.tsv" "${image}")"
  [[ "$(docker image inspect --format '{{.Id}}' "${image}")" == "${expected_id}" ]] || fail "loaded image ID mismatch for ${image}"
  if [[ "${image}" != postgres:17.10-bookworm ]]; then
    [[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${image}")" == "${candidate_sha}" ]] || fail "image revision mismatch for ${image}"
  fi
done
