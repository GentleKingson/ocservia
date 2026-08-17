#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-readiness.yml"
ARTIFACT_HELPER="${ROOT}/scripts/real-e2e-artifact.sh"
POSTGRES_INIT="${ROOT}/deploy/g6-readiness/postgres-init/001-g6-readiness.sh"

ruby -r yaml - "${WORKFLOW}" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)

def reject(message)
  warn message
  exit 1
end

concurrency = workflow.fetch("concurrency")
reject("G6 readiness must cancel an older run for the same authority") unless concurrency.fetch("cancel-in-progress") == true
for token in ["github.workflow", "github.ref", "inputs.authority"]
  reject("the G6 concurrency group must include #{token}") unless concurrency.fetch("group").include?(token)
end

jobs = workflow.fetch("jobs")
%w[g6-rd-fd-a g6-rd-fd-b].each do |job_id|
  steps = jobs.fetch(job_id).fetch("steps")
  hang_guard = steps.find { |step| step["name"] == "G6 readiness hang-guard tests" }
  reject("#{job_id} must run the hang-guard regression test") unless hang_guard&.fetch("run") == "scripts/test-g6-readiness-hang-guards.sh"

  diagnostics = steps.find { |step| step["name"]&.include?("Collect redacted") }
  cleanup = steps.find { |step| step["name"]&.start_with?("Clean failure domain") }
  upload = steps.find { |step| step["name"]&.include?("Upload failure domain") && step["name"]&.include?("diagnostics") }
  [[diagnostics, 5], [cleanup, 7], [upload, 5]].each do |step, timeout|
    reject("#{job_id} post-failure step is missing") unless step
    reject("#{job_id} post-failure step must run under always()") unless step.fetch("if") == "always()"
    reject("#{job_id} post-failure step must have timeout #{timeout}") unless step.fetch("timeout-minutes") == timeout
    reject("#{job_id} post-failure step must not mask the scenario result") if step["continue-on-error"] == true
  end
end

fd_b_names = jobs.fetch("g6-rd-fd-b").fetch("steps").map { |step| step["name"] }.compact
relay = fd_b_names.index("Start relay-b")
tunnel = fd_b_names.index("Start pinned tunnels")
standby = fd_b_names.index("Bootstrap the streaming standby")
reject("relay-b must be healthy before its pinned tunnel is advertised") unless relay && tunnel && relay < tunnel
reject("the standby must still bootstrap through the established pinned tunnel") unless tunnel && standby && tunnel < standby

critical_timeouts = {
  "g6-rd-fd-a" => {
    "Build failure domain A images and tunnel" => 35,
    "Enroll the failure domain A fleet" => 25,
    "Verify the PITR restore point" => 15,
    "Rejoin the former primary as standby" => 15
  },
  "g6-rd-fd-b" => {
    "Build failure domain B images and tunnel" => 35,
    "Bootstrap the streaming standby" => 15,
    "Enroll the failure domain B fleet" => 25,
    "Promote the standby under load" => 15,
    "Run the bounded observation window" => 10,
    "Build and verify the evidence bundle" => 15
  }
}
critical_timeouts.each do |job_id, expected|
  steps = jobs.fetch(job_id).fetch("steps")
  expected.each do |name, timeout|
    step = steps.find { |candidate| candidate["name"] == name }
    reject("#{job_id} is missing #{name}") unless step
    reject("#{name} must have timeout #{timeout}") unless step.fetch("timeout-minutes") == timeout
  end
end
RUBY

grep -qF 'REAL_E2E_ARTIFACT_CONNECT_TIMEOUT_SECONDS:-5' "${ARTIFACT_HELPER}" || {
  echo "artifact API calls must have a connect timeout" >&2
  exit 1
}
grep -qF 'REAL_E2E_ARTIFACT_API_TIMEOUT_SECONDS:-20' "${ARTIFACT_HELPER}" || {
  echo "artifact API calls must have a hard request timeout" >&2
  exit 1
}
grep -q -- '--retry-max-time' "${ARTIFACT_HELPER}" || {
  echo "artifact API retries must have a cumulative hard timeout" >&2
  exit 1
}
# shellcheck disable=SC2016  # assert the literal run-attempt expression
grep -qF '/attempts/${GITHUB_RUN_ATTEMPT}/jobs?per_page=100' "${ARTIFACT_HELPER}" || {
  echo "artifact waits must inspect the peer job in the exact run attempt" >&2
  exit 1
}
grep -qF "archive_command = 'test -f /var/lib/postgresql/archive/%f || cp %p /var/lib/postgresql/archive/%f'" "${POSTGRES_INIT}" || {
  echo "WAL archiving must succeed when PostgreSQL retries an already archived segment" >&2
  exit 1
}

# Exercise the asymmetric-failure path without touching GitHub. A failed peer
# step must terminate the wait immediately rather than consuming the artifact
# rendezvous's 30- or 40-minute outer bound.
tmp="$(mktemp -d)"
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
for argument in "$@"; do
  case "${argument}" in
    */jobs\?*)
      cat <<'JSON'
{"jobs":[{"id":42,"name":"G6 Readiness Failure Domain B","status":"in_progress","conclusion":null,"steps":[{"name":"Enroll the failure domain B fleet","status":"completed","conclusion":"failure"}]}]}
JSON
      exit 0
      ;;
    */artifacts\?*)
      printf '%s\n' '{"artifacts":[]}'
      exit 0
      ;;
  esac
done
echo "unexpected fake curl invocation" >&2
exit 22
FAKE_CURL
chmod +x "${tmp}/bin/curl"

started="${SECONDS}"
set +e
PATH="${tmp}/bin:${PATH}" \
GITHUB_RUN_ID=12345 \
GITHUB_RUN_ATTEMPT=2 \
GITHUB_JOB=g6-rd-fd-a \
GITHUB_TOKEN=test-token \
GITHUB_REPOSITORY=GentleKingson/ocservia \
GITHUB_API_URL=https://api.github.invalid \
RUNNER_TEMP="${tmp}" \
  "${ARTIFACT_HELPER}" wait-download \
    g6-rd-tunnel-fd-b-12345-2 "${tmp}/download" 60 \
    >"${tmp}/stdout" 2>"${tmp}/stderr"
status=$?
set -e

[[ "${status}" -eq 1 ]] || {
  echo "a failed peer must make artifact waiting fail closed" >&2
  cat "${tmp}/stderr" >&2
  exit 1
}
((SECONDS - started < 10)) || {
  echo "a failed peer was not detected promptly" >&2
  exit 1
}
grep -qF 'peer job G6 Readiness Failure Domain B failed at step Enroll the failure domain B fleet (failure)' "${tmp}/stderr" || {
  echo "peer failure diagnostics must identify the failed step" >&2
  cat "${tmp}/stderr" >&2
  exit 1
}
