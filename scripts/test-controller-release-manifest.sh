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
expected_head=0
for migration in "${ROOT}"/control-plane/migrations/*.up.sql; do
  number="$(basename "${migration}")"
  number="${number%%_*}"
  if (( 10#${number} > expected_head )); then expected_head=$((10#${number})); fi
done
[[ "${expected_head}" -gt 0 ]]
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
for role in edge relay signer mysql_backup mariadb_backup; do
  integrated_args+=(--image "${role}=registry.test/${role}@${digest}")
done
node "${GENERATOR}" --output "${fixture}/manifest-v2.json" --manifest-version 2 \
  "${common_args[@]}" "${image_args[@]}" "${integrated_args[@]}"
jq -e '.manifest_version == 2 and .signer_state_version == 1 and
  (.images | keys == ["backup", "control", "edge", "gateway", "mariadb_backup", "mysql_backup", "otel", "postgres", "relay", "signer", "transport"])' \
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

ruby -r yaml -r tmpdir -r open3 - "${ROOT}/.github/workflows/release.yml" \
  "${ROOT}/docs/operations/production-deployment.md" \
  "${ROOT}/scripts/release-controller-image-smoke.sh" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
jobs = workflow.fetch("jobs")
products = YAML.safe_load(File.read(File.join(File.dirname(ARGV.fetch(0)), "release-products.yml")), aliases: true)
controller = products.fetch("jobs").fetch("build-controller-images")
validate = jobs.fetch("validate-release-packages")
publish = jobs.fetch("publish-release-packages")
smoke = File.read(ARGV.fetch(2))
# The workflow owns draft -> upload -> publish and starts from a version tag.
triggers = workflow.key?("on") ? workflow.fetch("on") : workflow.fetch(true)
push_trigger = triggers.fetch("push")
abort("Release workflow must trigger only on version tag pushes") unless
  !push_trigger.key?("branches") && push_trigger.fetch("tags") == ["v*.*.*"]
abort("Release workflow must not trigger on release publication") if triggers.key?("release")
abort("Controller image builds must not rebuild accepted tag-push products") unless
  controller.fetch("if") == "github.event_name == 'workflow_dispatch'"
reuse = products.fetch("jobs").fetch("reuse-accepted")
abort("Tag-push products must reuse exact accepted artifacts") unless
  reuse.fetch("if") == "github.event_name == 'push'" &&
  reuse.fetch("steps").any? { |step| step.fetch("run", "").include?("scripts/reuse-accepted-products.mjs") }
abort("Controller publishing must require an uncancelled tag push and successful Release Check") unless
  publish.fetch("if") == "${{ !cancelled() && github.event_name == 'push' && needs.release-check.result == 'success' }}"
# The build legs must stay source-only: no registry credential may exist
# before the reviewer-gated publishing job.
abort("Controller image build legs must only read source") unless controller.fetch("permissions") == {
  "contents" => "read"
}
abort("Controller image build legs must not run in a protected environment") if
  controller.key?("environment")
security = jobs.fetch("controller-image-security")
abort("Controller image security job must gate tag pushes and full multi-arch dispatch dry runs") unless
  security.fetch("if") == "github.event_name == 'push' || inputs.arch == 'all'"
abort("Controller image security job must wait for the build legs") unless
  %w[build-amd64 build-arm64].all? { |id| Array(security.fetch("needs")).include?(id) }
abort("Controller image security job must only read source") unless
  security.fetch("permissions") == {"contents" => "read"}
abort("Controller image security job must not run in a protected environment") if
  security.key?("environment")
security_steps = Array(security.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
security_uses = Array(security.fetch("steps")).map { |step| step["uses"] }.compact
security_uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
abort("Controller image security job must bootstrap the pinned scanner") unless
  security_steps.include?("scripts/bootstrap.sh image-security")
abort("Controller image security job must scan the built image archives") unless
  security_steps.include?("IMAGE_ARCHIVES_TSV") && security_steps.include?("scripts/scan-release-images.sh")
%w[docker\ login docker\ push imagetools].each do |forbidden|
  abort("Controller image security job must not write to a registry: #{forbidden}") if
    security_steps.include?(forbidden)
end
gated = jobs.values.select { |job| job["environment"] == "release-publishing" }
abort("Exactly one release-publishing gated job must exist") unless gated.length == 1
abort("The release-publishing environment must gate the publish job only") unless gated.first == publish
arch_input = triggers.fetch("workflow_dispatch").fetch("inputs").fetch("arch")
abort("Dispatch must default to all supported architectures") unless
  arch_input.fetch("default") == "all" && arch_input.fetch("options").sort == %w[all amd64 arm64]
%w[amd64 arm64].each do |arch|
  caller = jobs.fetch("build-#{arch}")
  abort("missing native product producer for #{arch}") unless
    caller.fetch("uses") == "./.github/workflows/release-products.yml" && caller.dig("with", "arch") == arch
  abort("#{arch} products must support tag pushes and selected dispatches") unless
    caller.fetch("if").include?("github.event_name == 'push'") &&
    caller.fetch("if").include?("inputs.arch == 'all'") && caller.fetch("if").include?("inputs.arch == '#{arch}'")
end
products.fetch("jobs").each do |id, job|
  abort("#{id} must run on the selected native architecture") unless
    job.fetch("runs-on") == "${{ inputs.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}"
  expected_permissions = {"contents" => "read"}
  expected_permissions["actions"] = "read" unless id == "build-controller-images"
  abort("#{id} must remain a read-only product producer") unless
    job.fetch("permissions", products.fetch("permissions")) == expected_permissions &&
    !job.key?("environment") && !job.key?("secrets") && !job.to_s.include?("secrets.")
end
uses = Array(controller.fetch("steps")).map { |step| step["uses"] }.compact
uses.each do |use|
  abort("Controller release action is not SHA-pinned: #{use}") unless use.start_with?("./") || use.match?(/@[0-9a-f]{40}$/)
end
run_steps = Array(controller.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
abort("Controller release must use the shared native build entrypoint") unless
  run_steps.include?("bash scripts/build-release-controller.sh")
build_script = File.read(File.expand_path("../../scripts/build-release-controller.sh", File.dirname(ARGV.fetch(0))))
abort("Controller image legs must build one matrix platform per leg") unless
  build_script.include?('--platform "linux/${CONTROLLER_ARCH}"')
abort("Controller image legs must export Docker image archives") unless
  build_script.include?("type=docker,dest=") &&
  !build_script.include?("type=oci,dest=")
abort("Controller image legs must smoke the built images on the native runner") unless
  run_steps.include?("scripts/release-controller-image-smoke.sh")
abort("Controller image smoke must load the persisted Docker archive") unless
  smoke.include?('docker load --input "${archive}"') &&
  smoke.include?("docker image inspect --format '{{.Os}}/{{.Architecture}}'") &&
  !smoke.include?("archive_config_digest") &&
  !smoke.include?("tar -xOf")
%w[docker\ login push=true imagetools].each do |forbidden|
  abort("Controller image legs must not write to a registry: #{forbidden}") if
    (run_steps + build_script).include?(forbidden)
end

# The Release Check failure matrix and its Publish dependency are exercised
# by test-release-upgrade.sh, including image-security failures.
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
scanner_gate = publish.fetch("steps").find { |step| step["name"] == "Verify scanner summary at the publishing boundary" }
image_push = publish.fetch("steps").find { |step| step["id"] == "controller-index" }
abort("Publishing must read the scanner artifact's preserved subdirectory") unless
  image_push.dig("env", "SECURITY_SUMMARY") == '${{ runner.temp }}/image-security/controller-image-security/controller-image-security.json' &&
  publish_steps.include?('find "${RUNNER_TEMP}/image-security/controller-image-security" -maxdepth 1 -type f')
Dir.mktmpdir("ocservia-scanner-artifact-gate") do |dir|
  Dir.mkdir("#{dir}/image-security")
  Dir.mkdir("#{dir}/image-security/controller-image-security")
  summary = "#{dir}/image-security/controller-image-security/controller-image-security.json"
  File.write(summary, "{}\n")
  digest, status = Open3.capture2("sha256sum", summary)
  abort("Cannot hash scanner fixture") unless status.success?
  env = {"RUNNER_TEMP" => dir, "EXPECTED" => digest.split.first}
  _, status = Open3.capture2e(env, "bash", "-euo", "pipefail", "-c", scanner_gate.fetch("run"))
  abort("Publishing rejected the scanner artifact layout") unless status.success?
  File.write(summary, "changed\n")
  _, status = Open3.capture2e(env, "bash", "-euo", "pipefail", "-c", scanner_gate.fetch("run"))
  abort("Publishing accepted an altered scanner summary") if status.success?
end
abort("Controller publishing must not run the image scanner itself") unless
  !publish_steps.include?("scripts/scan-release-images.sh") &&
    !publish_steps.include?("scripts/bootstrap.sh image-security")
abort("Controller publishing must prove it pushes the scanned images") unless
  publish_steps.include?('.images[$name].platforms[$platform].config_digest') &&
  publish_steps.include?("docker image inspect --format '{{.Id}}'")
abort("Controller publishing must ship the scan binding record") unless
  publish_steps.include?("controller-image-security-bindings.json")
abort("Controller publishing must bind the pushed index digests into the scan binding record") unless
  publish_steps.include?("index_digest: .[0][4]")
abort("Controller publishing must verify the binding record against the final release manifests") unless
  publish_steps.include?(".images[$name].platforms[$arch].manifest_digest") &&
  publish_steps.include?("controller-release-${platform}.json")
final_gate = publish.fetch("steps").find { |step| step["name"] == "Verify signed release manifest" }
abort("Final release validation must retain the trusted key pin") unless
  final_gate.fetch("env").fetch("AGENT_TRUSTED_KEY_SHA256") == '${{ secrets.AGENT_TRUSTED_KEY_SHA256 }}'
pre_sign = publish.fetch("steps").find { |step| step["name"] == "Validate package set against the pinned release key" }
abort("Pre-sign and dry-run validation must still generate unsigned manifests") unless
  pre_sign.fetch("run").include?("WRITE_SHA256SUMS=1") && validate_steps.include?("WRITE_SHA256SUMS=1")
Dir.mktmpdir("ocservia-final-signature-gate") do |dir|
  Dir.mkdir("#{dir}/assets")
  Dir.mkdir("#{dir}/scripts")
  # The gate must reject before invoking package extraction/verification.
  validator = "#{dir}/scripts/validate-release-packages.sh"
  File.write(validator, <<~SH)
    #!/usr/bin/env bash
    set -eu
    test "${CONTROLLER_RELEASE_MANIFEST_REQUIRED}" = 1
    test "${AGENT_TRUSTED_KEY_SHA256}" = "#{'a' * 64}"
    test -s "${ASSET_DIR}/SHA256SUMS.sig"
    test "$1" = manifest
    test -n "${PAYLOAD_RECEIPT}"
    touch validation-called
  SH
  File.chmod(0755, validator)
  signature = "#{dir}/assets/SHA256SUMS.sig"
  env = {"RUNNER_TEMP" => dir, "version" => "0.2.0", "AGENT_TRUSTED_KEY_SHA256" => 'a' * 64}
  %w[missing empty symlink regular].each do |kind|
    File.unlink(signature) if File.exist?(signature) || File.symlink?(signature)
    File.write(signature, "") if kind == "empty"
    if kind == "symlink"
      File.write("#{dir}/target", "signature")
      File.symlink("#{dir}/target", signature)
    end
    File.write(signature, "signature") if kind == "regular"
    _, status = Open3.capture2e(env, "bash", "-euo", "pipefail", "-c", final_gate.fetch("run"), chdir: dir)
    expected = kind == "regular"
    abort("Final signature gate mishandled #{kind}") unless status.success? == expected
    abort("Final signature gate delegated #{kind} incorrectly") unless File.exist?("#{dir}/validation-called") == expected
  end
  File.unlink("#{dir}/validation-called")
  _, status = Open3.capture2e(env.merge("AGENT_TRUSTED_KEY_SHA256" => ""), "bash", "-euo", "pipefail", "-c", final_gate.fetch("run"), chdir: dir)
  abort("Final signature gate accepted missing key pin") if status.success? || File.exist?("#{dir}/validation-called")
end
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
