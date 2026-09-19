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
reject("Basic CI triggers drifted") unless workflow.fetch(true).keys.sort == %w[pull_request push workflow_dispatch]
reject("Basic CI permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read"}
worker_flags.each do |id, flag|
  reject("#{id} must follow routing") unless jobs.fetch(id).fetch("if") == "needs.ci-relevance.outputs.#{flag} == 'true'"
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
  selections = profile == "full" ? [worker_flags.keys] : [%w[docs], %w[web], %w[rust], %w[go database-smoke], worker_flags.keys]
  selections.each do |selected|
    flags = worker_flags.to_h { |id, flag| [flag, selected.include?(id).to_s] }
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

release_jobs = release_workflow.fetch("jobs")
security = YAML.safe_load(File.read(File.join(root, ".github/workflows/security.yml")), aliases: true)
reject("security checks must support scheduled, manual, and release runs") unless
  security.fetch(true).keys.sort == %w[schedule workflow_call workflow_dispatch]
reject("security scans must be read-only") unless security.fetch("permissions") == {"contents" => "read"}
scan = security.fetch("jobs").fetch("scan")
reject("security scans must not cancel siblings on failure") unless scan.fetch("strategy").fetch("fail-fast") == false
checks = scan.fetch("strategy").fetch("matrix").fetch("include")
reject("security scans must cover secrets and all three dependency ecosystems") unless
  checks.map { |check| check.fetch("profile") }.sort == %w[g6-secret-scan go-quality rust-validation web]
reject("secret scans need complete history") unless
  scan.fetch("steps").any? { |step| step.fetch("with", {})["fetch-depth"] == 0 }
reject("release must call candidate security checks") unless
  release_jobs.fetch("security").fetch("uses") == "./.github/workflows/security.yml"
reject("publishing must wait for security success") unless
  release_jobs.fetch("publish-release-packages").fetch("needs").include?("security")
build_steps = release_jobs.fetch("build-agent-packages").fetch("steps")
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
