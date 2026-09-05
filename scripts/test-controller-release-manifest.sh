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
  .database_migration == 30 and
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
  "${ROOT}/docs/operations/production-deployment.md" \
  "${ROOT}/scripts/release-controller-image-smoke.sh" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
jobs = workflow.fetch("jobs")
controller = jobs.fetch("build-controller-images")
validate = jobs.fetch("validate-release-packages")
publish = jobs.fetch("publish-release-packages")
smoke = File.read(ARGV.fetch(2))
# The workflow owns draft -> upload -> publish and starts from a version tag.
triggers = workflow.key?("on") ? workflow.fetch("on") : workflow.fetch(true)
push_trigger = triggers.fetch("push")
abort("Release workflow must trigger only on version tag pushes") unless
  !push_trigger.key?("branches") && push_trigger.fetch("tags") == ["v*.*.*"]
abort("Release workflow must not trigger on release publication") if triggers.key?("release")
abort("Controller image job must run for tag-push release runs and workflow dispatch dry runs") unless
  controller.fetch("if") == "github.event_name == 'push' || github.event_name == 'workflow_dispatch'"
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
arch_input = triggers.fetch("workflow_dispatch").fetch("inputs").fetch("arch")
abort("Dispatch must default to amd64 and offer amd64, arm64, all") unless
  arch_input.fetch("default") == "amd64" && arch_input.fetch("options") == %w[amd64 arm64 all]
%w[build-agent-packages build-controller-images].each do |job_name|
  matrix = jobs.fetch(job_name).fetch("strategy").fetch("matrix").fetch("include")
  abort("#{job_name} must select native architecture legs for tag pushes and dispatch") unless
    matrix.include?("github.event_name == 'push'") && matrix.include?("inputs.arch == 'all'") &&
    matrix.include?("inputs.arch == 'arm64'") &&
    matrix.include?("ubuntu-24.04") && matrix.include?("ubuntu-24.04-arm")
end
uses = Array(controller.fetch("steps")).map { |step| step["uses"] }.compact
uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
run_steps = Array(controller.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Controller image legs must build one matrix platform per leg") unless
  run_steps.include?('--platform "linux/${{ matrix.controller_arch }}"')
abort("Controller image legs must export Docker image archives") unless
  run_steps.include?("type=docker,dest=") &&
  !run_steps.include?("type=oci,dest=")
abort("Controller image legs must build each image once") unless
  run_steps.scan("docker buildx build").length == 1
abort("Controller image legs must smoke the built images on the native runner") unless
  run_steps.include?("scripts/release-controller-image-smoke.sh")
abort("Controller image smoke must load the persisted Docker archive") unless
  smoke.include?('docker load --input "${archive}"') &&
  smoke.include?("docker image inspect --format '{{.Os}}/{{.Architecture}}'") &&
  !smoke.include?("archive_config_digest") &&
  !smoke.include?("tar -xOf")
%w[docker\ login push=true imagetools].each do |forbidden|
  abort("Controller image legs must not write to a registry: #{forbidden}") if
    run_steps.include?(forbidden)
end

abort("Controller publishing must wait for the image build legs") unless
  Array(publish.fetch("needs")).include?("build-controller-images")
validate_steps = Array(validate.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Release dry runs must prepare both versioned bootstrap assets") unless
  validate_steps.include?('scripts/prepare-bootstrap-release-assets.sh "${RUNNER_TEMP}/assets"')
abort("Controller publishing permissions are too broad") unless publish.fetch("permissions") == {
  "contents" => "write",
  "packages" => "write"
}
publish_uses = Array(publish.fetch("steps")).map { |step| step["uses"] }.compact
publish_uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
publish_steps = Array(publish.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Release publishing must prepare bootstrap assets from the release checkout") unless
  publish_steps.include?('scripts/prepare-bootstrap-release-assets.sh "${RUNNER_TEMP}/assets"')
%w[controller-bootstrap.sh managed-node-bootstrap.sh].each do |asset|
  abort("Release publishing must include #{asset}") unless
    publish_steps.include?(asset)
end
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
abort("Controller publishing must bind the tag to the source commit") unless
  publish_steps.include?("git/ref/tags/") && publish_steps.include?('${tag_sha}" != "${source_commit}')
abort("workflow dispatch must not publish Controller images") if
  publish.fetch("if").include?("workflow_dispatch")
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
