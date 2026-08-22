#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
core = YAML.safe_load(File.read(ARGV[0]), aliases: true)
jobs = core.fetch("jobs")
assemble = jobs.fetch("g6-rd-assemble")
abort("assembly must always consume both raw domains") unless assemble.fetch("needs").sort == %w[g6-rd-fd-a g6-rd-fd-b] && assemble.fetch("if").include?("always()")
scan = jobs.fetch("g6-rd-secret-scan")
abort("secret scan must consume raw and assembled evidence") unless scan.fetch("needs").sort == %w[g6-rd-assemble g6-rd-fd-a g6-rd-fd-b] && scan.fetch("if").include?("always()")
verifier = jobs.fetch("g6-rd-verifier")
abort("verifier must be independent of runtime") unless verifier.fetch("needs") == ["g6-rd-assemble"] && verifier.fetch("if").include?("always()")
gate = jobs.fetch("g6-rd-gate")
abort("gate must aggregate every evidence layer") unless gate.fetch("needs").sort == %w[g6-rd-assemble g6-rd-fd-a g6-rd-fd-b g6-rd-secret-scan g6-rd-verifier] && gate.fetch("if").include?("always()")
%w[g6-rd-fd-a g6-rd-fd-b].each do |id|
  steps = Array(jobs.fetch(id).fetch("steps"))
  abort("#{id} must upload raw evidence even on failure") unless steps.any? { |step| step.fetch("name", "").include?("raw evidence") && step.fetch("if", "") == "always()" && step.fetch("uses", "").start_with?("actions/upload-artifact@") }
  abort("#{id} runtime must not build or verify evidence") if steps.any? { |step| step.fetch("run", "").match?(/build-g6-evidence|verify-g6-evidence/) }
end
smoke = jobs.fetch("g6-smoke-result")
steps = Array(smoke.fetch("steps"))
not_applicable = steps.find { |step| step["name"] == "Record a structured not-applicable result" }
abort("unrelated PRs need a structured not_applicable result") unless not_applicable&.fetch("if") == "${{ !inputs.smoke_relevant }}" && not_applicable.fetch("run").include?('status:"not_applicable"')
enforce = steps.find { |step| step["name"] == "Enforce the smoke contract" }.fetch("run")
abort("smoke cannot claim a formal verdict") unless enforce.include?("formal_verdict_eligible == false") && enforce.include?("expected_status=not_applicable")
RUBY
