#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml - "${ROOT}/.github/workflows/g6-readiness.yml" \
  "${ROOT}/.github/workflows/g6-harness-smoke.yml" \
  "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
formal_path, smoke_path, core_path = ARGV
formal = YAML.safe_load(File.read(formal_path), aliases: true)
smoke = YAML.safe_load(File.read(smoke_path), aliases: true)
core = YAML.safe_load(File.read(core_path), aliases: true)
abort("formal caller must remain workflow_dispatch-only") unless formal.fetch(true).keys == ["workflow_dispatch"]
abort("smoke caller must remain pull_request-only") unless smoke.fetch(true).keys == ["pull_request"]
[formal, smoke, core].each do |workflow|
  abort("G6 workflow permissions must remain read-only") unless workflow.fetch("permissions") == {"contents" => "read", "actions" => "read"}
end
abort("formal caller must stay thin") unless formal.fetch("jobs").keys == ["g6-harness-core"]
abort("smoke caller must keep relevance and one reusable core call") unless smoke.fetch("jobs").keys.sort == %w[g6-harness-core g6-smoke-relevance]
smoke_relevance = smoke.fetch("jobs").fetch("g6-smoke-relevance")
abort("smoke caller must use the unified three-dot impact classifier") unless
  Array(smoke_relevance.fetch("steps")).any? { |step| step.fetch("run", "").include?("scripts/ci-relevance.sh pull_request") } &&
    smoke_relevance.fetch("outputs").fetch("relevant").include?("run_g6_smoke")
abort("core must remain workflow_call-only") unless core.fetch(true).keys == ["workflow_call"]
inputs = core.fetch(true).fetch("workflow_call").fetch("inputs")
abort("core inputs drifted") unless inputs.keys.sort == %w[authority candidate_sha profile smoke_relevant]
abort("smoke relevance must be typed") unless inputs.fetch("smoke_relevant").values_at("type", "required") == ["boolean", true]
jobs = core.fetch("jobs")
required = %w[g6-contract g6-rd-release-image g6-rd-fd-a g6-rd-fd-b g6-rd-assemble g6-rd-secret-scan g6-rd-verifier g6-rd-gate g6-smoke-release g6-smoke-fd-a g6-smoke-fd-b g6-smoke-assemble g6-smoke-secret-scan g6-smoke-verifier g6-smoke-result]
abort("core semantic layers are incomplete") unless (required - jobs.keys).empty?
jobs.each do |id, job|
  abort("#{id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  abort("#{id} must have a timeout") unless job.fetch("timeout-minutes") > 0
  Array(job.fetch("steps", [])).each do |step|
    uses = step["uses"]
    next unless uses
    abort("#{id} has an unpinned action") unless uses.start_with?("./") || uses.match?(/@[0-9a-f]{40}\z/)
  end
end
RUBY

TIMING_HELPER="${ROOT}/scripts/g6-timing.sh"
INSTALL_HELPER="${ROOT}/scripts/g6-install-release.sh"
for stage in runner_preparation toolchain_bootstrap candidate_docker_image_build \
  docker_save_gzip fd_artifact_download checksum_provenance_verification \
  docker_load scenario_execution peer_observation_wait observation_300_seconds \
  evidence_collection release_artifact_upload \
  secret_scan_tool_bootstrap secret_scan_artifact_download \
  secret_scan_execution secret_scan_result_upload; do
  grep -qF "${stage}" "${ROOT}/.github/workflows/g6-harness-core.yml" \
    "${TIMING_HELPER}" "${INSTALL_HELPER}" \
    || { echo "G6 timing stage is missing: ${stage}" >&2; exit 1; }
done
# Per-build telemetry inside the candidate image build: every lane of both
# release producers (formal and smoke) must carry its own duration mark so a
# regression can be attributed to one build graph, and every frozen image
# must record its final size and image ID.
for stage in control_plane_build relay_build rust_workspace_build \
  transportd_build g6_probe_build g6_agent_build; do
  grep -qF "${stage}" "${ROOT}/.github/workflows/g6-harness-core.yml" \
    || { echo "G6 per-image build timing stage is missing: ${stage}" >&2; exit 1; }
done
measure_count="$(grep -cF 'g6-timing.sh measure' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${measure_count}" -ne 12 ]]; then
  echo "both release producers must time all six build lanes individually (expected 12 measures, found ${measure_count})" >&2
  exit 1
fi
image_mark_count="$(grep -cF 'record_image_timing ' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${image_mark_count}" -ne 10 ]]; then
  echo "both release producers must record all five frozen images (expected 10 marks, found ${image_mark_count})" >&2
  exit 1
fi
grep -qF 'g6-timing.sh image' "${ROOT}/.github/workflows/g6-harness-core.yml" \
  || { echo "release producers must record per-image size and image ID" >&2; exit 1; }

# Persistent BuildKit caches: both release producers must run a
# run-scoped docker-container builder, split caches across the three
# disjoint lane scopes, export only the heavyweight builds with mode=max,
# tolerate cache export failures, rebuild mutable runtime package stages, load
# every tagged build into the local store for freezing, and remove the builder
# in the always-cleanup step. Cache credentials are optional, so a cold build
# must remain possible when the runner does not provide them.
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
core = YAML.safe_load(File.read(ARGV[0]), aliases: true)
jobs = core.fetch("jobs")
%w[g6-rd-release-image g6-smoke-release].each do |job_id|
  steps = Array(jobs.fetch(job_id).fetch("steps"))
  prep = steps.find { |step| step["name"] == "Prepare the BuildKit cache builder" }
  relay = steps.find { |step| step["name"] == "Relay Actions cache credentials to the job environment" }
  freeze = steps.find do |step|
    run = step.fetch("run", "")
    run.include?("candidate_docker_image_build") && run.include?("scripts/g6-buildx-cache.sh")
  end
  cleanup = steps.find { |step| step["name"].to_s.start_with?("Clean ") }
  next abort("#{job_id} must prepare the BuildKit cache builder before freezing") unless
    prep && relay && freeze && steps.index(relay) < steps.index(prep) &&
    steps.index(prep) < steps.index(freeze) && cleanup
  relay_uses = relay.fetch("uses", "")
  next abort("#{job_id} must relay cache credentials via the repo-owned action") unless
    relay_uses == "./.github/actions/g6-cache-credentials"
  prep_run = prep.fetch("run")
  ["--driver docker-container", "--bootstrap", "--use",
   'image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8',
   'g6-buildx-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}',
   'G6_CACHE_AVAILABLE', 'buildx_driver_opts=()'].each do |token|
    next abort("#{job_id} builder preparation is missing #{token}") unless prep_run.include?(token)
  end
  abort("#{job_id} must not require cache credentials for a cold build") if
    prep_run.include?("ACTIONS_RUNTIME_TOKEN:?")
  cleanup_run = cleanup.fetch("run")
  ['docker buildx rm', 'docker buildx inspect',
   'g6-buildx-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}'].each do |token|
    next abort("#{job_id} cleanup is missing #{token}") unless cleanup_run.include?(token)
  end

  logical = []
  freeze.fetch("run").each_line do |line|
    line = line.chomp
    if !logical.empty? && logical.last.end_with?("\\")
      logical[-1] = "#{logical.last.chomp('\\').rstrip} #{line.strip}"
    else
      logical << line
    end
  end
  # The plain `docker build` alias always resolves to the default
  # docker-driver builder and silently ignores the selected docker-container
  # builder and its cache flags; release builds must go through
  # `docker buildx build` bound to the run-scoped builder.
  next abort("#{job_id} must not use the plain docker build alias") if
    freeze.fetch("run").match?(/docker build(?!x)[[:space:]]/)
  job_env = jobs.fetch(job_id).fetch("env", {})
  step_env = freeze.fetch("env", {})
  next abort("#{job_id} must pin BUILDX_BUILDER to the run-scoped builder") unless
    job_env.fetch("BUILDX_BUILDER", step_env.fetch("BUILDX_BUILDER", nil)) ==
    "g6-buildx-${{ github.run_id }}-${{ github.run_attempt }}"
  builds = logical.select { |line| line.include?("scripts/g6-buildx-cache.sh ") }
  abort("#{job_id} must keep the six lane builds plus the tunnel export (found #{builds.length})") unless builds.length == 7
  builds.each do |build|
    file = build[/--file (\S+)/, 1]
    abort("#{job_id} build is missing a Dockerfile") unless file
    if build.include?("--tag")
      ["--load", '--label "org.opencontainers.image.revision=${GITHUB_SHA}"'].each do |token|
        next abort("#{job_id} tagged builds must stay inspectable and candidate-labeled (#{token})") unless build.include?(token)
      end
    end
    next if build.include?("--target g6-tunnel-artifact")
    abort("#{job_id} #{file} build must use the shared cache fallback helper") unless
      build.include?("scripts/g6-buildx-cache.sh ")
  end
  cache_calls = freeze.fetch("run").scan(/scripts\/g6-buildx-cache\.sh\s+(g6-[a-z-]+)\s+(true|false)/)
  expected_calls = [
    ["g6-control-plane", "true"],
    ["g6-relay", "true"],
    ["g6-rust-runtime", "true"],
    ["g6-rust-runtime", "false"],
    ["g6-rust-runtime", "false"],
    ["g6-rust-runtime", "false"],
    ["g6-rust-runtime", "false"],
  ]
  abort("#{job_id} cache lanes drifted") unless cache_calls == expected_calls
  abort("#{job_id} cache helper must be present in every release build") unless
    freeze.fetch("run").include?("scripts/g6-buildx-cache.sh")
  abort("#{job_id} control-plane runtime packages must not come from the external cache") unless
    builds.any? { |build| build.include?("--file control-plane/Dockerfile") && build.include?("--no-cache-filter runtime-base") }
  abort("#{job_id} relay runtime packages must not come from the external cache") unless
    builds.any? { |build| build.include?("--file deploy/production/relay.Dockerfile") && build.include?("--no-cache-filter relay-runtime") }
  tunnel = builds.find { |b| b.include?("--target g6-tunnel-artifact") }
  abort("#{job_id} tunnel export must stay a local output export with rust cache import") unless
    tunnel&.include?('--output "type=local,dest=${tunnel_output}"') &&
    tunnel.include?("scripts/g6-buildx-cache.sh") &&
    !tunnel.include?("--cache-to")
end
RUBY
cache_helper="${ROOT}/scripts/g6-buildx-cache.sh"
test -x "${cache_helper}" || { echo "G6 cache fallback helper must be executable" >&2; exit 1; }
for cache_token in \
  "docker buildx build \"\${cache_args[@]}\" \"\$@\"" \
  "docker buildx build \"\$@\"" \
  'cache_timeout="${G6_CACHE_TIMEOUT:-60s}"' '--cache-from' '--cache-to' \
  'mode=max,ignore-error=true' 'PIPESTATUS[0]' \
  'failure_marker=' 'retrying once without external cache' \
  'G6_CACHE_STRICT_EXPORT' \
  'strict cache export requires Actions cache credentials' \
  'a completed cache export is required'; do
  grep -qF -- "${cache_token}" "${cache_helper}" \
    || { echo "G6 cache fallback helper is missing ${cache_token}" >&2; exit 1; }
done
ruby -r yaml - "${ROOT}/.github/workflows/rust-cache-provision.yml" <<'RUBY'
provision = YAML.safe_load(File.read(ARGV[0]), aliases: true)

abort("the Rust cache provisioner must have exactly push, schedule, and dispatch triggers") unless
  provision.fetch(true).keys.sort == %w[push schedule workflow_dispatch]
abort("the provisioner push trigger must fire for main only") unless
  provision.fetch(true).fetch("push").fetch("branches") == ["main"]
provision_paths = provision.fetch(true).fetch("push").fetch("paths")
%w[rust/** toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh .github/actions/g6-cache-credentials/**].each do |path|
  abort("the provisioner is missing cache input #{path}") unless provision_paths.include?(path)
end
abort("the provisioner must keep a scheduled refresh") unless
  provision.fetch(true).fetch("schedule").is_a?(Array) &&
    provision.fetch(true).fetch("schedule").all? { |entry| entry.key?("cron") }
abort("the provisioner permissions must stay read-only") unless
  provision.fetch("permissions") == {"contents" => "read", "actions" => "read"}
concurrency = provision.fetch("concurrency")
abort("the provisioner concurrency must be latest-wins per ref") unless
  concurrency.fetch("group").include?("github.ref") && concurrency.fetch("cancel-in-progress") == true

steps = provision.fetch("jobs").fetch("provision").fetch("steps")
guard = steps.first
abort("the first provisioner step must fail closed on a non-main ref before any checkout") unless
  guard["name"] == "Require main provisioning ref" &&
    guard.fetch("run").to_s.include?('test "${GITHUB_REF}" = "refs/heads/main"')
checkout_index = steps.index { |step| step["uses"].to_s.start_with?("actions/checkout") }
abort("the provisioner checkout must follow the main-ref guard") unless checkout_index == 1

build_step = steps.find { |step| step["name"] == "Build the workspace dependency cache" }
abort("the provisioner solve must use the strict exporter against the shared Rust builder") unless
  build_step&.dig("env", "G6_CACHE_STRICT_EXPORT") == "true" &&
    build_step&.dig("env", "G6_CACHE_TIMEOUT") == "300s" &&
    build_step&.fetch("run", "").to_s.include?("scripts/g6-buildx-cache.sh g6-rust-runtime true rust-cache-provision") &&
    build_step&.fetch("run", "").to_s.include?("--target g6-rust-builder")
RUBY
builder_count="$(grep -cF 'docker buildx create' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${builder_count}" -ne 2 ]]; then
  echo "both release producers must create exactly one run-scoped buildx builder (found ${builder_count})" >&2
  exit 1
fi
# The credential relay must stay best-effort and must never log values.
for relay_token in 'ACTIONS_RUNTIME_TOKEN' 'ACTIONS_RESULTS_URL' 'G6_CACHE_AVAILABLE' 'process.exit(0)'; do
  grep -qF "${relay_token}" "${ROOT}/.github/actions/g6-cache-credentials/index.js" \
    || { echo "the cache credential relay is missing ${relay_token}" >&2; exit 1; }
done
if grep -qF 'process.exit(1)' "${ROOT}/.github/actions/g6-cache-credentials/index.js"; then
  echo "the cache credential relay must not fail a build when credentials are unavailable" >&2
  exit 1
fi
grep -qF 'using: node24' "${ROOT}/.github/actions/g6-cache-credentials/action.yml" \
  || { echo "the cache credential relay must use the node24 action runtime" >&2; exit 1; }
if grep -nE 'console\.(log|error|info)\(.*value' "${ROOT}/.github/actions/g6-cache-credentials/index.js" \
  | grep -qv 'relayed'; then
  echo "the cache credential relay must not log credential values" >&2
  exit 1
fi
for scope_counts in 'g6-rust-runtime:10' 'g6-control-plane:2' 'g6-relay:2'; do
  scope="${scope_counts%%:*}"
  expected="${scope_counts##*:}"
  actual="$(grep -oF "scripts/g6-buildx-cache.sh ${scope}" "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')"
  if [[ "${actual}" -ne "${expected}" ]]; then
    echo "cache scope ${scope} must appear exactly ${expected} times (found ${actual})" >&2
    exit 1
  fi
done
if [[ "$(grep -oF -- 'scripts/g6-buildx-cache.sh ' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 14 ]] \
  || [[ "$(grep -oF 'image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 2 ]] \
  || [[ "$(grep -oF -- '--load' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 10 ]] \
  || [[ "$(grep -cF 'docker buildx rm' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 2 ]] \
  || [[ "$(grep -cF 'buildx_builder_prepare' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 4 ]] \
  || [[ "$(grep -cF -- '--no-cache-filter runtime-base' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 2 ]] \
  || [[ "$(grep -cF -- '--no-cache-filter relay-runtime' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 2 ]]; then
  echo "BuildKit cache flags drifted from the pinned release graph" >&2
  exit 1
fi
summary_count="$(grep -cF 'scripts/g6-timing.sh summary' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${summary_count}" -ne 8 ]]; then
  echo "every G6 release, failure domain, and secret-scan job must write a timing step summary (expected 8, found ${summary_count})" >&2
  exit 1
fi
upload_stage_count="$(grep -cF 'release_artifact_upload' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${upload_stage_count}" -ne 4 ]]; then
  echo "both frozen release producers must time the full artifact upload (expected 4 stage marks, found ${upload_stage_count})" >&2
  exit 1
fi
grep -qF 'GITHUB_STEP_SUMMARY' "${TIMING_HELPER}" \
  || { echo "G6 timing helper must write the step summary" >&2; exit 1; }
grep -qF 'rendezvous-dir' "${TIMING_HELPER}" \
  || { echo "G6 timing helper must aggregate rendezvous waits" >&2; exit 1; }
rendezvous_marker="rendezvous-dir \"\${timing}\""
if [[ "$(grep -cF "${rendezvous_marker}" "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 4 ]]; then
  echo "every G6 failure domain must record rendezvous timing diagnostics" >&2
  exit 1
fi
grep -qF 'G6_TIMING_FILE' "${ROOT}/.github/actions/g6-install-release/action.yml" \
  || { echo "release action must keep timing diagnostics non-authoritative" >&2; exit 1; }
for token in 'release-artifacts.sha256' 'harness Go version mismatch' \
  'image revision mismatch' 'G6_INSTALL_RELEASE_VERIFY_ONLY'; do
  grep -qF "${token}" "${INSTALL_HELPER}" \
    || { echo "release verifier is missing ${token}" >&2; exit 1; }
done

# The G6 secret-scan jobs scan published evidence with the pinned gitleaks
# binary only. They must bootstrap the minimal g6-secret-scan profile (never
# the heavyweight security profile), must keep scanning every published layer
# with the repository gitleaks configuration, and must carry non-authoritative
# tail telemetry for bootstrap, download, execution, and result publication.
if grep -qF 'scripts/bootstrap.sh security' "${ROOT}/.github/workflows/g6-harness-core.yml"; then
  echo "G6 secret-scan jobs must not bootstrap the heavyweight security profile" >&2
  exit 1
fi
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
core = YAML.safe_load(File.read(ARGV[0]), aliases: true)
jobs = core.fetch("jobs")
secret_scan_jobs = {
  "g6-rd-secret-scan" => %w[
    g6-rd-raw-fd-a g6-rd-raw-fd-b g6-rd-evidence-bundle
  ],
  "g6-smoke-secret-scan" => %w[
    g6-harness-smoke-fd-a g6-harness-smoke-fd-b g6-harness-smoke-bundle
  ]
}
secret_scan_jobs.each do |job_id, scan_targets|
  job = jobs.fetch(job_id) { abort("secret-scan job is missing: #{job_id}") }
  steps = Array(job.fetch("steps"))
  bootstrap_lines = steps.map { |step| step["run"].to_s }
    .flat_map { |run| run.lines.map(&:strip) }
    .select { |line| line.include?("scripts/bootstrap.sh") }
  unless bootstrap_lines == ["scripts/bootstrap.sh g6-secret-scan"]
    abort("#{job_id} must bootstrap exactly the minimal g6-secret-scan profile")
  end
  scan_step = steps.find { |step| step["name"].to_s.include?("Scan") }
  scan_run = scan_step.to_s
  abort("#{job_id} must scan every published evidence layer") unless
    scan_targets.all? { |target| scan_run.include?(target) }
  abort("#{job_id} must keep exactly one gitleaks invocation per evidence layer") unless
    scan_run.scan("gitleaks dir").length == scan_targets.length
  abort("#{job_id} must use the repository gitleaks configuration") unless
    scan_run.include?("scripts/g6-secret-scan.toml")
  job_run = steps.map { |step| step["run"].to_s }.join("\n")
  %w[
    secret_scan_tool_bootstrap secret_scan_artifact_download
    secret_scan_execution secret_scan_result_upload
  ].each do |stage|
    abort("#{job_id} is missing secret-scan tail timing stage #{stage}") unless
      job_run.include?(stage)
  end
  # Telemetry must fail open at the call site: the timing helper runs with
  # errexit and its ERR trap is not inherited by shell functions, so a
  # filesystem, jq, or expansion failure inside the helper returns non-zero.
  # An unguarded init or start would then skip or fail the authoritative
  # bootstrap, download, scan, or publication work that follows it.
  timing_lines = steps.map { |step| step["run"].to_s }
    .flat_map { |run| run.lines.map(&:strip) }
    .select { |line| line.include?("scripts/g6-timing.sh") }
  abort("#{job_id} must keep its timing telemetry") if timing_lines.empty?
  timing_lines.each do |line|
    abort("#{job_id} timing call must be || true guarded: #{line}") unless
      line.include?("|| true")
  end
  # The g6-secret-scan toolchain is small (one pinned gitleaks archive plus
  # host jq/openssl); restoring a tooling cache with no trusted producer
  # would only log a permanent miss while implying warm-cache behavior.
  cache_steps = steps.select { |step| step["uses"].to_s.start_with?("actions/cache") }
  abort("#{job_id} must not restore a tooling cache without a producer") unless
    cache_steps.empty?
  timing_upload = steps.select { |step| step["uses"].to_s.start_with?("actions/upload-artifact@") }
    .find { |step| step.fetch("with", {}).fetch("name", "").include?("g6-timing-") }
  abort("#{job_id} must publish its timing diagnostics") unless timing_upload
  unless timing_upload.fetch("with")["if-no-files-found"] == "warn" &&
         timing_upload.fetch("with")["retention-days"] == 5
    abort("#{job_id} timing diagnostics must stay non-authoritative")
  end
  # if-no-files-found only covers a missing local timing file: an artifact
  # service failure would otherwise fail the whole job and flip the formal
  # gate or smoke aggregate on pure telemetry. Only the telemetry upload is
  # exempt; the structured scan result uploads stay fail-closed.
  unless timing_upload["continue-on-error"] == true
    abort("#{job_id} timing diagnostics upload must be continue-on-error")
  end
  steps.select { |step| step["uses"].to_s.start_with?("actions/upload-artifact@") }.each do |step|
    next if step.equal?(timing_upload)
    abort("#{job_id} authoritative artifact upload must stay fail-closed") if
      step["continue-on-error"]
  end
end
RUBY
