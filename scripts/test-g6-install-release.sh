#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERIFY="${ROOT}/scripts/g6-install-release.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
candidate="$(printf 'a%.0s' {1..40})"
other="$(printf 'b%.0s' {1..40})"
id="sha256:$(printf 'c%.0s' {1..64})"
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
  (cd "${dir}" && sha256sum runtime-images.tar.gz image-ids.tsv ocservia-g6-tunnel tunnel-manifest.tsv ocservia-g6-harness harness-manifest.tsv >release-artifacts.sha256)
}
assert_fails() { if G6_INSTALL_RELEASE_VERIFY_ONLY=true "${VERIFY}" "$1" "${candidate}" go1.25.0 >/dev/null 2>&1; then echo "expected release verification to fail: $2" >&2; exit 1; fi; }

make_fixture "${fixture}/good"
G6_INSTALL_RELEASE_VERIFY_ONLY=true "${VERIFY}" "${fixture}/good" "${candidate}" go1.25.0
cp -R "${fixture}/good" "${fixture}/bad-aggregate"; printf x >>"${fixture}/bad-aggregate/release-artifacts.sha256"; assert_fails "${fixture}/bad-aggregate" aggregate
cp -R "${fixture}/good" "${fixture}/bad-runtime-lookalike"; cp "${fixture}/bad-runtime-lookalike/runtime-images.tar.gz" "${fixture}/bad-runtime-lookalike/runtime-imagesXtarYgz"; sed -i.bak 's/runtime-images\.tar\.gz/runtime-imagesXtarYgz/' "${fixture}/bad-runtime-lookalike/release-artifacts.sha256"; rm "${fixture}/bad-runtime-lookalike/release-artifacts.sha256.bak"; assert_fails "${fixture}/bad-runtime-lookalike" runtime-lookalike
cp -R "${fixture}/good" "${fixture}/bad-image-ids-lookalike"; cp "${fixture}/bad-image-ids-lookalike/image-ids.tsv" "${fixture}/bad-image-ids-lookalike/image-idsXtsv"; sed -i.bak 's/image-ids\.tsv/image-idsXtsv/' "${fixture}/bad-image-ids-lookalike/release-artifacts.sha256"; rm "${fixture}/bad-image-ids-lookalike/release-artifacts.sha256.bak"; assert_fails "${fixture}/bad-image-ids-lookalike" image-ids-lookalike
cp -R "${fixture}/good" "${fixture}/bad-extra-aggregate-entry"; cp "${fixture}/bad-extra-aggregate-entry/image-ids.tsv" "${fixture}/bad-extra-aggregate-entry/extra-payload"; (cd "${fixture}/bad-extra-aggregate-entry" && sha256sum extra-payload >>release-artifacts.sha256); assert_fails "${fixture}/bad-extra-aggregate-entry" extra-aggregate-entry
cp -R "${fixture}/good" "${fixture}/bad-duplicate-aggregate-entry"; duplicate_entry="$(head -n 1 "${fixture}/bad-duplicate-aggregate-entry/release-artifacts.sha256")"; printf '%s\n' "${duplicate_entry}" >>"${fixture}/bad-duplicate-aggregate-entry/release-artifacts.sha256"; assert_fails "${fixture}/bad-duplicate-aggregate-entry" duplicate-aggregate-entry
cp -R "${fixture}/good" "${fixture}/bad-missing-aggregate-entry"; sed -i.bak '/ocservia-g6-harness$/d' "${fixture}/bad-missing-aggregate-entry/release-artifacts.sha256"; rm "${fixture}/bad-missing-aggregate-entry/release-artifacts.sha256.bak"; assert_fails "${fixture}/bad-missing-aggregate-entry" missing-aggregate-entry
cp -R "${fixture}/good" "${fixture}/bad-aggregate-whitespace"; sed -i.bak '1s/  /   /' "${fixture}/bad-aggregate-whitespace/release-artifacts.sha256"; rm "${fixture}/bad-aggregate-whitespace/release-artifacts.sha256.bak"; assert_fails "${fixture}/bad-aggregate-whitespace" aggregate-whitespace
cp -R "${fixture}/good" "${fixture}/bad-leading-star-aggregate"; sed -i.bak '1s/  /  */' "${fixture}/bad-leading-star-aggregate/release-artifacts.sha256"; rm "${fixture}/bad-leading-star-aggregate/release-artifacts.sha256.bak"; assert_fails "${fixture}/bad-leading-star-aggregate" leading-star-aggregate
cp -R "${fixture}/good" "${fixture}/bad-candidate"; sed -i.bak "s/${candidate}/${other}/" "${fixture}/bad-candidate/harness-manifest.tsv"; rm "${fixture}/bad-candidate/harness-manifest.tsv.bak"; assert_fails "${fixture}/bad-candidate" candidate
cp -R "${fixture}/good" "${fixture}/bad-harness"; printf x >>"${fixture}/bad-harness/ocservia-g6-harness"; assert_fails "${fixture}/bad-harness" harness
cp -R "${fixture}/good" "${fixture}/bad-tunnel"; printf x >>"${fixture}/bad-tunnel/ocservia-g6-tunnel"; assert_fails "${fixture}/bad-tunnel" tunnel
cp -R "${fixture}/good" "${fixture}/bad-go"; sed -i.bak 's/go1.25.0/go0.0.0/' "${fixture}/bad-go/harness-manifest.tsv"; rm "${fixture}/bad-go/harness-manifest.tsv.bak"; assert_fails "${fixture}/bad-go" go
cp -R "${fixture}/good" "${fixture}/bad-images"; sed -i.bak '$d' "${fixture}/bad-images/image-ids.tsv"; rm "${fixture}/bad-images/image-ids.tsv.bak"; assert_fails "${fixture}/bad-images" image-count
cp -R "${fixture}/good" "${fixture}/bad-image-id"; sed -i.bak '1s/c/d/' "${fixture}/bad-image-id/image-ids.tsv"; rm "${fixture}/bad-image-id/image-ids.tsv.bak"; assert_fails "${fixture}/bad-image-id" image-id
cp -R "${fixture}/good" "${fixture}/bad-entry"; printf 'extra\tvalue\n' >>"${fixture}/bad-entry/tunnel-manifest.tsv"; assert_fails "${fixture}/bad-entry" manifest-entry
