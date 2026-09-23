#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/scripts/bootstrap.sh"
WORKFLOW="${ROOT}/.github/workflows/ci.yml"
RELEASE_WORKFLOW="${ROOT}/.github/workflows/release.yml"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands ruby jq
bash "${ROOT}/scripts/test-bootstrap-platforms.sh"

set +e
output="$(GITHUB_ACTIONS=true "${BOOTSTRAP}" 2>&1)"
status=$?
set -e
if [[ ${status} -ne 2 ]] || [[ "${output}" != *"bootstrap profile must be explicit in GitHub Actions"* ]]; then
  echo "bootstrap without a profile must fail clearly in GitHub Actions" >&2
  exit 1
fi

ruby -r yaml - "${ROOT}" "${WORKFLOW}" "${RELEASE_WORKFLOW}" <<'RUBY'
root, workflow_path, release_workflow_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
release_workflow = YAML.safe_load(File.read(release_workflow_path), aliases: true)
jobs = workflow.fetch("jobs")

def reject(message)
  warn message
  exit 1
end

worker_flags = {
  "docs" => "run_docs", "go" => "run_go", "rust" => "run_rust",
  "web" => "run_web", "database-smoke" => "run_database"
}
reject("Basic CI triggers drifted") unless workflow.fetch(true).keys.sort == %w[pull_request push workflow_call workflow_dispatch]
reject("reusable Basic CI must require an explicit profile") unless
  workflow.fetch(true).dig("workflow_call", "inputs", "profile") == {"type" => "string", "required" => true}
reject("Basic CI permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read"}
worker_flags.each do |id, flag|
  condition = "needs.ci-relevance.outputs.#{flag} == 'true'"
  condition += " || needs.ci-relevance.outputs.run_ci_tools == 'true'" if id == "go"
  condition += " || needs.ci-relevance.outputs.run_installers == 'true'" if id == "rust"
  reject("#{id} must follow routing") unless jobs.fetch(id).fetch("if") == condition
end
%w[go rust].each do |id|
  steps = jobs.fetch(id).fetch("steps")
  heavy = steps.select { |s| s.fetch("run", "").match?(/scripts\/(go-check|rust-check)\.sh|bootstrap.sh rust-basic/) }
  reject("#{id} heavy steps must not run for tools/installers alone") unless
    !heavy.empty? && heavy.all? { |s| s["if"] == "needs.ci-relevance.outputs.run_#{id} == 'true'" }
end
database = jobs.fetch("database-smoke")
reject("matrix must use the router's Quick/Full selection") unless
  database.fetch("strategy").fetch("matrix") == "${{ fromJSON(needs.ci-relevance.outputs.database_matrix) }}" &&
  database.fetch("env").fetch("DATABASE_TEST_SCOPE") == "${{ needs.ci-relevance.outputs.database_scope }}"
reject("Full retains short recovery on both implementations") unless
  jobs.fetch("database-recovery-full").fetch("strategy").fetch("matrix") == {"engine" => %w[mysql mariadb]}
reject("heavy history must not be a Basic CI job") if jobs.key?("database-history-full")
reject("Quick and Full must have separate concurrency groups") unless
  workflow.fetch("concurrency").fetch("group").include?("inputs.profile || 'quick'") &&
  workflow.fetch("concurrency").fetch("group").include?("github.ref")
guard = jobs.fetch("go").fetch("steps").find { |s| s["name"] == "CI guard and workflow contracts" }
reject("CI self-tests must be path selected") unless guard.fetch("if") == "needs.ci-relevance.outputs.run_ci_tools == 'true'"
jobs.each do |id, job|
  reject("#{id} needs a bounded runner") unless job.fetch("timeout-minutes") > 0
  reject("#{id} must propagate failures") if job["continue-on-error"]
  Array(job["steps"]).each do |step|
    reject("#{id} must propagate step failures") if step["continue-on-error"]
    next unless step["uses"]
    reject("#{id} has an unpinned action") unless step["uses"].match?(/@[0-9a-f]{40}\z/)
  end
end
result = jobs.fetch("basic-ci-result")
reject("required check identity or dependencies changed") unless
  result.fetch("name") == "Basic CI Result" && result.fetch("if") == "always()" &&
  result.fetch("needs").sort == (worker_flags.keys + %w[ci-relevance database-recovery-full]).sort
require "json"
require "open3"
summary = result.fetch("steps").first.fetch("run")
%w[quick full].each do |profile|
  selections = profile == "full" ? [worker_flags.keys] : [%w[docs], %w[web], %w[rust], %w[go database-smoke], %w[ci_tools], %w[installers], %w[ci_tools installers], worker_flags.keys]
  selections.each do |selected|
    flags = worker_flags.to_h { |id, flag| [flag, selected.include?(id).to_s] }
    flags["run_ci_tools"] = selected.include?("ci_tools").to_s
    flags["run_installers"] = selected.include?("installers").to_s
    selected = selected.dup
    selected << "go" if flags["run_ci_tools"] == "true"
    selected << "rust" if flags["run_installers"] == "true"
    needs = {"ci-relevance" => {"result" => "success", "outputs" => flags.merge("profile" => profile)}}
    worker_flags.each_key { |id| needs[id] = {"result" => selected.include?(id) ? "success" : "skipped"} }
    needs["database-recovery-full"] = {"result" => profile == "full" ? "success" : "skipped"}
    check = ->(data) { Open3.capture3({"RESULTS" => JSON.generate(data)}, "bash", "-eo", "pipefail", "-c", summary).last.success? }
    reject("summary rejected valid #{profile} selection") unless check.call(needs)
    needs.each_key do |id|
      %w[failure cancelled skipped missing].each do |state|
        next if state == needs[id]["result"]
        broken = Marshal.load(Marshal.dump(needs))
        if state == "missing" then broken.delete(id)
        else broken[id]["result"] = state end
        reject("summary accepted #{id}/#{state}") if check.call(broken)
      end
    end
    needs["ci-relevance"]["outputs"].delete("run_database")
    reject("summary accepted missing routing") if check.call(needs)
  end
end

# Exercise the selected entrypoints, with only unrelated tests and Go commands
# replaced by recording stubs. The Release upgrade contract itself stays real.
require "tmpdir"
require "fileutils"
standard = jobs.fetch("go").fetch("steps").find { |step| step["run"] == "scripts/go-check.sh standard" }
reject("standard Go checks must follow run_go") unless standard && standard["if"] == "needs.ci-relevance.outputs.run_go == 'true'"
reject("tool checks must receive the same Go selection") unless
  guard.fetch("env") == {"CI_SUITES" => "${{ needs.ci-relevance.outputs.ci_suites }}", "CI_RUN_GO" => "${{ needs.ci-relevance.outputs.run_go }}"}
Dir.mktmpdir("ci-entrypoints-") do |tmp|
  work = File.join(tmp, "work")
  files = Dir.glob(File.join(root, "scripts", "*")).select { |path| File.file?(path) }
  files += %w[.github/workflows/release.yml .github/workflows/release-upgrade.yml
              .github/workflows/release-products.yml .github/workflows/release-product-upgrade.yml
              rust/agent-build.Dockerfile].map { |path| File.join(root, path) }
  files.each do |source|
    target = File.join(work, source.delete_prefix(root + "/"))
    FileUtils.mkdir_p(File.dirname(target))
    FileUtils.cp(source, target)
  end
  Dir.glob(File.join(work, "scripts", "test-*.{sh,mjs}")).each do |path|
    next if %w[test-release-upgrade.sh test-release-upgrade.mjs].include?(File.basename(path))
    stub = if path.end_with?(".sh")
      "#!/usr/bin/env bash\nprintf '%s\\n' \"${0##*/}\" >> \"${CI_TRACE}\"\n"
    else
      "import fs from 'node:fs'; fs.appendFileSync(process.env.CI_TRACE, #{(File.basename(path) + "\n").to_json});\n"
    end
    File.write(path, stub)
  end
  %w[bin control-plane tools/g6-harness].each { |path| FileUtils.mkdir_p(File.join(work, path)) }
  %w[go gofmt].each do |name|
    path = File.join(work, "bin", name)
    File.write(path, "#!/usr/bin/env bash\nprintf '%s|%s|%s\\n' \"${0##*/}\" \"${PWD}\" \"$*\" >> \"${CI_TRACE}\"\n")
    FileUtils.chmod(0755, path)
  end
  route = File.join(tmp, "route")
  FileUtils.mkdir_p(route)
  run = lambda do |env, *command, **options|
    output, status = Open3.capture2e(env, *command, **options)
    reject("#{command.join(' ')} failed: #{output}") unless status.success?
    output
  end
  git = ->(*args) { run.call({}, "git", "-C", route, *args).strip }
  git.call("init", "-q")
  git.call("config", "user.name", "test")
  git.call("config", "user.email", "test@example.invalid")
  git.call("commit", "--allow-empty", "-qm", "base")
  base = git.call("rev-parse", "HEAD")
  g6_path = "tools/g6-harness/internal/runtime/runtime.go"
  {
    "Release-only" => [".github/workflows/release.yml"],
    "G6-only" => [g6_path],
    "Controller+G6" => ["control-plane/internal/platform/app/run.go", g6_path],
    "shared bootstrap" => ["scripts/bootstrap.sh"]
  }.each do |name, paths|
    git.call("checkout", "-q", "--detach", base)
    paths.each do |path|
      target = File.join(route, path)
      FileUtils.mkdir_p(File.dirname(target))
      File.write(target, "change\n")
    end
    git.call("add", ".")
    git.call("commit", "-qm", name)
    out = File.join(tmp, "routing")
    File.write(out, "")
    run.call({}, "bash", File.join(root, "scripts/ci-relevance.sh"), "pull_request", base, git.call("rev-parse", "HEAD"), out, chdir: route)
    routing = File.readlines(out, chomp: true).to_h { |line| line.split("=", 2) }
    reject("#{name} selected the wrong Go owner") unless
      routing.fetch("run_go") == (!%w[Release-only G6-only].include?(name)).to_s
    trace = File.join(tmp, "trace")
    File.write(trace, "")
    env = guard.fetch("env").transform_values do |value|
      routing.fetch(value.delete_prefix("${{ needs.ci-relevance.outputs.").delete_suffix(" }}"))
    end.merge("CI_TRACE" => trace, "PATH" => File.join(work, "bin") + ":" + ENV.fetch("PATH"))
    reject("#{name} omitted tool contracts") unless routing.fetch("run_ci_tools") == "true"
    run.call(env, "bash", "-eo", "pipefail", "-c", guard.fetch("run"), chdir: work)
    reject("#{name} tool suite duplicated standard Go checks") if
      routing.fetch("run_go") == "true" && File.readlines(trace).any? { |line| line.match?(/^go(fmt)?\|/) }
    run.call(env, "bash", "-eo", "pipefail", "-c", standard.fetch("run"), chdir: work) if routing.fetch("run_go") == "true"
    calls = File.readlines(trace, chomp: true)
    expected = name == "Release-only" ? 0 : 1
    reject("#{name} must format harness #{expected} times") unless calls.count { |line| line.start_with?("gofmt|") && line.include?("tools/g6-harness") } == expected
    ["vet ./...", "test -count=1 ./..."].each do |command|
      reject("#{name} must run harness #{command} #{expected} times") unless calls.count("go|#{work}/tools/g6-harness|#{command}") == expected
    end
    if expected == 1
      reject("#{name} lost G6 contract/evidence checks") unless calls.grep(/^test-g6-/).length == 13 &&
        %w[test-g6-workflow-contract.sh test-g6-evidence-pipeline.sh test-g6-evidence-verifier.mjs test-g6-resource-sampler.sh].all? { |test| calls.count(test) == 1 }
    else
      reject("Release-only must not run unrelated guards") if calls.include?("test-bootstrap-profiles.sh")
      path = File.join(work, ".github/workflows/release.yml")
      original = File.read(path)
      File.write(path, original.sub("uses: ./.github/workflows/security.yml", "uses: ./.github/workflows/ci.yml"))
      _, status = Open3.capture2e(env, "bash", "-eo", "pipefail", "-c", guard.fetch("run"), chdir: work)
      reject("Release-only accepted a substituted security entrypoint") if status.success?
      File.write(path, original)
    end
    puts "#{name}: selected entrypoints and harness command counts passed"
  end
end

release_jobs = release_workflow.fetch("jobs")
security = YAML.safe_load(File.read(File.join(root, ".github/workflows/security.yml")), aliases: true)
reject("security checks must support scheduled, manual, and release runs") unless
  security.fetch(true).keys.sort == %w[schedule workflow_call workflow_dispatch]
reject("security scans must be read-only") unless security.fetch("permissions") == {"contents" => "read"}
scan = security.fetch("jobs").fetch("scan")
reject("security scans must not cancel siblings on failure") unless scan.fetch("strategy").fetch("fail-fast") == false
checks = scan.fetch("strategy").fetch("matrix").fetch("include")
reject("security scans must cover secrets and all three dependency ecosystems") unless
  checks.map { |check| check.fetch("profile") }.sort == %w[g6-secret-scan go-security npm-security rust-security]
reject("secret scans need complete history") unless
  scan.fetch("steps").any? { |step| step.fetch("with", {})["fetch-depth"] == "${{ matrix.profile != 'g6-secret-scan' && 1 || 0 }}" }
bootstrap = File.read(File.join(root, "scripts/bootstrap.sh"))
dispatch = 'case "${PROFILE}" in' + bootstrap.split('case "${PROFILE}" in').last
stubs = bootstrap.scan(/^(install_\w+|verify_\w+)\(\)/).flatten.map do |name|
  "#{name}() { echo #{name}; }"
end.join("\n")
{
  "go-security" => %w[install_go install_govulncheck],
  "rust-security" => %w[install_rust install_cargo_audit install_cargo_deny],
  "npm-security" => %w[install_node install_npm],
  "package-tools" => %w[install_nfpm],
  "image-security" => %w[install_syft install_grype verify_host_command]
}.each do |profile, expected|
  output, status = Open3.capture2e({"PROFILE" => profile}, "bash", "-eu", "-c", stubs + "\n" + dispatch)
  reject("#{profile} installs unrelated tools/dependencies: #{output}") unless status.success? && output.split == expected
end
rust_scan = checks.find { |check| check["profile"] == "rust-security" }.fetch("command")
reject("workspace and Relay advisory scans must remain fresh and fail closed") unless
  rust_scan.lines.map(&:strip) == [
    "cd rust",
    "cargo audit",
    "cargo audit --file ../deploy/production/relay.Cargo.lock",
    "cargo deny --locked check advisories",
  ]
# test-release-upgrade.sh executes Release Check with failed security results
# and verifies that Publish depends on that gate.
products = YAML.safe_load(File.read(File.join(root, ".github/workflows/release-products.yml")), aliases: true)
build_steps = products.fetch("jobs").fetch("build-agent-packages").fetch("steps")
restore = build_steps.find { |step| step["name"] == "Restore native-package tool cache" }
save = build_steps.find { |step| step["name"] == "Save native-package tool cache" }
release_build = build_steps.find { |step| step.fetch("run", "").include?("bash scripts/build-release-agent.sh") }
reject("release tool cache restore must expose its primary key") unless
  restore && restore["id"] == "native-package-tools-cache"
reject("release tool cache save must reuse restore path and primary key") unless
  save && save.fetch("with").fetch("path") == restore.fetch("with").fetch("path") &&
    save.fetch("with").fetch("key") == "${{ steps.native-package-tools-cache.outputs.cache-primary-key }}"
reject("release tool cache save must require a successful miss") unless
  save.fetch("if").include?("success()") &&
    save.fetch("if").include?("steps.native-package-tools-cache.outputs.cache-hit != 'true'")
reject("release tool cache must not save before native build success") unless
  release_build && build_steps.index(release_build) < build_steps.index(save)
publish = release_jobs.fetch("publish-release-packages")
reject("release publishing environment changed") unless publish.fetch("environment") == "release-publishing"
# The exact publishing permission set is pinned by
# test-controller-release-manifest.sh; here it must only stay job-local with
# release-asset write access.
reject("release publishing must retain contents write as a job-local permission") unless
  publish.fetch("permissions").fetch("contents", nil) == "write"
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
