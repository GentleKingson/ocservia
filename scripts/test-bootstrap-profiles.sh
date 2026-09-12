#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/scripts/bootstrap.sh"
WORKFLOW="${ROOT}/.github/workflows/ci.yml"
RELEASE_WORKFLOW="${ROOT}/.github/workflows/release.yml"

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
bootstrap = File.read(File.join(root, "scripts/bootstrap.sh"))
makefile = File.read(File.join(root, "Makefile"))
checksums = File.read(File.join(root, "scripts/checksums.txt"))
toolchains = File.read(File.join(root, "toolchains.lock"))
database_script = File.read(File.join(root, "scripts/database-integration.sh"))
actions_doc = File.read(File.join(root, "docs/development/github-actions.md"))

def reject(message)
  warn message
  exit 1
end

worker_flags = {
  "docs" => "run_docs", "go" => "run_go", "rust" => "run_rust",
  "web" => "run_web", "database-smoke" => "run_database"
}
reject("Basic CI job set drifted") unless
  jobs.keys.sort == (worker_flags.keys + %w[ci-relevance basic-ci-result database-history-full]).sort
reject("workflow name must be Basic CI") unless workflow.fetch("name") == "Basic CI"
trigger = workflow.fetch(true)
reject("Basic CI triggers drifted") unless trigger.keys.sort == %w[pull_request push workflow_dispatch]
reject("pushes must target main only") unless trigger.fetch("push") == {"branches" => ["main"]}
reject("manual profile must default to full with only quick/full choices") unless
  trigger.fetch("workflow_dispatch").fetch("inputs").fetch("profile") == {
    "description" => "Basic CI coverage", "type" => "choice", "options" => %w[quick full],
    "default" => "full", "required" => true
  }
reject("Basic CI permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read"}
reject("only newer commits to the same PR may cancel a run") unless workflow.fetch("concurrency") == {
  "group" => "${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}",
  "cancel-in-progress" => "${{ github.event_name == 'pull_request' }}"
}
worker_flags.each do |id, flag|
  job = jobs.fetch(id)
  reject("#{id} must depend only on routing") unless job.fetch("needs") == "ci-relevance"
  reject("#{id} must use its basic flag") unless job.fetch("if") == "needs.ci-relevance.outputs.#{flag} == 'true'"
end
router = jobs.fetch("ci-relevance")
reject("router must expose five domains and the resolved mode") unless
  router.fetch("outputs").keys.sort == (worker_flags.values + %w[profile database_scope]).sort
reject("router requires full history") unless router.fetch("steps").any? { |step| step.fetch("with", {})["fetch-depth"] == 0 }
relevance = router.fetch("steps").find { |step| step["id"] == "relevance" }
reject("router must receive the unmodified manual input") unless
  relevance.fetch("env").fetch("CI_PROFILE") == "${{ inputs.profile }}" &&
  relevance.fetch("run") == 'scripts/ci-relevance.sh "${GITHUB_EVENT_NAME}" "${BASE_SHA}" "${HEAD_SHA}" "${GITHUB_OUTPUT}" "${CI_PROFILE}"'

expected_commands = {
  "docs" => ["scripts/docs-check.sh"],
  "go" => ["scripts/bootstrap.sh go-test", "scripts/go-check.sh standard"],
  "rust" => ["scripts/bootstrap.sh rust-basic", "scripts/rust-check.sh"],
  "web" => ["scripts/bootstrap.sh web", "source scripts/env.sh\ncd web\nnpx playwright install --with-deps chromium\n", "scripts/web-check.sh"],
  "database-history-full" => ["scripts/bootstrap.sh go-test", "bash scripts/database-foundation-integration.sh", 'echo "${ENGINE}=success" >> "${GITHUB_OUTPUT}"']
}
expected_commands.each do |id, commands|
  actual = jobs.fetch(id).fetch("steps").filter_map { |step| step["run"] }
  reject("#{id} must run only basic commands") unless actual == commands
end
{
  "go-test" => ["install_go", "verify_host_command jq"],
  "rust-basic" => ["install_rust", "install_rust_validation_components"],
  "web" => ["install_node", "install_npm", "install_web_dependencies"]
}.each do |profile, expected|
  body = bootstrap[/^  #{Regexp.escape(profile)}\)\n(.*?)^    ;;/m, 1]
  reject("#{profile} must install only its minimal tools") unless body.to_s.lines.map(&:strip).reject(&:empty?) == expected
end
reject("Web npm installs must disable audit and funding") unless jobs.fetch("web").fetch("env") == {
  "npm_config_audit" => "false", "npm_config_fund" => "false"
}
rust_check = File.read(File.join(root, "scripts/rust-check.sh"))
reject("basic Rust checks must not run audit or license checks") if rust_check.match?(/cargo (audit|deny)/)
database = jobs.fetch("database-smoke")
reject("database smoke must cover PostgreSQL 17/18, MySQL and MariaDB independently") unless
  database.fetch("env") == {"PG_MAJOR" => "${{ matrix.postgres }}", "ENGINE" => "${{ matrix.engine }}",
    "DATABASE_TEST_SCOPE" => "${{ needs.ci-relevance.outputs.database_scope }}"} &&
  database.fetch("strategy") == {"fail-fast" => false, "matrix" => {"include" => [
    {"engine" => "postgres", "postgres" => "17"},
    {"engine" => "postgres", "postgres" => "18"},
    {"engine" => "mysql"},
    {"engine" => "mariadb"}
  ]}}
database_commands = database.fetch("steps").select { |step| step.key?("run") }
reject("database jobs must route only to their own backend test script") unless
  database_commands == [
    {"run" => "scripts/bootstrap.sh go-test"},
    {"if" => "matrix.engine == 'postgres'", "run" => "scripts/database-integration.sh"},
    {"if" => "matrix.engine != 'postgres'",
     "run" => "unset DATABASE_FULL_PART\nif [[ \"${DATABASE_TEST_SCOPE}\" == full ]]; then export DATABASE_FULL_PART=current; fi\nbash scripts/database-foundation-integration.sh\n"},
    {"id" => "completed", "run" => 'echo "${ENGINE}${PG_MAJOR}=success" >> "${GITHUB_OUTPUT}"'}
  ]
history = jobs.fetch("database-history-full")
reject("history must consume routing with two independent full engines") unless
  history.fetch("needs") == "ci-relevance" &&
  history.fetch("if") == "needs.ci-relevance.outputs.profile == 'full'" &&
  history.fetch("strategy") == {"fail-fast" => false, "matrix" => {"engine" => %w[mysql mariadb]}} &&
  history.fetch("env") == {"ENGINE" => "${{ matrix.engine }}", "DATABASE_TEST_SCOPE" => "${{ needs.ci-relevance.outputs.database_scope }}", "DATABASE_FULL_PART" => "history"}
{"database-smoke" => %w[postgres17 postgres18 mysql mariadb], "database-history-full" => %w[mysql mariadb]}.each do |id, parts|
  reject("#{id} must expose every completed shard") unless jobs.fetch(id).fetch("outputs") ==
    parts.to_h { |part| [part, "${{ steps.completed.outputs.#{part} }}"] }
end
reject("database timeout budgets changed") unless [database, history].all? { |job| job.fetch("timeout-minutes") == 75 }
reject("database smoke must let the script build its own control binary") unless
  database_script.include?('go build -trimpath -o "${BIN}" ./cmd/ocserv-control')
foundation_script = File.read(File.join(root, "scripts/database-foundation-integration.sh"))
[database_script, foundation_script].each do |script|
  reject("manual scripts must default to full and reject invalid scope") unless
    script.include?('scope="${DATABASE_TEST_SCOPE-full}"') &&
    script.include?("full|regression) ;;") &&
    script.include?("DATABASE_TEST_SCOPE must be full or regression")
  reject("regression must explicitly select required groups") unless script.include?('--select')
  regression = script.split('if [[ "${scope}" == regression ]]; then', 2).last.split(/^else$/, 2).first
  reject("regression must not use a history blacklist") if regression.include?('-skip')
end
reject("pre-34 build must be full-only") unless
  database_script.index('if [[ "${scope}" == full ]]; then') < database_script.index('PRE34_ROOT="$(mktemp')
critical_pg = database_script.split('if [[ "${scope}" == regression ]]; then', 2).last.split("\n  else\n", 2).first
reject("PostgreSQL critical path must retain all business groups without an early exit") unless
  %w[regression-postgres regression-outbox regression-fencing regression-auth regression-oidc regression-telemetry].all? { |group| critical_pg.include?(group) } &&
  !critical_pg.include?('exit 0') && !critical_pg.include?('./internal/database/...') && !critical_pg.include?('PRE34')
reject("full MySQL must use complementary shards and retain business acceptance") unless
  foundation_script.include?('backend-mysql-current-full -race -timeout=60m "${package}" -skip "${history_pattern}"') &&
  foundation_script.include?('backend-mysql-history --select -race -timeout=60m') &&
  foundation_script.include?('if [[ "${part}" == history ]]; then exit 0; fi') &&
  foundation_script.include?("backend-coordination -race") && foundation_script.include?("backend-auth -race")
reject("PostgreSQL 17 must skip the legacy upgrade fixture") unless
  database_script.index('if [[ "${PG_MAJOR}" == "17" ]]; then') < database_script.index('container="${PREFIX}-upgrade"')
jobs.each do |id, job|
  reject("#{id} must use a bounded Ubuntu runner") unless
    job.fetch("runs-on") == "ubuntu-24.04" && job.fetch("timeout-minutes") > 0
  Array(job["steps"]).each do |step|
    next unless step["uses"]
    reject("#{id} has an unpinned action") unless step["uses"].match?(/@[0-9a-f]{40}\z/)
    reject("Basic CI must not have an artifact graph") if step["uses"].include?("artifact")
  end
end

result = jobs.fetch("basic-ci-result")
reject("Basic CI Result must always summarize exactly the basic jobs and router") unless
  result.fetch("name") == "Basic CI Result" && result.fetch("if") == "always()" &&
  result.fetch("needs").sort == (worker_flags.keys + %w[ci-relevance database-history-full]).sort
reject("Basic CI Result must be documented") unless actions_doc.include?("Basic CI Result")
require "json"
require "open3"
summary = result.fetch("steps").first.fetch("run")
# Exercise the actual summary command, including unexpected skips and missing flags.
%w[quick full].each do |profile|
[false, true].each do |selected|
  next if profile == "full" && !selected
  needs = {"ci-relevance" => {"result" => "success", "outputs" => worker_flags.values.to_h { |flag| [flag, selected.to_s] }}}
  needs["ci-relevance"]["outputs"].merge!("profile" => profile, "database_scope" => profile == "full" ? "full" : "regression")
  worker_flags.each_key { |id| needs[id] = {"result" => selected ? "success" : "skipped"} }
  needs["database-history-full"] = {"result" => profile == "full" ? "success" : "skipped"}
  {"database-smoke" => %w[postgres17 postgres18 mysql mariadb], "database-history-full" => %w[mysql mariadb]}.each do |id, parts|
    next unless needs[id]["result"] == "success"
    needs[id]["outputs"] = parts.to_h { |part| [part, "success"] }
  end
  _, _, status = Open3.capture3({"RESULTS" => JSON.generate(needs)}, "bash", "-eo", "pipefail", "-c", summary)
  reject("summary rejected valid selected/skipped results") unless status.success?
  %w[success failure cancelled skipped missing].each do |state|
    next if state == needs["database-history-full"]["result"]
    broken = Marshal.load(Marshal.dump(needs))
    if state == "missing" then broken.delete("database-history-full")
    else broken["database-history-full"]["result"] = state end
    _, _, status = Open3.capture3({"RESULTS" => JSON.generate(broken)}, "bash", "-eo", "pipefail", "-c", summary)
    reject("summary accepted unexpected history: #{profile}/#{state}") if status.success?
  end
  needs.each do |id, job|
    next unless id.start_with?("database-") && job["result"] == "success"
    job.fetch("outputs").each_key do |part|
      [nil, "", "failure", "cancelled", "skipped"].each do |value|
        broken = Marshal.load(Marshal.dump(needs))
        broken[id]["outputs"][part] = value
        _, _, status = Open3.capture3({"RESULTS" => JSON.generate(broken)}, "bash", "-eo", "pipefail", "-c", summary)
        reject("summary accepted incomplete shard: #{id}/#{part}") if status.success?
      end
    end
  end
  {"profile" => [nil, "", "invalid"], "database_scope" => [nil, "", "invalid", profile == "full" ? "regression" : "full"]}.each do |key, values|
    values.each do |value|
      broken = Marshal.load(Marshal.dump(needs))
      broken["ci-relevance"]["outputs"][key] = value
      _, _, status = Open3.capture3({"RESULTS" => JSON.generate(broken)}, "bash", "-eo", "pipefail", "-c", summary)
      reject("summary accepted invalid #{key}") if status.success?
    end
  end
  (worker_flags.keys + ["ci-relevance"]).each do |id|
    %w[failure cancelled skipped missing].each do |state|
      next if !selected && id != "ci-relevance" && state == "skipped"
      broken = Marshal.load(Marshal.dump(needs))
      if state == "missing" then broken.delete(id)
      else broken[id]["result"] = state end
      _, _, status = Open3.capture3({"RESULTS" => JSON.generate(broken)}, "bash", "-eo", "pipefail", "-c", summary)
      reject("summary accepted unexpected #{id}: #{state}") if status.success?
    end
  end
  needs["ci-relevance"]["outputs"].delete("run_go")
  _, _, status = Open3.capture3({"RESULTS" => JSON.generate(needs)}, "bash", "-eo", "pipefail", "-c", summary)
  reject("summary accepted a missing routing flag") if status.success?
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
release_build = build_steps.find { |step| step["name"] == "Build native Agent / privd / upgrader binaries" }
reject("release tool cache restore must expose its primary key") unless
  restore && restore["id"] == "native-package-tools-cache"
reject("release tool cache save must reuse restore path and primary key") unless
  save && save.fetch("with").fetch("path") == restore.fetch("with").fetch("path") &&
    save.fetch("with").fetch("key") == "${{ steps.native-package-tools-cache.outputs.cache-primary-key }}"
reject("release tool cache save must require a successful miss") unless
  save.fetch("if").include?("success()") &&
    save.fetch("if").include?("steps.native-package-tools-cache.outputs.cache-hit != 'true'")
reject("release tool cache must not save before native build success") unless
  build_steps.index(release_build) < build_steps.index(save)
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
