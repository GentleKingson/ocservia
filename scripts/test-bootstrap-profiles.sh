#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/scripts/bootstrap.sh"
WORKFLOW="${ROOT}/.github/workflows/ci.yml"
P1_WORKFLOW="${ROOT}/.github/workflows/p1-capacity.yml"

set +e
output="$(GITHUB_ACTIONS=true "${BOOTSTRAP}" 2>&1)"
status=$?
set -e
if [[ ${status} -ne 2 ]] || [[ "${output}" != *"bootstrap profile must be explicit in GitHub Actions"* ]]; then
  echo "bootstrap without a profile must fail clearly in GitHub Actions" >&2
  exit 1
fi

ruby -r yaml - "${ROOT}" "${WORKFLOW}" "${P1_WORKFLOW}" <<'RUBY'
root, workflow_path, p1_workflow_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
p1_workflow = YAML.safe_load(File.read(p1_workflow_path), aliases: true)
jobs = workflow.fetch("jobs")
bootstrap = File.read(File.join(root, "scripts/bootstrap.sh"))
makefile = File.read(File.join(root, "Makefile"))

def reject(message)
  warn message
  exit 1
end

execution_profiles = {
  "backend-integration" => "go-integration",
  "web-smoke" => "web",
  "quality-security-native" => "ci-quality"
}
reject("primary workflow must contain exactly three execution jobs") unless jobs.keys.sort == execution_profiles.keys.sort

legacy_job_ids = %w[public-policy contracts go rust web native-ocserv p1-smoke security-licenses]
reject("legacy execution job ID remains") unless (jobs.keys & legacy_job_ids).empty?

execution_profiles.each_key do |job_id|
  job = jobs.fetch(job_id)
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} must not depend on another job") if job.key?("needs")
end

(["all"] + execution_profiles.values).each do |profile|
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

execution_profiles.each do |job_id, profile|
  expected = "scripts/bootstrap.sh #{profile}"
  calls = all_bootstrap_calls.select { |candidate_job, _| candidate_job == job_id }.map(&:last)
  reject("#{job_id} must run exactly #{expected}") unless calls == [expected]
end
reject("workflow contains a bare bootstrap invocation") if all_bootstrap_calls.any? { |_, command| command.match?(/\A(?:\.\/)?scripts\/bootstrap\.sh\z/) }
reject("workflow execution jobs must not use the all profile") if all_bootstrap_calls.any? { |_, command| command.end_with?(" all") }
reject("unexpected workflow bootstrap caller") unless all_bootstrap_calls.map(&:first).sort == execution_profiles.keys.sort

cache_steps = {}
jobs.each do |job_id, job|
  cache_steps[job_id] = Array(job.fetch("steps")).select do |step|
    step["uses"].to_s.start_with?("actions/cache")
  end
  cache_steps[job_id].each do |step|
    reject("#{job_id} cache action must be pinned to a full commit SHA") unless step.fetch("uses").match?(/\Aactions\/cache(?:\/restore)?@[0-9a-f]{40}\z/)
    reject("#{job_id} cache must not use restore-keys") if step.fetch("with", {}).key?("restore-keys")
  end
end

def paths(step)
  step.fetch("with").fetch("path").lines.map(&:strip).reject(&:empty?)
end

tooling_inputs = ["toolchains.lock", "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh"]
execution_profiles.each do |job_id, profile|
  tooling = cache_steps.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("tooling-v3-") }
  reject("#{job_id} must have one v3 tooling cache") unless tooling.length == 1
  key = tooling.first.fetch("with").fetch("key")
  prefix = "tooling-v3-#{profile}-${{ runner.os }}-${{ runner.arch }}-"
  reject("#{job_id} tooling cache must include its profile") unless key.start_with?(prefix)
  tooling_inputs.each { |input| reject("#{job_id} tooling key is missing #{input}") unless key.include?(input) }
  reject("#{job_id} tooling cache paths are not minimal") unless paths(tooling.first).sort == [".cache/downloads", ".tools"].sort
end

npm_jobs = ["web-smoke", "quality-security-native"]
execution_profiles.each_key do |job_id|
  npm = cache_steps.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("npm-v3-") }
  if npm_jobs.include?(job_id)
    reject("#{job_id} must have one npm download cache") unless npm.length == 1
    key = npm.first.fetch("with").fetch("key")
    reject("#{job_id} npm cache must include toolchains.lock") unless key.include?("toolchains.lock")
    reject("#{job_id} npm cache must include web/package-lock.json") unless key.include?("web/package-lock.json")
    reject("#{job_id} npm cache path must be .cache/npm") unless paths(npm.first) == [".cache/npm"]
  else
    reject("#{job_id} must not have an npm cache") unless npm.empty?
  end
end

backend_cache = cache_steps.fetch("backend-integration").find { |step| step.fetch("with").fetch("key").start_with?("go-v3-backend-") }
reject("Backend Integration must have one Go build/module cache") unless backend_cache
backend_key = backend_cache.fetch("with").fetch("key")
["toolchains.lock", "go.work", "go.work.sum", "control-plane/go.mod", "control-plane/go.sum"].each do |input|
  reject("Backend Integration cache key is missing #{input}") unless backend_key.include?(input)
end
expected_backend_paths = [".cache/go-build", ".cache/go-mod", ".cache/gopath"]
reject("Backend Integration cache paths changed unexpectedly") unless paths(backend_cache).sort == expected_backend_paths.sort
backend_rust = cache_steps.fetch("backend-integration").find { |step| step.fetch("uses").start_with?("actions/cache/restore@") }
reject("Backend Integration must restore the Quality Rust cache without saving it") unless backend_rust
reject("Backend Integration Rust restore path must be rust/target") unless paths(backend_rust) == ["rust/target"]
reject("Backend Integration must use the Quality Rust cache key") unless backend_rust.fetch("with").fetch("key").start_with?("rust-v3-quality-")

quality_caches = cache_steps.fetch("quality-security-native")
rust_cache = quality_caches.find { |step| step.fetch("with").fetch("key").start_with?("rust-v3-quality-") }
reject("Quality must own one Rust target cache") unless rust_cache && paths(rust_cache) == ["rust/target"]
rust_key = rust_cache.fetch("with").fetch("key")
["toolchains.lock", "rust/Cargo.lock", "rust/Cargo.toml", "rust/rust-toolchain.toml", "rust/crates/**/Cargo.toml"].each do |input|
  reject("Quality Rust cache key is missing #{input}") unless rust_key.include?(input)
end
go_mod_cache = quality_caches.find { |step| step.fetch("with").fetch("key").start_with?("go-mod-v3-quality-") }
reject("Quality must have one Go module download cache") unless go_mod_cache && paths(go_mod_cache) == [".cache/go-mod"]
["toolchains.lock", "go.work", "go.work.sum", "control-plane/go.mod", "control-plane/go.sum", "scripts/license-check.sh"].each do |input|
  reject("Quality Go module cache key is missing #{input}") unless go_mod_cache.fetch("with").fetch("key").include?(input)
end

allowed_cache_paths = [
  ".cache/downloads",
  ".cache/npm",
  ".tools",
  ".cache/go-build",
  ".cache/go-mod",
  ".cache/gopath",
  "rust/target"
]
execution_profiles.each_key do |job_id|
  cache_steps.fetch(job_id).each do |step|
    paths(step).each do |path|
      reject("#{job_id} must not cache node_modules") if path.include?("node_modules")
      reject("#{job_id} cache path escapes repository caches") if path.start_with?("/tmp", "${{ runner.temp }}")
      reject("#{job_id} has an unapproved cache path: #{path}") unless allowed_cache_paths.include?(path)
    end
  end
end

execution_profiles.each_key do |job_id|
  uploads = Array(jobs.fetch(job_id).fetch("steps")).select { |step| step["uses"].to_s.start_with?("actions/upload-artifact@") }
  reject("#{job_id} must upload exactly one diagnostic artifact") unless uploads.length == 1
  reject("#{job_id} upload action must be pinned") unless uploads.first.fetch("uses").match?(/\Aactions\/upload-artifact@[0-9a-f]{40}\z/)
end

reject("primary workflow must not contain a CI Gate job") if jobs.key?("ci-gate")

web = jobs.fetch("web-smoke")
web_env = web.fetch("env")
expected_p1 = {
  "AGENT_COUNT" => 24,
  "HEARTBEAT_COUNT" => 2,
  "HEARTBEAT_INTERVAL_MS" => 500,
  "MINIMUM_RESOURCE_SAMPLES" => 8,
  "P1_PROFILE" => "smoke",
  "QUEUE_CAPACITY" => 256,
  "REQUEST_CONCURRENCY" => 8
}
reject("Web & Smoke changed the P1 smoke profile") unless web_env == expected_p1
web_steps = Array(web.fetch("steps"))
e2e = web_steps.find { |step| step["run"] == "scripts/e2e.sh" }
p1 = web_steps.find { |step| step["run"] == "scripts/p1-resilience-capacity.sh" }
reject("Web & Smoke must run E2E and P1 Smoke separately") unless e2e && p1
reject("E2E and P1 Smoke must use different Compose projects") if e2e.fetch("env").fetch("COMPOSE_PROJECT") == p1.fetch("env").fetch("COMPOSE_PROJECT")
reject("E2E and P1 Smoke must use different RUN_ID values") if e2e.fetch("env").fetch("RUN_ID") == p1.fetch("env").fetch("RUN_ID")

quality_steps = Array(jobs.fetch("quality-security-native").fetch("steps"))
step_names = quality_steps.map { |step| step["name"] }
native_index = step_names.index("Install ephemeral native fixtures")
upload_index = step_names.index("Upload Quality, Security, and Native diagnostics")
native_run = quality_steps.find { |step| step["name"] == "Native ocpasswd, OpenSSL, and Ocserv login" }.fetch("run")
required_before_native = [
  "Repository policy",
  "Documentation",
  "CI workflow policy tests",
  "Generate contract outputs",
  "Lint contract sources",
  "Check contract breaking changes",
  "Check generated output",
  "Rust checks",
  "Agent privilege boundary",
  "Transport boundary",
  "Secret scan",
  "License checks"
]
reject("Quality native fixture steps are missing") unless native_index && upload_index
required_before_native.each do |name|
  index = step_names.index(name)
  reject("#{name} must execute before Native Ocserv") unless index && index < native_index
end
reject("Native Ocserv must execute before artifact upload") unless native_index < upload_index
reject("Native Ocserv must use an isolated Cargo target") unless native_run.include?("CARGO_TARGET_DIR=\"${native_target}\"")
reject("Native Ocserv must remove its isolated Cargo target") unless native_run.include?("sudo rm -rf \"${native_target}\"")

trigger = p1_workflow.fetch(true)
reject("P1 Full must remain workflow_dispatch-only") unless trigger.keys == ["workflow_dispatch"]
p1_job = p1_workflow.fetch("jobs").fetch("p1-full")
reject("P1 Full must use ubuntu-24.04") unless p1_job.fetch("runs-on") == "ubuntu-24.04"
reject("P1 Full profile changed") unless p1_job.fetch("env").fetch("P1_PROFILE") == "full"
reject("P1 Full timeout changed") unless p1_job.fetch("timeout-minutes") == 45
RUBY

"${ROOT}/scripts/test-real-e2e-workflow.sh"
