#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERIFY="${ROOT}/scripts/g6-install-release.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
candidate="$(printf 'a%.0s' {1..40})"
other="$(printf 'b%.0s' {1..40})"
id="sha256:$(printf 'c%.0s' {1..64})"
tab="$(printf '\t')"
export G6RD_CONTROL_PLANE_IMAGE=control:test G6RD_TRANSPORTD_IMAGE=transport:test
export G6RD_RELAY_IMAGE=relay:test G6RD_PROBE_IMAGE=probe:test G6RD_AGENT_IMAGE=agent:test

make_fixture() {
  local dir="$1"
  mkdir -p "${dir}"
  printf 'runtime\n' >"${dir}/runtime-images.tar.gz"
  printf 'tunnel\n' >"${dir}/ocservia-g6-tunnel"
  printf 'harness\n' >"${dir}/ocservia-g6-harness"
  printf 'candidate_sha\t%s\nocservia-g6-tunnel\t%s\n' "${candidate}" "$(sha256sum "${dir}/ocservia-g6-tunnel" | awk '{print $1}')" >"${dir}/tunnel-manifest.tsv"
  printf 'candidate_sha\t%s\ngo_version\tgo1.25.0\nocservia-g6-harness\t%s\n' "${candidate}" "$(sha256sum "${dir}/ocservia-g6-harness" | awk '{print $1}')" >"${dir}/harness-manifest.tsv"
  : >"${dir}/image-ids.tsv"
  for image in postgres:17.10-bookworm control:test transport:test relay:test probe:test agent:test; do printf '%s\t%s\n' "${image}" "${id}" >>"${dir}/image-ids.tsv"; done
  reseal_aggregate "${dir}"
}

# Rebuild the aggregate after an intentional payload mutation.  Without this,
# every modified file is rejected by the aggregate checksum layer and the
# provenance guards under test are never reached.
reseal_aggregate() {
  (
    cd "$1"
    sha256sum runtime-images.tar.gz image-ids.tsv ocservia-g6-tunnel tunnel-manifest.tsv ocservia-g6-harness harness-manifest.tsv >release-artifacts.sha256
  )
}

# The verifier must fail through the exact fail-closed branch named in its
# stderr, not merely fail somewhere earlier.
assert_rejects() {
  local dir="$1" expected="$2" label="$3" stderr_file
  stderr_file="$(mktemp)"
  if G6_INSTALL_RELEASE_VERIFY_ONLY=true "${VERIFY}" "${dir}" "${candidate}" go1.25.0 >/dev/null 2>"${stderr_file}"; then
    echo "expected release verification to fail: ${label}" >&2
    rm -f -- "${stderr_file}"
    exit 1
  fi
  if ! grep -qF "${expected}" "${stderr_file}"; then
    echo "expected ${label} rejection '${expected}' but got:" >&2
    cat "${stderr_file}" >&2
    rm -f -- "${stderr_file}"
    exit 1
  fi
  rm -f -- "${stderr_file}"
}
edit_tsv() {
  local file="$1" key="$2" value="$3"
  awk -F '\t' -v OFS='\t' -v key="${key}" -v value="${value}" '$1 == key { $2 = value } { print }' "${file}" >"${file}.tmp"
  mv "${file}.tmp" "${file}"
}

make_fixture "${fixture}/good"
G6_INSTALL_RELEASE_VERIFY_ONLY=true "${VERIFY}" "${fixture}/good" "${candidate}" go1.25.0
cp -R "${fixture}/good" "${fixture}/resealed"
reseal_aggregate "${fixture}/resealed"
G6_INSTALL_RELEASE_VERIFY_ONLY=true "${VERIFY}" "${fixture}/resealed" "${candidate}" go1.25.0
# Aggregate checksum and file-set binding layer.
cp -R "${fixture}/good" "${fixture}/bad-missing-payload"; rm "${fixture}/bad-missing-payload/runtime-images.tar.gz"; assert_rejects "${fixture}/bad-missing-payload" "missing" missing-payload
cp -R "${fixture}/good" "${fixture}/bad-aggregate"; printf x >>"${fixture}/bad-aggregate/release-artifacts.sha256"; assert_rejects "${fixture}/bad-aggregate" "release aggregate checksum mismatch" aggregate
cp -R "${fixture}/good" "${fixture}/bad-aggregate-whitespace"; sed -i.bak '1s/  /   /' "${fixture}/bad-aggregate-whitespace/release-artifacts.sha256"; rm "${fixture}/bad-aggregate-whitespace/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-aggregate-whitespace" "release aggregate checksum mismatch" aggregate-whitespace
cp -R "${fixture}/good" "${fixture}/bad-leading-star-aggregate"; sed -i.bak '1s/  /  */' "${fixture}/bad-leading-star-aggregate/release-artifacts.sha256"; rm "${fixture}/bad-leading-star-aggregate/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-leading-star-aggregate" "release aggregate checksum mismatch" leading-star-aggregate
cp -R "${fixture}/good" "${fixture}/bad-binary-marker-aggregate"; sed -i.bak '1s/  / */' "${fixture}/bad-binary-marker-aggregate/release-artifacts.sha256"; rm "${fixture}/bad-binary-marker-aggregate/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-binary-marker-aggregate" "malformed release aggregate" binary-marker-aggregate
cp -R "${fixture}/good" "${fixture}/bad-runtime-lookalike"; cp "${fixture}/bad-runtime-lookalike/runtime-images.tar.gz" "${fixture}/bad-runtime-lookalike/runtime-imagesXtarYgz"; sed -i.bak 's/runtime-images\.tar\.gz/runtime-imagesXtarYgz/' "${fixture}/bad-runtime-lookalike/release-artifacts.sha256"; rm "${fixture}/bad-runtime-lookalike/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-runtime-lookalike" "release-artifacts.sha256 file set mismatch" runtime-lookalike
cp -R "${fixture}/good" "${fixture}/bad-image-ids-lookalike"; cp "${fixture}/bad-image-ids-lookalike/image-ids.tsv" "${fixture}/bad-image-ids-lookalike/image-idsXtsv"; sed -i.bak 's/image-ids\.tsv/image-idsXtsv/' "${fixture}/bad-image-ids-lookalike/release-artifacts.sha256"; rm "${fixture}/bad-image-ids-lookalike/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-image-ids-lookalike" "release-artifacts.sha256 file set mismatch" image-ids-lookalike
cp -R "${fixture}/good" "${fixture}/bad-extra-aggregate-entry"; cp "${fixture}/bad-extra-aggregate-entry/image-ids.tsv" "${fixture}/bad-extra-aggregate-entry/extra-payload"; (cd "${fixture}/bad-extra-aggregate-entry" && sha256sum extra-payload >>release-artifacts.sha256); assert_rejects "${fixture}/bad-extra-aggregate-entry" "release-artifacts.sha256 file set mismatch" extra-aggregate-entry
cp -R "${fixture}/good" "${fixture}/bad-duplicate-aggregate-entry"; duplicate_entry="$(head -n 1 "${fixture}/bad-duplicate-aggregate-entry/release-artifacts.sha256")"; printf '%s\n' "${duplicate_entry}" >>"${fixture}/bad-duplicate-aggregate-entry/release-artifacts.sha256"; assert_rejects "${fixture}/bad-duplicate-aggregate-entry" "release-artifacts.sha256 file set mismatch" duplicate-aggregate-entry
cp -R "${fixture}/good" "${fixture}/bad-missing-aggregate-entry"; sed -i.bak '/ocservia-g6-harness$/d' "${fixture}/bad-missing-aggregate-entry/release-artifacts.sha256"; rm "${fixture}/bad-missing-aggregate-entry/release-artifacts.sha256.bak"; assert_rejects "${fixture}/bad-missing-aggregate-entry" "release-artifacts.sha256 file set mismatch" missing-aggregate-entry
# Provenance layer: every fixture below reseals the aggregate so the mutated
# payload or manifest is judged by the guard it targets.
cp -R "${fixture}/good" "${fixture}/bad-tunnel-candidate"; sed -i.bak "s/${candidate}/${other}/" "${fixture}/bad-tunnel-candidate/tunnel-manifest.tsv"; rm "${fixture}/bad-tunnel-candidate/tunnel-manifest.tsv.bak"; reseal_aggregate "${fixture}/bad-tunnel-candidate"; assert_rejects "${fixture}/bad-tunnel-candidate" "tunnel candidate SHA mismatch" tunnel-candidate
cp -R "${fixture}/good" "${fixture}/bad-candidate"; sed -i.bak "s/${candidate}/${other}/" "${fixture}/bad-candidate/harness-manifest.tsv"; rm "${fixture}/bad-candidate/harness-manifest.tsv.bak"; reseal_aggregate "${fixture}/bad-candidate"; assert_rejects "${fixture}/bad-candidate" "harness candidate SHA mismatch" candidate
cp -R "${fixture}/good" "${fixture}/bad-go"; sed -i.bak 's/go1.25.0/go0.0.0/' "${fixture}/bad-go/harness-manifest.tsv"; rm "${fixture}/bad-go/harness-manifest.tsv.bak"; reseal_aggregate "${fixture}/bad-go"; assert_rejects "${fixture}/bad-go" "harness Go version mismatch" go
cp -R "${fixture}/good" "${fixture}/bad-tunnel"; printf x >>"${fixture}/bad-tunnel/ocservia-g6-tunnel"; reseal_aggregate "${fixture}/bad-tunnel"; assert_rejects "${fixture}/bad-tunnel" "tunnel SHA mismatch" tunnel
cp -R "${fixture}/good" "${fixture}/bad-harness"; printf x >>"${fixture}/bad-harness/ocservia-g6-harness"; reseal_aggregate "${fixture}/bad-harness"; assert_rejects "${fixture}/bad-harness" "harness SHA mismatch" harness
cp -R "${fixture}/good" "${fixture}/bad-tunnel-sha"; edit_tsv "${fixture}/bad-tunnel-sha/tunnel-manifest.tsv" ocservia-g6-tunnel not-a-sha; reseal_aggregate "${fixture}/bad-tunnel-sha"; assert_rejects "${fixture}/bad-tunnel-sha" "invalid tunnel SHA" tunnel-sha
cp -R "${fixture}/good" "${fixture}/bad-harness-sha"; edit_tsv "${fixture}/bad-harness-sha/harness-manifest.tsv" ocservia-g6-harness not-a-sha; reseal_aggregate "${fixture}/bad-harness-sha"; assert_rejects "${fixture}/bad-harness-sha" "invalid harness SHA" harness-sha
cp -R "${fixture}/good" "${fixture}/bad-images"; sed -i.bak '$d' "${fixture}/bad-images/image-ids.tsv"; rm "${fixture}/bad-images/image-ids.tsv.bak"; reseal_aggregate "${fixture}/bad-images"; assert_rejects "${fixture}/bad-images" "unexpected image ID count" image-count
cp -R "${fixture}/good" "${fixture}/bad-image-id"; sed -i.bak '1s/sha256:/md5:/' "${fixture}/bad-image-id/image-ids.tsv"; rm "${fixture}/bad-image-id/image-ids.tsv.bak"; reseal_aggregate "${fixture}/bad-image-id"; assert_rejects "${fixture}/bad-image-id" "malformed image IDs" image-id
cp -R "${fixture}/good" "${fixture}/bad-image-entry"; sed -i.bak "s/^postgres:17\.10-bookworm${tab}/postgres:17.11-bookworm${tab}/" "${fixture}/bad-image-entry/image-ids.tsv"; rm "${fixture}/bad-image-entry/image-ids.tsv.bak"; reseal_aggregate "${fixture}/bad-image-entry"; assert_rejects "${fixture}/bad-image-entry" "invalid ${fixture}/bad-image-entry/image-ids.tsv entry postgres:17.10-bookworm" image-entry
cp -R "${fixture}/good" "${fixture}/bad-entry"; printf 'extra\tvalue\n' >>"${fixture}/bad-entry/tunnel-manifest.tsv"; reseal_aggregate "${fixture}/bad-entry"; assert_rejects "${fixture}/bad-entry" "unexpected ${fixture}/bad-entry/tunnel-manifest.tsv entry count" manifest-entry
cp -R "${fixture}/good" "${fixture}/bad-manifest-duplicate"; awk -F '\t' -v OFS='\t' -v cand="${candidate}" '$1 == "go_version" { $1 = "candidate_sha"; $2 = cand } { print }' "${fixture}/good/harness-manifest.tsv" >"${fixture}/bad-manifest-duplicate/harness-manifest.tsv"; reseal_aggregate "${fixture}/bad-manifest-duplicate"; assert_rejects "${fixture}/bad-manifest-duplicate" "malformed ${fixture}/bad-manifest-duplicate/harness-manifest.tsv" manifest-duplicate
cp -R "${fixture}/good" "${fixture}/bad-manifest-fields"; awk -F '\t' -v OFS='\t' 'NR == 2 { $3 = "extra" } { print }' "${fixture}/good/tunnel-manifest.tsv" >"${fixture}/bad-manifest-fields/tunnel-manifest.tsv"; reseal_aggregate "${fixture}/bad-manifest-fields"; assert_rejects "${fixture}/bad-manifest-fields" "malformed ${fixture}/bad-manifest-fields/tunnel-manifest.tsv" manifest-fields
