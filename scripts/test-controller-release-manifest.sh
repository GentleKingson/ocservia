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
jq -e '
  .manifest_version == 1 and
  .release_version == "0.2.0" and
  .release_tag == "v0.2.0" and
  .source_commit == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .platform == "linux/amd64" and
  .database_migration == 29 and
  (.images | keys == ["backup", "control", "gateway", "otel", "postgres", "transport"]) and
  (.images | to_entries | all(.value | test("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")))
' "${fixture}/manifest-a.json" >/dev/null

arm64_args=("${common_args[@]}")
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
publish = jobs.fetch("publish-release-packages")
# Immutable-release publication model: the workflow owns the whole
# draft -> upload -> publish lifecycle (draft releases fire no workflow
# events), so it must be triggered by pushing the version tag itself and
# must never react to release publication.
triggers = workflow.key?("on") ? workflow.fetch("on") : workflow.fetch(true)
push_trigger = triggers.fetch("push")
abort("Release workflow must trigger only on version tag pushes") unless
  !push_trigger.key?("branches") && push_trigger.fetch("tags") == ["v*.*.*"]
abort("Release workflow must not trigger on release publication") if triggers.key?("release")
abort("Controller image job must run only for tag-push release runs") unless
  controller.fetch("if") == "github.event_name == 'push'"
abort("Controller publishing must run only for tag-push release runs") unless
  publish.fetch("if") == "github.event_name == 'push'"
# The build legs must stay source-only: no registry credential may exist
# before the reviewer-gated publishing job.
abort("Controller image build legs must only read source") unless controller.fetch("permissions") == {
  "contents" => "read"
}
abort("Controller image build legs must not run in a protected environment") if
  controller.key?("environment")
gated = jobs.values.select { |job| job["environment"] == "release-publishing" }
abort("Exactly one release-publishing gated job must exist") unless gated.length == 1
abort("The release-publishing environment must gate the publish job only") unless gated.first == publish
matrix_includes = Array(controller.fetch("strategy").fetch("matrix").fetch("include"))
controller_arches = matrix_includes.map { |entry| entry["controller_arch"] }.sort
abort("Controller image build must target amd64 and arm64") unless controller_arches == %w[amd64 arm64]
matrix_includes.each do |entry|
  expected_runner = entry["controller_arch"] == "amd64" ? "ubuntu-24.04" : "ubuntu-24.04-arm"
  abort("Controller #{entry['controller_arch']} leg must build natively on #{expected_runner}") unless
    entry["runs_on"] == expected_runner
end
uses = Array(controller.fetch("steps")).map { |step| step["uses"] }.compact
uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
run_steps = Array(controller.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Controller image legs must build one matrix platform per leg") unless
  run_steps.include?('--platform "linux/${{ matrix.controller_arch }}"')
abort("Controller image legs must export OCI archives instead of pushing") unless
  run_steps.include?("type=oci,dest=")
%w[docker\ login push=true imagetools].each do |forbidden|
  abort("Controller image legs must not write to a registry: #{forbidden}") if
    run_steps.include?(forbidden)
end

abort("Controller publishing must wait for the image build legs") unless
  Array(publish.fetch("needs")).include?("build-controller-images")
abort("Controller publishing permissions are too broad") unless publish.fetch("permissions") == {
  "contents" => "write",
  "packages" => "write",
  "id-token" => "write",
  "attestations" => "write"
}
publish_uses = Array(publish.fetch("steps")).map { |step| step["uses"] }.compact
publish_uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
attest = publish_uses.select { |use| use.start_with?("actions/attest@") }
abort("Controller release must attest four first-party images") unless attest.length == 4
abort("Controller release must use the verified actions/attest pin") unless
  attest.all? { |use| use == "actions/attest@508db95dd578ae2727ebd6217d5ba78e4fbda05d" }
publish_steps = Array(publish.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Controller publishing must load the built image archives") unless
  publish_steps.include?("docker load --input")
abort("Controller publishing must push the per-platform images") unless
  publish_steps.include?("docker push")
abort("Controller publishing must merge per-platform digests into one index") unless
  publish_steps.include?("docker buildx imagetools create")
abort("Controller release must generate its canonical manifests") unless
  publish_steps.include?("scripts/generate-controller-release-manifest.mjs") &&
    publish_steps.include?("--platform \"linux/${platform}\"") &&
    publish_steps.include?("controller-release-amd64.json") &&
    publish_steps.include?("controller-release-arm64.json") &&
    publish_steps.include?("controller-release.json")
%w[linux/amd64 linux/arm64].each do |platform|
  abort("Controller release must fail closed when an index lacks #{platform}") unless
    publish_steps.include?("grep -Fxq '#{platform}'")
end
create_at = publish_steps.index("gh release create")
upload_at = publish_steps.index("gh release upload")
publish_at = publish_steps.index("--draft=false")
abort("Controller publishing must create a draft, upload every asset, then publish") unless
  !create_at.nil? && !upload_at.nil? && !publish_at.nil? &&
  create_at < upload_at && upload_at < publish_at
abort("Controller publishing must never clobber release assets") if
  publish_steps.include?("--clobber")
abort("Controller release must verify the published release attestation") unless
  publish_steps.include?("gh release verify")
abort("Controller release must fail closed on a mutable release") unless
  publish_steps.include?("--jq .immutable")
abort("Controller publishing must preflight the immutable-releases prerequisite") unless
  publish_steps.include?("immutable-releases") && publish_steps.include?("REPO_ADMIN_READ_TOKEN")
abort("Controller publishing must bind the tag to the source commit before production writes") unless
  publish_steps.include?("check-tag-binding.sh") &&
  publish_steps.include?("git/ref/tags/")
publish_defs = Array(publish.fetch("steps"))
publishes_draft = publish_defs.select { |step| step.is_a?(Hash) && step["run"].to_s.include?("--draft=false") }
abort("Controller publishing must re-verify the tag binding immediately before publishing") unless
  publishes_draft.length == 1 &&
  publishes_draft.first["run"].include?("check-tag-binding.sh") &&
  publishes_draft.first["run"].index("check-tag-binding.sh") < publishes_draft.first["run"].index("gh release edit")
abort("Controller publishing must re-check the immutable prerequisite immediately before publishing") unless
  publishes_draft.first["run"].include?("check-immutable-prereq.sh") &&
  publishes_draft.first["run"].index("check-immutable-prereq.sh") < publishes_draft.first["run"].index("gh release edit")
abort("Publish job must keep a read-only recovery path for already-published releases") unless
  publish_steps.include?("verified in place; nothing was modified")
recovery_step = publish_defs.find { |step| step.is_a?(Hash) && step["run"].to_s.include?("verified in place; nothing was modified") }
abort("Read-only recovery must chain the published assets to the pinned release key") unless
  !recovery_step.nil? &&
  recovery_step["run"].to_s.include?("scripts/validate-release-packages.sh") &&
  recovery_step.fetch("env", {}).fetch("AGENT_TRUSTED_KEY_SHA256", "") ==
    "${{ secrets.AGENT_TRUSTED_KEY_SHA256 }}"
state_step = publish_defs.find { |step| step.is_a?(Hash) && step["id"] == "release_state" }
state_run = state_step.nil? ? "" : state_step["run"].to_s
abort("Release-state detection must fail closed on a state-query failure, not fall back to the publish path") unless
  !state_step.nil? &&
  state_run.include?("%{http_code}") &&
  state_run.include?("404") &&
  state_run.include?("exit 1") &&
  !state_run.include?("gh release view")
login_step = publish_defs.find { |step| step.is_a?(Hash) && step["run"].to_s.include?("docker login") }
abort("Production writes must be skipped by the read-only recovery path") unless
  !login_step.nil? && login_step["if"] == "steps.release_state.outputs.mode != 'verify-published'"
abort("workflow dispatch must not publish Controller images") if
  controller.fetch("if").include?("workflow_dispatch")
abort("Controller release must use the GHCR anonymous token flow") unless
  publish_steps.include?("https://ghcr.io/token") &&
    publish_steps.include?("scope=repository:gentlekingson/ocservia/${name}:pull") &&
    publish_steps.include?("Authorization: Bearer") &&
    publish_steps.include?("manifests/${digest}") &&
    !publish_steps.include?("manifests/${release_tag}")
docs = File.read(ARGV.fetch(1))
abort("production docs must declare the Controller image visibility prerequisite") unless
  docs.include?("be public") && docs.include?("linux/amd64") && docs.include?("linux/arm64")
RUBY

echo "Controller release manifest tests passed"
