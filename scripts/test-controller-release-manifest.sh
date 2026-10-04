#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GENERATOR="${ROOT}/scripts/generate-controller-release-manifest.mjs"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

commit="$(printf 'a%.0s' {1..40})"
digest="sha256:$(printf 'b%.0s' {1..64})"
common_args=(
  --release-version 0.2.0
  --release-tag v0.2.0
  --source-commit "${commit}"
  --migration-dir "${ROOT}/control-plane/migrations"
  --platform linux/amd64
)
image_args=(
  --image "gateway=ghcr.io/gentlekingson/ocservia/gateway@${digest}"
  --image "control=ghcr.io/gentlekingson/ocservia/control@${digest}"
  --image "transport=ghcr.io/gentlekingson/ocservia/transport@${digest}"
  --image "backup=ghcr.io/gentlekingson/ocservia/backup@${digest}"
  --image "postgres=docker.io/library/postgres@${digest}"
  --image "otel=docker.io/otel/opentelemetry-collector@${digest}"
)

run_manifest() {
  local output="$1"
  node "${GENERATOR}" --output "${output}" "${common_args[@]}" "${image_args[@]}"
}

assert_rejected() {
  local label="$1"
  shift
  if node "${GENERATOR}" --output "${fixture}/${label}.json" "$@" >/dev/null 2>&1; then
    echo "expected manifest generation to fail: ${label}" >&2
    exit 1
  fi
}

run_manifest "${fixture}/manifest-a.json"
run_manifest "${fixture}/manifest-b.json"
cmp -s "${fixture}/manifest-a.json" "${fixture}/manifest-b.json"
expected_head="$(awk -F= '/^-- ocservia:epoch=/{epoch=$2} /^-- ocservia:revision=/{revision=$2} END {printf "{\"epoch\":%s,\"revision\":%s}", epoch, revision}' "${ROOT}/control-plane/migrations/schema.sql")"
jq -e --argjson expected_head "${expected_head}" '
  .manifest_version == 1 and
  .release_version == "0.2.0" and
  .release_tag == "v0.2.0" and
  .source_commit == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .platform == "linux/amd64" and
  .database_migration == $expected_head and
  (.images | keys == ["backup", "control", "gateway", "otel", "postgres", "transport"]) and
  (.images | to_entries | all(.value | test("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")))
' "${fixture}/manifest-a.json" >/dev/null

arm64_args=("${common_args[@]}")
integrated_args=()
for role in edge relay signer mysql_backup; do
  integrated_args+=(--image "${role}=registry.test/${role}@${digest}")
done
node "${GENERATOR}" --output "${fixture}/manifest-v2.json" --manifest-version 2 \
  "${common_args[@]}" "${image_args[@]}" "${integrated_args[@]}"
jq -e '.manifest_version == 2 and .signer_state_version == 1 and
  (.images | keys == ["backup", "control", "edge", "gateway", "mysql_backup", "otel", "postgres", "relay", "signer", "transport"])' \
  "${fixture}/manifest-v2.json" >/dev/null
assert_rejected v1-extra-images "${common_args[@]}" "${image_args[@]}" "${integrated_args[@]}"
assert_rejected v2-missing-images --manifest-version 2 "${common_args[@]}" "${image_args[@]}"
assert_rejected v3 --manifest-version 3 "${common_args[@]}" "${image_args[@]}"
arm64_args[9]=linux/arm64
node "${GENERATOR}" --output "${fixture}/manifest-arm64.json" "${arm64_args[@]}" "${image_args[@]}"
jq -e '.platform == "linux/arm64"' "${fixture}/manifest-arm64.json" >/dev/null
platform_changes="$(diff -u "${fixture}/manifest-a.json" "${fixture}/manifest-arm64.json" \
  | grep -E '^[+-][^+-]' || true)"
if [[ "${platform_changes}" != $'-  "platform": "linux/amd64",\n+  "platform": "linux/arm64",' ]]; then
  echo "platform manifests must differ only in the platform field:" >&2
  printf '%s\n' "${platform_changes}" >&2
  exit 1
fi

unsupported_platform_args=("${common_args[@]}")
unsupported_platform_args[9]=linux/ppc64le
assert_rejected unsupported-platform "${unsupported_platform_args[@]}" "${image_args[@]}"

missing_platform_args=("${common_args[@]:0:8}")
assert_rejected missing-platform "${missing_platform_args[@]}" "${image_args[@]}"

missing_image_args=("${image_args[@]:0:6}" "${image_args[@]:8}")
assert_rejected missing-image "${common_args[@]}" "${missing_image_args[@]}"

version_image_args=("${image_args[@]}")
for ((index = 1; index < ${#version_image_args[@]}; index += 2)); do
  version_image_args[$index]="${version_image_args[$index]/@${digest}/:v0.2.0}"
done
node "${GENERATOR}" --output "${fixture}/version-images.json" "${common_args[@]}" "${version_image_args[@]}"
jq -e 'all(.images[]; endswith(":v0.2.0"))' "${fixture}/version-images.json" >/dev/null

mutable_image_args=("${image_args[@]}")
mutable_image_args[1]="gateway=ghcr.io/gentlekingson/ocservia/gateway:latest"
assert_rejected mutable-image "${common_args[@]}" "${mutable_image_args[@]}"

malformed_digest_args=("${image_args[@]}")
malformed_digest_args[3]="control=ghcr.io/gentlekingson/ocservia/control@sha256:deadbeef"
assert_rejected malformed-digest "${common_args[@]}" "${malformed_digest_args[@]}"

bad_source_args=("${common_args[@]}")
bad_source_args[5]="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
assert_rejected source-commit "${bad_source_args[@]}" "${image_args[@]}"

bad_tag_args=("${common_args[@]}")
bad_tag_args[3]=v0.2.1
assert_rejected release-tag "${bad_tag_args[@]}" "${image_args[@]}"

rc_args=("${common_args[@]}")
rc_args[1]=0.2.0-rc.1
rc_args[3]=v0.2.0-rc.1
rc_images=("${version_image_args[@]}")
for ((index = 1; index < ${#rc_images[@]}; index += 2)); do
  rc_images[index]="${rc_images[index]}-rc.1"
done
node "${GENERATOR}" --output "${fixture}/rc.json" "${rc_args[@]}" "${rc_images[@]}"
jq -e '.release_version == "0.2.0-rc.1" and .release_tag == "v0.2.0-rc.1" and
  all(.images[]; endswith(":v0.2.0-rc.1"))' "${fixture}/rc.json" >/dev/null
for invalid in 0.2.0-rc.0 0.2.0-rc.01 0.2.0-rc1 0.2.0-beta.1 0.2.0+build 0.2.0-rc.1+build; do
  rc_args[1]="${invalid}"
  rc_args[3]="v${invalid}"
  assert_rejected "${invalid}" "${rc_args[@]}" "${rc_images[@]}"
done

echo "Controller deployment configuration tests passed"
