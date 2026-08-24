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
  evidence_collection release_artifact_upload; do
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
# load every tagged build into the local store for freezing, and remove
# the builder in the always-cleanup step.
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
core = YAML.safe_load(File.read(ARGV[0]), aliases: true)
jobs = core.fetch("jobs")
expected_scope = {
  "control-plane/Dockerfile" => "g6-control-plane",
  "deploy/production/relay.Dockerfile" => "g6-relay",
  "rust/g6-runtime.Dockerfile" => "g6-rust-runtime",
}
export_builds = [
  ["control-plane/Dockerfile", nil],
  ["deploy/production/relay.Dockerfile", nil],
  ["rust/g6-runtime.Dockerfile", "g6-rust-builder"],
]
%w[g6-rd-release-image g6-smoke-release].each do |job_id|
  steps = Array(jobs.fetch(job_id).fetch("steps"))
  prep = steps.find { |step| step["name"] == "Prepare the BuildKit cache builder" }
  relay = steps.find { |step| step["name"] == "Relay Actions cache credentials to the job environment" }
  freeze = steps.find do |step|
    run = step.fetch("run", "")
    run.include?("candidate_docker_image_build") && run.include?("docker build")
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
   'g6-buildx-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}',
   'env.ACTIONS_RUNTIME_TOKEN=${ACTIONS_RUNTIME_TOKEN}',
   "ACTIONS_RUNTIME_TOKEN:?"].each do |token|
    next abort("#{job_id} builder preparation is missing #{token}") unless prep_run.include?(token)
  end
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
  builds = logical.select { |line| line.include?("docker buildx build ") }
  abort("#{job_id} must keep the six lane builds plus the tunnel export (found #{builds.length})") unless builds.length == 7
  builds.each do |build|
    file = build[/--file (\S+)/, 1]
    scope = expected_scope[file] || abort("#{job_id} builds an unexpected dockerfile: #{file}")
    froms = build.scan(/--cache-from type=gha,scope=(\S+)/).flatten
    abort("#{job_id} #{file} must import exactly its own lane cache") unless froms == [scope]
    if build.include?("--tag")
      ["--load", '--label "org.opencontainers.image.revision=${GITHUB_SHA}"'].each do |token|
        next abort("#{job_id} tagged builds must stay inspectable and candidate-labeled (#{token})") unless build.include?(token)
      end
    end
    target = build[/--target (\S+)/, 1]
    tos = build.scan(/--cache-to type=gha,scope=(\S+?),mode=max/).flatten
    if export_builds.any? { |f, t| f == file && t == target }
      next abort("#{job_id} #{file} #{target} must export its lane cache with mode=max") unless tos == [scope]
    elsif !tos.empty?
      abort("#{job_id} #{file} #{target} must not re-export the lane cache")
    end
    if build.include?("type=local") && !build.include?("--target g6-tunnel-artifact")
      abort("#{job_id} #{file} must not use a non-GHA cache backend")
    end
  end
  tunnel = builds.find { |b| b.include?("--target g6-tunnel-artifact") }
  abort("#{job_id} tunnel export must stay a local output export with rust cache import") unless
    tunnel&.include?('--output "type=local,dest=${tunnel_output}"') &&
    tunnel.include?("--cache-from type=gha,scope=g6-rust-runtime") &&
    !tunnel.include?("--cache-to")
end
RUBY
builder_count="$(grep -cF 'docker buildx create' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${builder_count}" -ne 2 ]]; then
  echo "both release producers must create exactly one run-scoped buildx builder (found ${builder_count})" >&2
  exit 1
fi
# The credential relay must stay fail-closed and must never log values.
for relay_token in 'ACTIONS_RUNTIME_TOKEN' 'ACTIONS_RESULTS_URL' 'process.exit(1)'; do
  grep -qF "${relay_token}" "${ROOT}/.github/actions/g6-cache-credentials/index.js" \
    || { echo "the cache credential relay is missing ${relay_token}" >&2; exit 1; }
done
if grep -nE 'console\.(log|error|info)\(.*value' "${ROOT}/.github/actions/g6-cache-credentials/index.js" \
  | grep -qv 'relayed'; then
  echo "the cache credential relay must not log credential values" >&2
  exit 1
fi
for scope_counts in 'g6-rust-runtime:12' 'g6-control-plane:4' 'g6-relay:4'; do
  scope="${scope_counts%%:*}"
  expected="${scope_counts##*:}"
  actual="$(grep -oF "scope=${scope}" "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')"
  if [[ "${actual}" -ne "${expected}" ]]; then
    echo "cache scope ${scope} must appear exactly ${expected} times (found ${actual})" >&2
    exit 1
  fi
done
if [[ "$(grep -oF -- '--cache-to type=gha' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 6 ]] \
  || [[ "$(grep -oF 'mode=max' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 6 ]] \
  || [[ "$(grep -oF -- '--load' "${ROOT}/.github/workflows/g6-harness-core.yml" | wc -l | tr -d ' ')" -ne 10 ]] \
  || [[ "$(grep -cF 'docker buildx rm' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 2 ]] \
  || [[ "$(grep -cF 'buildx_builder_prepare' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 4 ]]; then
  echo "BuildKit cache flags drifted from the pinned release graph" >&2
  exit 1
fi
summary_count="$(grep -cF 'scripts/g6-timing.sh summary' "${ROOT}/.github/workflows/g6-harness-core.yml")"
if [[ "${summary_count}" -ne 6 ]]; then
  echo "every G6 release and failure domain job must write a timing step summary (expected 6, found ${summary_count})" >&2
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
if [[ "$(grep -cF 'rendezvous-dir "${timing}"' "${ROOT}/.github/workflows/g6-harness-core.yml")" -ne 4 ]]; then
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
