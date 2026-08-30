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
jq -e '
  .manifest_version == 1 and
  .release_version == "0.2.0" and
  .release_tag == "v0.2.0" and
  .source_commit == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .platform == "linux/amd64" and
  .database_migration == 28 and
  (.images | keys == ["backup", "control", "gateway", "otel", "postgres", "transport"]) and
  (.images | to_entries | all(.value | test("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")))
' "${fixture}/manifest-a.json" >/dev/null

missing_image_args=("${image_args[@]:0:6}" "${image_args[@]:8}")
assert_rejected missing-image "${common_args[@]}" "${missing_image_args[@]}"

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

ruby -r yaml - "${ROOT}/.github/workflows/release.yml" \
  "${ROOT}/docs/operations/production-deployment.md" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
jobs = workflow.fetch("jobs")
controller = jobs.fetch("build-controller-images")
abort("Controller image job must be release-only") unless controller.fetch("if") == "github.event_name == 'release'"
abort("Controller image permissions are too broad") unless controller.fetch("permissions") == {
  "contents" => "read",
  "packages" => "write",
  "id-token" => "write",
  "attestations" => "write"
}
uses = Array(controller.fetch("steps")).map { |step| step["uses"] }.compact
uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
attest = uses.select { |use| use.start_with?("actions/attest@") }
abort("Controller release must attest four first-party images") unless attest.length == 4
abort("Controller release must use the verified actions/attest pin") unless
  attest.all? { |use| use == "actions/attest@508db95dd578ae2727ebd6217d5ba78e4fbda05d" }
run_steps = Array(controller.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Controller release must generate its canonical manifest") unless
  run_steps.include?("scripts/generate-controller-release-manifest.mjs")
abort("Controller release must declare its supported platform") unless
  run_steps.include?("--platform linux/amd64")
abort("Controller manifest publishing must wait for the controller image job") unless
  Array(jobs.fetch("publish-release-packages").fetch("needs")).include?("build-controller-images")
abort("workflow dispatch must not publish Controller images") if
  controller.fetch("if").include?("workflow_dispatch")
abort("Controller release must check anonymous image reads") unless
  run_steps.include?("curl") && run_steps.include?("manifests/${release_tag}")
docs = File.read(ARGV.fetch(1))
abort("production docs must declare the Controller image visibility prerequisite") unless
  docs.include?("be public") && docs.include?("linux/amd64")
RUBY

echo "Controller release manifest tests passed"
