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
  docker_load scenario_execution observation_300_seconds evidence_collection; do
  grep -qF "${stage}" "${ROOT}/.github/workflows/g6-harness-core.yml" \
    "${TIMING_HELPER}" "${INSTALL_HELPER}" \
    || { echo "G6 timing stage is missing: ${stage}" >&2; exit 1; }
done
grep -qF 'GITHUB_STEP_SUMMARY' "${TIMING_HELPER}" \
  || { echo "G6 timing helper must write the step summary" >&2; exit 1; }
grep -qF 'G6_TIMING_FILE' "${ROOT}/.github/actions/g6-install-release/action.yml" \
  || { echo "release action must keep timing diagnostics non-authoritative" >&2; exit 1; }
for token in 'release-artifacts.sha256' 'harness Go version mismatch' \
  'image revision mismatch' 'G6_INSTALL_RELEASE_VERIFY_ONLY'; do
  grep -qF "${token}" "${INSTALL_HELPER}" \
    || { echo "release verifier is missing ${token}" >&2; exit 1; }
done
