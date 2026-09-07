#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml -r open3 - "${ROOT}/.github/workflows/g6-readiness.yml" \
  "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
formal = YAML.safe_load(File.read(ARGV[0]), aliases: true)
core = YAML.safe_load(File.read(ARGV[1]), aliases: true)
authority = formal.fetch(true).fetch("workflow_dispatch").fetch("inputs").fetch("authority")
abort("formal authority enum drifted") unless authority.fetch("options") == %w[engineering production_readiness] && authority.fetch("default") == "engineering" && authority.fetch("required") == true
concurrency = formal.fetch("concurrency")
abort("formal runs must queue without cancellation") unless concurrency.fetch("queue") == "max" && !concurrency.key?("cancel-in-progress")
call = formal.fetch("jobs").fetch("g6-harness-core")
abort("formal caller may select only the formal profile") unless call.fetch("with") == {"profile"=>"formal", "authority"=>"${{ inputs.authority }}", "candidate_sha"=>"${{ github.sha }}"}
jobs = core.fetch("jobs")
jobs.select { |id, _| id.start_with?("g6-rd-") }.each do |id, job|
  environment = job.fetch("environment").fetch("name")
  abort("#{id} must select the protected production environment") unless environment.include?("g6-production-readiness") && environment.include?("inputs.authority")
end
gate = jobs.fetch("g6-rd-gate")
gate_text = Array(gate.fetch("steps")).map { |step| step.fetch("run", "") }.join("\n")
abort("formal gate must bind the caller authority") unless gate_text.include?('--authority "${G6_AUTHORITY}"') && gate_text.include?("g6-pipeline.mjs gate")
abort("authority must be passed through the environment") unless core.fetch("env").fetch("G6_AUTHORITY") == '${{ inputs.authority }}'
jobs.each do |id, job|
  job.fetch("steps").each do |step|
    script = step.fetch("run", "")
    abort("#{id} interpolates authority into shell") if script.match?(/\$\{\{\s*inputs\.authority\s*\}\}/)
  end
end
contract = jobs.fetch("g6-contract").fetch("steps").first.fetch("run")
%w[engineering production_readiness invalid].push('$(touch /tmp/authority-injection)', '"; exit 0; #').each do |value|
  _, _, status = Open3.capture3({"G6_CORE_AUTHORITY" => value, "G6_CORE_PROFILE" => "formal", "G6_CORE_CANDIDATE_SHA" => "a" * 40, "GITHUB_SHA" => "a" * 40}, "bash", "-c", contract)
  expected = %w[engineering production_readiness].include?(value)
  abort("authority validation changed for #{value.inspect}") unless status.success? == expected
end
RUBY
