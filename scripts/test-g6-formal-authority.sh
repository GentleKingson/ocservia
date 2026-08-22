#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml - "${ROOT}/.github/workflows/g6-readiness.yml" \
  "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
formal = YAML.safe_load(File.read(ARGV[0]), aliases: true)
core = YAML.safe_load(File.read(ARGV[1]), aliases: true)
authority = formal.fetch(true).fetch("workflow_dispatch").fetch("inputs").fetch("authority")
abort("formal authority enum drifted") unless authority.fetch("options") == %w[engineering production_readiness] && authority.fetch("default") == "engineering" && authority.fetch("required") == true
concurrency = formal.fetch("concurrency")
abort("formal runs must queue without cancellation") unless concurrency.fetch("queue") == "max" && !concurrency.key?("cancel-in-progress")
call = formal.fetch("jobs").fetch("g6-harness-core")
abort("formal caller may select only the formal profile") unless call.fetch("with") == {"profile"=>"formal", "authority"=>"${{ inputs.authority }}", "candidate_sha"=>"${{ github.sha }}", "smoke_relevant"=>true}
jobs = core.fetch("jobs")
jobs.select { |id, _| id.start_with?("g6-rd-") }.each do |id, job|
  environment = job.fetch("environment").fetch("name")
  abort("#{id} must select the protected production environment") unless environment.include?("g6-production-readiness") && environment.include?("inputs.authority")
end
jobs.select { |id, _| id.start_with?("g6-smoke-") }.each do |id, job|
  abort("#{id} must not enter a formal environment") if job.key?("environment")
end
gate = jobs.fetch("g6-rd-gate")
gate_text = Array(gate.fetch("steps")).map { |step| step.fetch("run", "") }.join("\n")
abort("formal gate must bind the caller authority") unless gate_text.include?('--authority "${{ inputs.authority }}"') && gate_text.include?("g6-pipeline.mjs gate")
abort("smoke result must remain non-formal") unless Array(jobs.fetch("g6-smoke-result").fetch("steps")).all? { |step| !step.fetch("run", "").include?("g6-pipeline.mjs gate") }
RUBY
