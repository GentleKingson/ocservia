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
checksums = File.read(File.join(root, "scripts/checksums.txt"))
toolchains = File.read(File.join(root, "toolchains.lock"))

def reject(message)
  warn message
  exit 1
end

worker_jobs = %w[
  runtime-artifacts go-standard go-race database stage-contracts
  production-relays credential-rotation local-slice web-validation browser-e2e
  p1-smoke contracts-policy rust-validation security-license native-ocserv
]
gate_needs = {
  "backend-integration" => %w[
    runtime-artifacts go-standard go-race database stage-contracts
    production-relays credential-rotation local-slice rust-validation
  ],
  "web-smoke" => %w[web-validation browser-e2e p1-smoke],
  "quality-security-native" => %w[
    contracts-policy rust-validation security-license native-ocserv
  ]
}
expected_jobs = worker_jobs + gate_needs.keys
reject("primary workflow execution graph changed unexpectedly") unless jobs.keys.sort == expected_jobs.sort

worker_jobs.each do |job_id|
  job = jobs.fetch(job_id)
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} must not be silently path-skipped before a change classifier is defined") if job.key?("if")
end

allowed_worker_dependencies = {
  "database" => ["runtime-artifacts"],
  "local-slice" => ["runtime-artifacts"]
}
worker_jobs.each do |job_id|
  actual = Array(jobs.fetch(job_id)["needs"])
  expected = allowed_worker_dependencies.fetch(job_id, [])
  reject("#{job_id} has an unexpected dependency") unless actual == expected
end

expected_gate_names = {
  "backend-integration" => "Backend Integration",
  "web-smoke" => "Web & Smoke",
  "quality-security-native" => "Quality, Security & Native"
}
gate_needs.each do |job_id, expected_needs|
  job = jobs.fetch(job_id)
  reject("#{job_id} changed its required-check name") unless job.fetch("name") == expected_gate_names.fetch(job_id)
  reject("#{job_id} must run after failed dependencies") unless job.fetch("if") == "${{ always() }}"
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} has the wrong aggregate dependencies") unless Array(job.fetch("needs")).sort == expected_needs.sort
  serialized = job.to_s
  reject("#{job_id} must accept successful or intentionally skipped workers") unless
    serialized.include?("success | skipped")
  reject("#{job_id} must fail for every other worker result") unless serialized.include?("*) exit 1")
  reject("#{job_id} must remain a lightweight result aggregator") if
    Array(job.fetch("steps")).any? { |step| step.key?("uses") }
end

trigger = workflow.fetch(true)
reject("CI must always trigger for pull requests") unless trigger.key?("pull_request")
reject("CI must not use workflow-level path filters") if trigger.fetch("pull_request").is_a?(Hash)
reject("CI must run on main pushes") unless trigger.fetch("push").fetch("branches") == ["main"]
reject("CI must support manual dispatch") unless trigger.key?("workflow_dispatch")

execution_profiles = {
  "runtime-artifacts" => "go-integration",
  "go-standard" => "go-integration",
  "go-race" => "go-integration",
  "database" => "go-integration",
  "production-relays" => "go-integration",
  "web-validation" => "web",
  "contracts-policy" => "contracts",
  "rust-validation" => "rust-validation",
  "security-license" => "security",
  "native-ocserv" => "native"
}
(["all"] + execution_profiles.values).uniq.each do |profile|
  reject("bootstrap profile is missing: #{profile}") unless bootstrap.match?(/^  #{Regexp.escape(profile)}\)$/)
end
reject("make bootstrap must request the complete profile explicitly") unless makefile.match?(/^\t\.\/scripts\/bootstrap\.sh all$/)

bootstrap_calls = Hash.new { |hash, key| hash[key] = [] }
jobs.each do |job_id, job|
  Array(job["steps"]).each do |step|
    next unless step["run"].is_a?(String)

    step["run"].lines.each do |line|
      command = line.strip
      bootstrap_calls[job_id] << command if command.include?("scripts/bootstrap.sh")
    end
  end
end
execution_profiles.each do |job_id, profile|
  expected = "scripts/bootstrap.sh #{profile}"
  reject("#{job_id} must run exactly #{expected}") unless bootstrap_calls.fetch(job_id) == [expected]
end
unexpected_bootstrap = bootstrap_calls.keys - execution_profiles.keys
reject("unexpected workflow bootstrap caller: #{unexpected_bootstrap.join(', ')}") unless unexpected_bootstrap.empty?
reject("workflow contains a bare bootstrap invocation") if
  bootstrap_calls.values.flatten.any? { |command| command.match?(/\A(?:\.\/)?scripts\/bootstrap\.sh\z/) }

cache_restore = Hash.new { |hash, key| hash[key] = [] }
cache_save = Hash.new { |hash, key| hash[key] = [] }
jobs.each do |job_id, job|
  Array(job.fetch("steps")).each do |step|
    use = step["uses"].to_s
    if use.start_with?("actions/cache/restore@")
      reject("#{job_id} cache restore must be SHA-pinned") unless use.match?(/\Aactions\/cache\/restore@[0-9a-f]{40}\z/)
      cache_restore[job_id] << step
    elsif use.start_with?("actions/cache/save@")
      reject("#{job_id} cache save must be SHA-pinned") unless use.match?(/\Aactions\/cache\/save@[0-9a-f]{40}\z/)
      condition = step.fetch("if")
      reject("#{job_id} cache publication must be limited to successful main pushes") unless
        condition.include?("success()") &&
        condition.include?("github.event_name == 'push'") &&
        condition.include?("github.ref == 'refs/heads/main'") &&
        condition.include?("outputs.cache-hit != 'true'")
      reject("#{job_id} cache save must reuse its restore primary key") unless
        step.fetch("with").fetch("key").match?(/\A\$\{\{ steps\.[a-z0-9-]+\.outputs\.cache-primary-key \}\}\z/)
      cache_save[job_id] << step
    elsif use.start_with?("actions/cache")
      reject("#{job_id} must use explicit cache restore/save actions")
    end
  end
end

def paths(step)
  step.fetch("with").fetch("path").lines.map(&:strip).reject(&:empty?)
end

tooling_inputs = ["toolchains.lock", "scripts/checksums.txt", "scripts/bootstrap.sh", "scripts/env.sh"]
execution_profiles.each do |job_id, profile|
  tooling = cache_restore.fetch(job_id).select do |step|
    step.fetch("with").fetch("key").start_with?("tooling-v4-")
  end
  reject("#{job_id} must restore one verified tooling cache") unless tooling.length == 1
  key = tooling.first.fetch("with").fetch("key")
  reject("#{job_id} tooling cache has the wrong profile") unless
    key.start_with?("tooling-v4-#{profile}-${{ runner.os }}-${{ runner.arch }}-")
  tooling_inputs.each { |input| reject("#{job_id} tooling key is missing #{input}") unless key.include?(input) }
  reject("#{job_id} tooling cache must be exact-key-only") if tooling.first.fetch("with").key?("restore-keys")
  reject("#{job_id} tooling paths changed") unless paths(tooling.first).sort == [".cache/downloads", ".tools"].sort
end

go_jobs = %w[runtime-artifacts go-standard go-race database production-relays security-license]
go_keys = go_jobs.map do |job_id|
  candidates = cache_restore.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("go-v4-") }
  reject("#{job_id} must restore one shared Go cache") unless candidates.length == 1
  cache = candidates.first
  reject("#{job_id} Go cache paths changed") unless
    paths(cache).sort == [".cache/go-build", ".cache/go-mod", ".cache/gopath"].sort
  key = cache.fetch("with").fetch("key")
  ["toolchains.lock", "go.work", "go.work.sum", "control-plane/go.mod", "control-plane/go.sum", "${{ github.sha }}"].each do |input|
    reject("#{job_id} Go cache key is missing #{input}") unless key.include?(input)
  end
  restore_keys = cache.fetch("with").fetch("restore-keys").lines.map(&:strip).reject(&:empty?)
  reject("#{job_id} Go cache must fall back by commit and dependency prefix") unless restore_keys.length == 2
  key
end
reject("Go jobs do not share one cache namespace") unless go_keys.uniq.length == 1
reject("only go-standard may publish the shared Go cache") unless
  cache_save.keys.select { |job_id| cache_save[job_id].any? { |step| paths(step).include?(".cache/go-build") } } == ["go-standard"]

npm_jobs = %w[web-validation contracts-policy security-license]
npm_keys = npm_jobs.map do |job_id|
  candidates = cache_restore.fetch(job_id).select { |step| step.fetch("with").fetch("key").start_with?("npm-v4-") }
  reject("#{job_id} must restore one shared npm cache") unless candidates.length == 1
  cache = candidates.first
  reject("#{job_id} npm cache path changed") unless paths(cache) == [".cache/npm"]
  key = cache.fetch("with").fetch("key")
  reject("#{job_id} npm cache key is incomplete") unless
    key.include?("toolchains.lock") && key.include?("web/package-lock.json")
  key
end
reject("npm jobs do not share one cache namespace") unless npm_keys.uniq.length == 1
reject("only web-validation may publish the shared npm cache") unless
  cache_save.keys.select { |job_id| cache_save[job_id].any? { |step| paths(step) == [".cache/npm"] } } == ["web-validation"]

all_cached_paths = cache_restore.values.flatten.flat_map { |step| paths(step) }
reject("Rust target archives must be replaced by sccache") if all_cached_paths.include?("rust/target")
reject("workflow must not cache node_modules") if all_cached_paths.any? { |path| path.include?("node_modules") }

sccache_jobs = %w[runtime-artifacts production-relays rust-validation security-license native-ocserv]
sccache_action = "mozilla-actions/sccache-action@fc920bf0ec8de6ee65d409111f7ec508035751ba"
sccache_jobs.each do |job_id|
  job = jobs.fetch(job_id)
  env = job.fetch("env")
  reject("#{job_id} must disable Cargo incremental compilation") unless env.fetch("CARGO_INCREMENTAL") == "0"
  reject("#{job_id} must use sccache as the Rust compiler wrapper") unless env.fetch("RUSTC_WRAPPER") == "sccache"
  reject("#{job_id} must use the GitHub Actions sccache backend") unless env.fetch("SCCACHE_GHA_ENABLED") == "true"
  reject("#{job_id} must normalize the sccache workspace base") unless env.fetch("SCCACHE_BASEDIRS") == "${{ github.workspace }}"
  reject("#{job_id} must select a usable sccache backend before bootstrap") unless
    Array(job.fetch("steps")).any? { |step| step["run"] == "scripts/configure-sccache.sh" }
  setup = Array(job.fetch("steps")).find { |step| step["uses"] == sccache_action }
  reject("#{job_id} must expose the GitHub Actions cache credentials through the pinned sccache action") unless setup
  reject("#{job_id} must request the repository-pinned sccache release") unless
    setup.fetch("with").fetch("version") == "v0.17.0"
  reject("#{job_id} must use the explicit sccache statistics step") unless
    setup.fetch("with").fetch("disable_annotations") == true
end
reject("sccache version is not pinned") unless toolchains.match?(/^sccache=0\.17\.0$/)
%w[aarch64-apple-darwin x86_64-unknown-linux-musl].each do |platform|
  reject("sccache checksum is missing for #{platform}") unless
    checksums.include?("sccache-v0.17.0-#{platform}.tar.gz")
end
reject("bootstrap must install sccache from verified downloads") unless bootstrap.include?("install_sccache()")

download_pin = "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c"
%w[database local-slice].each do |job_id|
  steps = Array(jobs.fetch(job_id).fetch("steps"))
  reject("#{job_id} must download the commit-bound runtime artifact") unless
    steps.any? { |step| step["uses"] == download_pin }
  reject("#{job_id} must verify the runtime artifact against GITHUB_SHA") unless
    steps.any? { |step| step["run"].to_s.include?("ci-runtime-artifact.sh extract") && step["run"].include?("GITHUB_SHA") }
end
runtime_upload = Array(jobs.fetch("runtime-artifacts").fetch("steps")).find do |step|
  step["uses"].to_s.start_with?("actions/upload-artifact@")
end
reject("runtime artifact upload must be SHA-pinned") unless
  runtime_upload && runtime_upload.fetch("uses").match?(/\Aactions\/upload-artifact@[0-9a-f]{40}\z/)
reject("runtime artifact must have one-day retention") unless runtime_upload.fetch("with").fetch("retention-days") == 1
jobs.each do |job_id, job|
  Array(job.fetch("steps")).select { |step| step["uses"].to_s.start_with?("actions/upload-artifact@") }.each do |step|
    reject("#{job_id} artifact retention exceeds the repository limit") unless
      step.fetch("with").fetch("retention-days") == 1
  end
end

database = jobs.fetch("database")
matrix = database.fetch("strategy").fetch("matrix").fetch("pg-major")
reject("PostgreSQL 17 and 18 must run as a matrix") unless matrix == [17, 18]
reject("PostgreSQL matrix must not fail fast") unless database.fetch("strategy").fetch("fail-fast") == false
database_step = Array(database.fetch("steps")).find { |step| step["run"] == "scripts/database-integration.sh" }
reject("database job must pass one PG_MAJOR to the script") unless
  database_step && database_step.fetch("env").fetch("PG_MAJOR") == "${{ matrix.pg-major }}"

stage_run = Array(jobs.fetch("stage-contracts").fetch("steps")).map { |step| step["run"].to_s }.join("\n")
%w[
  i14-quota-expiry-backport.sh i15-config-plan.sh i16-config-apply.sh
  i17-certificate-secret.sh i19-five-minute-offline-recovery.sh
].each do |script|
  reject("#{script} must run contract-only in CI") unless stage_run.include?("#{script} --contract-only")
end
production_run = Array(jobs.fetch("production-relays").fetch("steps")).map { |step| step["run"].to_s }.join("\n")
reject("I18 must not repeat language suites") unless production_run.include?("i18-production-relays.sh --contract-only")
reject("Go standard job must retain ordinary tests") unless
  Array(jobs.fetch("go-standard").fetch("steps")).any? { |step| step["run"].to_s.include?("go-check.sh standard") }
reject("Go race job must retain the full race suite") unless
  Array(jobs.fetch("go-race").fetch("steps")).any? { |step| step["run"].to_s.include?("go-check.sh race") }
reject("Rust validation must retain the full Rust suite") unless
  Array(jobs.fetch("rust-validation").fetch("steps")).any? { |step| step["run"].to_s.include?("scripts/rust-check.sh") }

expected_p1 = {
  "AGENT_COUNT" => 24,
  "HEARTBEAT_COUNT" => 2,
  "HEARTBEAT_INTERVAL_MS" => 500,
  "MINIMUM_RESOURCE_SAMPLES" => 8,
  "P1_PROFILE" => "smoke",
  "QUEUE_CAPACITY" => 256,
  "REQUEST_CONCURRENCY" => 8
}
reject("P1 smoke profile changed") unless jobs.fetch("p1-smoke").fetch("env") == expected_p1

native_steps = Array(jobs.fetch("native-ocserv").fetch("steps"))
native_run = native_steps.find { |step| step["name"] == "Native ocpasswd, OpenSSL, and Ocserv login" }.fetch("run")
reject("Native Ocserv must use an isolated Cargo target") unless native_run.include?("CARGO_TARGET_DIR=\"${native_target}\"")
reject("Native Ocserv must remove its isolated Cargo target") unless native_run.include?("sudo rm -rf \"${native_target}\"")

p1_trigger = p1_workflow.fetch(true)
reject("P1 Full must remain workflow_dispatch-only") unless p1_trigger.keys == ["workflow_dispatch"]
p1_job = p1_workflow.fetch("jobs").fetch("p1-full")
reject("P1 Full must use ubuntu-24.04") unless p1_job.fetch("runs-on") == "ubuntu-24.04"
reject("P1 Full profile changed") unless p1_job.fetch("env").fetch("P1_PROFILE") == "full"
reject("P1 Full timeout changed") unless p1_job.fetch("timeout-minutes") == 45
RUBY

for script in \
  i14-quota-expiry-backport.sh \
  i15-config-plan.sh \
  i16-config-apply.sh \
  i17-certificate-secret.sh \
  i18-production-relays.sh \
  i19-five-minute-offline-recovery.sh; do
  if "${ROOT}/scripts/${script}" --unsupported >/dev/null 2>&1; then
    echo "${script} accepted an unsupported execution mode" >&2
    exit 1
  fi
done

if "${ROOT}/scripts/go-check.sh" unsupported >/dev/null 2>&1; then
  echo "go-check.sh accepted an unsupported execution mode" >&2
  exit 1
fi

"${ROOT}/scripts/test-real-e2e-workflow.sh"
