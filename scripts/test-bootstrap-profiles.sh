#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/scripts/bootstrap.sh"
WORKFLOW="${ROOT}/.github/workflows/ci.yml"

set +e
output="$(GITHUB_ACTIONS=true "${BOOTSTRAP}" 2>&1)"
status=$?
set -e
if [[ ${status} -ne 2 ]] || [[ "${output}" != *"bootstrap profile must be explicit in GitHub Actions"* ]]; then
  echo "bootstrap without a profile must fail clearly in GitHub Actions" >&2
  exit 1
fi

ruby -r yaml - "${ROOT}" "${WORKFLOW}" <<'RUBY'
root, workflow_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
jobs = workflow.fetch("jobs")
bootstrap = File.read(File.join(root, "scripts/bootstrap.sh"))
makefile = File.read(File.join(root, "Makefile"))

def reject(message)
  warn message
  exit 1
end

profiles = {
  "contracts" => "contracts",
  "go" => "go-integration",
  "rust" => "rust-validation",
  "web" => "web",
  "native-ocserv" => "native",
  "security-licenses" => "security"
}

(["all"] + profiles.values.uniq).each do |profile|
  branch = /^  #{Regexp.escape(profile)}\)$/
  reject("bootstrap profile is missing: #{profile}") unless bootstrap.match?(branch)
end
reject("make bootstrap must request the complete profile explicitly") unless makefile.match?(/^\t\.\/scripts\/bootstrap\.sh all$/)

all_bootstrap_calls = []
jobs.each do |job_id, job|
  Array(job["steps"]).each do |step|
    next unless step["run"].is_a?(String)

    step["run"].lines.each do |line|
      command = line.strip
      all_bootstrap_calls << [job_id, command] if command.include?("scripts/bootstrap.sh")
    end
  end
end

profiles.each do |job_id, profile|
  expected = "scripts/bootstrap.sh #{profile}"
  calls = all_bootstrap_calls.select { |candidate_job, _| candidate_job == job_id }.map(&:last)
  reject("#{job_id} must run exactly #{expected}") unless calls == [expected]
end
reject("workflow contains a bare bootstrap invocation") if all_bootstrap_calls.any? { |_, command| command.match?(/\A(?:\.\/)?scripts\/bootstrap\.sh\z/) }
reject("workflow execution jobs must not use the all profile") if all_bootstrap_calls.any? { |_, command| command.end_with?(" all") }
reject("unexpected workflow bootstrap caller") unless all_bootstrap_calls.map(&:first).sort == profiles.keys.sort

cache_steps = {}
jobs.each do |job_id, job|
  cache_steps[job_id] = Array(job.fetch("steps")).select do |step|
    step["uses"].to_s.start_with?("actions/cache@")
  end
  cache_steps[job_id].each do |step|
    reject("#{job_id} cache action must be pinned to a full commit SHA") unless step.fetch("uses").match?(/\Aactions\/cache@[0-9a-f]{40}\z/)
    reject("#{job_id} cache must not use restore-keys") if step.fetch("with", {}).key?("restore-keys")
  end
end

def paths(step)
  step.fetch("with").fetch("path").lines.map(&:strip).reject(&:empty?)
end

tooling_inputs = ["toolchains.lock", "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh"]
profiles.each do |job_id, profile|
  tooling = cache_steps.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("tooling-v2-") }
  reject("#{job_id} must have one v2 tooling cache") unless tooling.length == 1
  key = tooling.first.fetch("with").fetch("key")
  prefix = "tooling-v2-#{profile}-${{ runner.os }}-${{ runner.arch }}-"
  reject("#{job_id} tooling cache must include its profile") unless key.start_with?(prefix)
  tooling_inputs.each { |input| reject("#{job_id} tooling key is missing #{input}") unless key.include?(input) }
  reject("#{job_id} tooling cache must not depend on web/package-lock.json") if key.include?("web/package-lock.json")
  reject("#{job_id} tooling cache paths are not minimal") unless paths(tooling.first).sort == [".cache/downloads", ".tools"].sort
end

npm_jobs = ["contracts", "web", "security-licenses"]
jobs.each_key do |job_id|
  npm = cache_steps.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("npm-v2-") }
  if npm_jobs.include?(job_id)
    reject("#{job_id} must have one npm download cache") unless npm.length == 1
    key = npm.first.fetch("with").fetch("key")
    reject("#{job_id} npm cache must include pinned toolchain inputs") unless key.include?("toolchains.lock")
    reject("#{job_id} npm cache must include web/package-lock.json") unless key.include?("web/package-lock.json")
    reject("#{job_id} npm cache path must be .cache/npm") unless paths(npm.first) == [".cache/npm"]
  else
    reject("#{job_id} must not have an npm cache") unless npm.empty?
  end
end
npm_path_jobs = cache_steps.each_with_object([]) do |(job_id, steps), result|
  result << job_id if steps.any? { |step| paths(step).include?(".cache/npm") }
end
reject("npm cache is present outside its dependency jobs") unless npm_path_jobs.sort == npm_jobs.sort

go_cache = cache_steps.fetch("go").find { |step| step.fetch("with").fetch("key").start_with?("go-v2-") }
reject("Go must have a v2 build/module cache") unless go_cache
go_key = go_cache.fetch("with").fetch("key")
["toolchains.lock", "go.work", "go.work.sum", "control-plane/go.mod", "control-plane/go.sum", "rust/Cargo.lock"].each do |input|
  reject("Go cache key is missing #{input}") unless go_key.include?(input)
end
reject("Go cache must not depend on web/package-lock.json") if go_key.include?("web/package-lock.json")
expected_go_paths = [".cache/go-build", ".cache/go-mod", ".cache/gopath", "rust/target"]
reject("Go cache paths changed unexpectedly") unless paths(go_cache).sort == expected_go_paths.sort

rust_cache = cache_steps.fetch("rust").find { |step| step.fetch("with").fetch("key").start_with?("rust-v2-") }
reject("Rust must have a v2 target cache") unless rust_cache
rust_key = rust_cache.fetch("with").fetch("key")
["toolchains.lock", "rust/Cargo.lock", "rust/Cargo.toml", "rust/rust-toolchain.toml", "rust/crates/**/Cargo.toml"].each do |input|
  reject("Rust cache key is missing #{input}") unless rust_key.include?(input)
end
reject("Rust cache must not depend on web/package-lock.json") if rust_key.include?("web/package-lock.json")
reject("Rust target cache path changed unexpectedly") unless paths(rust_cache) == ["rust/target"]

allowed_cache_paths = [
  ".cache/downloads",
  ".cache/npm",
  ".tools",
  ".cache/go-build",
  ".cache/go-mod",
  ".cache/gopath",
  "rust/target"
]
jobs.each_key do |job_id|
  cache_steps.fetch(job_id).each do |step|
    paths(step).each do |path|
      reject("#{job_id} must not cache node_modules") if path.include?("node_modules")
      reject("#{job_id} cache path escapes repository caches") if path.start_with?("/tmp", "${{ runner.temp }}")
      reject("#{job_id} has an unapproved cache path: #{path}") unless allowed_cache_paths.include?(path)
    end
  end
end

expected_needs = ["public-policy", "contracts", "go", "rust", "web", "native-ocserv", "p1-smoke", "security-licenses"]
gate = jobs.fetch("ci-gate")
actual_needs = Array(gate.fetch("needs"))
reject("CI Gate required job set changed") unless actual_needs.sort == expected_needs.sort
reject("CI Gate must run regardless of upstream result") unless gate.fetch("if") == "always()"
expected_results = {
  "PUBLIC_POLICY_RESULT" => "${{ needs.public-policy.result }}",
  "CONTRACTS_RESULT" => "${{ needs.contracts.result }}",
  "GO_RESULT" => "${{ needs.go.result }}",
  "RUST_RESULT" => "${{ needs.rust.result }}",
  "WEB_RESULT" => "${{ needs.web.result }}",
  "NATIVE_OCSERV_RESULT" => "${{ needs.native-ocserv.result }}",
  "P1_SMOKE_RESULT" => "${{ needs.p1-smoke.result }}",
  "SECURITY_LICENSES_RESULT" => "${{ needs.security-licenses.result }}"
}
gate_env = gate.fetch("env")
reject("CI Gate result environment changed") unless gate_env == expected_results
gate_run = Array(gate.fetch("steps")).map { |step| step["run"] }.compact.join("\n")
expected_results.each_key do |variable|
  reject("CI Gate no longer checks #{variable}") unless gate_run.include?("\"${#{variable}}\"")
end

policy_run = Array(jobs.fetch("public-policy").fetch("steps")).map { |step| step["run"] }.compact.join("\n")
reject("Public Repository Policy must run the bootstrap policy test") unless policy_run.lines.map(&:strip).include?("scripts/test-bootstrap-profiles.sh")
RUBY
