#!/usr/bin/env bash
# shellcheck disable=SC1090,SC2016,SC2030,SC2031,SC2329
# This test sources a path variable, matches literal expansions, and isolates
# environment mutations in subshell fixtures.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-readiness.yml"
CI_WORKFLOW="${ROOT}/.github/workflows/ci.yml"
COMPOSE_FILE="${ROOT}/deploy/g6-readiness/compose.yaml"
SUPERVISOR="${ROOT}/deploy/g6-readiness/agent-supervisor.sh"
AGENT_MAIN="${ROOT}/rust/crates/agent/src/main.rs"
LIB="${ROOT}/scripts/g6-readiness-lib.sh"
FD_A="${ROOT}/scripts/g6-readiness-fd-a.sh"
FD_B="${ROOT}/scripts/g6-readiness-fd-b.sh"
AUTHORITY_HISTORY_SQL="${ROOT}/scripts/g6-authority-history.sql"
CONTROL_APP="${ROOT}/control-plane/internal/platform/app/app.go"
CONTROL_CONFIG="${ROOT}/control-plane/internal/platform/config/config.go"
COORDINATION_MAINTENANCE="${ROOT}/control-plane/internal/coordination/maintenance.go"
BUILDER="${ROOT}/scripts/build-g6-evidence.mjs"
CONTRACT="${ROOT}/scripts/g6-contract-lib.mjs"
SLO="${ROOT}/docs/acceptance/g6-slo.yaml"
PROBE_DOCKERFILE="${ROOT}/rust/g6-probe.Dockerfile"
TRANSPORT_DOCKERFILE="${ROOT}/rust/transportd.Dockerfile"
TRANSPORT_LIB="${ROOT}/rust/crates/transportd/src/lib.rs"
G6_TUNNEL_LIB="${ROOT}/rust/crates/g6-tunnel/src/lib.rs"
RELAY_DOCKERFILE="${ROOT}/deploy/production/relay.Dockerfile"
POSTGRES_INIT="${ROOT}/deploy/g6-readiness/postgres-init/001-g6-readiness.sh"
OCSERV_FIXTURE="${ROOT}/deploy/g6-readiness/fake-ocserv/shims/ocserv"

ruby -r yaml - "${WORKFLOW}" "${COMPOSE_FILE}" "${CI_WORKFLOW}" <<'RUBY'
workflow_path, compose_path, ci_workflow_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
compose = YAML.safe_load(File.read(compose_path), aliases: true)
ci_workflow = YAML.safe_load(File.read(ci_workflow_path), aliases: true)

def reject(message)
  warn message
  exit 1
end

trigger = workflow.fetch(true)
reject("G6 readiness must remain workflow_dispatch-only") unless trigger.keys == ["workflow_dispatch"]
authority = trigger.fetch("workflow_dispatch").fetch("inputs").fetch("authority")
reject("the authority input must be a required choice") unless authority.fetch("type") == "choice" && authority.fetch("required") == true
reject("the authority enum is frozen") unless authority.fetch("options") == %w[engineering production_readiness]
reject("engineering must stay the default authority") unless authority.fetch("default") == "engineering"
reject("G6 readiness permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read", "actions" => "read"}
concurrency = workflow.fetch("concurrency")
reject("G6 readiness concurrency must bind ref and authority") unless concurrency.fetch("group").include?("github.ref") && concurrency.fetch("group").include?("inputs.authority")
reject("a replacement dispatch must cancel the same ref and authority") unless concurrency.fetch("cancel-in-progress") == true

jobs = workflow.fetch("jobs")
expected = %w[g6-rd-release-image g6-rd-fd-a g6-rd-fd-b g6-rd-secret-scan g6-rd-verifier]
reject("G6 readiness must contain exactly the five harness jobs") unless jobs.keys.sort == expected.sort
policy_commands = %w[
  scripts/test-g6-readiness-workflow.sh
  scripts/test-g6-readiness-hang-guards.sh
]
ci_jobs = ci_workflow.fetch("jobs")
contracts_steps = Array(ci_jobs.fetch("contracts-policy").fetch("steps"))
policy_commands.each do |command|
  ci_count = ci_jobs.values.sum do |job|
    Array(job.fetch("steps", [])).sum do |step|
      step.fetch("run", "").lines.count { |line| line.strip == command }
    end
  end
  reject("#{command} must run exactly once in required Contracts and Policy CI") unless ci_count == 1
  reject("Contracts and Policy CI must run #{command}") unless contracts_steps.any? do |step|
    step.fetch("run", "").lines.any? { |line| line.strip == command }
  end
  reject("failure-domain jobs must not repeat #{command}") if %w[g6-rd-fd-a g6-rd-fd-b].any? do |job_id|
    Array(jobs.fetch(job_id).fetch("steps")).any? do |step|
      step.fetch("run", "").lines.any? { |line| line.strip == command }
    end
  end
end
reject("Contracts and Policy must remain in the required quality aggregate") unless
  Array(ci_jobs.fetch("quality-security-native").fetch("needs")).include?("contracts-policy")
jobs.each do |job_id, job|
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} job env must not reference the step-only runner context") if job.fetch("env", {}).values.any? { |value| value.to_s.include?("runner.") }
  timeout_bound = job_id.start_with?("g6-rd-fd-") ? 90 : (job_id == "g6-rd-release-image" ? 35 : 20)
  reject("#{job_id} must stay within the bounded window") unless job.fetch("timeout-minutes") <= timeout_bound
  reject("#{job_id} Action is not pinned to a full SHA") if Array(job.fetch("steps")).any? { |step| step.key?("uses") && !step.fetch("uses").match?(/@[0-9a-f]{40}\z/) }
  reject("#{job_id} must not force a failing check green") if Array(job.fetch("steps")).any? { |step| step.key?("run") && step.fetch("run").include?("continue-on-error") }
  reject("#{job_id} must not mask a failed step") if Array(job.fetch("steps")).any? { |step| step["continue-on-error"] == true }
  environment = job.fetch("environment").fetch("name")
  reject("#{job_id} must gate both authorities through GitHub environments") unless environment.include?("g6-production-readiness") && environment.include?("g6-engineering-rehearsal") && environment.include?("inputs.authority")
end
fd_b_names = Array(jobs.fetch("g6-rd-fd-b").fetch("steps")).map { |step| step["name"] }.compact
peer_merge = fd_b_names.index("Merge the peer control evidence timeline")
relay_scenario = fd_b_names.index("Relay failover scenario")
scheduler_scenario = fd_b_names.index("Scheduler leadership failover scenario")
owner_scenario = fd_b_names.index("Connection owner fencing scenario")
reject("relay failover must run immediately after peer readiness, before longer fault scenarios") unless peer_merge && relay_scenario && scheduler_scenario && owner_scenario && relay_scenario == peer_merge + 1 && relay_scenario < scheduler_scenario && scheduler_scenario < owner_scenario
release_job = jobs.fetch("g6-rd-release-image")
release_steps = Array(release_job.fetch("steps"))
release_build = release_steps.find { |step| step["name"] == "Build and freeze the release images" }
release_upload = release_steps.find { |step| step["name"] == "Publish the frozen release images" }
release_cleanup = release_steps.find { |step| step["name"] == "Clean release-image resources" }
release_run = release_build&.fetch("run")
release_variables = %w[G6RD_CONTROL_PLANE_IMAGE G6RD_TRANSPORTD_IMAGE G6RD_RELAY_IMAGE G6RD_PROBE_IMAGE G6RD_AGENT_IMAGE]
required_dockerfiles = %w[control-plane/Dockerfile rust/transportd.Dockerfile deploy/production/relay.Dockerfile rust/g6-probe.Dockerfile rust/g6-agent.Dockerfile]
reject("the complete release image set must be candidate-labeled and exported once") unless release_run&.include?("org.opencontainers.image.revision=${GITHUB_SHA}") && required_dockerfiles.all? { |path| release_run.include?(path) } && release_run.include?("postgres:17.10-bookworm") && release_run.include?("docker save") && release_run.include?("sha256sum runtime-images.tar.gz image-ids.tsv")
tunnel_release_tokens = [
  "--target g6-tunnel-artifact",
  "--output \"type=local,dest=${tunnel_output}\"",
  "ocservia-g6-tunnel",
  "tunnel-manifest.tsv",
  "candidate_sha",
  "release-artifacts.sha256",
]
reject("the host-side tunnel must be built once and frozen with the release") unless
  tunnel_release_tokens.all? { |token| release_run.include?(token) }
reject("parallel release builds must be PID-scoped and propagate every failure") unless release_run.include?('build_pids+=("$!")') && release_run.include?('for pid in "${build_pids[@]}"') && release_run.include?('if ! wait "${pid}"') && release_run.include?('test "${build_status}" -eq 0')
reject("the release image artifact must be run scoped") unless release_upload&.fetch("with")&.fetch("name")&.include?("github.run_id") && release_upload.fetch("with").fetch("name").include?("github.run_attempt")
reject("the release image archive must use the step-scoped runner temp directory") unless release_upload.fetch("with").fetch("path").include?("runner.temp") && release_run.include?("RUNNER_TEMP")
reject("the release image producer must clean its scoped images") unless release_cleanup&.fetch("if") == "always()" && release_cleanup.fetch("timeout-minutes") == 5 && release_variables.all? { |variable| release_cleanup.fetch("run").include?(variable) }
release_images = release_variables.to_h { |variable| [variable, release_job.fetch("env").fetch(variable)] }
%w[g6-rd-fd-a g6-rd-fd-b].each do |job_id|
  reject("#{job_id} must depend only on the shared release image") unless jobs.fetch(job_id).fetch("needs") == "g6-rd-release-image"
  reject("#{job_id} must use the producer's complete release image set") unless release_variables.all? { |variable| jobs.fetch(job_id).fetch("env").fetch(variable) == release_images.fetch(variable) }
  reject("#{job_id} must use the exact 25-Agent formal default") if
    jobs.fetch(job_id).fetch("env", {}).keys.any? { |key| %w[G6_AGENTS_A G6_AGENTS_B].include?(key) }
  steps = Array(jobs.fetch(job_id).fetch("steps"))
  names = steps.map { |step| step["name"] }.compact
  download = steps.find { |step| step["name"] == "Download the frozen release images" }
  load = steps.find { |step| step["name"] == "Verify and load the release images" }
  reject("#{job_id} must download the exact run-scoped release images") unless download&.fetch("with")&.fetch("name") == release_upload.fetch("with").fetch("name")
  reject("#{job_id} must verify, load, and candidate-bind every release image") unless load&.fetch("run")&.include?("sha256sum --check") && load.fetch("run").include?("docker load") && load.fetch("run").include?("image-ids.tsv") && load.fetch("run").include?("org.opencontainers.image.revision") && load.fetch("run").include?("GITHUB_SHA") && release_variables.all? { |variable| load.fetch("run").include?(variable) }
  tunnel_load_tokens = [
    "release-artifacts.sha256",
    "tunnel-manifest.tsv",
    "candidate_sha",
    "expected_tunnel_sha",
    "sha256sum",
    "ocservia-g6-tunnel",
  ]
  reject("#{job_id} must candidate-bind and install the exact frozen tunnel") unless
    tunnel_load_tokens.all? { |token| load.fetch("run").include?(token) }
  bootstrap_runs = steps.each_with_object([]) do |step, runs|
    runs << step["run"] if step["run"]&.include?("scripts/bootstrap.sh")
  end
  reject("#{job_id} must bootstrap only the minimal pinned G6 Node runtime") unless
    bootstrap_runs == ["scripts/bootstrap.sh g6-runtime"]
  reject("#{job_id} must not perform a host-side Rust build") if
    steps.any? { |step| step.fetch("run", "").match?(/(?:bootstrap\.sh native|\bcargo (?:build|run|test)\b)/) }
  tooling = steps.find { |step| step["name"] == "Restore verified G6 Node runtime" }
  reject("#{job_id} tooling cache must bind the minimal G6 runtime lock") unless
    tooling&.fetch("with")&.fetch("key")&.include?("tooling-v4-g6-runtime-") &&
      tooling.fetch("with").fetch("key").include?("scripts/g6-runtime/package-lock.json")
  reject("#{job_id} must collect diagnostics before cleanup") unless names.index { |n| n.include?("diagnostics") }.to_i < names.index { |n| n.include?("Clean") }.to_i
  diagnostics = steps.find { |step| step["name"]&.include?("diagnostics") }.fetch("run")
  cleanup = steps.find { |step| step["name"]&.include?("Clean") }.fetch("run")
  reject("#{job_id} diagnostics must have a hard timeout") unless diagnostics.start_with?("timeout --signal=TERM --kill-after=15s 120s ")
  reject("#{job_id} cleanup must have a hard timeout") unless cleanup.start_with?("timeout --signal=TERM --kill-after=15s 180s ")
  peer = job_id.end_with?("a") ? "G6 Readiness Failure Domain B" : "G6 Readiness Failure Domain A"
  waits = steps.select { |step| step["run"]&.include?("real-e2e-artifact.sh wait-download") }
  reject("#{job_id} artifact waits must name their producer job") unless waits.all? { |step| step.fetch("run").end_with?(%Q{"#{peer}"}) }
end
fd_a_steps = Array(jobs.fetch("g6-rd-fd-a").fetch("steps"))
fd_b_steps = Array(jobs.fetch("g6-rd-fd-b").fetch("steps"))
relay_pre_fault = fd_b_steps.find { |step| step["name"] == "Capture the pre-fault relay-a session" }
reject("the pre-fault relay phase must outlive its existing bounded inner operations") unless
  relay_pre_fault&.fetch("timeout-minutes") == 8
resource_preflight = fd_b_steps.find { |step| step["name"] == "Preflight bounded resource evidence" }
window_step = fd_b_steps.find { |step| step["name"] == "Run the bounded observation window" }
reject("fd-b must run a hard-bounded real resource preflight") unless
  resource_preflight&.fetch("timeout-minutes") == 3 &&
  resource_preflight.fetch("run") == "timeout --signal=TERM --kill-after=15s 120s scripts/g6-readiness-fd-b.sh resource-preflight"
reject("the resource preflight must precede rather than replace the complete window") unless
  resource_preflight && window_step && fd_b_steps.index(resource_preflight) < fd_b_steps.index(window_step)
barrier_b_order = [
  "Arm failure domain B observation barriers",
  "Publish the observation-window barrier request",
  "Preflight bounded resource evidence",
  "Wait for failure domain A observation barriers",
  "Run the bounded observation window",
]
barrier_b_positions = barrier_b_order.map { |name| fd_b_steps.index { |step| step["name"] == name } }
reject("fd-b observation barrier rendezvous is incomplete") if barrier_b_positions.any?(&:nil?)
reject("fd-b must arm both domains before the all-fleet opening wave") unless
  barrier_b_positions == barrier_b_positions.sort &&
  window_step.fetch("run") == 'scripts/g6-readiness-fd-b.sh window "${RUNNER_TEMP}/g6-rd-window-barrier-armed-fd-a"'
barrier_a_order = [
  "Wait for the observation-window barrier request",
  "Arm failure domain A observation barriers",
  "Publish failure domain A barrier acknowledgement",
  "Release failure domain A barriers after the all-fleet proof",
  "Wait for the final freeze request",
]
barrier_a_positions = barrier_a_order.map { |name| fd_a_steps.index { |step| step["name"] == name } }
reject("fd-a observation barrier rendezvous is incomplete") if barrier_a_positions.any?(&:nil?)
reject("fd-a must acknowledge its barriers before waiting on the exact all-fleet proof") unless
  barrier_a_positions == barrier_a_positions.sort
reject("fd-a must use the trust-independent image build phase") unless fd_a_steps.any? { |step| step["run"] == "scripts/g6-readiness-fd-a.sh build-images" }
build_order = [
  "Wait for failure domain A rendezvous",
  "Import the peer tunnel identities",
  "Prepare failure domain B images",
  "Wait for the shared trust rendezvous",
  "Materialize the peer runtime trust",
  "Start relay-b",
  "Start pinned tunnels"
]
positions = build_order.map { |name| fd_b_steps.index { |step| step["name"] == name } }
reject("fd-b build/runtime steps are incomplete") if positions.any?(&:nil?)
reject("fd-b must build before waiting for shared trust, then materialize runtime state") unless positions == positions.sort
fd_b_build = fd_b_steps.fetch(positions.fetch(2))
reject("fd-b must use the trust-independent image build phase") unless fd_b_build.fetch("run") == "scripts/g6-readiness-fd-b.sh build-images"
fd_b_materialize = fd_b_steps.fetch(positions.fetch(4))
reject("fd-b must materialize real trust only after the rendezvous") unless fd_b_materialize.fetch("run").start_with?("scripts/g6-readiness-fd-b.sh materialize-runtime ")
%w[g6-rd-secret-scan g6-rd-verifier].each do |job_id|
  job = jobs.fetch(job_id)
  reject("#{job_id} must depend on both failure domains") unless job.fetch("needs").sort == %w[g6-rd-fd-a g6-rd-fd-b]
  condition = job.fetch("if")
  reject("#{job_id} must run after producer failure when both evidence artifacts exist") unless
    condition.include?("always()") &&
    condition.include?("needs.g6-rd-fd-a.outputs.evidence-artifact-id != ''") &&
    condition.include?("needs.g6-rd-fd-b.outputs.evidence-artifact-id != ''") &&
    !condition.include?(".result == 'success'")
end
{
  "g6-rd-fd-a" => "fd-a-evidence-upload",
  "g6-rd-fd-b" => "evidence-bundle-upload",
}.each do |job_id, upload_id|
  job = jobs.fetch(job_id)
  reject("#{job_id} must expose its evidence artifact ID") unless
    job.fetch("outputs").fetch("evidence-artifact-id").include?("steps.#{upload_id}.outputs.artifact-id")
  reject("#{job_id} must expose its evidence artifact digest") unless
    job.fetch("outputs").fetch("evidence-artifact-digest").include?("steps.#{upload_id}.outputs.artifact-digest")
  upload = Array(job.fetch("steps")).find { |step| step["id"] == upload_id }
  reject("#{job_id} evidence upload step is not bound to its outputs") unless upload
end
verifier_steps = Array(jobs.fetch("g6-rd-verifier").fetch("steps"))
verifier_bootstrap = verifier_steps.find { |step| step["run"]&.include?("scripts/bootstrap.sh") }
reject("the independent verifier must use only the pinned G6 Node runtime") unless
  verifier_bootstrap&.fetch("run") == "scripts/bootstrap.sh g6-runtime"
verifier_tooling = verifier_steps.find do |step|
  step.fetch("with", {}).fetch("key", "").start_with?("tooling-")
end
reject("the independent verifier tooling cache must bind the minimal G6 runtime lock") unless
  verifier_tooling&.fetch("with")&.fetch("key")&.include?("tooling-v4-g6-runtime-") &&
    verifier_tooling.fetch("with").fetch("key").include?("scripts/g6-runtime/package-lock.json")

services = compose.fetch("services")
required = %w[postgres migrate api worker scheduler transportd
              controller-key-init transport-runtime-init transport-endpoint-bootstrap
              relay g6-probe]
reject("G6 readiness compose service set is incomplete") unless (required - services.keys).empty?
runtime_init = services.fetch("transport-runtime-init")
runtime_init_command = Array(runtime_init.fetch("command")).join("\n")
reject("transport runtime init must assign the stats volume to transportd") unless runtime_init_command.include?("chown 65532:65532 /run/transport-stats")
runtime_init_volumes = Array(runtime_init.fetch("volumes"))
reject("transport runtime init must mount the transport stats volume") unless runtime_init_volumes.any? { |volume| volume.is_a?(Hash) && volume["source"] == "transport-stats" && volume["target"] == "/run/transport-stats" }
relay_mount_sources = {
  "relay" => "${G6_RELAY_DIR:?}/relay",
  "transportd" => "${G6_RELAY_DIR:?}/transportd",
  "g6-probe" => "${G6_RELAY_DIR:?}/probe"
}
relay_mount_sources.each do |service_name, expected_source|
  reject("#{service_name} must use its image's fixed runtime principal") if services.fetch(service_name).key?("user")
  mount = Array(services.fetch(service_name).fetch("volumes")).find do |volume|
    volume.is_a?(Hash) && volume["target"] == "/run/relay-secrets"
  end
  reject("#{service_name} must receive a scoped relay-material bind") unless mount
  reject("#{service_name} relay material must be mounted read-only") unless mount["read_only"] == true
  reject("#{service_name} must receive only its own relay material") unless mount["source"] == expected_source
end
reject("relay consumers must not share one bind source") unless relay_mount_sources.values.uniq.length == relay_mount_sources.length
reject("postgres must receive stop signals directly so fencing leaves a clean data directory") unless services.fetch("postgres").fetch("init") == false
reject("postgres must never pull after the support image preflight") unless services.fetch("postgres").fetch("pull_policy") == "never"
roles = %w[api worker scheduler].to_h { |role| [role, services.fetch(role).fetch("command").fetch(0)] }
reject("control-plane roles must be split") unless roles == {"api" => "--role=api", "worker" => "--role=worker", "scheduler" => "--role=scheduler"}
scheduler_environment = services.fetch("scheduler").fetch("environment")
reject("G6 must explicitly enable the non-production scheduler maintenance evidence hook") unless scheduler_environment.fetch("OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE") == "true"
worker_environment = services.fetch("worker").fetch("environment")
reject("worker must own the trust socket") unless worker_environment.key?("OCSERV_TRUST_SOCKET")
reject("the G6 worker transport timeout must be 15s for the deterministic crash barrier") unless worker_environment.fetch("OCSERV_TRANSPORT_TIMEOUT") == "15s"
reject("the G6 Worker pre-send barrier must use an absolute in-container path") unless worker_environment.fetch("OCSERV_TEST_PRE_SEND_BARRIER_DIR") == "/run/g6-result-barrier/pre-send"
reject("the exact armed G6 command lease must stay at its hard 60s ceiling") unless worker_environment.fetch("OCSERV_TEST_COMMAND_LEASE") == "60s"
worker_volumes = Array(services.fetch("worker").fetch("volumes"))
reject("the Worker pre-send barrier must share only the scoped result-barrier bind") unless worker_volumes.any? { |volume| volume.is_a?(Hash) && volume["target"] == "/run/g6-result-barrier" }
role_environment = services.fetch("api").fetch("environment")
reject("G6 API must enable session authentication") unless role_environment.fetch("OCSERV_SESSION_KEY_FILE") == "/run/ocservia-signing/session-key"
reject("G6 API must use the test OIDC fixture") unless role_environment.fetch("OCSERV_OIDC_ISSUER") == "https://oidc.g6.invalid"
reject("postgres must publish only loopback") unless services.fetch("postgres").fetch("ports") == ["127.0.0.1:5432:5432"]
reject("the API host port must stay on loopback for the tunnel to serve") unless services.fetch("api").fetch("ports").fetch(0).start_with?("127.0.0.1:")
reject("postgres must run data checksums") unless services.fetch("postgres").fetch("environment").fetch("POSTGRES_INITDB_ARGS").include?("data-checksums")
pg_caps = services.fetch("postgres").fetch("cap_add")
%w[CHOWN FOWNER DAC_OVERRIDE SETUID SETGID].each do |cap|
  reject("postgres must keep #{cap} for its root entrypoint phase under cap_drop ALL") unless pg_caps.include?(cap)
end
transportd_command = services.fetch("transportd").fetch("command")
reject("transportd must enforce owner fencing for the stale-rejection scenarios") unless transportd_command.include?("--require-fencing")
pgappname_count = compose.fetch("services").values.count { |service| service.dig("environment", "PGAPPNAME").to_s.start_with?("${G6_FD_ID:?}-") }
reject("each role service must set its PGAPPNAME from G6_FD_ID (found #{pgappname_count})") unless pgappname_count == 3
RUBY

# Debian already assigns UID 65534 to nobody. The probe must reuse that
# account rather than attempting to create a duplicate numeric identity.
if grep -Eq 'useradd.*--uid 65534' "${PROBE_DOCKERFILE}"; then
  echo "the G6 probe image must not create Debian's existing UID 65534" >&2
  exit 1
fi
grep -qF 'usermod --gid ocservia nobody' "${PROBE_DOCKERFILE}" || {
  echo "the G6 probe image must join nobody to the transport peer group" >&2
  exit 1
}
grep -q '^USER nobody:ocservia$' "${PROBE_DOCKERFILE}" || {
  echo "the G6 probe image must run as the transport-authorized nobody account" >&2
  exit 1
}
grep -q -- '--package ocservia-g6-tunnel' "${PROBE_DOCKERFILE}" || {
  echo "the release probe build stage must compile the host-side G6 tunnel once" >&2
  exit 1
}
grep -q '^FROM scratch AS g6-tunnel-artifact$' "${PROBE_DOCKERFILE}" || {
  echo "the release probe Dockerfile must expose the frozen G6 tunnel target" >&2
  exit 1
}
grep -qF 'useradd --system --uid 65532 --gid ocservia transportd' \
  "${TRANSPORT_DOCKERFILE}" || {
  echo "the transportd image principal must stay bound to uid:gid 65532:65532" >&2
  exit 1
}
grep -q '^USER transportd:ocservia$' "${TRANSPORT_DOCKERFILE}" || {
  echo "the transportd image must run as its fixed unprivileged principal" >&2
  exit 1
}
grep -qF 'useradd --system --uid 65532 --gid relay' "${RELAY_DOCKERFILE}" || {
  echo "the relay image principal must stay bound to uid:gid 65532:65532" >&2
  exit 1
}
grep -q '^USER relay:relay$' "${RELAY_DOCKERFILE}" || {
  echo "the relay image must run as its fixed unprivileged principal" >&2
  exit 1
}

# The FD runners must keep using the exact candidate-bound tunnel bytes after
# bootstrap and across the phase boundaries, not merely trust the first
# artifact verification step.
frozen_tunnel_fixture="$(mktemp -d)"
frozen_tunnel_status=0
(
  # shellcheck source=scripts/g6-readiness-lib.sh
  source "${LIB}"
  G6RD_TUNNEL_BIN="${frozen_tunnel_fixture}/ocservia-g6-tunnel"
  G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})"
  printf '#!/usr/bin/env bash\nexit 0\n' >"${G6RD_TUNNEL_BIN}"
  chmod 0755 "${G6RD_TUNNEL_BIN}"
  tunnel_sha="$(sha256sum "${G6RD_TUNNEL_BIN}" | awk '{print $1}')"
  printf 'candidate_sha\t%s\nocservia-g6-tunnel\t%s\n' \
    "${G6RD_CANDIDATE_SHA}" "${tunnel_sha}" \
    >"${frozen_tunnel_fixture}/tunnel-manifest.tsv"
  g6rd_verify_tunnel

  printf 'tampered\n' >>"${G6RD_TUNNEL_BIN}"
  if g6rd_verify_tunnel >"${frozen_tunnel_fixture}/tamper.out" 2>&1; then
    echo "frozen tunnel verification accepted mutated binary bytes" >&2
    exit 1
  fi
  grep -q 'checksum mismatch' "${frozen_tunnel_fixture}/tamper.out" || {
    echo "frozen tunnel mutation must fail with bounded checksum diagnostics" >&2
    exit 1
  }

  printf '#!/usr/bin/env bash\nexit 0\n' >"${G6RD_TUNNEL_BIN}"
  chmod 0755 "${G6RD_TUNNEL_BIN}"
  tunnel_sha="$(sha256sum "${G6RD_TUNNEL_BIN}" | awk '{print $1}')"
  printf 'candidate_sha\t%s\nocservia-g6-tunnel\t%s\n' \
    "$(printf 'b%.0s' {1..40})" "${tunnel_sha}" \
    >"${frozen_tunnel_fixture}/tunnel-manifest.tsv"
  if g6rd_verify_tunnel >"${frozen_tunnel_fixture}/candidate.out" 2>&1; then
    echo "frozen tunnel verification accepted a different candidate SHA" >&2
    exit 1
  fi
  grep -q 'does not match candidate' "${frozen_tunnel_fixture}/candidate.out" || {
    echo "frozen tunnel candidate mismatch must fail with bounded diagnostics" >&2
    exit 1
  }
) || frozen_tunnel_status=$?
rm -rf -- "${frozen_tunnel_fixture}"
if ((frozen_tunnel_status != 0)); then
  exit "${frozen_tunnel_status}"
fi

# The production adapter accepts only supported numeric Ocserv versions. Run
# the exact shim copied into every managed-node image so a decorative suffix
# cannot make the initial privd snapshot fail before Agent enrollment sync.
fixture_version="$("${OCSERV_FIXTURE}" --version)"
[[ "${fixture_version}" == "ocserv 1.2.3" ]] || {
  echo "the G6 ocserv fixture must emit a production-parseable version banner" >&2
  exit 1
}

# The supervisor helper must perform the process replacement itself. Shell
# functions cannot be passed as the command operand to exec.
grep -A2 '^as_agent()' "${SUPERVISOR}" | grep -q 'exec setpriv' || {
  echo "the agent identity helper must exec setpriv" >&2
  exit 1
}
if grep -q 'exec as_agent' "${SUPERVISOR}"; then
  echo "the agent supervisor must not ask exec to resolve a shell function" >&2
  exit 1
fi
controller_address_helper="$(sed -n '/^fn controller_address(/,/^}/p' "${AGENT_MAIN}")"
for token in \
  'EndpointAddr::new(controller)' \
  'if let RelayMode::Custom(relays) = relay_mode' \
  'address.with_relay_url(relay)'; do
  grep -qF "${token}" <<<"${controller_address_helper}" || {
    echo "dedicated relay addressing no longer supplies the controller hint: ${token}" >&2
    exit 1
  }
done
controller_dial_target_helper="$(sed -n '/^fn controller_dial_target(/,/^}/p' "${AGENT_MAIN}")"
for token in \
  'EndpointAddr::new(controller)' \
  'let RelayMode::Custom(relays) = relay_mode' \
  'address.with_relay_url(relay)'; do
  grep -qF "${token}" <<<"${controller_dial_target_helper}" || {
    echo "dedicated relay addressing no longer supplies the redial hint: ${token}" >&2
    exit 1
  }
done
grep -qF 'let target = controller_dial_target(controller, relay_mode, endpoint);' \
  "${AGENT_MAIN}" || {
  echo "the Agent runtime must bind controller redial to its dedicated relay hints" >&2
  exit 1
}
grep -A8 'let connection = endpoint' "${AGENT_MAIN}" \
  | grep -qF 'controller_address(controller, &config.relay_mode)' || {
  echo "Agent enrollment must bind controller dialing to its dedicated relay hints" >&2
  exit 1
}
prepare_mode="$(sed -n '/^prepare)/,/^    ;;/p' "${SUPERVISOR}")"
grep -q 'agent_identity_args.*--prepare-enrollment' <<<"${prepare_mode}" || {
  echo "enrollment preparation must use only identity and controller arguments" >&2
  exit 1
}
if grep -Eq 'agent_base_args|G6_RELAY|relay-(mode|url|token|ca)' <<<"${prepare_mode}"; then
  echo "enrollment preparation must not receive runtime relay configuration" >&2
  exit 1
fi
enroll_mode="$(sed -n '/^enroll)/,/^    ;;/p' "${SUPERVISOR}")"
for argument in user-password-seal-key-id user-password-seal-public-key-sha256 \
  p12-password-seal-key-id p12-password-seal-public-key-sha256; do
  grep -q -- "--${argument}" <<<"${enroll_mode}" || {
    echo "agent enrollment must advertise ${argument}" >&2
    exit 1
  }
done
mint_enrollment_token="$(sed -n '/^g6rd_mint_enrollment_token() {/,/^}/p' "${LIB}")"
grep -q 'g6rd_api_session_curl requester' <<<"${mint_enrollment_token}" || {
  echo "enrollment token minting must use the authenticated requester session" >&2
  exit 1
}
if grep -q 'ttl_seconds' <<<"${mint_enrollment_token}"; then
  echo "the harness must use the enrollment API default TTL instead of duplicating its limit" >&2
  exit 1
fi
grep -q '"${status}" != 201' <<<"${mint_enrollment_token}" || {
  echo "enrollment token minting must require the API 201 response contract" >&2
  exit 1
}
grep -q 'enrollment token API returned HTTP.*detail' <<<"${mint_enrollment_token}" || {
  echo "enrollment token failures must expose safe HTTP problem details" >&2
  exit 1
}
grep -q 'rm -f -- "${response}"' <<<"${mint_enrollment_token}" || {
  echo "the one-time enrollment response must be removed after parsing" >&2
  exit 1
}
for fd_script in "${FD_A}" "${FD_B}"; do
  enroll_phase="$(sed -n '/^phase_agents_enroll() {/,/^}/p' "${fd_script}")"
  grep -q 'g6rd_extract_enrollment_node_id' <<<"${enroll_phase}" || {
    echo "agent enrollment must parse the exact UUIDv7 protocol value" >&2
    exit 1
  }
  grep -q 'g6rd_wait_for_controller_relay' <<<"${enroll_phase}" || {
    echo "agent enrollment must verify the controller relay path first" >&2
    exit 1
  }
  if grep -q 'G6_MODE=enroll.*tail -1' <<<"${enroll_phase}"; then
    echo "agent enrollment must not assume the UUID is the final log line" >&2
    exit 1
  fi
done
for fd_script in "${FD_A}" "${FD_B}"; do
  build_phase="$(sed -n '/^phase_build_images() {/,/^}/p' "${fd_script}")"
  grep -q 'g6rd_prepare_build_environment' <<<"${build_phase}" || {
    echo "each failure domain must build with inert Compose substitutions" >&2
    exit 1
  }
  grep -q 'g6rd_prepare_release_images' <<<"${build_phase}" || {
    echo "each failure domain must consume the complete frozen release image set" >&2
    exit 1
  }
  if grep -q 'g6rd_compose build' <<<"${build_phase}"; then
    echo "failure domains must not rebuild release images outside the shared producer" >&2
    exit 1
  fi
  if grep -q 'g6rd_export_common_env' <<<"${build_phase}"; then
    echo "image construction must not consume live runtime trust" >&2
    exit 1
  fi
done
materialize_phase="$(sed -n '/^phase_materialize_runtime() {/,/^}/p' "${FD_B}")"
grep -q 'phase_import_peer_secrets' <<<"${materialize_phase}" || {
  echo "fd-b runtime materialization must import the peer trust bundle" >&2
  exit 1
}
grep -q 'g6rd_export_common_env' <<<"${materialize_phase}" || {
  echo "fd-b runtime materialization must validate the complete live environment" >&2
  exit 1
}
promote_phase="$(sed -n '/^phase_promote() {/,/^}/p' "${FD_B}")"
transport_init_line="$(grep -n 'g6rd_compose run --rm --no-deps transport-runtime-init' \
  <<<"${promote_phase}" | cut -d: -f1)"
transport_socket_line="$(grep -n 'era-2 transportd socket' \
  <<<"${promote_phase}" | cut -d: -f1)"
endpoint_barrier_line="$(grep -n 'g6rd_wait_until 15 1 "era-2 transportd controller endpoint"' \
  <<<"${promote_phase}" | cut -d: -f1)"
reconnect_line="$(grep -n 'agents reconnected to era-2 transportd' \
  <<<"${promote_phase}" | cut -d: -f1)"
release_barrier_line="$(grep -n 'g6rd_release_synthetic_barriers' \
  <<<"${promote_phase}" | cut -d: -f1)"
worker_start_line="$(grep -n 'g6rd_compose up --detach worker' \
  <<<"${promote_phase}" | cut -d: -f1)"
[[ -n "${transport_init_line}" && -n "${worker_start_line}" \
  && "${transport_init_line}" -lt "${worker_start_line}" ]] || {
  echo "fd-b must initialize transport volumes synchronously before era-2 roles" >&2
  exit 1
}
[[ -n "${transport_socket_line}" && -n "${endpoint_barrier_line}" \
  && -n "${release_barrier_line}" && -n "${reconnect_line}" \
  && "${transport_socket_line}" -lt "${endpoint_barrier_line}" \
  && "${endpoint_barrier_line}" -lt "${release_barrier_line}" \
  && "${release_barrier_line}" -lt "${reconnect_line}" ]] || {
  echo "fd-b must verify the era-2 controller and unblock active command streams before reconnecting" >&2
  exit 1
}
grep -q 'g6rd_wait_until_deadline 180 5' <<<"${promote_phase}" || {
  echo "fd-b promotion recovery waits must use a wall-clock deadline" >&2
  exit 1
}
grep -q 'report_node_connection_timeout' <<<"${promote_phase}" || {
  echo "fd-b reconnect timeout must report the final transport probe and API inventory" >&2
  exit 1
}
grep -q 'report_load_command_timeout' <<<"${promote_phase}" || {
  echo "fd-b reconciliation timeout must report the final command state" >&2
  exit 1
}
load_timeout_report="$(sed -n '/^report_load_command_timeout() {/,/^}/p' "${FD_B}")"
if ! grep -q 'node_command_leases' <<<"${load_timeout_report}" \
  || ! grep -q 'agent_command_results' <<<"${load_timeout_report}" \
  || ! grep -q 'outbox.attempts' <<<"${load_timeout_report}"; then
  echo "fd-b reconciliation timeout report must expose bounded state, lease, outbox, and result evidence" >&2
  exit 1
fi
for settled_function in load_commands_settled wait_commands_settled; do
  settled_body="$(sed -n "/^${settled_function}() {/,/^}/p" "${FD_B}")"
  if ! grep -q "'rejected'" <<<"${settled_body}" \
    || ! grep -q "'rolled_back'" <<<"${settled_body}"; then
    echo "${settled_function} must recognize every terminal command result" >&2
    exit 1
  fi
done
connected_probe="$(sed -n '/^all_nodes_connected() {/,/^}/p' "${FD_B}")"
if ! grep -q '\.owner_epoch > 0' <<<"${connected_probe}" \
  || ! grep -q 'index("ocserv.fencing.v2")' <<<"${connected_probe}" \
  || ! grep -q '\.session_expires_at' <<<"${connected_probe}"; then
  echo "fd-b reconnect barrier must require a live fenced mutation session" >&2
  exit 1
fi
controller_key_phase="$(sed -n '/^g6rd_install_controller_key() {/,/^}/p' "${LIB}")"
grep -q 'g6rd_compose run --rm --no-deps controller-key-init' \
  <<<"${controller_key_phase}" || {
  echo "both failure domains must initialize their controller key volume synchronously" >&2
  exit 1
}
if grep -qE 'up --detach controller-key-init|\|\| true' <<<"${controller_key_phase}"; then
  echo "controller key installation must not race or mask its initializer" >&2
  exit 1
fi
fd_a_bootstrap="$(sed -n '/^bootstrap_controller_endpoint() {/,/^}/p' "${FD_A}")"
fd_a_install_line="$(grep -n 'g6rd_install_controller_key' <<<"${fd_a_bootstrap}" | cut -d: -f1)"
fd_a_endpoint_line="$(grep -n 'transport-endpoint-bootstrap' <<<"${fd_a_bootstrap}" | head -1 | cut -d: -f1)"
[[ -n "${fd_a_install_line}" && -n "${fd_a_endpoint_line}" \
  && "${fd_a_install_line}" -lt "${fd_a_endpoint_line}" ]] || {
  echo "fd-a must install the shared controller key before endpoint bootstrap" >&2
  exit 1
}
primary_up_phase="$(sed -n '/^phase_primary_up() {/,/^}/p' "${FD_A}")"
primary_migrate_line="$(grep -nF 'g6rd_compose run --rm migrate' <<<"${primary_up_phase}" | cut -d: -f1)"
primary_journal_line="$(grep -nF 'g6rd_psql -c "$(<"${ROOT}/scripts/g6-authority-history.sql")"' <<<"${primary_up_phase}" | cut -d: -f1)"
primary_service_line="$(grep -nF 'bootstrap_controller_endpoint' <<<"${primary_up_phase}" | cut -d: -f1)"
[[ -n "${primary_migrate_line}" && -n "${primary_journal_line}" \
  && -n "${primary_service_line}" \
  && "${primary_migrate_line}" -lt "${primary_journal_line}" \
  && "${primary_journal_line}" -lt "${primary_service_line}" ]] || {
  echo "fd-a must install durable authority history after migrations and before services" >&2
  exit 1
}
for token in \
  "relpersistence='p'" \
  "tgname IN ('g6_journal_connection_owner'," \
  'AND prosecdef' \
  'count(*)=0 FROM g6_connection_owner_history' \
  'count(*)=0 FROM g6_scheduler_leadership_history' \
  'count(*)=0 FROM g6_scheduler_maintenance_history' \
  'count(*)=0 FROM connection_owner_fencing' \
  'epoch=0 FROM scheduler_leadership'; do
  grep -qF "${token}" <<<"${primary_up_phase}" || {
    echo "fd-a authority-journal installation is not verified: ${token}" >&2
    exit 1
  }
done
if grep -qE '\|\| true|continue-on-error' <<<"${primary_up_phase}"; then
  echo "fd-a authority-journal installation must fail closed" >&2
  exit 1
fi
psql_helper="$(sed -n '/^g6rd_psql() {/,/^}/p' "${LIB}")"
if grep -qE -- '--interactive|(^|[[:space:]])-i([[:space:]]|$)' <<<"${psql_helper}"; then
  echo "the shared psql helper must not consume caller stdin" >&2
  exit 1
fi

for token in \
  'BEGIN;' \
  'LOCK TABLE public.connection_owner_fencing, public.scheduler_leadership' \
  'IN SHARE ROW EXCLUSIVE MODE;' \
  'CREATE TABLE IF NOT EXISTS public.g6_connection_owner_history' \
  'CREATE TABLE IF NOT EXISTS public.g6_scheduler_leadership_history' \
  'CREATE TABLE IF NOT EXISTS public.g6_scheduler_maintenance_history' \
  'history_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY' \
  'ALTER TABLE public.g6_connection_owner_history SET LOGGED' \
  'ALTER TABLE public.g6_scheduler_leadership_history SET LOGGED' \
  'ALTER TABLE public.g6_scheduler_maintenance_history SET LOGGED' \
  'SECURITY DEFINER' \
  'SET search_path = pg_catalog' \
  'CREATE OR REPLACE FUNCTION public.g6_record_scheduler_maintenance' \
  'leadership.lease_until > pg_catalog.clock_timestamp()' \
  'FOR SHARE OF leadership' \
  'GRANT EXECUTE ON FUNCTION public.g6_record_scheduler_maintenance(uuid, bigint, bigint) TO ocservia_app' \
  'AFTER INSERT OR UPDATE ON public.connection_owner_fencing' \
  'AFTER INSERT OR UPDATE ON public.scheduler_leadership' \
  'BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_connection_owner_history' \
  'BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_scheduler_leadership_history' \
  'BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_scheduler_maintenance_history' \
  'FROM public.connection_owner_fencing AS current' \
  'FROM public.scheduler_leadership AS current' \
  'COMMIT;'; do
  grep -qF "${token}" "${AUTHORITY_HISTORY_SQL}" || {
    echo "the durable authority journal is missing: ${token}" >&2
    exit 1
  }
done
if grep -qE 'UNLOGGED|SET search_path = .*public' "${AUTHORITY_HISTORY_SQL}"; then
  echo "the durable authority journal must be WAL-logged with a hardened function search path" >&2
  exit 1
fi
scheduler_scenario_body="$(sed -n '/^phase_scenario_scheduler() {/,/^}/p' "${FD_B}")"
scheduler_completion_probe="$(sed -n '/^scheduler_maintenance_completed() {/,/^}/p' "${FD_B}")"
for token in \
  'scheduler-replacement-term' \
  'replacement scheduler completed exact-term fenced maintenance' \
  'scheduler_maintenance_completed' \
  'SET ROLE ocservia_app' \
  'g6_record_scheduler_maintenance' \
  'scheduler maintenance term is not the exact live leader'; do
  grep -qF "${token}" <<<"${scheduler_scenario_body}" || {
    echo "the scheduler failover scenario lacks exact-term maintenance proof: ${token}" >&2
    exit 1
  }
done
scheduler_lease_probe="$(sed -n '/^scheduler_lease_lapsed() {/,/^}/p' "${FD_B}")"
for token in \
  'g6rd_wait_until_deadline 120 2 "old scheduler lease lapsed"' \
  'G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary' \
  'lease_until <= clock_timestamp()'; do
  grep -qF "${token}" <<<"${scheduler_scenario_body}${scheduler_lease_probe}" || {
    echo "scheduler lease expiry must use one bounded database-clock predicate: ${token}" >&2
    exit 1
  }
done
if grep -qE 'to_char\(lease_until|date -u' <<<"${scheduler_lease_probe}"; then
  echo "scheduler lease expiry must not compare a truncated database lease to the runner clock" >&2
  exit 1
fi
for token in \
  'FROM g6_scheduler_maintenance_history' \
  'instance_id=' \
  'incarnation=' \
  'epoch=' \
  'WITH marker AS MATERIALIZED' \
  'ORDER BY maintenance_id' \
  'LIMIT 1' \
  'observed AS MATERIALIZED' \
  'SELECT clock_timestamp() AS at' \
  "'marker_completed_at'" \
  "'committed_observed_at'" \
  'FROM marker CROSS JOIN observed' \
  'WHERE marker.completed_at<=observed.at' \
  'mv -f -- "${temporary}" "${output}"'; do
  grep -qF "${token}" <<<"${scheduler_completion_probe}" || {
    echo "the scheduler completion probe is not exact-term bound: ${token}" >&2
    exit 1
  }
done
if grep -qF 'SELECT count(*)' <<<"${scheduler_completion_probe}" \
  || [[ "$(grep -cF 'psql_primary_probe -qAtc' <<<"${scheduler_completion_probe}")" -ne 1 ]]; then
  echo "the scheduler completion boundary must come from one independent marker-observation snapshot" >&2
  exit 1
fi
if grep -qE '\|\| true|continue-on-error' <<<"${scheduler_scenario_body}${scheduler_completion_probe}"; then
  echo "the scheduler maintenance proof must fail closed" >&2
  exit 1
fi
scheduler_observation_fixture="$(mktemp -d)"
(
  export G6RD_STATE="${scheduler_observation_fixture}"
  export SCHEDULER_REPLACEMENT_INSTANCE="sched-b"
  export SCHEDULER_REPLACEMENT_INCARNATION="1800000000000000002"
  export SCHEDULER_REPLACEMENT_EPOCH="2"
  scheduler_probe_calls=0
  psql_primary_probe() {
    scheduler_probe_calls=$((scheduler_probe_calls + 1))
    printf '%s\n' "${scheduler_fixture_json}"
  }
  eval "${scheduler_completion_probe}"
  scheduler_fixture_json='{"maintenance_id":"2002","instance_id":"sched-b","incarnation":"1800000000000000002","epoch":2,"marker_completed_at":"2026-08-19T00:00:30.000000Z","committed_observed_at":"2026-08-19T00:00:30.000001Z"}'
  scheduler_maintenance_completed
  scheduler_maintenance_completed
  [[ "${scheduler_probe_calls}" -eq 1 ]] || {
    echo "the scheduler observation was not frozen at its first successful snapshot" >&2
    exit 1
  }
  jq -e '.maintenance_id == "2002"
    and .marker_completed_at == "2026-08-19T00:00:30.000000Z"
    and .committed_observed_at == "2026-08-19T00:00:30.000001Z"' \
    "${G6RD_STATE}/scheduler-maintenance-observation.json" >/dev/null || {
    echo "the scheduler observation fixture did not atomically persist the snapshot" >&2
    exit 1
  }
  rm -f -- "${G6RD_STATE}/scheduler-maintenance-observation.json"
  scheduler_fixture_json='{"maintenance_id":"2002","instance_id":"sched-b","incarnation":"1800000000000000002","epoch":2,"marker_completed_at":"2026-08-19T00:00:30.000002Z","committed_observed_at":"2026-08-19T00:00:30.000001Z"}'
  if scheduler_maintenance_completed; then
    echo "the scheduler observation accepted a marker after its observation boundary" >&2
    exit 1
  fi
  [[ ! -e "${G6RD_STATE}/scheduler-maintenance-observation.json" ]] || {
    echo "a rejected scheduler observation polluted the frozen state" >&2
    exit 1
  }
)
rm -rf -- "${scheduler_observation_fixture}"
checkpoint_line="$(grep -nF 'auditManager.CheckpointAll(sessionCtx)' "${CONTROL_APP}" | cut -d: -f1)"
maintenance_record_line="$(grep -nF 'coordination.RecordMaintenanceCompletion(sessionCtx, pool, session)' "${CONTROL_APP}" | cut -d: -f1)"
[[ -n "${checkpoint_line}" && -n "${maintenance_record_line}" \
  && "${checkpoint_line}" -lt "${maintenance_record_line}" ]] || {
  echo "the scheduler must record maintenance only after the real maintenance body completes" >&2
  exit 1
}
for token in \
  'OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE' \
  'scheduler maintenance evidence is test-only' \
  'c.TestSchedulerEvidence && c.Environment == "production"'; do
  grep -qF "${token}" "${CONTROL_CONFIG}" || {
    echo "the scheduler maintenance hook is not explicitly test-only: ${token}" >&2
    exit 1
  }
done
for token in \
  'SELECT public.g6_record_scheduler_maintenance($1,$2,$3)' \
  'identity.InstanceID, identity.Incarnation, session.Epoch()' \
  'CommitFenced(ctx, tx, session)'; do
  grep -qF "${token}" "${COORDINATION_MAINTENANCE}" || {
    echo "the scheduler completion recorder is not exact-term fenced: ${token}" >&2
    exit 1
  }
done
grep -q 'g6rd_install_controller_key' <<<"${promote_phase}" || {
  echo "fd-b must install the handed-over controller key before promotion" >&2
  exit 1
}
node_connection_probe="$(sed -n '/^g6rd_probe_node_connection() {/,/^}/p' "${LIB}")"
if ! grep -q 'G6RD_COMPOSE_TIMEOUT_SECONDS="${timeout_seconds}"' \
  <<<"${node_connection_probe}" \
  || ! grep -q 'g6rd_compose --profile probe run' \
    <<<"${node_connection_probe}"; then
  echo "node connection probes must use the bounded release-image-aware Compose path" >&2
  exit 1
fi
if grep -q 'docker compose.*COMPOSE_FILE' <<<"${node_connection_probe}"; then
  echo "node connection probes must not bypass the frozen release-image overlay" >&2
  exit 1
fi
relay_connection_probe="$(sed -n '/^relay_probe_named() {/,/^}/p' "${FD_B}")"
for token in \
  '.expected_path == "relay"' \
  '.node_id == $node' \
  '.endpoint_id == $endpoint' \
  '.found == true and .matched == true' \
  '.path == "relay"' \
  'contains($relay)' \
  'index("ocserv.fencing.v2") != null' \
  'mv -f -- "${temporary}" "${output}"'; do
  grep -qF "${token}" <<<"${relay_connection_probe}" || {
    echo "relay failover must persist a bound authenticated raw probe: ${token}" >&2
    exit 1
  }
done
relay_phase="$(sed -n '/^phase_scenario_relay() {/,/^}/p' "${FD_B}")"
for token in \
  'relay-b-node-id' \
  'relay-b-before-command.json' \
  'relay-b-observation.json' \
  'g6rd_enqueue_command "${cross_vm_node}" "${key}"' \
  'capture_relay_dispatch_proof' \
  'capture_relay_command_proof' \
  'relay_observations_same_session' \
  'capture_database_clock >"${active_at_file}"' \
  'relay_probe_relay_b "${cross_vm_node}" "${observation_file}"' \
  'require_file "${observation_file}"'; do
  grep -qF "${token}" <<<"${relay_phase}" || {
    echo "relay scenario does not preserve its chosen-node raw probe: ${token}" >&2
    exit 1
  }
done
relay_pre_fault_phase="$(sed -n '/^phase_relay_pre_fault() {/,/^}/p' "${FD_B}")"
for token in \
  'validate_relay_a_only_readiness "${readiness}"' \
  'G6RD_COMPOSE_TIMEOUT_SECONDS=30 g6rd_compose stop relay' \
  'relay_b_stopped' \
  'relay-b-disabled.json' \
  'relay-a-only-readiness.json' \
  'relay_probe_named relay-a "${cross_vm_node}" "${before}"' \
  'g6rd_enqueue_command "${cross_vm_node}" "${key}"' \
  '"${out}/relay-a-dispatch-proof.json" relay-a' \
  '"${out}/relay-a-command-proof.json"' \
  'relay_observations_same_session "${before}" "${observation}"' \
  'capture_database_clock >"${out}/observed-at"' \
  'printf '\''%s\n'\'' "${cross_vm_node}" >"${out}/node-id"'; do
  grep -qF "${token}" <<<"${relay_pre_fault_phase}" || {
    echo "relay failover lacks a frozen pre-fault relay-a session: ${token}" >&2
    exit 1
  }
done
relay_b_stop_line="$(grep -nF 'G6RD_COMPOSE_TIMEOUT_SECONDS=30 g6rd_compose stop relay' \
  <<<"${relay_pre_fault_phase}" | cut -d: -f1)"
relay_a_preproof_line="$(grep -nF 'relay_probe_named relay-a "${cross_vm_node}" "${before}"' \
  <<<"${relay_pre_fault_phase}" | cut -d: -f1)"
[[ -n "${relay_b_stop_line}" && -n "${relay_a_preproof_line}" \
  && "${relay_b_stop_line}" -lt "${relay_a_preproof_line}" ]] || {
  echo "relay-b must be stopped before the controlled relay-a command proof" >&2
  exit 1
}
if grep -Eq 'relay_probe_named|g6rd_wait_until.*relay-a' \
  <<<"$(sed -n "1,$((relay_b_stop_line - 1))p" <<<"${relay_pre_fault_phase}")"; then
  echo "fd-b must not add a relay-a behavioral dependency before stopping relay-b" >&2
  exit 1
fi
if grep -qF 'restart transportd' <<<"${relay_pre_fault_phase}"; then
  echo "the relay-a proof must exercise live transportd failover without a cold restart" >&2
  exit 1
fi
for token in \
  'phase_relay_up' \
  'relay-b-started.json' \
  'relay_b_stopped' \
  'relay-b did not start strictly after' \
  'relay_probe_relay_b "${cross_vm_node}" "${before_file}"'; do
  grep -qF "${token}" <<<"${relay_phase}" || {
    echo "relay-b cut-after startup proof is incomplete: ${token}" >&2
    exit 1
  }
done
relay_b_start_line="$(grep -nF 'phase_relay_up' <<<"${relay_phase}" | cut -d: -f1)"
relay_b_postproof_line="$(grep -nF 'relay_probe_relay_b "${cross_vm_node}" "${before_file}"' \
  <<<"${relay_phase}" | cut -d: -f1)"
[[ -n "${relay_b_start_line}" && -n "${relay_b_postproof_line}" \
  && "${relay_b_start_line}" -lt "${relay_b_postproof_line}" ]] || {
  echo "relay-b must start after the cut and before its authenticated proof" >&2
  exit 1
}
relay_ready_phase="$(sed -n '/^phase_relay_rejoin_ready() {/,/^}/p' "${FD_A}")"
relay_retirement_probe="$(sed -n '/^relay_prior_connection_retired() {/,/^}/p' "${FD_A}")"
relay_topology_match="$(sed -n '/^relay_a_only_topology_matches() {/,/^}/p' "${FD_A}")"
relay_topology_restore="$(sed -n '/^relay_a_only_topology_restore() {/,/^}/p' "${FD_A}")"
for token in \
  'network create --internal' \
  'ocservia.g6.run-id=${RUN_ID}' \
  'network connect --alias relay-a' \
  'network disconnect "${default_network}" "${agent}"' \
  'relay_a_only_topology_matches' \
  'prior-connection-id' \
  'prior-owner-epoch' \
  'FROM connection_owner_fencing' \
  'G6RD_COMPOSE_TIMEOUT_SECONDS=30' \
  'g6rd_agent_compose stop "$(relay_a_only_agent_service)"' \
  'g6rd_wait_until_deadline 45 1 "selected relay Agent prior connection retired"' \
  'g6rd_agent_compose start "$(relay_a_only_agent_service)"' \
  'relay-a-only-readiness.json'; do
  grep -qF "${token}" <<<"${relay_ready_phase}" || {
    echo "relay-a controlled topology setup is incomplete: ${token}" >&2
    exit 1
  }
done
[[ "$(grep -Fc 'G6RD_COMPOSE_TIMEOUT_SECONDS=30' <<<"${relay_ready_phase}")" -eq 2 ]] || {
  echo "relay-a readiness must bound both selected-Agent stop and start" >&2
  exit 1
}
if grep -Eq 'g6rd_agent_compose (restart|up)' <<<"${relay_ready_phase}"; then
  echo "relay-a readiness must preserve the external topology with exact stop/start" >&2
  exit 1
fi
for token in \
  'G6_DB_PORT=15432 G6RD_PSQL_TIMEOUT_SECONDS=5 g6rd_psql' \
  'SELECT CASE' \
  "connection_id=decode('\${prior_connection_id}','hex')" \
  'owner_epoch=${prior_owner_epoch}' \
  "connection_id<>decode('\${prior_connection_id}','hex')" \
  'owner_epoch>${prior_owner_epoch}' \
  'AND lease_until<=clock_timestamp()' \
  "THEN 'expired'" \
  "THEN 'changed-expired'" \
  "ELSE 'live-or-invalid'" \
  '[[ "${state}" == expired || "${state}" == changed-expired ]]'; do
  grep -qF "${token}" <<<"${relay_retirement_probe}" || {
    echo "the selected relay Agent retirement gate is incomplete: ${token}" >&2
    exit 1
  }
done
relay_ready_topology_positions="$(grep -nF 'relay_a_only_topology_matches' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_topology_count="$(printf '%s\n' "${relay_ready_topology_positions}" \
  | awk 'NF { count++ } END { print count + 0 }')"
relay_ready_topology_first="$(printf '%s\n' "${relay_ready_topology_positions}" | sed -n '1p')"
relay_ready_topology_second="$(printf '%s\n' "${relay_ready_topology_positions}" | sed -n '2p')"
relay_ready_restore_line="$(grep -nF 'relay_a_only_topology_restore || return 1' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_clock_line="$(grep -nF 'ready_at="$(G6_DB_PORT=15432' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_prior_line="$(grep -nF 'baseline="$(G6_DB_PORT=15432' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_rewire_line="$(grep -nF 'relay_topology_docker network create --internal' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_stop_line="$(grep -nF 'g6rd_agent_compose stop' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_retirement_line="$(grep -nF 'g6rd_wait_until_deadline 45 1 "selected relay Agent prior connection retired"' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_start_line="$(grep -nF 'g6rd_agent_compose start' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
relay_ready_publish_line="$(grep -nF 'mv -f -- "${temporary}" "${out}/relay-a-only-readiness.json"' \
  <<<"${relay_ready_phase}" | cut -d: -f1)"
[[ "${relay_ready_topology_count}" -eq 2 \
  && -n "${relay_ready_restore_line}" && -n "${relay_ready_clock_line}" \
  && -n "${relay_ready_prior_line}" && -n "${relay_ready_rewire_line}" \
  && -n "${relay_ready_stop_line}" && -n "${relay_ready_retirement_line}" \
  && -n "${relay_ready_start_line}" \
  && -n "${relay_ready_publish_line}" \
  && "${relay_ready_restore_line}" -lt "${relay_ready_prior_line}" \
  && "${relay_ready_prior_line}" -lt "${relay_ready_rewire_line}" \
  && "${relay_ready_rewire_line}" -lt "${relay_ready_topology_first}" \
  && "${relay_ready_topology_first}" -lt "${relay_ready_stop_line}" \
  && "${relay_ready_stop_line}" -lt "${relay_ready_retirement_line}" \
  && "${relay_ready_retirement_line}" -lt "${relay_ready_start_line}" \
  && "${relay_ready_start_line}" -lt "${relay_ready_topology_second}" \
  && "${relay_ready_topology_second}" -lt "${relay_ready_clock_line}" \
  && "${relay_ready_clock_line}" -lt "${relay_ready_publish_line}" ]] || {
  echo "relay-a readiness must freeze the live term before rewire, then stop/retire/start before publishing" >&2
  exit 1
}
for token in \
  '.[0].Internal == true' \
  '([.[0].Containers[]?.Name] | sort) == ([$agent,$relay] | sort)' \
  '(.[0].NetworkSettings.Networks | keys) == [$network]' \
  'com.docker.compose.project' \
  'com.docker.compose.service' \
  'index("relay-a")'; do
  grep -qF "${token}" <<<"${relay_topology_match}" || {
    echo "relay-a controlled topology is not exact: ${token}" >&2
    exit 1
  }
done
[[ "$(grep -Fc '.[0].State.Running == true' <<<"${relay_topology_match}")" -eq 2 ]] || {
  echo "relay-a exact topology must require both the selected Agent and relay to be running" >&2
  exit 1
}
for token in \
  'network connect "${default_network}" "${agent}"' \
  'network disconnect "${network}" "${agent}"' \
  'network disconnect "${network}" "${relay}"' \
  'network rm "${network}"'; do
  grep -qF "${token}" <<<"${relay_topology_restore}" || {
    echo "relay-a controlled topology restore is incomplete: ${token}" >&2
    exit 1
  }
done
relay_dispatch_capture="$(sed -n '/^capture_relay_dispatch_proof() {/,/^}/p' "${FD_B}")"
for token in \
  'g6rd_compose logs --no-color' \
  'event_type == "command_frame_written"' \
  '($matches | length) == 1' \
  '.path == "relay"' \
  'contains($relay)' \
  '.owner_fence_id == ($observation' \
  '.connection_id == ($observation' \
  '.owner_epoch == $observation'; do
  grep -qF "${token}" <<<"${relay_dispatch_capture}" || {
    echo "relay dispatch proof is not exact-command/session fail-closed: ${token}" >&2
    exit 1
  }
done
transport_send_command="$(sed -n '/async fn send_command(/,/async fn fetch_artifact(/p' "${TRANSPORT_LIB}")"
relay_frame_finish_line="$(grep -nF 'send.finish()' <<<"${transport_send_command}" | cut -d: -f1)"
relay_frame_log_line="$(grep -nF 'event_type = "command_frame_written"' <<<"${transport_send_command}" | cut -d: -f1)"
[[ -n "${relay_frame_finish_line}" && -n "${relay_frame_log_line}" \
  && "${relay_frame_finish_line}" -lt "${relay_frame_log_line}" ]] || {
  echo "transportd must log the relay dispatch only after the command frame is written" >&2
  exit 1
}
for token in \
  'metadata_path(&response_connection)' \
  'command_id = %hex::encode(&command.command_id)' \
  'node_id = %hex::encode(&node_id)' \
  'owner_fence_id =' \
  'connection_id =' \
  'owner_epoch =' \
  'path = dispatch_path' \
  'path_detail = %dispatch_path_detail'; do
  grep -qF "${token}" <<<"${transport_send_command}" || {
    echo "transportd relay dispatch log lacks an exact authenticated tuple: ${token}" >&2
    exit 1
  }
done

relay_dispatch_fixture="$(mktemp -d)"
(
  eval "${relay_dispatch_capture}"
  command_id=00000000-0000-7000-8000-000000000123
  node_id=00000000-0000-7000-8000-000000000456
  observation="${relay_dispatch_fixture}/observation.json"
  output="${relay_dispatch_fixture}/dispatch.json"
  printf '%s\n' '{"observations":[{"owner_fence_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","connection_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","owner_epoch":2}]}' >"${observation}"
  good_log='{"fields":{"event_type":"command_frame_written","command_id":"00000000000070008000000000000123","node_id":"00000000000070008000000000000456","owner_fence_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","connection_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","owner_epoch":2,"path":"relay","path_detail":"iroh/relay-b"}}'
  relay_log="${good_log}"
  g6rd_compose() { printf '%s\n' "${relay_log}"; }
  capture_relay_dispatch_proof \
    "${command_id}" "${node_id}" "${observation}" "${output}" relay-b
  jq -e '.event_type == "command_frame_written" and .path_detail == "iroh/relay-b"' \
    "${output}" >/dev/null
  relay_log="${good_log}"$'\n'"${good_log}"
  if capture_relay_dispatch_proof \
    "${command_id}" "${node_id}" "${observation}" "${output}" relay-b \
    >/dev/null 2>&1; then
    echo "duplicate relay dispatch log events must fail closed" >&2
    exit 1
  fi
  relay_log="${good_log/iroh\/relay-b/iroh\/direct}"
  relay_log="${relay_log/\"path\":\"relay\"/\"path\":\"direct\"}"
  if capture_relay_dispatch_proof \
    "${command_id}" "${node_id}" "${observation}" "${output}" relay-b \
    >/dev/null 2>&1; then
    echo "a direct-path command must not prove relay failover" >&2
    exit 1
  fi
  relay_log="${good_log/00000000000070008000000000000123/ffffffffffffffffffffffffffffffff}"
  if capture_relay_dispatch_proof \
    "${command_id}" "${node_id}" "${observation}" "${output}" relay-b \
    >/dev/null 2>&1; then
    echo "an unrelated command dispatch must not prove relay failover" >&2
    exit 1
  fi
  relay_log="${good_log}"
  if capture_relay_dispatch_proof \
    "${command_id}" "${node_id}" "${observation}" "${output}" relay-a \
    >/dev/null 2>&1; then
    echo "a relay-b frame must not prove the pre-fault relay-a command" >&2
    exit 1
  fi
)
rm -rf -- "${relay_dispatch_fixture}"

# Enrollment and privd must pin the identical SPKI DER fingerprint. Hashing
# the PEM envelope instead passes enrollment but makes every Agent fail closed
# as soon as privd derives the key's canonical DER representation.
seal_test="$(mktemp -d)"
(
  export G6RD_SECRETS="${seal_test}"
  source "${LIB}"
  g6rd_generate_seal_keys
  for pair in user-password p12; do
    expected="$(openssl rsa -in "${seal_test}/seal-${pair}.key" \
      -pubout -outform DER 2>/dev/null | openssl dgst -sha256 -r | cut -d ' ' -f1)"
    actual="$(<"${seal_test}/seal-${pair}-sha256")"
    [[ "${actual}" == "${expected}" && "${actual}" =~ ^[0-9a-f]{64}$ ]] || {
      echo "${pair} sealing-key fingerprint must use canonical SPKI DER" >&2
      exit 1
    }
  done
)
rm -rf "${seal_test}"

parsed_node_id="$(printf '%s\n' \
  'relay startup' '018f2f10-7abc-7def-8abc-0123456789ab' 'relay shutdown' \
  | (source "${LIB}"; g6rd_extract_enrollment_node_id))"
[[ "${parsed_node_id}" == 018f2f10-7abc-7def-8abc-0123456789ab ]] || {
  echo "the enrollment result parser must tolerate trailing runtime logs" >&2
  exit 1
}
for fd_script in "${FD_A}" "${FD_B}"; do
  start_phase="$(sed -n '/^phase_agents_start() {/,/^}/p' "${fd_script}")"
  grep -q 'g6rd_start_agent_fleet' <<<"${start_phase}" || {
    echo "each failure domain must cross the controller-observed Agent readiness barrier" >&2
    exit 1
  }
  require_line="$(grep -n 'g6rd_stage_agent_node_state' <<<"${start_phase}" | cut -d: -f1)"
  chown_line="$(grep -n 'g6rd_chown_agent_dirs' <<<"${start_phase}" | cut -d: -f1)"
  [[ -n "${require_line}" && -n "${chown_line}" && "${require_line}" -lt "${chown_line}" ]] || {
    echo "each Agent fleet must validate persisted node ids before uid handoff" >&2
    exit 1
  }
done
agent_chown="$(sed -n '/^g6rd_chown_agent_dirs() {/,/^}/p' "${LIB}")"
grep -q '/chown/identity /chown/journal' <<<"${agent_chown}" || {
  echo "the Agent uid handoff must retain identity and journal ownership" >&2
  exit 1
}
if grep -q '/chown/state' <<<"${agent_chown}"; then
  echo "the runner must retain state ownership so it can release synthetic barriers" >&2
  exit 1
fi
prepare_agent_material="$(sed -n '/^g6rd_prepare_agent_material() {/,/^}/p' "${LIB}")"
grep -q 'chmod 0755 "${dir}/state"' <<<"${prepare_agent_material}" || {
  echo "the harness-owned Agent state directory must remain traversable by the Agent" >&2
  exit 1
}
grep -q 'chmod 0644 "${dir}/state/synthetic-barrier"' <<<"${prepare_agent_material}" || {
  echo "the harness-owned synthetic barrier must remain readable by the Agent" >&2
  exit 1
}
grep -q 'chmod 0666 "${dir}/state/synthetic-barrier.received"' <<<"${prepare_agent_material}" || {
  echo "the exact Agent receipt inode must be writable by both runtime principals" >&2
  exit 1
}
start_fleet="$(sed -n '/^g6rd_start_agent_fleet() {/,/^}/p' "${LIB}")"
grep -q 'controller API endpoint before Agent startup' <<<"${start_fleet}" || {
  echo "Agent startup must verify the controller API before launching a canary" >&2
  exit 1
}
grep -q 'g6rd_agent_compose restart' <<<"${start_fleet}" || {
  echo "Agent startup must perform one bounded reconnect restart" >&2
  exit 1
}
grep -q 'g6rd_agent_services_needing_restart' <<<"${start_fleet}" || {
  echo "Agent startup recovery must target only observed-unready local services" >&2
  exit 1
}
journal_query_runtime="$(sed -n '/^g6rd_agent_journal_query() {/,/^}/p' "${LIB}")"
journal_principal_smoke="$(sed -n '/^g6rd_verify_agent_journal_observer_principals() {/,/^}/p' "${LIB}")"
grep -qF 'g6rd_agent_compose exec -T --user 65532:65532 "${service}"' \
  <<<"${journal_query_runtime}" || {
  echo "the shared Agent journal observer must read as the journal owner" >&2
  exit 1
}
grep -qF 'g6rd_agent_compose exec -T --user 0:0 "${service}"' \
  <<<"${journal_principal_smoke}" || {
  echo "the Agent journal smoke must exercise the capless supervisor principal" >&2
  exit 1
}
[[ "$(grep -cF 'g6rd_agent_journal_query "${service}"' \
  <<<"${journal_principal_smoke}")" -eq 2 ]] || {
  echo "the Agent journal smoke must prove owner access before and after its DAC probe" >&2
  exit 1
}
grep -qF '65532:65532:700' <<<"${journal_principal_smoke}" || {
  echo "the Agent journal smoke must assert the owner-only journal tree" >&2
  exit 1
}
grep -qF 'the capless uid 0 Agent journal DAC probe failed' \
  <<<"${journal_principal_smoke}" || {
  echo "the Agent journal smoke must fail closed on an invalid DAC result" >&2
  exit 1
}
for fd_script in "${FD_A}" "${FD_B}"; do
  start_phase="$(sed -n '/^phase_agents_start() {/,/^}/p' "${fd_script}")"
  grep -qF 'g6rd_verify_agent_journal_observer_principals "agent-${FD_ID}-01"' \
    <<<"${start_phase}" || {
    echo "each failure domain must run the live Agent journal principal smoke" >&2
    exit 1
  }
done
(
  eval "${journal_query_runtime}"
  eval "${journal_principal_smoke}"
  expect_status() {
    local expected="${1:?expected status}" status=0
    shift
    "$@" >/dev/null 2>&1 || status=$?
    [[ "${status}" == "${expected}" ]] || {
      echo "expected status ${expected}, got ${status}: $*" >&2
      exit 1
    }
  }
  g6rd_agent_compose() {
    local user=""
    while (($#)); do
      if [[ "${1}" == --user ]]; then
        user="${2:-}"
        break
      fi
      shift
    done
    case "${user}" in
      65532:65532)
        [[ "${SMOKE_OWNER:-ok}" == ok ]] || return 1
        printf '0\n'
        ;;
      0:0) [[ "${SMOKE_ROOT:-denied}" != allowed ]] ;;
      *) return 2 ;;
    esac
  }
  expect_status 0 g6rd_verify_agent_journal_observer_principals agent-fd-b-01
  SMOKE_OWNER=failed expect_status 1 \
    g6rd_verify_agent_journal_observer_principals agent-fd-b-01
  SMOKE_ROOT=allowed expect_status 1 \
    g6rd_verify_agent_journal_observer_principals agent-fd-b-01
)
stopped_restart_line="$(grep -n 'g6rd_agent_services_not_running' \
  <<<"${start_fleet}" | cut -d: -f1)"
unready_restart_line="$(grep -n 'g6rd_agent_services_needing_restart' \
  <<<"${start_fleet}" | cut -d: -f1)"
[[ -n "${stopped_restart_line}" && -n "${unready_restart_line}" \
  && "${stopped_restart_line}" -lt "${unready_restart_line}" ]] || {
  echo "Agent recovery must restart exited services before considering controller-unready services" >&2
  exit 1
}
if grep -q 'g6rd_agent_compose restart "${services\[@\]}"' <<<"${start_fleet}"; then
  echo "one unhealthy Agent must not restart the complete local fleet" >&2
  exit 1
fi
grep -q 'g6rd_report_agent_readiness' "${LIB}" || {
  echo "Agent readiness timeout must print the last controller response" >&2
  exit 1
}
wait_for_readiness="$(sed -n '/^g6rd_wait_for_agent_readiness() {/,/^}/p' "${LIB}")"
grep -q 'g6rd_agent_service_running' <<<"${wait_for_readiness}" || {
  echo "Agent canary readiness must fail immediately when the container exits" >&2
  exit 1
}
grep -q 'PermissionDenied.*ancestry.*metadata invalid.*attestation' "${LIB}" || {
  echo "Agent startup diagnostics must surface local key and attestation failures" >&2
  exit 1
}

# Exercise the observed-state predicate without starting containers. The
# controller response is authoritative only when every expected node is
# active, online, fresh, and carries a heartbeat.
source "${LIB}"
# Runtime workflow variables must not select a branch in this hermetic policy
# test before that branch is exercised explicitly below.
unset G6RD_AGENT_IMAGE G6RD_CONTROL_PLANE_IMAGE G6RD_TRANSPORTD_IMAGE \
  G6RD_RELAY_IMAGE G6RD_PROBE_IMAGE
(
  export G6_OWNER_PASSWORD=fixture-owner
  export G6_DEV_AUTH_TOKEN=fixture-token
  export G6_FD_ID=fd-b
  export G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=7
  g6rd_compose() {
    [[ "${G6RD_COMPOSE_TIMEOUT_SECONDS:-}" == 7 ]] || {
      echo "node connection probe did not preserve its per-attempt timeout" >&2
      return 1
    }
    printf '%s\n' "$@"
  }
  probe_invocation="$(g6rd_probe_node_connection relay node-a node-b)"
  expected_invocation="$(printf '%s\n' \
    --profile probe run --rm --no-deps g6-probe node-connection \
    --socket /run/ocserv-platform/transportd.sock \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
    --expect-path relay --node-id node-a --node-id node-b)"
  [[ "${probe_invocation}" == "${expected_invocation}" ]] || {
    echo "node connection probe did not delegate through the shared Compose wrapper" >&2
    exit 1
  }
)
sampler_trace="$(mktemp)"
(
  export G6RD_ENVIRONMENT_ID=fixture-environment
  export G6RD_CANDIDATE_SHA=0123456789abcdef0123456789abcdef01234567
  sampler_fixture_value() {
    case "$*" in
      *VmRSS*) printf '1\n' ;;
      */fd*) printf '2\n' ;;
      *) printf '3\n' ;;
    esac
  }
  g6rd_compose() {
    printf 'base\n' >>"${sampler_trace}"
    sampler_fixture_value "$@"
  }
  g6rd_agent_compose() {
    [[ "$#" == 8 && "$1" == exec && "$2" == -T \
      && "$3" == --user && "$4" == 65532:65532 \
      && "$5" == agent-fd-b-01 && "$6" == sh && "$7" == -c ]] || {
      echo "Agent resource sampling did not use its process owner" >&2
      return 1
    }
    printf 'agent\n' >>"${sampler_trace}"
    sampler_fixture_value "$@"
  }
  sample="$(g6rd_sampler_row agent agent-fd-b-01 agent-fd-b-01 \
    'cat /run/ocserv-platform/agent.pid' \
    'cat /run/ocservia-agent/journal/tasks.json' 0 '' \
    2026-08-19T00:00:00Z)"
  [[ "${sample}" == \
    '2026-08-19T00:00:00Z,agent,agent-fd-b-01,1024,2,3,0,,fixture-environment,0123456789abcdef0123456789abcdef01234567' ]] || {
    echo "the resource sampler did not emit the expected Agent sample" >&2
    exit 1
  }
  if [[ "$(grep -c '^agent$' "${sampler_trace}")" != 3 ]] \
    || grep -q '^base$' "${sampler_trace}"; then
    echo "Agent resource samples must use the Agent Compose overlay" >&2
    exit 1
  fi
  g6rd_agent_compose() { return 1; }
  if sampler_error="$(g6rd_sampler_row agent agent-fd-b-01 agent-fd-b-01 \
    'cat /run/ocserv-platform/agent.pid' \
    'cat /run/ocservia-agent/journal/tasks.json' 0 '' \
    2026-08-19T00:00:00Z 2>&1)"; then
    echo "a failed Agent sample was accepted" >&2
    exit 1
  fi
  grep -qF 'resource sampler agent-fd-b-01 RSS probe failed' \
    <<<"${sampler_error}" || {
    echo "a resource probe failure did not identify its instance and field" >&2
    exit 1
  }
)
rm -f "${sampler_trace}"
sampler_failure_output="$(mktemp)"
(
  export FD_ID=fd-b
  g6rd_now() { printf '2026-08-19T00:00:00Z\n'; }
  g6rd_psql() { printf '1\n'; }
  g6rd_sampler_row() { [[ "$1" != agent ]]; }
  if g6rd_sampler_tick "${sampler_failure_output}"; then
    echo "the resource sampler hid a missing required component sample" >&2
    exit 1
  fi
)
rm -f "${sampler_failure_output}"
sampler_preflight_fixture="$(mktemp -d)"
sampler_preflight_valid="${sampler_preflight_fixture}/valid.csv"
sampler_preflight_header='timestamp,component,instance,rss_bytes,fd_count,tasks,queue_depth,db_connections,environment_id,candidate_sha'
sampler_preflight_sha=0123456789abcdef0123456789abcdef01234567
{
  printf '%s\n' "${sampler_preflight_header}"
  printf '2026-08-19T00:00:00.123456Z,controller,api-fd-b,1024,10,20,0,,fixture-environment,%s\n' "${sampler_preflight_sha}"
  printf '2026-08-19T00:00:00.123456Z,controller,worker-fd-b,2048,11,21,0,,fixture-environment,%s\n' "${sampler_preflight_sha}"
  printf '2026-08-19T00:00:00.123456Z,controller,scheduler-fd-b,3072,12,22,0,,fixture-environment,%s\n' "${sampler_preflight_sha}"
  printf '2026-08-19T00:00:00.123456Z,transportd,transportd-fd-b,4096,13,23,0,,fixture-environment,%s\n' "${sampler_preflight_sha}"
  printf '2026-08-19T00:00:00.123456Z,agent,agent-fd-b-01,5120,14,24,0,,fixture-environment,%s\n' "${sampler_preflight_sha}"
  printf '2026-08-19T00:00:00.123456Z,postgres,postgres-fd-b,6144,15,25,7,8,fixture-environment,%s\n' "${sampler_preflight_sha}"
} >"${sampler_preflight_valid}"
(
  export FD_ID=fd-b G6RD_ENVIRONMENT_ID=fixture-environment
  export G6RD_CANDIDATE_SHA="${sampler_preflight_sha}"
  g6rd_validate_sampler_batch "${sampler_preflight_valid}" || {
    echo "a complete nonnegative resource preflight batch was rejected" >&2
    exit 1
  }
  for mutation in negative nan missing duplicate; do
    invalid="${sampler_preflight_fixture}/${mutation}.csv"
    case "${mutation}" in
      negative) sed '2s/,1024,/, -1,/' "${sampler_preflight_valid}" >"${invalid}" ;;
      nan) sed '3s/,2048,/,NaN,/' "${sampler_preflight_valid}" >"${invalid}" ;;
      missing) sed '4d' "${sampler_preflight_valid}" >"${invalid}" ;;
      duplicate) sed '5s/transportd,transportd/agent,agent/' "${sampler_preflight_valid}" >"${invalid}" ;;
    esac
    if g6rd_validate_sampler_batch "${invalid}" >/dev/null 2>&1; then
      echo "resource preflight accepted an invalid ${mutation} raw sampler batch" >&2
      exit 1
    fi
  done
)
rm -rf -- "${sampler_preflight_fixture}"
if command -v timeout >/dev/null 2>&1 \
  && timeout --version 2>/dev/null | grep -q 'GNU coreutils'; then
  sampler_timeout_fixture="$(mktemp -d)"
  mkdir -p "${sampler_timeout_fixture}/bin"
  cat >"${sampler_timeout_fixture}/bin/docker" <<'SHIM'
#!/usr/bin/env bash
set -eu
printf '%s\n' "${BASHPID}" >"${FAKE_DOCKER_PARENT_PID}"
trap '' TERM
(
  trap '' TERM
  printf '%s\n' "${BASHPID}" >"${FAKE_DOCKER_CHILD_PID}"
  while :; do sleep 1; done
) &
wait "$!"
SHIM
  chmod +x "${sampler_timeout_fixture}/bin/docker"
  (
    export PATH="${sampler_timeout_fixture}/bin:${PATH}"
    export FAKE_DOCKER_PARENT_PID="${sampler_timeout_fixture}/parent.pid"
    export FAKE_DOCKER_CHILD_PID="${sampler_timeout_fixture}/child.pid"
    export G6_OWNER_PASSWORD=fixture-owner G6_DEV_AUTH_TOKEN=fixture-token
    export G6_FD_ID=fd-b COMPOSE_PROJECT=fixture-project
    export G6RD_RELEASE_COMPOSE="${sampler_timeout_fixture}/missing-release.yml"
    started="${SECONDS}"
    if G6RD_TIMEOUT_PROCESS_GROUP=1 G6RD_COMPOSE_TIMEOUT_SECONDS=1 \
      g6rd_compose ps >"${sampler_timeout_fixture}/output" 2>&1; then
      echo "sampler process-group timeout accepted a hung Docker probe" >&2
      exit 1
    fi
    elapsed=$((SECONDS - started))
    ((elapsed <= 8)) || {
      echo "sampler process-group timeout exceeded its hard bound: ${elapsed}s" >&2
      exit 1
    }
    for pid_file in "${FAKE_DOCKER_PARENT_PID}" "${FAKE_DOCKER_CHILD_PID}"; do
      [[ -s "${pid_file}" ]] || {
        echo "hung Docker timeout fixture did not record its process" >&2
        exit 1
      }
      pid="$(<"${pid_file}")"
      if kill -0 "${pid}" 2>/dev/null; then
        echo "sampler process-group timeout left process ${pid} alive" >&2
        kill -KILL "${pid}" 2>/dev/null || :
        exit 1
      fi
    done
  )
  rm -rf -- "${sampler_timeout_fixture}"
fi
reclaim_owned_test="$(mktemp -d)"
mkdir -p "${reclaim_owned_test}/nested"
: >"${reclaim_owned_test}/nested/runner-owned"
docker() {
  echo "ownership reclaim started a helper for runner-owned paths" >&2
  return 1
}
g6rd_reclaim_directory "${reclaim_owned_test}"
unset -f docker
rm -rf "${reclaim_owned_test}"
release_overlay_test="$(mktemp -d)"
export G6RD_RELEASE_COMPOSE="${release_overlay_test}/release-images.yaml"
export G6RD_CONTROL_PLANE_IMAGE=ocservia-g6-control-plane:test-release
export G6RD_TRANSPORTD_IMAGE=ocservia-g6-transportd:test-release
export G6RD_RELAY_IMAGE=ocservia-g6-relay:test-release
export G6RD_PROBE_IMAGE=ocservia-g6-probe:test-release
docker() {
  [[ "$1" == image && "$2" == inspect ]] || {
    echo "release image fixture invoked an unexpected docker command" >&2
    return 1
  }
}
g6rd_prepare_release_images
for mapping in \
  'api:ocservia-g6-control-plane:test-release' \
  'worker:ocservia-g6-control-plane:test-release' \
  'scheduler:ocservia-g6-control-plane:test-release' \
  'transportd:ocservia-g6-transportd:test-release' \
  'relay:ocservia-g6-relay:test-release' \
  'g6-probe:ocservia-g6-probe:test-release'; do
  service="${mapping%%:*}"
  image="${mapping#*:}"
  grep -A1 "^  ${service}:$" "${G6RD_RELEASE_COMPOSE}" \
    | grep -qF "image: ${image}" || {
    echo "the frozen release overlay is missing ${service}:${image}" >&2
    exit 1
  }
done
unset G6RD_PROBE_IMAGE
if g6rd_prepare_release_images 2>/dev/null; then
  echo "a partial frozen release image set was accepted" >&2
  exit 1
fi
unset -f docker
unset G6RD_RELEASE_COMPOSE G6RD_CONTROL_PLANE_IMAGE G6RD_TRANSPORTD_IMAGE \
  G6RD_RELAY_IMAGE
rm -rf "${release_overlay_test}"
relay_failure_test="$(mktemp -d)"
for relay_failure_stage in validator principal-smoke; do
  (
    export G6RD_SECRETS="${relay_failure_test}/${relay_failure_stage}/secrets"
    export G6RD_WORK="${relay_failure_test}/${relay_failure_stage}/work"
    export G6RD_ARCHIVE="${G6RD_WORK}/archive"
    export G6RD_BASEBACKUP="${G6RD_WORK}/basebackup"
    export G6RD_RESULT_BARRIER="${G6RD_WORK}/result-barrier"
    export RUN_ID="relay-${relay_failure_stage}-failure" FD_ID=fd-a
    export G6_SIGNING_DIR="${relay_failure_test}/signing"
    mkdir -p "${G6RD_SECRETS}" "${G6RD_WORK}"
    for required in owner-password app-password replication-password dev-auth-token \
      oidc-client-secret session-key requester-identity-id requester-session-id \
      requester-session-cookie approver-identity-id approver-session-id \
      approver-session-cookie command-signing.pem command-verification.pem \
      relay-chain.crt relay-leaf.key relay-ca.pem relay-token; do
      printf 'fixture-%s\n' "${required}" >"${G6RD_SECRETS}/${required}"
    done
    docker() { return 0; }
    if [[ "${relay_failure_stage}" == validator ]]; then
      g6rd_relay_material_cache_valid() { return 1; }
      g6rd_verify_relay_material_principals() { return 0; }
    else
      g6rd_relay_material_cache_valid() { return 0; }
      g6rd_verify_relay_material_principals() { return 1; }
    fi
    unset G6_RELAY_DIR
    if g6rd_export_common_env 2>/dev/null; then
      echo "a relay ${relay_failure_stage} failure was hidden by common environment setup" >&2
      exit 1
    fi
    [[ -z "${G6_RELAY_DIR+x}" ]] || {
      echo "a relay ${relay_failure_stage} failure exported an empty or stale relay path" >&2
      exit 1
    }
  )
done
rm -rf "${relay_failure_test}"
build_environment_test="$(mktemp -d)"
(
  export RUNNER_TEMP="${build_environment_test}/runner-temp"
  export RUN_ID=build-environment-fd-b
  export FD_ID=fd-b
  export FD_ALIAS=fd-beta
  export G6_AUTHORITY=engineering
  export G6RD_CANDIDATE_SHA=5f9a2a943d7aa38224bc3266b7176f0a061a6b6c
  export G6_AGENTS_B=1
  g6rd_init_environment
  export G6_OWNER_PASSWORD=live-owner G6_APP_PASSWORD=live-app
  export G6_REPLICATION_PASSWORD=live-replication G6_DEV_AUTH_TOKEN=live-token
  export G6_SIGNING_DIR=/live/signing G6_RELAY_DIR=/live/relay
  export G6_PROBE_CONTROLLER_KEY_DIR=/live/controller
  export OCSERV_CONTROLLER_ENDPOINT_ID=live-controller-id
  g6rd_prepare_build_environment
  for name in G6_OWNER_PASSWORD G6_APP_PASSWORD G6_REPLICATION_PASSWORD G6_DEV_AUTH_TOKEN; do
    [[ "${!name}" == harness-placeholder ]] || {
      echo "image build environment retained live ${name}" >&2
      exit 1
    }
  done
  [[ "${G6_SIGNING_DIR}" == "${G6RD_WORK}/signing" ]]
  [[ "${G6_RELAY_DIR}" == "${G6RD_WORK}/relay-secrets" ]]
  [[ "${G6_PROBE_CONTROLLER_KEY_DIR}" == "${G6RD_WORK}/probe-controller-key" ]]
  [[ -z "${OCSERV_CONTROLLER_ENDPOINT_ID:-}" ]]
  for dir in "${G6_SIGNING_DIR}" "${G6_RELAY_DIR}" "${G6_RELAY_DIR}/relay" \
    "${G6_RELAY_DIR}/transportd" "${G6_RELAY_DIR}/probe" \
    "${G6_PROBE_CONTROLLER_KEY_DIR}" \
    "${G6RD_AGENTS}/agent-fd-b-01/identity" \
    "${G6RD_AGENTS}/agent-fd-b-01/journal" \
    "${G6RD_AGENTS}/agent-fd-b-01/privd" \
    "${G6RD_AGENTS}/agent-fd-b-01/secrets" \
    "${G6RD_AGENTS}/agent-fd-b-01/state"; do
    [[ -d "${dir}" ]] || {
      echo "image build placeholder directory is missing: ${dir}" >&2
      exit 1
    }
  done
)
rm -rf "${build_environment_test}"
relay_topology_test="$(mktemp -d)"
export G6RD_AGENTS="${relay_topology_test}/agents"
export G6RD_AGENT_COMPOSE="${relay_topology_test}/agents.yaml"
export G6RD_SECRETS="${relay_topology_test}/secrets"
mkdir -p "${G6RD_SECRETS}"
: >"${G6RD_SECRETS}/relay-ca.pem"

export FD_ID=fd-a
export G6_RELAY_URL_A=https://relay-a:3443
export G6_RELAY_URL_B=https://relay-b:3443
g6rd_export_relay_urls
[[ "${G6_RELAY_URL_A}" == https://relay-a:3443 ]]
[[ "${G6_RELAY_URL_B}" == https://relay-b:3443 ]]

export FD_ID=fd-b
export G6_RELAY_URL_A=https://relay-a:3443
export G6_RELAY_URL_B=https://relay-b:3443
export G6_AGENTS_B=1
mkdir -p "${G6RD_AGENTS}/agent-fd-b-01/state"
printf '%s\t%s\t%s\n' \
  g6-fd-b-01 018f2f10-7abc-7def-8abc-0123456789ab \
  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  >"${relay_topology_test}/nodes.tsv"
g6rd_stage_agent_node_state "${relay_topology_test}/nodes.tsv"
[[ "$(<"${G6RD_AGENTS}/agent-fd-b-01/state/node-id")" == \
  018f2f10-7abc-7def-8abc-0123456789ab ]] || {
  echo "Agent startup must restore a missing node-id bind from enrollment state" >&2
  exit 1
}
g6rd_write_agent_overlay 1
grep -q 'dockerfile: rust/g6-agent.Dockerfile' "${G6RD_AGENT_COMPOSE}"
[[ "${G6_RELAY_URL_A}" == https://relay-a:3443 ]]
[[ "${G6_RELAY_URL_B}" == https://relay-b:3443 ]]
grep -q 'G6_RELAY_URL_A: "https://relay-a:3443"' "${G6RD_AGENT_COMPOSE}"
grep -q 'G6_RELAY_URL_B: "https://relay-b:3443"' "${G6RD_AGENT_COMPOSE}"
grep -q 'G6_NODE_ID: "018f2f10-7abc-7def-8abc-0123456789ab"' \
  "${G6RD_AGENT_COMPOSE}" || {
  echo "the runtime overlay must pass the enrolled node id explicitly" >&2
  exit 1
}
grep -q 'relay-a:host-gateway' "${G6RD_AGENT_COMPOSE}"
if grep -q 'relay-b:host-gateway' "${G6RD_AGENT_COMPOSE}"; then
  echo "the local fd-b relay must remain on Docker DNS" >&2
  exit 1
fi
export G6RD_AGENT_IMAGE=ocservia-g6-agent:test-release
g6rd_write_agent_overlay 1
grep -q 'image: ocservia-g6-agent:test-release' "${G6RD_AGENT_COMPOSE}"
if grep -q 'dockerfile: rust/g6-agent.Dockerfile' "${G6RD_AGENT_COMPOSE}"; then
  echo "the frozen Agent overlay must not rebuild the release image" >&2
  exit 1
fi
unset G6RD_AGENT_IMAGE

curl() {
  printf '%s\n' "$@" >"${relay_topology_test}/curl.args"
}
g6rd_relay_endpoint_ready "${G6_RELAY_URL_A}" 13443
grep -qx -- 'relay-a:3443:127.0.0.1:13443' "${relay_topology_test}/curl.args"
grep -qx -- 'https://relay-a:3443/ping' "${relay_topology_test}/curl.args"
if g6rd_relay_endpoint_ready https://example.invalid:3444 2>/dev/null; then
  echo "relay readiness must reject a URL outside the fixed G6 topology" >&2
  exit 1
fi
rm -rf "${relay_topology_test}"
unset -f curl
unset FD_ID G6_AGENTS_B G6_RELAY_URL_A G6_RELAY_URL_B G6RD_AGENTS \
  G6RD_AGENT_COMPOSE G6RD_SECRETS
readiness_test="$(mktemp -d)"
(
  export G6RD_STATE="${readiness_test}"
  export G6RD_WORKSPACE_ID=018f2f10-7abc-7def-8abc-0123456789ab
  printf '%s\t%s\t%s\n' \
    g6-fd-a-01 018f2f10-7abc-7def-8abc-0123456789ab endpoint \
    >"${readiness_test}/nodes.tsv"
  g6rd_api_session_curl() {
    printf '%s\n' '{"items":[{"id":"018f2f10-7abc-7def-8abc-0123456789ab","name":"g6-fd-a-01","trust_status":"active","connection_state":"online","freshness":"fresh","last_heartbeat_at":"2026-08-17T00:00:00Z"}],"page":{"has_more":false}}'
  }
  g6rd_capture_agent_readiness "${readiness_test}/nodes.tsv"
  g6rd_api_session_curl() {
    printf '%s\n' '{"items":[{"id":"018f2f10-7abc-7def-8abc-0123456789ab","name":"g6-fd-a-01","trust_status":"active","connection_state":"offline","freshness":"stale","last_heartbeat_at":"2026-08-17T00:00:00Z"}],"page":{"has_more":false}}'
  }
  if g6rd_capture_agent_readiness "${readiness_test}/nodes.tsv"; then
    echo "Agent readiness accepted an offline controller observation" >&2
    exit 1
  fi
  jq -e '.items[0].connection_state == "offline"' \
    "${readiness_test}/agent-readiness-last.json" >/dev/null
)
rm -rf "${readiness_test}"

# A global readiness failure may name peer nodes, but this runner owns only
# the services listed in its local node file. Prefer exited local services;
# controller lag must not churn healthy late-arriving batch members.
restart_scope_test="$(mktemp -d)"
(
  export FD_ID=fd-a
  export G6RD_STATE="${restart_scope_test}"
  cat >"${restart_scope_test}/nodes.tsv" <<'EOF'
g6-fd-a-01	018f2f10-7abc-7def-8abc-0123456789ab	endpoint-a
g6-fd-a-02	018f2f10-7abc-7def-8abc-1123456789ab	endpoint-b
EOF
  cat >"${restart_scope_test}/agent-readiness-last.json" <<'EOF'
{"items":[{"id":"018f2f10-7abc-7def-8abc-0123456789ab","trust_status":"active","connection_state":"online","freshness":"fresh","last_heartbeat_at":"2026-08-17T00:00:00Z"},{"id":"018f2f10-7abc-7def-8abc-1123456789ab","trust_status":"active","connection_state":"offline","freshness":"never","last_heartbeat_at":null},{"id":"018f2f10-7abc-7def-8abc-2123456789ab","trust_status":"active","connection_state":"offline","freshness":"never","last_heartbeat_at":null}]}
EOF
  g6rd_agent_service_running() { return 0; }
  selected="$(g6rd_agent_services_needing_restart "${restart_scope_test}/nodes.tsv")"
  [[ "${selected}" == agent-fd-a-02 ]] || {
    echo "Agent recovery selected the wrong local services: ${selected}" >&2
    exit 1
  }
  g6rd_agent_service_running() { [[ "${1}" != agent-fd-a-01 ]]; }
  stopped="$(g6rd_agent_services_not_running "${restart_scope_test}/nodes.tsv")"
  [[ "${stopped}" == agent-fd-a-01 ]] || {
    echo "Agent recovery did not isolate the exited local service: ${stopped}" >&2
    exit 1
  }
  selected="$(g6rd_agent_services_needing_restart "${restart_scope_test}/nodes.tsv")"
  [[ "${selected}" == agent-fd-a-02 ]] || {
    echo "Agent recovery mixed an exited service into controller-unready selection: ${selected}" >&2
    exit 1
  }
)
rm -rf "${restart_scope_test}"

# The operations API requires a quoted strong ETag. Exercise the actual curl
# argument and ensure a rejected response emits only its safe problem detail.
enqueue_test="$(mktemp -d)"
(
  export RUNNER_TEMP="${enqueue_test}"
  export G6RD_STATE="${enqueue_test}"
  g6rd_node_revision() {
    if [[ "${G6RD_TEST_FIXED_REVISION:-0}" == 1 ]]; then
      printf '7\n'
    elif [[ -e "${enqueue_test}/retry-seen" ]]; then
      printf '8\n'
    else
      printf '7\n'
    fi
  }
  g6rd_now() {
    local counter=0 counter_file="${enqueue_test}/now-counter"
    [[ ! -e "${counter_file}" ]] || counter="$(<"${counter_file}")"
    printf '%s\n' "$((counter + 1))" >"${counter_file}"
    printf '2026-08-17T00:00:%02dZ\n' "${counter}"
  }
  g6rd_secret() { printf 'test-development-token\n'; }
  g6rd_api_port() { printf '18080\n'; }
  curl() {
    local output="" argument arguments=("$@")
    printf '%s\n' "${arguments[@]}" >"${enqueue_test}/curl.args"
    for argument in "${arguments[@]}"; do
      if [[ "${argument}" == "X-Request-ID: "* ]]; then
        printf '%s\n' "${argument}" >>"${enqueue_test}/curl-request-ids"
      fi
    done
    while (($#)); do
      case "$1" in
        --output)
          output="$2"
          shift 2
          ;;
        *) shift ;;
      esac
    done
    [[ -n "${output}" ]] || return 2
    case "${G6RD_TEST_CURL_MODE}" in
      accepted)
        printf '%s\n' '{"command_id":"018f2f10-7abc-7def-8abc-3123456789ab"}' >"${output}"
        printf '202 0.125'
        ;;
      retry)
        if [[ ! -e "${enqueue_test}/retry-seen" ]]; then
          touch "${enqueue_test}/retry-seen"
          printf '%s\n' '{"type":"https://ocservia.dev/problems/stale-revision","detail":"the node changed after this operation was prepared"}' >"${output}"
          printf '409 0.050'
        else
          printf '%s\n' '{"command_id":"018f2f10-7abc-7def-8abc-4123456789ab"}' >"${output}"
          printf '202 0.075'
        fi
        ;;
      stale)
        touch "${enqueue_test}/retry-seen"
        printf '%s\n' '{"type":"https://ocservia.dev/problems/stale-revision","detail":"the node changed after this operation was prepared"}' >"${output}"
        printf '409 0.050'
        ;;
      wrong-stale-type)
        printf '%s\n' '{"type":"https://ocservia.dev/problems/conflict","detail":"the node changed after this operation was prepared"}' >"${output}"
        printf '409 0.050'
        ;;
      wrong-stale-detail)
        printf '%s\n' '{"type":"https://ocservia.dev/problems/stale-revision","detail":"some other conflict"}' >"${output}"
        printf '409 0.050'
        ;;
      other-status)
        printf '%s\n' '{"type":"https://ocservia.dev/problems/unavailable","detail":"try later"}' >"${output}"
        printf '503 0.050'
        ;;
      *)
        printf '%s\n' '{"type":"https://ocservia.dev/problems/expected-version-required","detail":"provide a quoted revision"}' >"${output}"
        printf '400 0.050'
        ;;
    esac
  }

  G6RD_TEST_CURL_MODE=accepted
  g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab test-key
  grep -Fxq 'If-Match: "revision-7"' "${enqueue_test}/curl.args" || {
    echo "synthetic enqueue did not send the API's quoted revision ETag" >&2
    exit 1
  }
  [[ "$(sed -n '1p' "${enqueue_test}/curl-request-ids")" == \
    "X-Request-ID: test-key.attempt-1" ]] || {
    echo "synthetic enqueue did not send its recorded attempt request identity" >&2
    exit 1
  }
  grep -Fxq '{"kind":"noop"}' "${enqueue_test}/curl.args" || {
    echo "synthetic enqueue did not send the operations API's noop kind" >&2
    exit 1
  }
  jq -e '
    .status == 202 and .command_id != ""
    and .idempotency_key == "test-key"
    and .attempt_request_id == "test-key.attempt-1"
    and .attempt_ordinal == 1 and .attempt_limit == 3
    and .requested_revision == 7
    and .problem_type == "" and .problem_detail == ""
  ' \
    "${enqueue_test}/enqueue-log.jsonl" >/dev/null

  G6RD_TEST_CURL_MODE=retry
  retry_log="${enqueue_test}/retry-log.jsonl"
  g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab retry-key \
    "${retry_log}"
  jq -se '
    length == 2
    and .[0].status == 409 and .[0].command_id == ""
    and .[1].status == 202 and .[1].command_id != ""
    and all(.[]; .idempotency_key == "retry-key")
    and [.[].attempt_ordinal] == [1, 2]
    and [.[].requested_revision] == [7, 8]
    and all(.[]; .attempt_limit == 3)
    and (.[0].attempt_request_id != .[1].attempt_request_id)
    and .[1].at > .[0].at
    and .[0].problem_type == "https://ocservia.dev/problems/stale-revision"
    and .[0].problem_detail == "the node changed after this operation was prepared"
  ' "${retry_log}" >/dev/null || {
    echo "synthetic enqueue did not preserve both stale-revision attempts" >&2
    exit 1
  }
  grep -Fxq 'If-Match: "revision-8"' "${enqueue_test}/curl.args" || {
    echo "synthetic enqueue did not refresh the revision before retry" >&2
    exit 1
  }
  [[ "$(sed -n '2p' "${enqueue_test}/curl-request-ids")" == \
    "X-Request-ID: retry-key.attempt-1" && \
    "$(sed -n '3p' "${enqueue_test}/curl-request-ids")" == \
    "X-Request-ID: retry-key.attempt-2" ]] || {
    echo "synthetic enqueue did not send distinct request identities for both retries" >&2
    exit 1
  }

  rm -f "${enqueue_test}/retry-seen"
  G6RD_TEST_CURL_MODE=stale
  G6RD_ENQUEUE_STALE_RETRIES=1
  export G6RD_ENQUEUE_STALE_RETRIES
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab stale-key \
    "${enqueue_test}/stale-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue exceeded its stale-revision retry bound" >&2
    exit 1
  fi
  jq -se 'length == 2 and all(.[]; .status == 409)' \
    "${enqueue_test}/stale-log.jsonl" >/dev/null || {
    echo "synthetic enqueue did not retain every bounded stale-revision attempt" >&2
    exit 1
  }
  unset G6RD_ENQUEUE_STALE_RETRIES

  rm -f "${enqueue_test}/retry-seen"
  G6RD_TEST_CURL_MODE=stale
  G6RD_TEST_FIXED_REVISION=1
  export G6RD_TEST_FIXED_REVISION
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab same-revision-key \
    "${enqueue_test}/same-revision-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue retried without refreshing the stale revision" >&2
    exit 1
  fi
  jq -se 'length == 1 and .[0].status == 409 and .[0].requested_revision == 7' \
    "${enqueue_test}/same-revision-log.jsonl" >/dev/null || {
    echo "synthetic enqueue did not stop before reusing a stale revision" >&2
    exit 1
  }
  unset G6RD_TEST_FIXED_REVISION

  G6RD_TEST_CURL_MODE=wrong-stale-type
  export G6RD_TEST_CURL_MODE
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab wrong-type-key \
    "${enqueue_test}/wrong-type-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue retried a 409 with the wrong RFC7807 problem type" >&2
    exit 1
  fi
  jq -se 'length == 1 and .[0].status == 409' \
    "${enqueue_test}/wrong-type-log.jsonl" >/dev/null || {
    echo "synthetic enqueue did not fail closed on the first wrong-type 409" >&2
    exit 1
  }

  G6RD_TEST_CURL_MODE=wrong-stale-detail
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab wrong-detail-key \
    "${enqueue_test}/wrong-detail-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue retried a 409 with the wrong RFC7807 detail" >&2
    exit 1
  fi
  jq -se 'length == 1 and .[0].problem_detail == "some other conflict"' \
    "${enqueue_test}/wrong-detail-log.jsonl" >/dev/null

  G6RD_TEST_CURL_MODE=other-status
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab other-status-key \
    "${enqueue_test}/other-status-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue retried a non-409 response" >&2
    exit 1
  fi
  jq -se 'length == 1 and .[0].status == 503' \
    "${enqueue_test}/other-status-log.jsonl" >/dev/null

  G6RD_ENQUEUE_STALE_RETRIES=3
  export G6RD_ENQUEUE_STALE_RETRIES
  if g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab unbounded-key \
    "${enqueue_test}/unbounded-log.jsonl" 2>/dev/null; then
    echo "synthetic enqueue accepted an attempt limit above the formal bound" >&2
    exit 1
  fi
  [[ ! -e "${enqueue_test}/unbounded-log.jsonl" ]] || {
    echo "synthetic enqueue issued a request before rejecting an excessive retry bound" >&2
    exit 1
  }
  unset G6RD_ENQUEUE_STALE_RETRIES

  G6RD_TEST_CURL_MODE=rejected
  set +e
  rejection="$(g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab rejected-key 2>&1)"
  status=$?
  set -e
  [[ "${status}" -eq 1 ]] || {
    echo "synthetic enqueue accepted a rejected API response" >&2
    exit 1
  }
  grep -qF 'returned HTTP 400: provide a quoted revision' <<<"${rejection}" || {
    echo "synthetic enqueue did not expose the safe API problem detail" >&2
    exit 1
  }
)
rm -rf "${enqueue_test}"

approve_node="$(sed -n '/^g6rd_approve_node() {/,/^}/p' "${LIB}")"
grep -q 'g6rd_api_session_curl requester /api/v1/approval-requests' <<<"${approve_node}" || {
  echo "node activation must create a content-bound request as the requester" >&2
  exit 1
}
grep -q 'g6rd_api_session_curl approver' <<<"${approve_node}" || {
  echo "node activation must use a distinct authenticated approver" >&2
  exit 1
}
grep -q 'X-Approval-ID: ${approval_id}' <<<"${approve_node}" || {
  echo "node activation must consume the independently approved request" >&2
  exit 1
}
grep -q 'ocservia.dev/problems/transport-unavailable' <<<"${approve_node}" || {
  echo "node activation must recognize only the defined pending transport outcome" >&2
  exit 1
}
apply_line="$(grep -n '/nodes/${node_id}/approval' <<<"${approve_node}" | cut -d: -f1)"
reconcile_line="$(grep -n 'ocservia.dev/problems/transport-unavailable' <<<"${approve_node}" | cut -d: -f1)"
if [[ -z "${apply_line}" || -z "${reconcile_line}" || "${apply_line}" -ge "${reconcile_line}" ]]; then
  echo "pending transport reconciliation must inspect the final activation response" >&2
  exit 1
fi
grep -q '"${node_status}" == active.*"${approval_status}" == consumed' <<<"${approve_node}" || {
  echo "pending transport outcomes must reconcile active node and consumed approval state" >&2
  exit 1
}
grep -q "jq -er '.trust_status'" <<<"${approve_node}" || {
  echo "node activation reconciliation must use the node read model trust_status field" >&2
  exit 1
}
grep -qF 'cap_add: [SETUID, SETGID]' "${LIB}" || {
  echo "the root supervisor must retain only the capabilities required to drop to the agent uid" >&2
  exit 1
}

# Keep the shell wrapper aligned with the tunnel binary's asymmetric CLI:
# serve forwards to a target, while forward binds a local listener.
grep -A12 '^g6rd_tunnel_forward()' "${LIB}" | grep -q -- '--listen "0.0.0.0:${listen_port}"' || {
  echo "the G6 tunnel forward wrapper must pass the local listener with --listen" >&2
  exit 1
}
if grep -A12 '^g6rd_tunnel_forward()' "${LIB}" | grep -q -- '--forward'; then
  echo "the G6 tunnel forward wrapper must not pass the serve-only --forward flag" >&2
  exit 1
fi

# Bind-mounted runtime material must be readable by the exact container
# principals that consume it, while remaining non-world-readable.
grep -q 'chown 65534:65532 /fix/command-signing.pem' "${LIB}" || {
  echo "the control-plane signing key must be owned by its container uid" >&2
  exit 1
}
grep -qF 'chown 0:65532 /fix; chmod 0755 /fix; chown 65534:65532 /fix/command-signing.pem' "${LIB}" || {
  echo "the signing directory must have root-owned ancestry accepted by every consumer" >&2
  exit 1
}
grep -q 'chown 65534:65532 /fix/controller.key' "${LIB}" || {
  echo "the probe controller key must be owned by its container uid" >&2
  exit 1
}
relay_material="$(sed -n '/^g6rd_materialize_relay_dir() {/,/^}/p' "${LIB}")"
for scoped_path in relay/relay-token transportd/relay-token probe/relay-token; do
  grep -qF "\${dir}/${scoped_path}" <<<"${relay_material}" || {
    echo "relay token copy is missing for ${scoped_path}" >&2
    exit 1
  }
done
grep -qF 'chown 65532:65532 /fix/relay/* /fix/transportd/*' \
  <<<"${relay_material}" || {
  echo "relay and transportd files must be owned by their runtime uid" >&2
  exit 1
}
grep -qF 'chown 65534:65532 /fix/probe/*' <<<"${relay_material}" || {
  echo "the probe files must be owned by its exact UDS peer uid" >&2
  exit 1
}
if grep -Eq 'chown[[:space:]]+[^[:space:]]+[[:space:]]+/fix/(relay|transportd|probe)([[:space:]]|$)' \
  <<<"${relay_material}"; then
  echo "relay material directories must remain runner-owned for bounded rebuild and cleanup" >&2
  exit 1
fi
grep -qF 'chmod 0755 /fix/relay /fix/transportd /fix/probe' \
  <<<"${relay_material}" || {
  echo "scoped relay material directories must remain runner-owned and traversable" >&2
  exit 1
}
grep -qF 'chmod 0600 /fix/relay/relay.key /fix/relay/relay-token' \
  <<<"${relay_material}" || {
  echo "relay private material must remain process-only" >&2
  exit 1
}
grep -qF '/fix/transportd/relay-token /fix/probe/relay-token' \
  <<<"${relay_material}" || {
  echo "transportd and probe token copies must remain process-only" >&2
  exit 1
}
if grep -Eq 'chmod[[:space:]]+0?[0-7]{2}[1-7][[:space:]].*relay-token' \
  <<<"${relay_material}"; then
  echo "a relay token copy must never become group- or world-readable" >&2
  exit 1
fi
relay_principal_smoke="$(sed -n '/^g6rd_verify_relay_material_principals() {/,/^}/p' "${LIB}")"
[[ "$(grep -c -- '--user 65532:65532' <<<"${relay_principal_smoke}")" -eq 2 ]] || {
  echo "relay and transportd token smoke checks must run as uid 65532" >&2
  exit 1
}
[[ "$(grep -c -- '--user 65534:65532' <<<"${relay_principal_smoke}")" -eq 1 ]] || {
  echo "the probe token smoke check must run as the UDS peer uid 65534" >&2
  exit 1
}
for scoped_path in relay transportd probe; do
  grep -qF "\${dir}/${scoped_path}:/run/relay-secrets:ro" \
    <<<"${relay_principal_smoke}" || {
    echo "the ${scoped_path} principal smoke must use only its scoped read-only bind" >&2
    exit 1
  }
done
[[ "$(grep -Ec 'relay-token\).*6553[24]:65532:600' <<<"${relay_principal_smoke}")" -eq 3 ]] || {
  echo "every runtime principal must assert an owner-only relay token" >&2
  exit 1
}
[[ "$(grep -c 'stat -c.*%a.*relay-secrets).*755' <<<"${relay_principal_smoke}")" -eq 3 ]] || {
  echo "every runtime principal must see a runner-owned mode-0755 mount root" >&2
  exit 1
}
relay_cache_validator="$(sed -n '/^g6rd_relay_material_cache_valid() {/,/^}/p' "${LIB}")"
grep -qF 'const directory = fs.opendirSync(target);' <<<"${relay_cache_validator}" || {
  echo "relay material cache validation must inspect the host tree without a phase-local container" >&2
  exit 1
}
grep -qF 'while (names.length <= expectedNames.length)' <<<"${relay_cache_validator}" || {
  echo "relay material cache traversal must stop after the first unexpected entry" >&2
  exit 1
}
if grep -q 'docker run' <<<"${relay_cache_validator}"; then
  echo "relay material cache validation must not start a support container in every phase" >&2
  exit 1
fi
grep -qF 'assertDirectory(root, runnerUid, runnerGid, 0o700);' \
  <<<"${relay_cache_validator}" || {
  echo "relay material cache root must remain runner-owned mode 0700" >&2
  exit 1
}
grep -qF 'assertDirectory(scoped, runnerUid, runnerGid, 0o755);' \
  <<<"${relay_cache_validator}" || {
  echo "relay material cache directories must remain runner-owned mode 0755" >&2
  exit 1
}
for exact_stat in \
  '"relay.crt": [65532, 65532, 0o644]' \
  '"relay.key": [65532, 65532, 0o600]' \
  '"relay-ca.pem": [65532, 65532, 0o644]' \
  '"relay-ca.pem": [65534, 65532, 0o644]' \
  '"relay-token": [65534, 65532, 0o600]'; do
  grep -qF "${exact_stat}" <<<"${relay_cache_validator}" || {
    echo "relay cache validator is missing exact file metadata: ${exact_stat}" >&2
    exit 1
  }
done
[[ "$(grep -Fc '"relay-token": [65532, 65532, 0o600]' \
  <<<"${relay_cache_validator}")" -eq 2 ]] || {
  echo "relay cache validator must require both uid-65532 token copies" >&2
  exit 1
}
grep -qF 'g6rd_relay_material_cache_valid "${dir}"' <<<"${relay_material}" || {
  echo "relay material cache hits and rebuilds must pass the closed validator" >&2
  exit 1
}
grep -qF 'relay_dir="$(g6rd_materialize_relay_dir)" || return 1' "${LIB}" || {
  echo "relay material validation failure must propagate out of common environment setup" >&2
  exit 1
}
if grep -qF 'chown 0:65532 /fix; chmod 0750 /fix' "${LIB}"; then
  echo "runtime material directories must remain reachable by later runner phases" >&2
  exit 1
fi
agent_material="$(sed -n '/^g6rd_prepare_agent_material() {/,/^}/p' "${LIB}")"
grep -q 'chown 0:65532 /fix-secrets' <<<"${agent_material}" || {
  echo "Agent secret ancestry must be root-owned before startup" >&2
  exit 1
}
grep -q 'chmod 0440 /fix-secrets/command-verification-agent.pem' <<<"${agent_material}" || {
  echo "the unprivileged Agent must receive only its group-readable verification key" >&2
  exit 1
}
grep -q 'chmod 0400 /fix-secrets/command-verification-privd.pem' <<<"${agent_material}" || {
  echo "privd command and sealing keys must remain root-only" >&2
  exit 1
}
grep -q 'chown 0:0 /fix-privd' <<<"${agent_material}" || {
  echo "privd attestation state must use a separate root-only bind" >&2
  exit 1
}
grep -q 'target: /run/ocservia-privd' "${LIB}" || {
  echo "the Agent overlay must mount persistent root-only privd state" >&2
  exit 1
}
grep -q '\$PRIVD_STATE/attestation.key' "${SUPERVISOR}" || {
  echo "privd must keep its attestation key outside Agent-owned state" >&2
  exit 1
}
grep -q 'setpriv --reuid 0 --regid 65532 --clear-groups' "${SUPERVISOR}" || {
  echo "privd must match the production root:ocserv-agent service principal" >&2
  exit 1
}
grep -q '\$SOCKET_DIR/agent.pid' "${SUPERVISOR}" || {
  echo "the root supervisor must write process metadata to its runtime directory" >&2
  exit 1
}
if grep -q '\$STATE/agent.pid' "${SUPERVISOR}"; then
  echo "the capability-restricted root supervisor cannot write Agent-owned state" >&2
  exit 1
fi
grep -qF "'cat /run/ocserv-platform/agent.pid'" "${LIB}" || {
  echo "the resource sampler must read the root-owned Agent PID" >&2
  exit 1
}
enrollment_installer="$(sed -n '/^g6rd_install_agent_enrollment_token() {/,/^}/p' "${LIB}")"
grep -q -- '--network none' <<<"${enrollment_installer}" || {
  echo "enrollment-token materialization must not have network access" >&2
  exit 1
}
grep -q 'chmod 0600 /fix/enrollment-token' <<<"${enrollment_installer}" || {
  echo "one-time enrollment tokens must be process-owned mode 0600" >&2
  exit 1
}
for script in "${FD_A}" "${FD_B}"; do
  grep -q 'g6rd_install_agent_enrollment_token' "${script}" || {
    echo "each failure domain must install enrollment tokens through the root-owned bind" >&2
    exit 1
  }
  if grep -q 'chmod 0644 .*enrollment-token' "${script}"; then
    echo "one-time enrollment tokens must not be world-readable" >&2
    exit 1
  fi
done
grep -q '"${dir}/secrets/command-verification-agent.pem"' "${LIB}" || {
  echo "agent material must derive its verification copies from the shared key" >&2
  exit 1
}
grep -q 'g6rd_reclaim_directory "${G6RD_WORK}"' "${LIB}" || {
  echo "cleanup must reclaim uid-mapped runtime material before removal" >&2
  exit 1
}
for fd_script in "${FD_A}" "${FD_B}"; do
  sed -n '/^phase_prepare() {/,/^}/p' "${fd_script}" \
    | grep -q 'g6rd_prepare_support_image' || {
    echo "each failure domain must cache the cleanup support image before isolation" >&2
    exit 1
  }
done
if grep -nE 'docker run (--rm|-d)( |$)' "${LIB}" "${FD_A}" "${FD_B}" \
  | grep -v -- '--pull=never'; then
  echo "G6 readiness docker run commands must never pull from the network" >&2
  exit 1
fi
reclaim_directory="$(sed -n '/^g6rd_reclaim_directory() {/,/^}/p' "${LIB}")"
grep -q -- '--pull=never' <<<"${reclaim_directory}" || {
  echo "cleanup ownership reclaim must not pull an image" >&2
  exit 1
}
grep -q 'ownership_mismatch' <<<"${reclaim_directory}" || {
  echo "cleanup must skip its helper when all scoped paths are already runner-owned" >&2
  exit 1
}
grep -q '^g6rd_cleanup_bounded()' "${LIB}" || {
  echo "the shared cleanup path must enforce an overall hard timeout" >&2
  exit 1
}
fd_a_cleanup="$(sed -n '/^phase_cleanup() {/,/^}/p' "${FD_A}")"
if ! grep -qF 'cleanup) phase_cleanup' "${FD_A}" \
  || ! grep -qF 'relay_a_only_topology_restore' <<<"${fd_a_cleanup}" \
  || ! grep -qF 'g6rd_cleanup_bounded' <<<"${fd_a_cleanup}"; then
  echo "failure domain A must restore topology and use bounded cleanup" >&2
  exit 1
fi
fd_b_cleanup="$(sed -n '/^phase_cleanup() {/,/^}/p' "${FD_B}")"
fd_b_cleanup_prelude="$(sed -n '/^phase_cleanup_prelude() {/,/^}/p' "${FD_B}")"
if ! grep -qF 'cleanup) phase_cleanup' "${FD_B}" \
  || ! grep -qF 'cleanup-prelude) phase_cleanup_prelude' "${FD_B}" \
  || ! grep -qF 'g6rd_cleanup_bounded' <<<"${fd_b_cleanup}"; then
  echo "failure domain B must extend rather than replace bounded cleanup" >&2
  exit 1
fi
grep -qF 'timeout --foreground --signal=TERM --kill-after=5s 45s' \
  <<<"${fd_b_cleanup}" || {
  echo "failure domain B cleanup prelude must have an overall hard bound" >&2
  exit 1
}
for cleanup_token in \
  'stop_watchers' \
  'release_armed_pre_send_barrier' \
  'release_armed_post_send_barrier' \
  'release_armed_result_commit_barrier' \
  'g6rd_release_synthetic_barriers' \
  'for service in transportd api scheduler worker' \
  'unpause_scoped_container "${COMPOSE_PROJECT}-${service}-1"'; do
  grep -qF "${cleanup_token}" <<<"${fd_b_cleanup_prelude}" || {
    echo "failure domain B cleanup is missing scoped recovery: ${cleanup_token}" >&2
    exit 1
  }
done
cleanup_stop_line="$(grep -nF 'stop_watchers' <<<"${fd_b_cleanup_prelude}" | cut -d: -f1)"
cleanup_release_line="$(grep -nF 'release_armed_pre_send_barrier' <<<"${fd_b_cleanup_prelude}" | cut -d: -f1)"
[[ -n "${cleanup_stop_line}" && -n "${cleanup_release_line}" \
  && "${cleanup_stop_line}" -lt "${cleanup_release_line}" ]] || {
  echo "cleanup must stop detached watchers before removing barrier/run state" >&2
  exit 1
}
grep -qF "archive_command = 'test -f /var/lib/postgresql/archive/%f || cp %p /var/lib/postgresql/archive/%f'" \
  "${POSTGRES_INIT}" || {
  echo "PostgreSQL archive_command must succeed when the WAL segment already exists" >&2
  exit 1
}
archive_test="$(mktemp -d)"
touch "${archive_test}/already-archived"
(
  test -f "${archive_test}/already-archived" || cp "${archive_test}/missing-source" \
    "${archive_test}/already-archived"
) || {
  echo "the configured archive existence guard is not idempotent" >&2
  exit 1
}
rm -rf "${archive_test}"
sed -n '/^phase_publish_shared_secrets() {/,/^}/p' "${FD_A}" \
  | grep -q 'relay-chain.crt' || {
  echo "the shared-trust handoff must include the relay certificate chain" >&2
  exit 1
}
sed -n '/^phase_publish_shared_secrets() {/,/^}/p' "${FD_A}" \
  | grep -q 'requester-session-cookie approver-identity-id' || {
  echo "the peer must receive the short-lived authenticated session fixtures" >&2
  exit 1
}
grep -q '^seed_authenticated_approval_fixtures()' "${FD_A}" || {
  echo "fd-a must seed independent authenticated approval principals" >&2
  exit 1
}
grep -q 'relay-ca.pem relay-chain.crt relay-leaf.crt' "${FD_B}" || {
  echo "fd-b must import the shared relay certificate chain" >&2
  exit 1
}

# Each workflow step starts a fresh shell. State created after the initial
# environment export must be exported immediately or copied from rendezvous
# before the next phase derives its Compose environment.
grep -q 'export OCSERV_CONTROLLER_ENDPOINT_ID="${endpoint}"' "${FD_A}" || {
  echo "fd-a must export the controller endpoint after bootstrapping it" >&2
  exit 1
}
standby_bootstrap="$(sed -n '/^phase_standby_bootstrap() {/,/^}/p' "${FD_B}")"
grep -q 'standby-bootstrap) phase_standby_bootstrap "${2:?primary rendezvous directory}"' "${FD_B}" || {
  echo "the standby dispatcher must pass the primary rendezvous argument" >&2
  exit 1
}
grep -q 'controller-endpoint-id.*G6RD_STATE.*controller-endpoint-id' <<<"${standby_bootstrap}" || {
  echo "fd-b must import the controller endpoint from the primary rendezvous" >&2
  exit 1
}
grep -q 'workspace-id.*G6RD_STATE.*workspace-id' <<<"${standby_bootstrap}" || {
  echo "fd-b must import the workspace id from the primary rendezvous" >&2
  exit 1
}

# Every required observation event from g6-slo.yaml must be produced by the
# harness timeline: this stage runs the real fault scenarios, so all sixteen
# observations are in scope, including the load bracket around the failover.
for event in \
  load_started primary_failure_injected new_primary_writable api_recovered \
  worker_recovered load_stopped \
  old_primary_isolated new_primary_promoted old_primary_write_rejected \
  marker_a_written restore_point_created marker_b_written restore_verified \
  owner_a_paused owner_b_acquired owner_a_resumed \
  stale_transport_rejected stale_agent_rejected \
  scheduler_a_paused scheduler_b_acquired scheduler_a_resumed \
  stale_scheduler_commit_rejected \
  api_instance_failed gateway_traffic_transferred api_slo_measured \
  worker_instance_failed worker_replacement_active dispatch_recovered \
  relay_a_failed relay_b_active \
  direct_path_active direct_path_failed relay_path_active \
  direct_path_recovered \
  bulk_disconnect_injected reconnect_started reconnect_completed \
  outbox_claim_committed worker_crashed_before_send command_recovered \
  transport_send_accepted worker_crashed_before_mark_sent command_reconciled \
  result_received ingress_crashed_before_commit result_reconciled; do
  grep -q "timeline_event ${event}" "${FD_B}" || {
    echo "timeline event ${event} is not produced by the harness" >&2
    exit 1
  }
done

relay_a_stop_phase="$(sed -n '/^phase_relay_a_stop() {/,/^}/p' "${FD_A}")"
for token in \
  'G6_DB_PORT=15432 G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql' \
  'WITH cut AS MATERIALIZED (SELECT clock_timestamp() AS at)' \
  'connection_owner_fencing AS fencing CROSS JOIN cut' \
  'AND fencing.lease_until>cut.at' \
  'relay-fault-cut.json' \
  'relay-a-only-readiness.json' \
  'relay-b-disabled.json' \
  'relay_a_only_topology_matches' \
  'trap relay_a_stop_restore EXIT' \
  'relay_a_only_topology_restore' \
  'jq -er' \
  "'.cut_at'" \
  'mv -f -- "${temporary}" "${stamp}"'; do
  grep -qF "${token}" <<<"${relay_a_stop_phase}" || {
    echo "the relay fault boundary must come from the promoted database clock: ${token}" >&2
    exit 1
  }
done
grep -qF 'g6rd_compose stop relay || return 1' <<<"${relay_a_stop_phase}" || {
  echo "relay-a stop must fail closed while retaining topology restoration" >&2
  exit 1
}
relay_fault_stamp_line="$(grep -nF 'mv -f -- "${temporary}" "${stamp}"' \
  <<<"${relay_a_stop_phase}" | cut -d: -f1)"
relay_fault_stop_line="$(grep -nF 'g6rd_compose stop relay' \
  <<<"${relay_a_stop_phase}" | cut -d: -f1)"
[[ -n "${relay_fault_stamp_line}" && -n "${relay_fault_stop_line}" \
  && "${relay_fault_stamp_line}" -lt "${relay_fault_stop_line}" ]] || {
  echo "the relay fault clock must be frozen before relay shutdown begins" >&2
  exit 1
}
g6rd_cleanup_phase="$(sed -n '/^g6rd_cleanup() {/,/^}/p' "${LIB}")"
for token in \
  'local relay_topology_network="${COMPOSE_PROJECT}_relay-a-only"' \
  'ocservia.g6.role' \
  'docker network rm "${relay_topology_network}"' \
  'for network in agent-shared agent-isolated relay-a-only'; do
  grep -qF "${token}" <<<"${g6rd_cleanup_phase}" || {
    echo "bounded cleanup does not remove/check the scoped relay topology: ${token}" >&2
    exit 1
  }
done
for token in \
  'relay-a-only-readiness.json' \
  'relay-b-disabled.json' \
  'relay-b-started.json' \
  'topology_mode: "relay-a-only"' \
  'topology_network_name: relayTopologyReadiness.network_name' \
  'topology_network_internal: relayTopologyReadiness.network_internal' \
  'relayTopologyReadiness.agent_default_network_connected' \
  'relay_b_disabled_at: relayBDisabledAt' \
  'relay_b_started_at: relayBStartedAt'; do
  grep -qF "${token}" "${BUILDER}" || {
    echo "the evidence builder does not bind controlled relay topology: ${token}" >&2
    exit 1
  }
done
for token in \
  '"topology_mode"' \
  '"topology_network_name"' \
  '"topology_network_internal"' \
  '"topology_agent_default_network_connected"' \
  '"relay_b_disabled_at"' \
  '"relay_b_started_at"' \
  'traffic.relayBDisabledNs' \
  'traffic.relayBStartedNs'; do
  grep -qF "${token}" "${CONTRACT}" || {
    echo "the independent verifier does not bind controlled relay topology: ${token}" >&2
    exit 1
  }
done

relay_topology_failure_fixture="$(mktemp -d)"
(
  export COMPOSE_PROJECT=g6-rd-relay-fixture
  export RUN_ID=relay-fixture-fd-a
  export G6RD_ENVIRONMENT_ID=g6-relayfixture
  export G6RD_CANDIDATE_SHA=1234567890123456789012345678901234567890
  export G6RD_OUTBOX="${relay_topology_failure_fixture}/outbox"
  nodes_file="${G6RD_OUTBOX}/agents/nodes.tsv"
  mkdir -p "${G6RD_OUTBOX}/agents"
  printf 'g6-fd-a-01\t018fc001-0000-7000-8000-000000000001\t%s\n' \
    "$(printf 'a%.0s' {1..64})" >"${nodes_file}"
  eval "$(sed -n '/^relay_a_only_network() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^relay_a_only_agent_service() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^relay_a_only_agent_container() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^relay_a_only_relay_container() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^require_file() {/,/^}/p' "${FD_A}")"
  eval "${relay_ready_phase}"
  restore_log="${relay_topology_failure_fixture}/restore.log"
  relay_a_only_topology_restore() {
    printf '%s\n' restore >>"${restore_log}"
  }
  relay_a_only_topology_matches() { return 0; }
  relay_topology_docker() {
    if [[ " $* " == *' network connect --alias relay-a '* ]]; then
      return 42
    fi
    return 0
  }
  g6rd_psql() {
    if [[ "$*" == *'SELECT encode(connection_id'* ]]; then
      printf '%s\t%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 7
    else
      printf '%s\n' '2026-08-19T00:00:00.000001Z'
    fi
  }
  if phase_relay_rejoin_ready >/dev/null 2>&1; then
    echo "relay topology setup hid an intermediate Docker failure" >&2
    exit 1
  fi
  [[ "$(wc -l <"${restore_log}" | tr -d ' ')" == 2 ]] || {
    echo "relay topology setup failure did not run scoped restoration" >&2
    exit 1
  }
  [[ ! -e "${G6RD_OUTBOX}/relay-rejoin-ready/candidate-sha" ]] || {
    echo "failed relay topology setup published false readiness" >&2
    exit 1
  }
)
rm -rf -- "${relay_topology_failure_fixture}"

# Every workflow phase is a fresh process. Exercise the real dispatcher with
# only its persisted Agent rendezvous, so a phase-local inventory assumption
# cannot be hidden by variables exported from an earlier policy fixture.
relay_dispatch_fixture="$(mktemp -d)"
mkdir -p "${relay_dispatch_fixture}/bin" \
  "${relay_dispatch_fixture}/runner-temp/g6-readiness-relay-dispatch-fd-a/outbox/agents" \
  "${relay_dispatch_fixture}/runner-temp/g6-readiness-relay-dispatch-fd-a/secrets"
cat >"${relay_dispatch_fixture}/bin/timeout" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "--foreground" ]] && shift
[[ "${1:-}" == "--signal=TERM" ]] && shift
[[ "${1:-}" == "--kill-after=5s" ]] && shift
[[ "${1:-}" =~ ^[0-9]+s$ ]] && shift
exec "$@"
SHIM
cat >"${relay_dispatch_fixture}/bin/docker" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${G6RD_TEST_DOCKER_LOG:?}"
if [[ " $* " == *' SELECT encode(connection_id'* ]]; then
  printf '%s\t%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 7
  exit 0
fi
exit 42
SHIM
chmod +x "${relay_dispatch_fixture}/bin/timeout" \
  "${relay_dispatch_fixture}/bin/docker"
printf 'g6-fd-a-01\t018fc001-0000-7000-8000-000000000001\t%s\n' \
  "$(printf 'a%.0s' {1..64})" \
  >"${relay_dispatch_fixture}/runner-temp/g6-readiness-relay-dispatch-fd-a/outbox/agents/nodes.tsv"
printf '%s\n' fixture-owner-password \
  >"${relay_dispatch_fixture}/runner-temp/g6-readiness-relay-dispatch-fd-a/secrets/owner-password"
(
  unset NODES_FILE
  export PATH="${relay_dispatch_fixture}/bin:${PATH}"
  export RUNNER_TEMP="${relay_dispatch_fixture}/runner-temp"
  export RUN_ID=relay-dispatch-fd-a
  export FD_ID=fd-a
  export FD_ALIAS=fd-alpha
  export G6_AUTHORITY=engineering
  export G6RD_ENVIRONMENT_ID=g6-relaydispatch
  export G6RD_CANDIDATE_SHA=1234567890123456789012345678901234567890
  export G6RD_TEST_DOCKER_LOG="${relay_dispatch_fixture}/docker.log"
  status=0
  "${FD_A}" relay-rejoin-ready \
    >"${relay_dispatch_fixture}/stdout.log" \
    2>"${relay_dispatch_fixture}/stderr.log" || status=$?
  [[ "${status}" != 0 ]] || {
    echo "the relay readiness dispatcher hid a Docker topology failure" >&2
    exit 1
  }
  if grep -qF 'NODES_FILE: unbound variable' "${relay_dispatch_fixture}/stderr.log"; then
    echo "the relay readiness dispatcher depended on a non-persisted inventory variable" >&2
    exit 1
  fi
  grep -qF 'network create --internal' "${G6RD_TEST_DOCKER_LOG}" || {
    echo "the relay readiness dispatcher did not load its persisted Agent inventory" >&2
    exit 1
  }
  [[ ! -e "${RUNNER_TEMP}/g6-readiness-${RUN_ID}/outbox/relay-rejoin-ready/candidate-sha" ]] || {
    echo "the relay readiness dispatcher published readiness after topology failure" >&2
    exit 1
  }
)
rm -rf -- "${relay_dispatch_fixture}"

relay_readiness_gate_fixture="$(mktemp -d)"
for relay_readiness_stage in baseline stop retirement missing changed-live changed-expired start topology success; do
  (
    export COMPOSE_PROJECT=g6-rd-relay-readiness
    export RUN_ID="relay-readiness-${relay_readiness_stage}-fd-a"
    export G6RD_ENVIRONMENT_ID=g6-relayreadiness
    export G6RD_CANDIDATE_SHA=1234567890123456789012345678901234567890
    export G6RD_OUTBOX="${relay_readiness_gate_fixture}/${relay_readiness_stage}/outbox"
    export G6RD_STATE="${relay_readiness_gate_fixture}/${relay_readiness_stage}/state"
    mkdir -p "${G6RD_OUTBOX}/agents" "${G6RD_STATE}"
    node=018fc001-0000-7000-8000-000000000001
    prior=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    printf 'g6-fd-a-01\t%s\t%s\n' "${node}" "$(printf 'b%.0s' {1..64})" \
      >"${G6RD_OUTBOX}/agents/nodes.tsv"
    eval "$(sed -n '/^require_file() {/,/^}/p' "${FD_A}")"
    eval "$(sed -n '/^relay_prior_connection_retired() {/,/^}/p' "${FD_A}")"
    eval "${relay_ready_phase}"
    relay_a_only_network() { printf '%s_relay-a-only\n' "${COMPOSE_PROJECT}"; }
    relay_a_only_agent_service() { printf '%s\n' agent-fd-a-01; }
    relay_a_only_agent_container() { printf '%s\n' "${COMPOSE_PROJECT}-agent-fd-a-01-1"; }
    relay_a_only_relay_container() { printf '%s\n' "${COMPOSE_PROJECT}-relay-1"; }
    relay_a_only_topology_restore() {
      printf '%s\n' restore >>"${G6RD_STATE}/events"
      return 0
    }
    relay_topology_docker() {
      printf 'docker %s\n' "$*" >>"${G6RD_STATE}/events"
      return 0
    }
    relay_a_only_topology_matches() {
      printf '%s\n' topology >>"${G6RD_STATE}/events"
      if [[ "${relay_readiness_stage}" == topology \
        && "$(grep -c '^topology$' "${G6RD_STATE}/events")" -eq 2 ]]; then
        return 1
      fi
      return 0
    }
    g6rd_psql() {
      case "$*" in
      *'SELECT to_char(clock_timestamp()'*)
        printf '%s\n' '2026-08-19T00:00:00.000001Z'
        ;;
      *'SELECT encode(connection_id'*)
        printf '%s\n' baseline >>"${G6RD_STATE}/events"
        if [[ "${relay_readiness_stage}" == baseline ]]; then
          printf ''
        else
          printf '%s\t%s\n' "${prior}" 7
        fi
        ;;
      *'SELECT CASE'*)
        if [[ "${relay_readiness_stage}" == retirement ]]; then
          printf '%s\n' live-or-invalid
        elif [[ "${relay_readiness_stage}" == missing ]]; then
          printf ''
        elif [[ "${relay_readiness_stage}" == changed-live ]]; then
          printf '%s\n' live-or-invalid
        elif [[ "${relay_readiness_stage}" == changed-expired ]]; then
          printf '%s\n' changed-expired
        else
          printf '%s\n' expired
        fi
        ;;
      *) return 1 ;;
      esac
    }
    g6rd_agent_compose() {
      printf 'agent-compose %s\n' "$*" >>"${G6RD_STATE}/events"
      if [[ "$*" == 'stop agent-fd-a-01' && "${relay_readiness_stage}" == stop ]]; then
        return 41
      fi
      if [[ "$*" == 'start agent-fd-a-01' && "${relay_readiness_stage}" == start ]]; then
        return 42
      fi
      return 0
    }
    g6rd_wait_until_deadline() {
      printf 'retirement-wait %s %s %s\n' "$1" "$2" "$3" >>"${G6RD_STATE}/events"
      shift 3
      "$@"
    }
    status=0
    phase_relay_rejoin_ready >"${G6RD_STATE}/stdout" \
      2>"${G6RD_STATE}/stderr" || status=$?
    if [[ "${relay_readiness_stage}" == baseline ]]; then
      if grep -q '^docker network create' "${G6RD_STATE}/events"; then
        echo "relay readiness rewired before freezing a live baseline term" >&2
        exit 1
      fi
    else
      grep -qxF 'agent-compose stop agent-fd-a-01' "${G6RD_STATE}/events" || {
        echo "relay readiness did not stop only the selected Agent" >&2
        exit 1
      }
    fi
    if [[ "${relay_readiness_stage}" == changed-expired \
      || "${relay_readiness_stage}" == start \
      || "${relay_readiness_stage}" == topology \
      || "${relay_readiness_stage}" == success ]]; then
      grep -qxF 'agent-compose start agent-fd-a-01' "${G6RD_STATE}/events" || {
        echo "relay readiness did not start only the selected Agent after retirement" >&2
        exit 1
      }
    elif grep -qxF 'agent-compose start agent-fd-a-01' "${G6RD_STATE}/events"; then
      echo "relay readiness started the selected Agent before proving retirement" >&2
      exit 1
    fi
    if [[ "${relay_readiness_stage}" == success \
      || "${relay_readiness_stage}" == changed-expired ]]; then
      [[ "${status}" == 0 ]] || {
        echo "relay readiness rejected a retired prior connection" >&2
        exit 1
      }
      [[ "$(<"${G6RD_OUTBOX}/relay-rejoin-ready/prior-connection-id")" == "${prior}" \
        && "$(<"${G6RD_OUTBOX}/relay-rejoin-ready/prior-owner-epoch")" == 7 \
        && "$(<"${G6RD_OUTBOX}/relay-rejoin-ready/candidate-sha")" == "${G6RD_CANDIDATE_SHA}" ]] || {
        echo "relay readiness did not publish its successful causal boundary" >&2
        exit 1
      }
      [[ "$(grep -c '^topology$' "${G6RD_STATE}/events")" -eq 2 ]] || {
        echo "relay readiness did not revalidate topology after the stop/start" >&2
        exit 1
      }
      restore_line="$(grep -nF restore "${G6RD_STATE}/events" | head -1 | cut -d: -f1)"
      baseline_line="$(grep -nF baseline "${G6RD_STATE}/events" | head -1 | cut -d: -f1)"
      rewire_line="$(grep -nF 'docker network create --internal' \
        "${G6RD_STATE}/events" | head -1 | cut -d: -f1)"
      topology_line="$(grep -nF topology "${G6RD_STATE}/events" | head -1 | cut -d: -f1)"
      [[ "${restore_line}" -lt "${baseline_line}" \
        && "${baseline_line}" -lt "${rewire_line}" \
        && "${rewire_line}" -lt "${topology_line}" ]] || {
        echo "relay readiness did not freeze its baseline before the network rewire" >&2
        exit 1
      }
    else
      [[ "${status}" != 0 ]] || {
        echo "relay ${relay_readiness_stage} failure was hidden" >&2
        exit 1
      }
      [[ ! -e "${G6RD_OUTBOX}/relay-rejoin-ready/candidate-sha" ]] || {
        echo "relay ${relay_readiness_stage} failure published false readiness" >&2
        exit 1
      }
    fi
  )
done
rm -rf -- "${relay_readiness_gate_fixture}"

relay_stop_failure_fixture="$(mktemp -d)"
(
  export COMPOSE_PROJECT=g6-rd-relay-fixture
  export RUN_ID=relay-fixture-fd-a
  export G6RD_ENVIRONMENT_ID=g6-relayfixture
  export G6RD_CANDIDATE_SHA=1234567890123456789012345678901234567890
  export G6RD_OUTBOX="${relay_stop_failure_fixture}/outbox"
  pre_fault="${relay_stop_failure_fixture}/pre-fault"
  mkdir -p "${G6RD_OUTBOX}" "${pre_fault}"
  node=018fc001-0000-7000-8000-000000000001
  owner=018fc001-0000-7000-8000-000000000002
  connection=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" >"${pre_fault}/candidate-sha"
  printf '%s\n' "${node}" >"${pre_fault}/node-id"
  printf '%s\n' '2026-08-19T00:00:00.000001Z' >"${pre_fault}/observed-at"
  jq -cn --arg node "${node}" --arg owner "${owner}" \
    --arg connection "${connection}" '{
      mode:"node_connection",expected_path:"relay",all_matched:true,
      observations:[{node_id:$node,owner_instance_id:$owner,
        owner_incarnation:1,connection_id:$connection,owner_epoch:1,
        owner_lease_until:"2026-08-19T00:00:10.000000Z",path:"relay",
        path_detail:"iroh/relay-a"}]
    }' >"${pre_fault}/relay-a-observation.json"
  jq -cn --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" --arg node "${node}" \
    --arg network "${COMPOSE_PROJECT}_relay-a-only" '{
      schema_version:"ocservia.g6-relay-topology.v1",
      environment_id:$environment,candidate_sha:$candidate,node_id:$node,
      agent_service:"agent-fd-a-01",network_name:$network,
      network_internal:true,agent_default_network_connected:false,
      relay_alias:"relay-a",topology_ready_at:"2026-08-19T00:00:00.000000Z"
    }' >"${pre_fault}/relay-a-only-readiness.json"
  jq -cn --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" --arg node "${node}" '{
      schema_version:"ocservia.g6-relay-state.v1",
      environment_id:$environment,candidate_sha:$candidate,node_id:$node,
      relay:"relay-b",state:"stopped",disabled_at:"2026-08-19T00:00:00.000000Z"
    }' >"${pre_fault}/relay-b-disabled.json"
  eval "$(sed -n '/^require_file() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^relay_a_only_network() {/,/^}/p' "${FD_A}")"
  eval "$(sed -n '/^relay_a_only_agent_service() {/,/^}/p' "${FD_A}")"
  eval "${relay_a_stop_phase}"
  restore_log="${relay_stop_failure_fixture}/restore.log"
  compose_log="${relay_stop_failure_fixture}/compose.log"
  relay_a_only_topology_matches() { return 0; }
  relay_a_only_topology_restore() {
    printf '%s\n' restore >>"${restore_log}"
  }
  g6rd_psql() {
    jq -cn --arg node "${node//-/}" --arg owner "${owner}" \
      --arg connection "${connection}" '{
        cut_at:"2026-08-19T00:00:01.000000Z",node_id:$node,
        owner_instance:$owner,owner_incarnation:"1",connection_id:$connection,
        owner_epoch:1,authority_lease_until:"2026-08-19T00:00:10.000000Z"
      }'
  }
  g6rd_compose() {
    printf '%s\n' "$*" >>"${compose_log}"
    [[ "${1:-} ${2:-}" != 'stop relay' ]] || return 17
  }
  if phase_relay_a_stop "${pre_fault}" >/dev/null 2>&1; then
    echo "relay-a stop hid a Compose shutdown failure" >&2
    exit 1
  fi
  [[ "$(wc -l <"${restore_log}" | tr -d ' ')" == 1 ]] || {
    echo "relay-a stop failure did not restore the selected Agent topology" >&2
    exit 1
  }
  [[ "$(grep -c '^stop relay$' "${compose_log}")" == 1 ]] || {
    echo "relay-a stop fixture failed before exercising the Compose shutdown" >&2
    exit 1
  }
)
rm -rf -- "${relay_stop_failure_fixture}"

# Timeline ordering must retain sub-second producer boundaries. In particular,
# a reconnect completion captured later in the same second must not be floored
# before the durable owner registration it closes.
timeline_precision_fixture="$(mktemp -d)"
(
  # shellcheck source=scripts/g6-readiness-lib.sh
  source "${LIB}"
  export G6RD_STATE="${timeline_precision_fixture}/state"
  export G6RD_OUTBOX="${timeline_precision_fixture}/outbox"
  export G6RD_ENVIRONMENT_ID=g6-abcd1234
  export G6RD_CANDIDATE_SHA=2234567890123456789012345678901234567890
  mkdir -p "${G6RD_STATE}" "${G6RD_OUTBOX}"
  g6rd_now() { printf '%s\n' '2026-08-19T12:00:00Z'; }
  g6rd_timeline_init
  printf '%s\n' '2026-08-19T12:00:00.700000Z' >"${G6RD_STATE}/precise-at"
  g6rd_timeline_event precise_boundary "${G6RD_STATE}/precise-at"
  printf '%s\n' '2026-08-19T12:00:00.600000Z' >"${G6RD_STATE}/earlier-at"
  g6rd_timeline_event clamped_boundary "${G6RD_STATE}/earlier-at"
  jq -se '
    length == 2 and
    .[0].timestamp == "2026-08-19T12:00:00.700000000Z" and
    .[1].timestamp == "2026-08-19T12:00:00.700000001Z" and
    .[0].sequence == 1 and .[1].sequence == 2
  ' "${G6RD_OUTBOX}/timeline.jsonl" >/dev/null
)
rm -rf "${timeline_precision_fixture}"

# A connection fence is bound to the target Agent's authenticated Iroh
# endpoint, not to the controller endpoint the Agent dials. Both stale-owner
# probes therefore retain five local terms, while owner recovery itself must
# cover every managed session through the active transport endpoint.
owner_phase="$(sed -n '/^phase_scenario_owner() {/,/^}/p' "${FD_B}")"
owner_capture="$(sed -n '/^capture_live_owner_terms() {/,/^}/p' "${FD_B}")"
owner_expiry_values="$(sed -n '/^owner_expiry_values() {/,/^}/p' "${FD_B}")"
owner_expiry_wait="$(sed -n '/^owner_leases_lapsed() {/,/^}/p' "${FD_B}")"
owner_replaced="$(sed -n '/^owner_replaced() {/,/^}/p' "${FD_B}")"
owner_values="$(sed -n '/^owner_replacement_values() {/,/^}/p' "${FD_B}")"
owner_session_capture="$(sed -n '/^capture_owner_replacement_sessions() {/,/^}/p' "${FD_B}")"
owner_timeout_report="$(sed -n '/^report_owner_replacement_timeout() {/,/^}/p' "${FD_B}")"
reconnect_validator="$(sed -n '/^validate_reconnect_sessions() {/,/^}/p' "${FD_B}")"
reconnect_capture="$(sed -n '/^capture_reconnect_sessions() {/,/^}/p' "${FD_B}")"
managed_node_count_helper="$(sed -n '/^managed_node_count() {/,/^}/p' "${FD_B}")"
for precise_validator in "${owner_session_capture}" "${reconnect_validator}"; do
  if ! grep -qF '[0-9]{1,9}' <<<"${precise_validator}" \
    || ! grep -qF '000000000' <<<"${precise_validator}" \
    || grep -qF 'fromdateiso8601' <<<"${precise_validator}"; then
    echo "owner and reconnect session timestamps must use exact padded nanosecond keys" >&2
    exit 1
  fi
done
node_service_helper="$(sed -n '/^node_service() {/,/^}/p' "${FD_B}")"
grep -qF -- '-v id="${node_id}"' <<<"${node_service_helper}" || {
  echo "the owner scenario node-to-service lookup must bind its awk id value" >&2
  exit 1
}
grep -qF 'capture_live_owner_terms' <<<"${owner_phase}" || {
  echo "the owner scenario must freeze the live owner population before injection" >&2
  exit 1
}
for token in \
  'expected_count="$(managed_node_count)"' \
  '[[ "${#managed_nodes[@]}" == "${expected_count}" ]]' \
  'owner-all-terms.tsv' \
  'owner-a-terms.tsv' \
  'WITH cut AS MATERIALIZED (SELECT clock_timestamp() AS at)' \
  'WHERE lease_until>cut.at' \
  "encode(node_id,'hex') IN (\${sql_nodes})" \
  '[[ "${#seen_nodes[@]}" == "${#managed_nodes[@]}" ]]' \
  '((sample_count == 5))'; do
  grep -qF "${token}" <<<"${owner_capture}" || {
    echo "the owner snapshot does not enforce full coverage plus five local stale terms: ${token}" >&2
    exit 1
  }
done
if grep -qF 'g6rd_agent_compose restart' <<<"${owner_phase}"; then
  echo "owner recovery must reconnect the full fleet instead of only five Agents" >&2
  exit 1
fi
for token in \
  'all frozen owner leases expired' \
  'all managed owners registered higher epochs' \
  'all Agents connected through replacement owners' \
  'report_owner_replacement_timeout'; do
  grep -qF "${token}" <<<"${owner_phase}" || {
    echo "owner replacement is missing its lease-expiry, fleet-wide barrier: ${token}" >&2
    exit 1
  }
done
grep -qF 'G6RD_COMPOSE_TIMEOUT_SECONDS=15 g6rd_compose kill --signal KILL worker' \
  <<<"${owner_phase}" || {
  echo "the owner scenario must crash the worker under a scoped hard timeout" >&2
  exit 1
}
if grep -qF 'g6rd_compose stop worker' <<<"${owner_phase}"; then
  echo "the owner scenario must not gracefully release the frozen leases" >&2
  exit 1
fi
owner_worker_crash_line="$(grep -nF 'g6rd_compose kill --signal KILL worker' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_pause_line="$(grep -nF 'g6rd_timeline_event owner_a_paused' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_expiry_line="$(grep -nF 'all frozen owner leases expired' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_worker_start_line="$(grep -nF 'g6rd_compose up --detach worker' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_worker_ready_line="$(grep -nF 'replacement worker trust socket' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_transport_stop_line="$(grep -nF 'g6rd_compose stop transportd' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_transport_start_line="$(grep -nF 'g6rd_compose up --detach transportd' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
owner_epoch_wait_line="$(grep -nF 'all managed owners registered higher epochs' <<<"${owner_phase}" | head -1 | cut -d: -f1)"
[[ -n "${owner_worker_crash_line}" && -n "${owner_pause_line}" \
  && -n "${owner_expiry_line}" && -n "${owner_worker_start_line}" \
  && -n "${owner_worker_ready_line}" \
  && -n "${owner_transport_stop_line}" && -n "${owner_transport_start_line}" \
  && -n "${owner_epoch_wait_line}" \
  && "${owner_worker_crash_line}" -lt "${owner_pause_line}" \
  && "${owner_pause_line}" -lt "${owner_expiry_line}" \
  && "${owner_expiry_line}" -lt "${owner_worker_start_line}" \
  && "${owner_worker_start_line}" -lt "${owner_worker_ready_line}" \
  && "${owner_worker_ready_line}" -lt "${owner_transport_stop_line}" \
  && "${owner_transport_stop_line}" -lt "${owner_transport_start_line}" \
  && "${owner_transport_start_line}" -lt "${owner_epoch_wait_line}" ]] || {
  echo "all frozen leases must expire before the replacement worker and bounded fleet reconnect" >&2
  exit 1
}
for token in \
  'owner-all-terms.tsv' \
  'frozen_lease_us' \
  'current.owner_epoch=expected.old_epoch' \
  'current.connection_id=expected.old_connection_id' \
  'current.lease_until)*1000000)::bigint>=expected.frozen_lease_us' \
  'current.lease_until<=clock_timestamp()' \
  'clock_timestamp()' \
  '[[ "${expired}" == "${expected_count}" ]]'; do
  grep -qF "${token}" <<<"${owner_expiry_values}${owner_expiry_wait}" || {
    echo "the owner expiry barrier must use the frozen population and database clock: ${token}" >&2
    exit 1
  }
done
owner_expiry_fixture="$(mktemp -d)"
(
  G6RD_STATE="${owner_expiry_fixture}"
  NODES_FILE="${owner_expiry_fixture}/nodes.tsv"
  G6_AGENTS_A=1
  G6_AGENTS_B=1
  printf 'g6-fd-a-01\t00000000-0000-7000-8000-000000000001\tendpoint-a\n' >"${NODES_FILE}"
  printf 'g6-fd-b-01\t00000000-0000-7000-8000-000000000002\tendpoint-b\n' >>"${NODES_FILE}"
  printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tinstance-a\t1\tcccccccccccccccccccccccccccccccc\t1\t100\n' \
    >"${G6RD_STATE}/owner-all-terms.tsv"
  printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tinstance-b\t1\tdddddddddddddddddddddddddddddddd\t1\t200\n' \
    >>"${G6RD_STATE}/owner-all-terms.tsv"
  require_file() { [[ -s "${1:?path is required}" ]]; }
  eval "${managed_node_count_helper}"
  eval "${owner_expiry_values}"
  eval "${owner_expiry_wait}"
  psql_primary_probe() { printf '1\n'; }
  if owner_leases_lapsed; then
    echo "the owner expiry barrier accepted an incomplete frozen population" >&2
    exit 1
  fi
  psql_primary_probe() { printf '2\n'; }
  owner_leases_lapsed || {
    echo "the owner expiry barrier rejected the fully expired frozen population" >&2
    exit 1
  }
)
rm -rf -- "${owner_expiry_fixture}"
for token in \
  'owner-all-terms.tsv' \
  "current.owner_epoch>expected.old_epoch" \
  "current.connection_id<>expected.old_connection_id" \
  "current.lease_until>clock_timestamp()"; do
  grep -qF "${token}" <<<"${owner_replaced}${owner_values}" || {
    echo "owner replacement must advance every frozen node through a new live connection: ${token}" >&2
    exit 1
  }
done
grep -qF '[[ "${advanced}" == "${expected_count}" ]]' <<<"${owner_replaced}" || {
  echo "owner replacement must require every configured managed term to advance" >&2
  exit 1
}
for token in 'LEFT JOIN connection_owner_fencing' 'current.owner_epoch<=expected.old_epoch' \
  'current.lease_until<=clock_timestamp()'; do
  grep -qF "${token}" <<<"${owner_timeout_report}" || {
    echo "owner timeout diagnostics must identify every missing, stale, or expired replacement" >&2
    exit 1
  }
done
if grep -qF 'g6rd_enqueue_command' <<<"${owner_phase}"; then
  echo "enqueueing work must not stand in for an owner-session reconnect" >&2
  exit 1
fi
for token in \
  'owner-b-terms.tsv' \
  'owner-replacement-sessions.json' \
  'owner-b-acquired-at' \
  'expected_count="$(managed_node_count)"' \
  '[[ "$(wc -l <"${terms_tmp}" | tr -d '\''[:space:]'\'')" == "${expected_count}" ]]' \
  'g6rd_probe_node_connection any "${args[@]}"' \
  '([.observations[].node_id] | unique | length) == $expected_count' \
  '.owner_instance_id == $terms[$node_hex].instance' \
  '.owner_incarnation == $terms[$node_hex].incarnation' \
  '.connection_id == $terms[$node_hex].connection' \
  '.owner_epoch == $terms[$node_hex].epoch' \
  '((.connected_at | stamp_key) >= ($terms[$node_hex].registered_at | stamp_key))' \
  '((.connected_at | stamp_key) <= ($boundary | stamp_key))'; do
  grep -qF "${token}" <<<"${owner_session_capture}" || {
    echo "replacement-owner session capture is missing an exact full-population binding: ${token}" >&2
    exit 1
  }
done
owner_session_capture_line="$(grep -nF 'capture_owner_replacement_sessions' <<<"${owner_phase}" | cut -d: -f1)"
owner_acquired_event_line="$(grep -nF 'g6rd_timeline_event owner_b_acquired "${G6RD_STATE}/owner-b-acquired-at"' <<<"${owner_phase}" | cut -d: -f1)"
[[ -n "${owner_session_capture_line}" && -n "${owner_acquired_event_line}" \
  && "${owner_session_capture_line}" -lt "${owner_acquired_event_line}" ]] || {
  echo "owner takeover completion must freeze every replacement session before its timeline boundary" >&2
  exit 1
}
for token in \
  'reconnect-sessions.json' \
  'expected_count="$(managed_node_count)"' \
  '[[ "${#args[@]}" == "${expected_count}" ]]' \
  'g6rd_probe_node_connection any "${args[@]}"' \
  'validate_reconnect_sessions "${temporary}" "${bulk_disconnect_file}"' \
  'mv -f -- "${temporary}" "${output}"'; do
  grep -qF "${token}" <<<"${reconnect_capture}" || {
    echo "the reconnect storm must freeze a complete validated transport inventory: ${token}" >&2
    exit 1
  }
done
for token in \
  '.all_matched == true' \
  '.expected_path == "any"' \
  '([.observations[].node_id] | unique | length) == $expected_count' \
  '(.endpoint_id == $managed[.node_id])' \
  'test("^[1-9][0-9]*$")' \
  'index("ocserv.fencing.v2") != null' \
  'def stamp_key:' \
  '(.connected_at | stamp_key) > ($bulk_disconnect | stamp_key)' \
  '(.owner_lease_until | stamp_key) > (.last_seen | stamp_key)'; do
  grep -qF "${token}" <<<"${reconnect_validator}" || {
    echo "the reconnect inventory validator is missing a fail-closed binding: ${token}" >&2
    exit 1
  }
done
reconnect_wait_line="$(grep -nF 'all agents reconnected after the storm' <<<"${owner_phase}" | cut -d: -f1)"
reconnect_capture_line="$(grep -nF 'capture_reconnect_sessions "${bulk_disconnect_file}"' <<<"${owner_phase}" | cut -d: -f1)"
reconnect_complete_line="$(grep -nF 'g6rd_timeline_event reconnect_completed' <<<"${owner_phase}" | cut -d: -f1)"
[[ -n "${reconnect_wait_line}" && -n "${reconnect_capture_line}" \
  && -n "${reconnect_complete_line}" \
  && "${reconnect_wait_line}" -lt "${reconnect_capture_line}" \
  && "${reconnect_capture_line}" -lt "${reconnect_complete_line}" ]] || {
  echo "the causal reconnect inventory must be persisted before reconnect completion" >&2
  exit 1
}
bulk_stamp_line="$(grep -nF 'g6rd_atomic_now "${bulk_disconnect_file}"' <<<"${owner_phase}" | cut -d: -f1)"
bulk_stop_line="$(grep -nF 'g6rd_compose stop transportd' <<<"${owner_phase}" | tail -1 | cut -d: -f1)"
bulk_event_line="$(grep -nF 'g6rd_timeline_event bulk_disconnect_injected "${bulk_disconnect_file}"' <<<"${owner_phase}" | cut -d: -f1)"
[[ -n "${bulk_stamp_line}" && -n "${bulk_stop_line}" && -n "${bulk_event_line}" \
  && "${bulk_stamp_line}" -lt "${bulk_stop_line}" \
  && "${bulk_stop_line}" -lt "${bulk_event_line}" ]] || {
  echo "the reconnect fault clock must be frozen before transport shutdown begins" >&2
  exit 1
}
grep -qF 'capture_database_clock >"${G6RD_STATE}/reconnect-completed-at"' \
  <<<"${owner_phase}" || {
  echo "reconnect completion must preserve the database clock precision after capture" >&2
  exit 1
}
grep -qF 'target_endpoint="$(awk -F' <<<"${owner_phase}" || {
  echo "the owner scenario must resolve its target Agent endpoint from inventory" >&2
  exit 1
}
grep -qF "'\$2 == id {print \$3; exit}'" <<<"${owner_phase}" || {
  echo "the owner scenario endpoint lookup must bind the selected node id" >&2
  exit 1
}
if [[ "$(grep -cF -- '--endpoint-id "${target_endpoint}"' <<<"${owner_phase}")" != 2 ]] \
  || grep -qF -- '--endpoint-id "$(<"${G6RD_STATE}/controller-endpoint-id")"' \
    <<<"${owner_phase}"; then
  echo "both stale-owner probes must use the target Agent endpoint" >&2
  exit 1
fi
grep -qF '[[ "${target_endpoint}" =~ ^[0-9a-f]{64}$ ]]' <<<"${owner_phase}" || {
  echo "the owner scenario must validate the selected Agent endpoint" >&2
  exit 1
}

reconnect_fixture="$(mktemp -d)"
printf 'g6-fd-a-01\t00000000-0000-7000-8000-000000000001\t%s\n' \
  "$(printf 'a%.0s' {1..64})" >"${reconnect_fixture}/nodes.tsv"
printf 'g6-fd-b-01\t00000000-0000-7000-8000-000000000002\t%s\n' \
  "$(printf 'b%.0s' {1..64})" >>"${reconnect_fixture}/nodes.tsv"
printf '2026-08-19T10:00:01.123456788Z\n' >"${reconnect_fixture}/bulk-at"
jq -n \
  --arg endpoint_a "$(printf 'a%.0s' {1..64})" \
  --arg endpoint_b "$(printf 'b%.0s' {1..64})" '
  {
    expected_path: "any",
    all_matched: true,
    observations: [
      {
        node_id: "00000000-0000-7000-8000-000000000001",
        found: true,
        endpoint_id: $endpoint_a,
        agent_instance_id: "11111111111111111111111111111111",
        path: "relay",
        matched: true,
        connected_at: "2026-08-19T10:00:01.123456789Z",
        last_seen: "2026-08-19T10:00:03.250Z",
        session_expires_at: "2026-08-19T10:05:00.500Z",
        owner_fence_id: "22222222222222222222222222222222",
        owner_instance_id: "00000000-0000-7000-8000-000000000010",
        owner_incarnation: "10",
        connection_id: "33333333333333333333333333333333",
        owner_lease_until: "2026-08-19T10:00:30.750Z",
        owner_epoch: 4,
        authorization_revision: 2,
        negotiated_capabilities: ["ocserv.fencing.v2"]
      },
      {
        node_id: "00000000-0000-7000-8000-000000000002",
        found: true,
        endpoint_id: $endpoint_b,
        agent_instance_id: "44444444444444444444444444444444",
        path: "direct",
        matched: true,
        connected_at: "2026-08-19T10:00:02.125Z",
        last_seen: "2026-08-19T10:00:04.250Z",
        session_expires_at: "2026-08-19T10:05:00.500Z",
        owner_fence_id: "55555555555555555555555555555555",
        owner_instance_id: "00000000-0000-7000-8000-000000000011",
        owner_incarnation: "11",
        connection_id: "66666666666666666666666666666666",
        owner_lease_until: "2026-08-19T10:00:30.750Z",
        owner_epoch: 5,
        authorization_revision: 3,
        negotiated_capabilities: ["ocserv.fencing.v2", "synthetic.noop"]
      }
    ]
  }' >"${reconnect_fixture}/sessions.json"
(
  NODES_FILE="${reconnect_fixture}/nodes.tsv"
  G6_AGENTS_A=1
  G6_AGENTS_B=1
  node_ids() { cut -f2 "${NODES_FILE}"; }
  require_file() { [[ -s "${1:?path is required}" ]]; }
  eval "${managed_node_count_helper}"
  eval "${reconnect_validator}"
  validate_reconnect_sessions "${reconnect_fixture}/sessions.json" \
    "${reconnect_fixture}/bulk-at" || {
    echo "the valid causal reconnect inventory fixture was rejected" >&2
    exit 1
  }
  jq '.observations[1].node_id = .observations[0].node_id' \
    "${reconnect_fixture}/sessions.json" >"${reconnect_fixture}/duplicate.json"
  jq '.observations[0].connected_at = "2026-08-19T10:00:00Z"' \
    "${reconnect_fixture}/sessions.json" >"${reconnect_fixture}/pre-bulk.json"
  jq '.observations[0].connected_at = "2026-08-19T10:00:01.123456787Z"' \
    "${reconnect_fixture}/sessions.json" >"${reconnect_fixture}/same-second-pre-bulk.json"
  jq 'del(.observations[0].connection_id)' \
    "${reconnect_fixture}/sessions.json" >"${reconnect_fixture}/missing-fence.json"
  jq '.observations[0].endpoint_id = .observations[1].endpoint_id' \
    "${reconnect_fixture}/sessions.json" >"${reconnect_fixture}/wrong-endpoint.json"
  for invalid in duplicate pre-bulk same-second-pre-bulk missing-fence wrong-endpoint; do
    if validate_reconnect_sessions "${reconnect_fixture}/${invalid}.json" \
      "${reconnect_fixture}/bulk-at" >/dev/null 2>&1; then
      echo "the invalid ${invalid} reconnect inventory fixture was accepted" >&2
      exit 1
    fi
  done
)
rm -rf -- "${reconnect_fixture}"

# Every outbox crash point runs on an Agent owned by fd-b. The peer rows are
# deliberately first in the real inventory, so ordinal selection must filter
# by failure domain before choosing a target.
local_node_helper="$(sed -n '/^local_node_id() {/,/^}/p' "${FD_B}")"
crash1_phase="$(sed -n '/^phase_outbox_claim_before_send() {/,/^}/p' "${FD_B}")"
crash2_phase="$(sed -n '/^phase_outbox_send_before_mark() {/,/^}/p' "${FD_B}")"
# shellcheck disable=SC2034  # referenced through phase_variable below
crash3_phase="$(sed -n '/^phase_outbox_result_before_commit() {/,/^}/p' "${FD_B}")"
for ordinal in 1 2 3; do
  phase_variable="crash${ordinal}_phase"
  grep -qF "node=\"\$(local_node_id ${ordinal})\"" <<<"${!phase_variable}" || {
    echo "outbox crash window ${ordinal} must select local FD-B node ${ordinal}" >&2
    exit 1
  }
  if grep -qE 'node_ids[[:space:]]*\|[[:space:]]*head' <<<"${!phase_variable}"; then
    echo "outbox crash window ${ordinal} must not select from the peer-first global inventory" >&2
    exit 1
  fi
done
crash_scope_test="$(mktemp -d)"
cat >"${crash_scope_test}/nodes.tsv" <<'EOF'
g6-fd-a-01	018f2f10-7abc-7def-8abc-0123456789a1	endpoint-a1
g6-fd-b-01	018f2f10-7abc-7def-8abc-0123456789b1	endpoint-b1
g6-fd-a-02	018f2f10-7abc-7def-8abc-0123456789a2	endpoint-a2
g6-fd-b-02	018f2f10-7abc-7def-8abc-0123456789b2	endpoint-b2
g6-fd-b-03	018f2f10-7abc-7def-8abc-0123456789b3	endpoint-b3
EOF
printf '%s\n' "${local_node_helper}" >"${crash_scope_test}/local-node-helper.sh"
(
  export FD_ID=fd-b NODES_FILE="${crash_scope_test}/nodes.tsv"
  # shellcheck source=/dev/null
  source "${crash_scope_test}/local-node-helper.sh"
  selected=()
  for ordinal in 1 2 3; do
    selected+=("$(local_node_id "${ordinal}")")
  done
  [[ "${selected[*]}" == \
    '018f2f10-7abc-7def-8abc-0123456789b1 018f2f10-7abc-7def-8abc-0123456789b2 018f2f10-7abc-7def-8abc-0123456789b3' ]] || {
    echo "local crash-node selection crossed the failure-domain boundary" >&2
    exit 1
  }
  if local_node_id 4 >/dev/null 2>&1; then
    echo "local crash-node selection accepted a missing ordinal" >&2
    exit 1
  fi
)
rm -rf -- "${crash_scope_test}"

# Worker and transportd form one replacement unit. Each exact worker kill must
# be followed by a transportd kill, ordered startup, and a full fenced-session
# inventory before reconciliation continues.
replacement_helper="$(sed -n '/^restart_worker_transport_unit() {/,/^}/p' "${FD_B}")"
replacement_worker_line="$(grep -nF 'g6rd_compose up --detach worker' <<<"${replacement_helper}" | cut -d: -f1)"
replacement_trust_line="$(grep -nF 'replacement worker trust socket' <<<"${replacement_helper}" | cut -d: -f1)"
replacement_transport_line="$(grep -nF 'g6rd_compose up --detach transportd' <<<"${replacement_helper}" | cut -d: -f1)"
replacement_socket_line="$(grep -nF 'replacement transportd socket' <<<"${replacement_helper}" | cut -d: -f1)"
replacement_sessions_line="$(grep -nF 'all_nodes_connected' <<<"${replacement_helper}" | cut -d: -f1)"
[[ -n "${replacement_worker_line}" && -n "${replacement_trust_line}" \
  && -n "${replacement_transport_line}" && -n "${replacement_socket_line}" \
  && -n "${replacement_sessions_line}" \
  && "${replacement_worker_line}" -lt "${replacement_trust_line}" \
  && "${replacement_trust_line}" -lt "${replacement_transport_line}" \
  && "${replacement_transport_line}" -lt "${replacement_socket_line}" \
  && "${replacement_socket_line}" -lt "${replacement_sessions_line}" ]] || {
  echo "the worker+transportd replacement unit startup or fenced-session barrier is misordered" >&2
  exit 1
}
grep -qF 'g6rd_wait_until_deadline 180 5' <<<"${replacement_helper}" || {
  echo "replacement recovery must retain the complete 180-second reconnect window" >&2
  exit 1
}
for phase_variable in crash1_phase crash2_phase crash3_phase; do
  phase="${!phase_variable}"
  worker_kill="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-worker-1"' <<<"${phase}" | cut -d: -f1)"
  transport_kill="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-transportd-1"' <<<"${phase}" | cut -d: -f1)"
  replacement="$(grep -nF 'restart_worker_transport_unit' <<<"${phase}" | cut -d: -f1)"
  [[ -n "${worker_kill}" && -n "${transport_kill}" && -n "${replacement}" \
    && "${worker_kill}" -lt "${transport_kill}" \
    && "${transport_kill}" -lt "${replacement}" ]] || {
    echo "${phase_variable} must kill the worker then transportd before replacing the unit" >&2
    exit 1
  }
  if grep -qF 'g6rd_compose up --detach worker' <<<"${phase}"; then
    echo "${phase_variable} must not restart a worker outside its transportd replacement unit" >&2
    exit 1
  fi
done

# Both pre-Send and post-Send fault points are exact Worker hooks. The first
# signals only after the committed Claim has been extended for this command;
# the second signals only after SendCommand returns and before MarkSent starts.
# A separate exact Agent signal closes the gap between transport write and the
# Agent's decode/fence-verification boundary before the Worker is killed.
agent_command_handler="$(sed -n '/^async fn handle_command_stream(/,/^fn synthetic_barrier_target(/p' "${AGENT_MAIN}")"
agent_fence_line="$(grep -nF 'verify_fenced_operation(' <<<"${agent_command_handler}" | tail -1 | cut -d: -f1)"
agent_receipt_line="$(grep -nF 'wait_for_synthetic_barrier(' <<<"${agent_command_handler}" | cut -d: -f1)"
agent_journal_line="$(grep -nF 'session.command_executor.deliver' <<<"${agent_command_handler}" | cut -d: -f1)"
[[ -n "${agent_fence_line}" && -n "${agent_receipt_line}" \
  && -n "${agent_journal_line}" \
  && "${agent_fence_line}" -lt "${agent_receipt_line}" \
  && "${agent_receipt_line}" -lt "${agent_journal_line}" ]] || {
  echo "the exact Agent receipt must follow fence verification and precede journal delivery" >&2
  exit 1
}
grep -qF 'envelope.command_id.as_slice()' <<<"${agent_command_handler}" || {
  echo "the Agent receipt barrier must bind the decoded command UUID" >&2
  exit 1
}
claim_helper="$(sed -n '/^outbox_row_claimed() {/,/^}/p' "${FD_B}")"
journal_query_helper="$(sed -n '/^journal_query() {/,/^}/p' "${FD_B}")"
journal_has_helper="$(sed -n '/^journal_has_command() {/,/^}/p' "${FD_B}")"
journal_ready_helper="$(sed -n '/^journal_command_ready() {/,/^}/p' "${FD_B}")"
journal_wait_helper="$(sed -n '/^wait_for_journal_command() {/,/^}/p' "${FD_B}")"
journal_state_helper="$(sed -n '/^journal_result_state() {/,/^}/p' "${FD_B}")"
agent_receipt_helper="$(sed -n '/^agent_synthetic_receipt_reached() {/,/^}/p' "${FD_B}")"
grep -qF 'g6rd_agent_journal_query "${service}" "${sql}"' \
  <<<"${journal_query_helper}" || {
  echo "fd-b Agent journal queries must use the bounded owner observer" >&2
  exit 1
}
if grep -qF 'sqlite3 -readonly /run/ocservia-agent/journal/agent.db' \
  "${FD_A}" "${FD_B}"; then
  echo "failure-domain scripts must not bypass the bounded owner journal observer" >&2
  exit 1
fi
if grep -qF '2>/dev/null' <<<"${journal_has_helper}${journal_state_helper}"; then
  echo "Agent journal probes must not hide sqlite or Compose failures" >&2
  exit 1
fi
for helper in "${journal_has_helper}" "${journal_state_helper}"; do
  grep -qF 'return 2' <<<"${helper}" || {
    echo "Agent journal probe errors must remain distinct from not-ready state" >&2
    exit 1
  }
done
grep -qF '0) return 1' <<<"${journal_ready_helper}" || {
  echo "a missing Agent journal row must remain a retryable not-ready result" >&2
  exit 1
}
grep -qF 'if ((status != 1))' <<<"${journal_wait_helper}" || {
  echo "the strict Agent journal wait must abort on operational probe errors" >&2
  exit 1
}
grep -qF 'G6RD_JOURNAL_QUERY_TIMEOUT_SECONDS="${query_timeout}"' \
  <<<"${journal_wait_helper}" || {
  echo "the strict Agent journal wait must bound each observer call by its remaining deadline" >&2
  exit 1
}
grep -qF 'return "${status}"' <<<"${journal_wait_helper}" || {
  echo "the strict Agent journal wait must preserve operational probe errors" >&2
  exit 1
}
(
  eval "${journal_has_helper}"
  eval "${journal_ready_helper}"
  eval "${journal_wait_helper}"
  expect_status() {
    local expected="${1:?expected status}" status=0
    shift
    "$@" >/dev/null 2>&1 || status=$?
    [[ "${status}" == "${expected}" ]] || {
      echo "expected status ${expected}, got ${status}: $*" >&2
      exit 1
    }
  }
  journal_query() {
    case "${JOURNAL_STUB:-one}" in
      error) return 9 ;;
      zero) printf '0\n' ;;
      one) printf '1\n' ;;
      duplicate) printf '2\n' ;;
      garbage) printf 'not-a-count\n' ;;
      *) return 8 ;;
    esac
  }
  JOURNAL_STUB=error expect_status 2 journal_command_ready service command
  JOURNAL_STUB=zero expect_status 1 journal_command_ready service command
  JOURNAL_STUB=one expect_status 0 journal_command_ready service command
  JOURNAL_STUB=duplicate expect_status 2 journal_command_ready service command
  JOURNAL_STUB=garbage expect_status 2 journal_command_ready service command
  journal_command_ready() { return 2; }
  started_at="${SECONDS}"
  expect_status 2 wait_for_journal_command 5 1 journal-probe service command
  ((SECONDS - started_at < 2)) || {
    echo "the strict Agent journal wait retried an operational probe failure" >&2
    exit 1
  }
)
fd_a_evidence="$(sed -n '/^phase_evidence() {/,/^}/p' "${FD_A}")"
grep -qF 'g6rd_agent_journal_query "${service}"' \
  <<<"${fd_a_evidence}" || {
  echo "fd-a final evidence must use the bounded owner journal observer" >&2
  exit 1
}
grep -qF '[[ -f "${receipt}" && ! -L "${receipt}" && -s "${receipt}" ]]' \
  <<<"${agent_receipt_helper}" || {
  echo "the Agent receipt predicate must reject missing, symlinked, or empty signals" >&2
  exit 1
}
grep -qF '== "${command_id}"' <<<"${agent_receipt_helper}" || {
  echo "the Agent receipt predicate must bind the exact command UUID" >&2
  exit 1
}
for journal_helper in "${journal_has_helper}" "${journal_state_helper}"; do
  grep -qF 'lower(hex(command_id))' <<<"${journal_helper}" || {
    echo "Agent journal UUID lookup must normalize SQLite hex output to PostgreSQL UUID case" >&2
    exit 1
  }
done
for predicate in \
  'outbox.published_at IS NULL' \
  'outbox.locked_by=lease.worker_id' \
  'outbox.locked_until>clock_timestamp()' \
  'lease.leased_until>clock_timestamp()' \
  "attempt.state='sending' AND attempt.finished_at IS NULL"; do
  grep -qF "${predicate}" <<<"${claim_helper}" || {
    echo "the outbox claim barrier is missing strict predicate: ${predicate}" >&2
    exit 1
  }
done
if grep -qE '^send_before_mark_|^SEND_BEFORE_MARK|start_send_before_mark|stop_send_before_mark|pg_advisory.*MarkSent' "${FD_B}"; then
  echo "the send-before-MarkSent phase must not use a process-wide advisory inference" >&2
  exit 1
fi
pre_send_arm_helper="$(sed -n '/^pre_send_barrier_arm() {/,/^}/p' "${FD_B}")"
pre_send_reached_helper="$(sed -n '/^pre_send_barrier_reached() {/,/^}/p' "${FD_B}")"
pre_send_release_helper="$(sed -n '/^pre_send_barrier_release() {/,/^}/p' "${FD_B}")"
post_send_arm_helper="$(sed -n '/^post_send_barrier_arm() {/,/^}/p' "${FD_B}")"
post_send_reached_helper="$(sed -n '/^post_send_barrier_reached() {/,/^}/p' "${FD_B}")"
post_send_release_helper="$(sed -n '/^post_send_barrier_release() {/,/^}/p' "${FD_B}")"
post_send_attempt_helper="$(sed -n '/^exact_post_send_attempt_id() {/,/^}/p' "${FD_B}")"
post_send_proof_helper="$(sed -n '/^exact_post_send_attempt_proof() {/,/^}/p' "${FD_B}")"
post_send_report_helper="$(sed -n '/^report_exact_post_send_attempt_failure() {/,/^}/p' "${FD_B}")"
grep -qF 'G6RD_PRE_SEND_BARRIER="${G6RD_RESULT_BARRIER}/pre-send"' "${LIB}" || {
  echo "the Worker pre-send barrier must remain inside the scoped result-barrier bind" >&2
  exit 1
}
grep -qF 'chmod 0777 "${G6RD_RESULT_BARRIER}" "${G6RD_PRE_SEND_BARRIER}"' "${LIB}" || {
  echo "the unprivileged Worker must be able to atomically signal its scoped barrier" >&2
  exit 1
}
for helper in "${pre_send_arm_helper}" "${pre_send_reached_helper}" \
  "${pre_send_release_helper}" "${post_send_arm_helper}" \
  "${post_send_reached_helper}" "${post_send_release_helper}"; do
  grep -qF '"${command_id}"' <<<"${helper}" || {
    echo "each Worker crash barrier must bind file operations to the exact command id" >&2
    exit 1
  }
done
for predicate in \
  'attempt.attempt_number=outbox.attempts' \
  "attempt.state='sending' AND attempt.finished_at IS NULL" \
  'command.state,operation.state,outbox.published_at IS NULL' \
  'outbox.locked_by=attempt.worker_id' \
  'COALESCE(outbox.locked_until>clock_timestamp(),false)' \
  'COALESCE(lease.leased_until>clock_timestamp(),false)' \
  "WHERE command.id='\${command_id}' AND attempt.id='\${attempt_id}'" \
  'NOT EXISTS (' \
  'FROM agent_command_results AS result'; do
  grep -qF "${predicate}" <<<"${post_send_attempt_helper}${post_send_proof_helper}" || {
    echo "the post-Send proof is missing exact attempt predicate: ${predicate}" >&2
    exit 1
  }
done
for diagnostic in \
  'outbox.locked_until,outbox.attempts,attempt.attempt_number' \
  'lease.worker_id,lease.leased_until' \
  "SELECT 'attempt'" \
  "SELECT 'result'"; do
  grep -qF "${diagnostic}" <<<"${post_send_report_helper}" || {
    echo "the exact post-Send failure matrix is missing: ${diagnostic}" >&2
    exit 1
  }
done
if grep -qF 'ORDER BY attempt_number LIMIT 1' <<<"${crash2_phase}"; then
  echo "send-before-MarkSent must not infer the exact dispatch from the first attempt" >&2
  exit 1
fi
for name in post-send-arm post-send-received post-send-release; do
  grep -qF "${name}" "${FD_B}" || {
    echo "the exact post-Send Worker hook is missing ${name}" >&2
    exit 1
  }
done

for token in \
  'docker pause "${COMPOSE_PROJECT}-worker-1"' \
  'pre_send_barrier_arm "${command_id}"' \
  'post_send_barrier_arm "${command_id}"' \
  'docker unpause "${COMPOSE_PROJECT}-worker-1"' \
  'pre_send_barrier_reached "${command_id}"' \
  'send-before-MarkSent strict outbox claim' \
  'pre_send_barrier_release "${command_id}"' \
  'exact post-send Worker barrier' \
  'post_send_barrier_reached "${command_id}"' \
  'attempt_id="$(exact_post_send_attempt_id "${command_id}")"' \
  'exact Agent receipt before worker crash' \
  'agent_synthetic_receipt_reached "${synthetic_receipt}" "${command_id}"' \
  'docker kill "${COMPOSE_PROJECT}-worker-1"' \
  'docker kill "${COMPOSE_PROJECT}-transportd-1"' \
  'rm -f -- "${synthetic_barrier}"' \
  'exact Agent journal receipt after worker crash' \
  'wait_for_journal_command 15 1 "exact Agent journal receipt after worker crash"' \
  'attempt_proof="$(exact_post_send_attempt_proof "${command_id}" "${attempt_id}")"' \
  'report_exact_post_send_attempt_failure "${command_id}" "${attempt_id}"' \
  'pre_send_barrier_disarm'; do
  grep -qF "${token}" <<<"${crash2_phase}" || {
    echo "the deterministic send-before-MarkSent sequence is missing: ${token}" >&2
    exit 1
  }
done
if grep -qF 'docker pause "${COMPOSE_PROJECT}-transportd-1"' <<<"${crash2_phase}"; then
  echo "send-before-MarkSent must not use transportd process suspension as a send barrier" >&2
  exit 1
fi
if ! grep -qF 'printf '\''%s\n'\'' "${command_id}" >"${synthetic_barrier}"' <<<"${crash2_phase}" \
  || ! grep -qF ': >"${synthetic_receipt}"' <<<"${crash2_phase}" \
  || ! grep -qF 'rm -f -- "${synthetic_barrier}"' <<<"${crash2_phase}"; then
  echo "the send-before-MarkSent phase must arm an exact Agent barrier and receipt" >&2
  exit 1
fi
crash2_pause_line="$(grep -nF 'docker pause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_receipt_reset_line="$(grep -nF ': >"${synthetic_receipt}"' <<<"${crash2_phase}" | tail -1 | cut -d: -f1)"
crash2_enqueue_line="$(grep -nF 'g6rd_enqueue_command "${node}" "${key}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_agent_hold_line="$(grep -nF 'printf '\''%s\n'\'' "${command_id}" >"${synthetic_barrier}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_arm_line="$(grep -nF 'pre_send_barrier_arm "${command_id}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_post_arm_line="$(grep -nF 'post_send_barrier_arm "${command_id}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_worker_unpause_line="$(grep -nF 'docker unpause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash2_phase}" | tail -1 | cut -d: -f1)"
crash2_reached_line="$(grep -nF 'pre_send_barrier_reached "${command_id}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_claim_line="$(grep -nF 'send-before-MarkSent strict outbox claim' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_release_line="$(grep -nF 'pre_send_barrier_release "${command_id}"' <<<"${crash2_phase}" | tail -1 | cut -d: -f1)"
crash2_post_reached_line="$(grep -nF 'post_send_barrier_reached "${command_id}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_attempt_capture_line="$(grep -nF 'attempt_id="$(exact_post_send_attempt_id "${command_id}")"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_accepted_line="$(grep -nF 'g6rd_timeline_event transport_send_accepted' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_agent_receipt_line="$(grep -nF 'agent_synthetic_receipt_reached "${synthetic_receipt}" "${command_id}"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_worker_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-worker-1"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_release_agent_line="$(grep -nF 'rm -f -- "${synthetic_barrier}"' <<<"${crash2_phase}" | tail -1 | cut -d: -f1)"
crash2_journal_line="$(grep -nF 'wait_for_journal_command 15 1 "exact Agent journal receipt after worker crash"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_transport_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-transportd-1"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_disarm_line="$(grep -nF 'pre_send_barrier_disarm' <<<"${crash2_phase}" | tail -1 | cut -d: -f1)"
crash2_attempt_proof_line="$(grep -nF 'attempt_proof="$(exact_post_send_attempt_proof "${command_id}" "${attempt_id}")"' <<<"${crash2_phase}" | cut -d: -f1)"
crash2_restart_line="$(grep -nF 'restart_worker_transport_unit send-before-MarkSent' <<<"${crash2_phase}" | cut -d: -f1)"
[[ -n "${crash2_pause_line}" && -n "${crash2_receipt_reset_line}" \
  && -n "${crash2_enqueue_line}" && -n "${crash2_agent_hold_line}" \
  && -n "${crash2_arm_line}" \
  && -n "${crash2_post_arm_line}" \
  && -n "${crash2_worker_unpause_line}" && -n "${crash2_reached_line}" \
  && -n "${crash2_claim_line}" \
  && -n "${crash2_release_line}" && -n "${crash2_post_reached_line}" \
  && -n "${crash2_attempt_capture_line}" \
  && -n "${crash2_accepted_line}" \
  && -n "${crash2_agent_receipt_line}" \
  && -n "${crash2_worker_kill_line}" && -n "${crash2_release_agent_line}" \
  && -n "${crash2_journal_line}" && -n "${crash2_transport_kill_line}" \
  && -n "${crash2_disarm_line}" && -n "${crash2_attempt_proof_line}" \
  && -n "${crash2_restart_line}" \
  && "${crash2_pause_line}" -lt "${crash2_receipt_reset_line}" \
  && "${crash2_receipt_reset_line}" -lt "${crash2_enqueue_line}" \
  && "${crash2_enqueue_line}" -lt "${crash2_agent_hold_line}" \
  && "${crash2_agent_hold_line}" -lt "${crash2_arm_line}" \
  && "${crash2_arm_line}" -lt "${crash2_post_arm_line}" \
  && "${crash2_post_arm_line}" -lt "${crash2_worker_unpause_line}" \
  && "${crash2_worker_unpause_line}" -lt "${crash2_reached_line}" \
  && "${crash2_reached_line}" -lt "${crash2_claim_line}" \
  && "${crash2_claim_line}" -lt "${crash2_release_line}" \
  && "${crash2_release_line}" -lt "${crash2_post_reached_line}" \
  && "${crash2_post_reached_line}" -lt "${crash2_attempt_capture_line}" \
  && "${crash2_attempt_capture_line}" -lt "${crash2_accepted_line}" \
  && "${crash2_accepted_line}" -lt "${crash2_agent_receipt_line}" \
  && "${crash2_agent_receipt_line}" -lt "${crash2_worker_kill_line}" \
  && "${crash2_worker_kill_line}" -lt "${crash2_transport_kill_line}" \
  && "${crash2_transport_kill_line}" -lt "${crash2_release_agent_line}" \
  && "${crash2_release_agent_line}" -lt "${crash2_journal_line}" \
  && "${crash2_journal_line}" -lt "${crash2_disarm_line}" \
  && "${crash2_disarm_line}" -lt "${crash2_attempt_proof_line}" \
  && "${crash2_attempt_proof_line}" -lt "${crash2_restart_line}" ]] || {
  echo "the exact send-before-MarkSent fault sequence is misordered" >&2
  exit 1
}
for cleanup_token in \
  'if [[ "${completed}" != 1 ]]' \
  'pre_send_barrier_release "${command_id:-}"' \
  'post_send_barrier_release "${command_id:-}"' \
  'docker unpause "${COMPOSE_PROJECT}-worker-1"' \
  'rm -f -- "${synthetic_barrier}"' \
  'docker unpause "${COMPOSE_PROJECT}-transportd-1"'; do
  grep -qF "${cleanup_token}" <<<"${crash2_phase}" || {
    echo "the send-before-MarkSent failure trap is missing: ${cleanup_token}" >&2
    exit 1
  }
done
if grep -qF 'docker pause "${COMPOSE_PROJECT}-transportd-1"' <<<"${crash1_phase}"; then
  echo "claim-before-send must not use transportd process suspension as a send barrier" >&2
  exit 1
fi
crash1_pause_line="$(grep -nF 'docker pause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_enqueue_line="$(grep -nF 'g6rd_enqueue_command "${node}" "${key}"' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_arm_line="$(grep -nF 'pre_send_barrier_arm "${command_id}"' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_unpause_line="$(grep -nF 'docker unpause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash1_phase}" | tail -1 | cut -d: -f1)"
crash1_reached_line="$(grep -nF 'pre_send_barrier_reached "${command_id}"' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_transport_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-transportd-1"' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_claim_line="$(grep -nF '"exact committed outbox claim" outbox_row_claimed' <<<"${crash1_phase}" | cut -d: -f1)"
crash1_worker_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-worker-1"' <<<"${crash1_phase}" | cut -d: -f1)"
[[ -n "${crash1_pause_line}" && -n "${crash1_enqueue_line}" \
  && -n "${crash1_arm_line}" && -n "${crash1_unpause_line}" \
  && -n "${crash1_reached_line}" && -n "${crash1_claim_line}" \
  && -n "${crash1_worker_kill_line}" && -n "${crash1_transport_kill_line}" \
  && "${crash1_pause_line}" -lt "${crash1_enqueue_line}" \
  && "${crash1_enqueue_line}" -lt "${crash1_arm_line}" \
  && "${crash1_arm_line}" -lt "${crash1_unpause_line}" \
  && "${crash1_unpause_line}" -lt "${crash1_reached_line}" \
  && "${crash1_reached_line}" -lt "${crash1_claim_line}" \
  && "${crash1_claim_line}" -lt "${crash1_worker_kill_line}" \
  && "${crash1_worker_kill_line}" -lt "${crash1_transport_kill_line}" ]] || {
  echo "the exact claim-before-send Worker barrier sequence is misordered" >&2
  exit 1
}
for cleanup_token in \
  'if [[ "${completed}" != 1 ]]' \
  'pre_send_barrier_release "${command_id:-}"' \
  'docker unpause "${COMPOSE_PROJECT}-worker-1"'; do
  grep -qF "${cleanup_token}" <<<"${crash1_phase}" || {
    echo "the claim-before-send failure trap is missing: ${cleanup_token}" >&2
    exit 1
  }
done

# The result-ingress barrier signal is created by uid 65532. The harness must
# pre-create a writable/readable inode before resuming the worker, and its
# failure trap must release the exact command instead of stranding a DB tx.
crash3_pause_line="$(grep -nF 'docker pause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_enqueue_line="$(grep -nF 'g6rd_enqueue_command "${node}" "${key}"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_arm_line="$(grep -nF 'printf '\''%s\n'\'' "${command_id}" >"${barrier}/arm"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_received_line="$(grep -nF ': >"${barrier}/received"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_mode_line="$(grep -nF 'chmod 0666 "${barrier}/arm" "${barrier}/received"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_unpause_line="$(grep -nF 'docker unpause "${COMPOSE_PROJECT}-worker-1"' <<<"${crash3_phase}" | tail -1 | cut -d: -f1)"
crash3_wait_line="$(grep -nF '"ingress result commit barrier" test -s' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_precommit_line="$(grep -nF 'agent_command_results WHERE command_id=' <<<"${crash3_phase}" | head -1 | cut -d: -f1)"
crash3_worker_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-worker-1"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_transport_kill_line="$(grep -nF 'docker kill "${COMPOSE_PROJECT}-transportd-1"' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_postkill_line="$(grep -nF 'agent_command_results WHERE command_id=' <<<"${crash3_phase}" | sed -n '2p' | cut -d: -f1)"
crash3_restart_line="$(grep -nF 'restart_worker_transport_unit result-before-commit' <<<"${crash3_phase}" | cut -d: -f1)"
crash3_result_line="$(grep -nF 'agent_command_results WHERE command_id=' <<<"${crash3_phase}" | tail -1 | cut -d: -f1)"
[[ -n "${crash3_pause_line}" && -n "${crash3_enqueue_line}" \
  && -n "${crash3_arm_line}" && -n "${crash3_received_line}" \
  && -n "${crash3_mode_line}" \
  && -n "${crash3_unpause_line}" && -n "${crash3_wait_line}" \
  && -n "${crash3_precommit_line}" && -n "${crash3_worker_kill_line}" \
  && -n "${crash3_transport_kill_line}" && -n "${crash3_postkill_line}" \
  && -n "${crash3_restart_line}" && -n "${crash3_result_line}" \
  && "${crash3_pause_line}" -lt "${crash3_enqueue_line}" \
  && "${crash3_enqueue_line}" -lt "${crash3_arm_line}" \
  && "${crash3_arm_line}" -lt "${crash3_received_line}" \
  && "${crash3_received_line}" -lt "${crash3_mode_line}" \
  && "${crash3_mode_line}" -lt "${crash3_unpause_line}" \
  && "${crash3_unpause_line}" -lt "${crash3_wait_line}" \
  && "${crash3_wait_line}" -lt "${crash3_precommit_line}" \
  && "${crash3_precommit_line}" -lt "${crash3_worker_kill_line}" \
  && "${crash3_worker_kill_line}" -lt "${crash3_transport_kill_line}" \
  && "${crash3_transport_kill_line}" -lt "${crash3_postkill_line}" \
  && "${crash3_postkill_line}" -lt "${crash3_restart_line}" \
  && "${crash3_restart_line}" -lt "${crash3_result_line}" ]] || {
  echo "the exact result-before-commit fault and recovery sequence is misordered" >&2
  exit 1
}
for cleanup_token in \
  'if [[ "${completed}" != 1 ]]' \
  'printf "%s\n" "${command_id}" >"${barrier}/release"' \
  'chmod 0666 "${barrier}/release"' \
  'docker unpause "${COMPOSE_PROJECT}-worker-1"'; do
  grep -qF "${cleanup_token}" <<<"${crash3_phase}" || {
    echo "the result-before-commit failure release is missing: ${cleanup_token}" >&2
    exit 1
  }
done

# Durable journal mirrors remain live through slow collection. The API is
# frozen before the final observations, and two independently verified
# transport inventories bracket one DB-clock authority cut. The cut freezes
# both current authority and complete journal arrays in one MVCC snapshot.
collect_phase="$(sed -n '/^phase_evidence_collect() {/,/^}/p' "${FD_B}")"
writer_quiesce="$(sed -n '/^quiesce_control_plane_writers() {/,/^}/p' "${FD_B}")"
ingress_quiesce="$(sed -n '/^quiesce_transport_ingress() {/,/^}/p' "${FD_B}")"
renewer_quiesce="$(sed -n '/^quiesce_authority_renewers() {/,/^}/p' "${FD_B}")"
authority_cut="$(sed -n '/^capture_final_authority_cut() {/,/^}/p' "${FD_B}")"
session_assert="$(sed -n '/^assert_final_session_authority() {/,/^}/p' "${FD_B}")"
fencing_watcher="$(sed -n '/^g6rd_watch_fencing_history() {/,/^}/p' "${LIB}")"
leadership_watcher="$(sed -n '/^g6rd_watch_leadership_history() {/,/^}/p' "${LIB}")"
watcher_start="$(sed -n '/^start_watchers() {/,/^}/p' "${FD_B}")"
watcher_stop="$(sed -n '/^stop_watchers() {/,/^}/p' "${FD_B}")"
final_history_snapshot="$(sed -n '/^append_final_history_snapshot() {/,/^}/p' "${FD_B}")"
telemetry_capture="$(grep -F 'last_telemetry_at' <<<"${collect_phase}")"
grep -qF 'HH24:MI:SS.US\"Z\"' <<<"${telemetry_capture}" || {
  echo "telemetry evidence must preserve subsecond heartbeat timestamps" >&2
  exit 1
}
for artifact in commands attempts outbox audit; do
  artifact_capture="$(grep -B1 -F '>"${dir}/'"${artifact}"'.jsonl"' <<<"${collect_phase}")"
  if [[ -z "${artifact_capture}" ]] \
    || ! grep -qF 'HH24:MI:SS.US\"Z\"' <<<"${artifact_capture}"; then
    echo "${artifact} evidence must preserve subsecond database timestamps" >&2
    exit 1
  fi
done
collect_instances_line="$(grep -nF 'done >>"${dir}/instances.tsv"' <<<"${collect_phase}" | cut -d: -f1)"
collect_api_freeze_line="$(grep -nF 'quiesce_control_plane_writers' <<<"${collect_phase}" | cut -d: -f1)"
collect_telemetry_line="$(grep -nF '>"${dir}/telemetry.jsonl"' <<<"${collect_phase}" | cut -d: -f1)"
collect_sessions_before_line="$(grep -nF '>"${dir}/final-sessions-before.json"' <<<"${collect_phase}" | cut -d: -f1)"
collect_before_complete_line="$(grep -nF '>"${dir}/final-sessions-before-complete-at"' <<<"${collect_phase}" | cut -d: -f1)"
collect_sessions_after_line="$(grep -nF '>"${dir}/final-sessions-after.json"' <<<"${collect_phase}" | cut -d: -f1)"
collect_after_start_line="$(grep -nF '>"${dir}/final-sessions-after-start-at"' <<<"${collect_phase}" | cut -d: -f1)"
collect_observed_line="$(grep -nF '>"${dir}/final-session-observed-at"' <<<"${collect_phase}" | cut -d: -f1)"
collect_ingress_freeze_line="$(grep -nF 'quiesce_transport_ingress' <<<"${collect_phase}" | cut -d: -f1)"
collect_cut_line="$(grep -nF 'capture_final_authority_cut' <<<"${collect_phase}" | cut -d: -f1)"
collect_assert_line="$(grep -nF 'assert_final_session_authority' <<<"${collect_phase}" | cut -d: -f1)"
collect_renewers_line="$(grep -nF 'quiesce_authority_renewers' <<<"${collect_phase}" | cut -d: -f1)"
collect_stop_line="$(grep -nF 'stop_watchers' <<<"${collect_phase}" | cut -d: -f1)"
collect_final_history_line="$(grep -nF 'append_final_history_snapshot' <<<"${collect_phase}" | cut -d: -f1)"
collect_copy_line="$(grep -nF 'cp -f "${G6RD_STATE}/${history}-history.jsonl"' <<<"${collect_phase}" | cut -d: -f1)"
collect_stamp_line="$(grep -nF '>"${dir}/snapshot-taken-at"' <<<"${collect_phase}" | cut -d: -f1)"
[[ -n "${collect_instances_line}" && -n "${collect_api_freeze_line}" \
  && -n "${collect_telemetry_line}" && -n "${collect_sessions_before_line}" \
  && -n "${collect_before_complete_line}" \
  && -n "${collect_sessions_after_line}" && -n "${collect_after_start_line}" \
  && -n "${collect_observed_line}" && -n "${collect_ingress_freeze_line}" \
  && -n "${collect_cut_line}" && -n "${collect_assert_line}" \
  && -n "${collect_renewers_line}" \
  && -n "${collect_stop_line}" && -n "${collect_final_history_line}" \
  && -n "${collect_copy_line}" && -n "${collect_stamp_line}" \
  && "${collect_instances_line}" -lt "${collect_api_freeze_line}" \
  && "${collect_api_freeze_line}" -lt "${collect_telemetry_line}" \
  && "${collect_telemetry_line}" -lt "${collect_sessions_before_line}" \
  && "${collect_sessions_before_line}" -lt "${collect_before_complete_line}" \
  && "${collect_before_complete_line}" -lt "${collect_cut_line}" \
  && "${collect_cut_line}" -lt "${collect_stop_line}" \
  && "${collect_stop_line}" -lt "${collect_final_history_line}" \
  && "${collect_final_history_line}" -lt "${collect_after_start_line}" \
  && "${collect_after_start_line}" -lt "${collect_sessions_after_line}" \
  && "${collect_sessions_after_line}" -lt "${collect_assert_line}" \
  && "${collect_assert_line}" -lt "${collect_observed_line}" \
  && "${collect_observed_line}" -lt "${collect_ingress_freeze_line}" \
  && "${collect_ingress_freeze_line}" -lt "${collect_renewers_line}" \
  && "${collect_renewers_line}" -lt "${collect_copy_line}" \
  && "${collect_copy_line}" -lt "${collect_stamp_line}" ]] || {
  echo "evidence collection does not form a bracketed final authority cut" >&2
  exit 1
}
grep -qF 'docker pause "${COMPOSE_PROJECT}-api-1"' <<<"${writer_quiesce}" || {
  echo "evidence collection must stop public writes before final observations" >&2
  exit 1
}
if grep -qE 'worker-1|scheduler-1|transportd-1' <<<"${writer_quiesce}"; then
  echo "short authority leases must remain renewable during the final live probe" >&2
  exit 1
fi
grep -qF 'docker pause "${COMPOSE_PROJECT}-transportd-1"' <<<"${ingress_quiesce}" || {
  echo "evidence collection must freeze transport ingress after the proven bracket" >&2
  exit 1
}
for service in worker scheduler; do
  grep -qF "docker pause \"\${COMPOSE_PROJECT}-${service}-1\"" <<<"${renewer_quiesce}" || {
    echo "evidence collection must stop ${service} only after the authority cut" >&2
    exit 1
  }
done
for token in \
  'WITH cut AS MATERIALIZED' \
  'owner_journal AS MATERIALIZED' \
  'scheduler_journal AS MATERIALIZED' \
  'maintenance_journal AS MATERIALIZED' \
  'HH24:MI:SS.US\"Z\"' \
  'fencing.lease_until>cut.at' \
  'leadership.lease_until>cut.at' \
  'FROM g6_connection_owner_history AS history' \
  'FROM g6_scheduler_leadership_history AS history' \
  'FROM g6_scheduler_maintenance_history AS history' \
  'ORDER BY history.history_id' \
  "'owner_history',owner_journal.entries" \
  "'scheduler_history',scheduler_journal.entries" \
  "'scheduler_maintenance_history',maintenance_journal.entries" \
  "'lease_until',to_char(owner.lease_until" \
  "'owner_instance_id',owner.owner_instance_id::text" \
  "'owner_incarnation',owner.owner_incarnation::text" \
  "'connection_id',encode(owner.connection_id,'hex')" \
  "'instance_id',entry.instance_id::text" \
  "'incarnation',entry.incarnation::text" \
  "'epoch',entry.epoch" \
  "'lease_until',to_char(entry.lease_until"; do
  grep -qF "${token}" <<<"${authority_cut}" || {
    echo "the single DB authority cut is missing: ${token}" >&2
    exit 1
  }
done
assert_journal_watcher() {
  local watcher_body=$1 watcher_query=$2 watcher_file=$3
  if ! grep -qF "${watcher_query}" <<<"${watcher_body}" \
    || ! grep -qF 'mv -f "${temp}"' <<<"${watcher_body}" \
    || ! grep -qF "${watcher_file}" <<<"${watcher_body}"; then
    echo "authority watcher is not an ordered atomic journal mirror: ${watcher_file}" >&2
    exit 1
  fi
}
assert_journal_watcher "${fencing_watcher}" \
  'FROM g6_connection_owner_history ORDER BY history_id' \
  'fencing-history.jsonl'
assert_journal_watcher "${leadership_watcher}" \
  'FROM g6_scheduler_leadership_history ORDER BY history_id' \
  'leadership-history.jsonl'
if grep -qF 'FROM connection_owner_fencing ORDER BY node_id' <<<"${fencing_watcher}" \
  || grep -qF 'FROM scheduler_leadership WHERE id=1' <<<"${leadership_watcher}"; then
  echo "authority evidence must not poll mutable current-state rows" >&2
  exit 1
fi
for token in 'fencing-watcher-failed-at' 'leadership-watcher-failed-at'; do
  grep -qF "${token}" <<<"${watcher_start}" || {
    echo "authority watcher failure markers are not reset: ${token}" >&2
    exit 1
  }
done
for token in \
  'failure="${G6RD_STATE}/${name}-watcher-failed-at"' \
  'if [[ -e "${failure}" ]]' \
  'status=1'; do
  grep -qF "${token}" <<<"${watcher_stop}" || {
    echo "authority watcher failures are not propagated: ${token}" >&2
    exit 1
  }
done
for token in \
  "jq -er '.owner_history[]'" \
  "jq -er '.scheduler_history[]'" \
  "jq -er '.scheduler_maintenance_history[]'" \
  'history_id <= previous' \
  'mv -f "${owner_tmp}" "${G6RD_STATE}/fencing-history.jsonl"' \
  'mv -f "${scheduler_tmp}" "${G6RD_STATE}/leadership-history.jsonl"'; do
  grep -qF "${token}" <<<"${final_history_snapshot}" || {
    echo "the final authority cut does not atomically publish frozen journals: ${token}" >&2
    exit 1
  }
done
grep -qF 'mv -f "${maintenance_tmp}" "${G6RD_STATE}/scheduler-maintenance-history.jsonl"' \
  <<<"${final_history_snapshot}" || {
  echo "the final authority cut does not publish the frozen scheduler maintenance journal" >&2
  exit 1
}
if grep -qE '>>.*(fencing|leadership)-history' <<<"${final_history_snapshot}"; then
  echo "post-cut authority history must replace, not append to, live mirrors" >&2
  exit 1
fi
clock_capture="$(sed -n '/^capture_database_clock() {/,/^}/p' "${FD_B}")"
for token in 'clock_timestamp()' 'HH24:MI:SS.US\"Z\"'; do
  grep -qF "${token}" <<<"${clock_capture}" || {
    echo "the transport bracket DB clock capture is missing: ${token}" >&2
    exit 1
  }
done
for token in \
  'final-sessions-before.json' \
  'final-sessions-after.json' \
  '.owner_lease_until | stamp_key' \
  'cmp -s "${before_terms}" "${after_terms}"' \
  'cmp -s "${session_authority}" "${authority_terms}"'; do
  grep -qF "${token}" <<<"${session_assert}" || {
    echo "final session authority validation is missing: ${token}" >&2
    exit 1
  }
done
if ! grep -qF '[0-9]{1,9}' <<<"${session_assert}" \
  || ! grep -qF '000000000' <<<"${session_assert}" \
  || grep -qF 'fromdateiso8601' <<<"${session_assert}"; then
  echo "final session authority timestamps must use exact padded nanosecond keys" >&2
  exit 1
fi
final_session_precision_fixture="$(mktemp -d)"
mkdir -p "${final_session_precision_fixture}/evidence"
jq -n '{
  cut_at:"2026-08-19T10:00:00.500000000Z",
  owners:[{
    node_hex:"00000000000070008000000000000001",
    owner_instance_id:"00000000-0000-7000-8000-000000000010",
    owner_incarnation:"10",
    connection_id:"33333333333333333333333333333333",
    owner_epoch:4
  }]
}' >"${final_session_precision_fixture}/final-authority-cut.json"
jq -n '{all_matched:true,observations:[{
  node_id:"00000000-0000-7000-8000-000000000001",
  found:true,endpoint_id:("a" * 64),agent_instance_id:("1" * 32),
  connected_at:"2026-08-19T10:00:00.499999999Z",
  session_expires_at:"2026-08-19T10:00:00.500000001Z",
  owner_fence_id:("2" * 32),
  owner_instance_id:"00000000-0000-7000-8000-000000000010",
  owner_incarnation:"10",connection_id:("3" * 32),owner_epoch:4,
  owner_lease_until:"2026-08-19T10:00:00.500000001Z",
  authorization_revision:2,negotiated_capabilities:["ocserv.fencing.v2"]
}]}' >"${final_session_precision_fixture}/evidence/final-sessions-before.json"
cp "${final_session_precision_fixture}/evidence/final-sessions-before.json" \
  "${final_session_precision_fixture}/evidence/final-sessions-after.json"
(
  G6RD_STATE="${final_session_precision_fixture}"
  node_ids() { printf '%s\n' 00000000-0000-7000-8000-000000000001; }
  eval "${session_assert}"
  assert_final_session_authority
  for name in final-sessions-before final-sessions-after; do
    jq '.observations[0].connected_at = "2026-08-19T10:00:00.500000001Z"' \
      "${G6RD_STATE}/evidence/${name}.json" >"${G6RD_STATE}/evidence/${name}.tmp"
    mv "${G6RD_STATE}/evidence/${name}.tmp" "${G6RD_STATE}/evidence/${name}.json"
  done
  if assert_final_session_authority >/dev/null 2>&1; then
    echo "a same-second post-cut session was accepted by the final authority bracket" >&2
    exit 1
  fi
)
rm -rf -- "${final_session_precision_fixture}"
timestamp_formatter="$(sed -n '/^fn timestamp_to_rfc3339(/,/^}/p' \
  "${ROOT}/rust/crates/g6-probe/src/main.rs")"
for token in \
  '253_402_300_799' \
  'nanos >= 1_000_000_000' \
  '{nanos:09}Z'; do
  grep -qF "${token}" <<<"${timestamp_formatter}" || {
    echo "g6-probe timestamp JSON does not preserve valid nanoseconds: ${token}" >&2
    exit 1
  }
done
grep -qF 'Some("1970-01-01T00:00:00.123456789Z")' \
  "${ROOT}/rust/crates/g6-probe/src/main.rs" || {
  echo "g6-probe lacks the nanosecond producer regression" >&2
  exit 1
}
for token in \
  '--signing-key-file /run/ocservia-signing/command-signing.pem' \
  'get_owner_fence' \
  'verify_connection_fence_v2_at' \
  'fence_capabilities != session_capabilities'; do
  if [[ "${token}" == --signing-key-file* ]]; then
    grep -qF -- "${token}" "${LIB}" || {
      echo "node connection probe is not pinned to the Controller verification key" >&2
      exit 1
    }
  else
    grep -qF -- "${token}" "${ROOT}/rust/crates/g6-probe/src/main.rs" || {
      echo "node connection probe lacks independent fence verification: ${token}" >&2
      exit 1
    }
  fi
done
grep -qF "jq -er '.cut_at'" <<<"${collect_phase}" || {
  echo "the public snapshot stamp must come from the DB authority cut" >&2
  exit 1
}
if ! grep -qF 'require_file "${G6RD_STATE}/${history}-history.jsonl"' <<<"${collect_phase}" \
  || ! grep -qF '"${G6RD_OUTBOX}/${history}-history.jsonl"' <<<"${collect_phase}"; then
  echo "evidence collection must fail closed and publish both required history files" >&2
  exit 1
fi

# The fault-free observation window and the sampler cadence are harness
# margins over the frozen SLO limits, so they must demonstrably clear them.
resource_preflight_phase="$(sed -n '/^phase_resource_preflight() {/,/^}/p' "${FD_B}")"
for token in \
  'G6RD_SAMPLER_COMPOSE_TIMEOUT_SECONDS=8' \
  'G6RD_SAMPLER_PSQL_TIMEOUT_SECONDS=8' \
  'g6rd_sampler_tick "${temporary}"' \
  'g6rd_validate_sampler_batch "${temporary}"' \
  'mv -f -- "${temporary}" "${output}"'; do
  grep -qF "${token}" <<<"${resource_preflight_phase}" || {
    echo "the resource preflight is not a bounded real sampler gate: ${token}" >&2
    exit 1
  }
done
sampler_tick="$(sed -n '/^g6rd_sampler_tick() {/,/^}/p' "${LIB}")"
for token in \
  'local G6RD_COMPOSE_TIMEOUT_SECONDS="${G6RD_SAMPLER_COMPOSE_TIMEOUT_SECONDS:-3}"' \
  'local G6RD_PSQL_TIMEOUT_SECONDS="${G6RD_SAMPLER_PSQL_TIMEOUT_SECONDS:-3}"' \
  'local G6RD_TIMEOUT_PROCESS_GROUP=1'; do
  grep -qF "${token}" <<<"${sampler_tick}" || {
    echo "production sampler probes lack their per-probe hard bound: ${token}" >&2
    exit 1
  }
done
window_phase="$(sed -n '/^phase_window() (/,/^)/p' "${FD_B}")"
window_wait_helper="$(sed -n '/^wait_for_window_enqueue_wave() {/,/^}/p' "${FD_B}")"
window_arm_phase="$(sed -n '/^phase_window_barrier_arm() {/,/^}/p' "${FD_B}")"
window_active_predicate="$(sed -n '/^window_opening_commands_active() {/,/^}/p' "${FD_B}")"
window_active_capture="$(sed -n '/^capture_window_opening_active() {/,/^}/p' "${FD_B}")"
window_proof_record="$(sed -n '/^record_window_opening_proof() {/,/^}/p' "${FD_B}")"
fd_a_window_proof="$(sed -n '/^fd_a_window_opening_proof_recorded() {/,/^}/p' "${FD_A}")"
fd_a_window_release="$(sed -n '/^phase_window_barrier_release_after_proof() (/,/^)/p' "${FD_A}")"
for token in \
  'g6rd_arm_synthetic_barriers' \
  'g6rd_synthetic_barriers_armed' \
  'window-barrier-arm-request/candidate-sha'; do
  grep -qF "${token}" <<<"${window_arm_phase}" || {
    echo "the local observation barrier arm is incomplete: ${token}" >&2
    exit 1
  }
done
for token in \
  "count(DISTINCT opening.node_id)" \
  "opening.state IN ('dispatched','accepted','running')" \
  'count(result.command_id)' \
  '"${expected}"$'\''\t'\''"${expected}"$'\''\t'\''"${expected}"$'\''\t0'; do
  grep -qF "${token}" <<<"${window_active_predicate}" || {
    echo "the all-fleet production inflight predicate is incomplete: ${token}" >&2
    exit 1
  }
done
for token in \
  'expected_count' \
  'HH24:MI:SS.US\"Z\"' \
  '(.commands | length) == $expected' \
  '([.commands[].node_id] | unique | length) == $expected' \
  'all(.commands[]; (.state | IN("dispatched","accepted","running")))' \
  '.result_count == 0' \
  'mv -f -- "${temporary}" "${output}"'; do
  grep -qF "${token}" <<<"${window_active_capture}" || {
    echo "the frozen all-fleet production inflight proof is incomplete: ${token}" >&2
    exit 1
  }
done
for token in \
  'window-opening-proof-${RUN_ID}' \
  "'window_opening_proof'" \
  'RETURNING id'; do
  grep -qF "${token}" <<<"${window_proof_record}" || {
    echo "the durable all-fleet production inflight marker is incomplete: ${token}" >&2
    exit 1
  }
done
for token in \
  'window-opening-proof-${RUN_ID%-fd-a}-fd-b' \
  "phase='window_opening_proof'" \
  'G6_DB_PORT=15432' \
  '[[ "${observed}" == 1 ]]'; do
  grep -qF "${token}" <<<"${fd_a_window_proof}" || {
    echo "failure domain A does not bind release to the durable inflight marker: ${token}" >&2
    exit 1
  }
done
window_opening_enqueue_line="$(grep -nF 'g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-opening-${count}"' <<<"${window_phase}" | cut -d: -f1)"
window_active_wait_line="$(grep -nF '"exact fifty-command production inflight proof"' <<<"${window_phase}" | cut -d: -f1)"
window_active_capture_line="$(grep -nF 'capture_window_opening_active' <<<"${window_phase}" | cut -d: -f1)"
window_proof_record_line="$(grep -nF 'record_window_opening_proof' <<<"${window_phase}" | cut -d: -f1)"
window_barrier_release_line="$(grep -nF 'g6rd_release_synthetic_barriers' <<<"${window_phase}" | tail -1 | cut -d: -f1)"
[[ -n "${window_opening_enqueue_line}" && -n "${window_active_wait_line}" \
  && -n "${window_active_capture_line}" && -n "${window_proof_record_line}" \
  && -n "${window_barrier_release_line}" \
  && "${window_opening_enqueue_line}" -lt "${window_active_wait_line}" \
  && "${window_active_wait_line}" -lt "${window_active_capture_line}" \
  && "${window_active_capture_line}" -lt "${window_proof_record_line}" \
  && "${window_proof_record_line}" -lt "${window_barrier_release_line}" ]] || {
  echo "the observation window must arm, prove, freeze, durably mark, then release the exact fifty-command population" >&2
  exit 1
}
for token in \
  'g6rd_synthetic_barriers_armed' \
  '"durably frozen fifty-command production inflight proof"' \
  'fd_a_window_opening_proof_recorded' \
  'g6rd_release_synthetic_barriers'; do
  grep -qF "${token}" <<<"${fd_a_window_release}" || {
    echo "failure domain A does not hold its barriers through the exact proof: ${token}" >&2
    exit 1
  }
done
(
  eval "${fd_a_window_proof}"
  export RUN_ID=fixture-run-fd-a
  g6rd_psql() { printf '0\n'; }
  if fd_a_window_opening_proof_recorded; then
    echo "failure domain A accepted barrier release without the durable proof marker" >&2
    exit 1
  fi
  g6rd_psql() { printf '1\n'; }
  fd_a_window_opening_proof_recorded || {
    echo "failure domain A rejected the exact durable proof marker" >&2
    exit 1
  }
)
(
  eval "${window_active_predicate}"
  export RUN_ID=fixture-run-fd-b
  managed_node_count() { printf '2\n'; }
  psql_window_probe() { printf '2\t2\t2\t0\n'; }
  window_opening_commands_active || {
    echo "the all-fleet production inflight predicate rejected an exact active population" >&2
    exit 1
  }
  psql_window_probe() { printf '2\t2\t1\t0\n'; }
  if window_opening_commands_active; then
    echo "the all-fleet production inflight predicate accepted a non-active command" >&2
    exit 1
  fi
  psql_window_probe() { printf '2\t2\t2\t1\n'; }
  if window_opening_commands_active; then
    echo "the all-fleet production inflight predicate accepted a completed result" >&2
    exit 1
  fi
)
if grep -qE '^[[:space:]]*wait[[:space:]]*$' <<<"${window_phase}"; then
  echo "the observation window must not wait for the long-lived sampler" >&2
  exit 1
fi
grep -qF 'enqueue_pids+=("$!")' <<<"${window_phase}" || {
  echo "the observation window must capture every opening-wave enqueue PID" >&2
  exit 1
}
grep -qF 'wait_for_window_enqueue_wave "${enqueue_pids[@]}"' \
  <<<"${window_phase}" || {
  echo "the observation window must wait only for its opening-wave enqueues" >&2
  exit 1
}
(
  eval "${window_wait_helper}"
  sleep 30 &
  sampler_pid=$!
  trap 'kill "${sampler_pid}" 2>/dev/null || true; wait "${sampler_pid}" 2>/dev/null || true' EXIT
  true &
  enqueue_pid=$!
  started_at="${SECONDS}"
  wait_for_window_enqueue_wave "${enqueue_pid}"
  ((SECONDS - started_at < 2)) || {
    echo "the enqueue-wave wait blocked on an unrelated background sampler" >&2
    exit 1
  }
  kill -0 "${sampler_pid}" || {
    echo "the enqueue-wave wait terminated the unrelated background sampler" >&2
    exit 1
  }
  false &
  failed_pid=$!
  marker="$(mktemp)"
  rm -f -- "${marker}"
  (sleep 1; touch "${marker}") &
  succeeding_pid=$!
  if wait_for_window_enqueue_wave "${failed_pid}" "${succeeding_pid}" \
    >/dev/null 2>&1; then
    echo "the enqueue-wave wait hid a failed enqueue child" >&2
    exit 1
  fi
  [[ -e "${marker}" ]] || {
    echo "the enqueue-wave wait did not reap children after an earlier failure" >&2
    exit 1
  }
  rm -f -- "${marker}"
)

slo_limit() {
  awk -v metric="$1" '$0 == "  " metric ":" { in_metric = 1 } in_metric && $1 == "limit:" { print $2; exit }' "${SLO}"
}
(
  unset G6_AGENTS_A G6_AGENTS_B
  export FD_ID=fd-a
  default_a="$(g6rd_agent_count)"
  export FD_ID=fd-b
  default_b="$(g6rd_agent_count)"
  default_total="$(g6rd_total_agent_count)"
  [[ "${default_a}" == 25 && "${default_b}" == 25 && "${default_total}" == 50 ]] || {
    echo "the formal G6 fleet must default to exactly 25+25=50 Agents" >&2
    exit 1
  }
  tunnel_agents_per_fd="$(awk '$2 == "G6_FORMAL_AGENTS_PER_FAILURE_DOMAIN:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_enrollment_endpoints="$(awk '$2 == "G6_TRANSIENT_ENROLLMENT_ENDPOINTS_PER_FAILURE_DOMAIN:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_support_endpoints="$(awk '$2 == "G6_RELAY_SUPPORT_ENDPOINTS_PER_FAILURE_DOMAIN:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_burst_per_endpoint="$(awk '$2 == "G6_RELAY_TCP_BURST_PER_ENDPOINT:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_control_connections="$(awk '$2 == "G6_RELAY_TUNNEL_CONTROL_CONNECTIONS:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_capacity="$(awk '$2 == "MAX_TUNNEL_CONNECTIONS:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_queue_capacity="$(awk '$2 == "MAX_TUNNEL_QUEUED_CONNECTIONS:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  tunnel_stream_capacity="$(awk '$2 == "MAX_TUNNEL_STREAMS:" {gsub(/;/, "", $5); print $5}' "${G6_TUNNEL_LIB}")"
  [[ "${tunnel_agents_per_fd}" == "${default_a}" \
    && "${tunnel_agents_per_fd}" == "${default_b}" \
    && "${tunnel_enrollment_endpoints}" == "${tunnel_agents_per_fd}" ]] || {
    echo "the G6 tunnel capacity budget no longer covers both formal Agent endpoint generations" >&2
    exit 1
  }
  required_tunnel_capacity="$(((tunnel_enrollment_endpoints + tunnel_agents_per_fd + tunnel_support_endpoints) * tunnel_burst_per_endpoint + tunnel_control_connections))"
  ((required_tunnel_capacity == 205 \
    && tunnel_capacity >= required_tunnel_capacity \
    && tunnel_capacity == 256 \
    && tunnel_queue_capacity > 0 \
    && tunnel_queue_capacity <= tunnel_capacity \
    && tunnel_stream_capacity == 320 \
    && tunnel_stream_capacity >= tunnel_capacity + tunnel_queue_capacity)) || {
    echo "the bounded G6 tunnel cannot carry the formal relay failover burst" >&2
    exit 1
  }
  grep -Fq 'const PERMIT_ACQUIRE_TIMEOUT: Duration = Duration::from_secs(5);' \
    "${G6_TUNNEL_LIB}" || {
    echo "the G6 tunnel connection-capacity wait is not bounded to five seconds" >&2
    exit 1
  }
  if ! {
    grep -Fq 'reason = "permit_timeout"' "${G6_TUNNEL_LIB}" \
      && grep -Fq 'reason = "queue_full"' "${G6_TUNNEL_LIB}" \
      && grep -Fq 'high_water = snapshot.high_water' "${G6_TUNNEL_LIB}"
  }; then
    echo "the G6 tunnel no longer emits structured saturation diagnostics" >&2
    exit 1
  fi
  tunnel_handler="$(sed -n '/^impl ProtocolHandler for ForwardHandler {/,/^async fn serve_stream(/p' "${G6_TUNNEL_LIB}")"
  if [[ "$(grep -Fc 'self.endpoint.connect(self.peer.clone(), TUNNEL_ALPN)' "${G6_TUNNEL_LIB}")" != 1 \
    || "$(grep -Ec 'endpoint\.connect\(' "${G6_TUNNEL_LIB}")" != 1 \
    || "$(grep -Fc '.max_concurrent_bidi_streams(VarInt::from_u32(MAX_TUNNEL_STREAMS))' "${G6_TUNNEL_LIB}")" != 1 \
    || "$(grep -Fc 'stream = connection.accept_bi() =>' <<<"${tunnel_handler}")" != 1 \
    || "$(grep -Fc 'self.limiter.admit("serve")' <<<"${tunnel_handler}")" != 1 \
    || "$(grep -Fc 'connection.close(' <<<"${tunnel_handler}")" != 1 ]]; then
    echo "the G6 tunnel must multiplex local TCP flows over one authenticated outer connection" >&2
    exit 1
  fi
  [[ "${default_total}" == "$(slo_limit authorized_real_agents)" ]] || {
    echo "the default fleet no longer exactly meets the authorized-real-Agent contract" >&2
    exit 1
  }
  ((default_total >= $(slo_limit max_production_command_inflight))) || {
    echo "the default fleet cannot sustain the formal production-command inflight floor" >&2
    exit 1
  }
)
window_default="$(grep -oE 'G6RD_WINDOW_SECONDS:-[0-9]+' "${FD_B}" | head -1 | cut -d: -f2- | tr -d ':-')"
if [[ -z "${window_default}" || "${window_default}" -le "$(slo_limit stability_sample_span_seconds)" ]]; then
  echo "the observation window default (${window_default:-unset}) must exceed the SLO span limit" >&2
  exit 1
fi
sampler_loop="$(sed -n '/^g6rd_sampler_loop() {/,/^}/p' "${LIB}")"
for token in \
  'local next_tick="${SECONDS}"' \
  'next_tick=$((next_tick + 3))' \
  'if ((SECONDS < next_tick))' \
  'sleep "$((next_tick - SECONDS))"' \
  'next_tick="${SECONDS}"'; do
  grep -qF "${token}" <<<"${sampler_loop}" || {
    echo "the sampler loop is not anchored to its start deadline: ${token}" >&2
    exit 1
  }
done
if grep -qE '^[[:space:]]*sleep 3[[:space:]]*$' <<<"${sampler_loop}"; then
  echo "the sampler must not add a fixed delay after probe work" >&2
  exit 1
fi
sampler_cadence_fixture="$(mktemp -d)"
(
  export G6RD_STATE="${sampler_cadence_fixture}"
  export G6RD_SAMPLER_OUT="${sampler_cadence_fixture}/samples.csv"
  SECONDS=0
  tick_count=0
  g6rd_sampler_tick() {
    local duration
    tick_count=$((tick_count + 1))
    printf '%s\n' "${SECONDS}" >>"${sampler_cadence_fixture}/starts"
    case "${tick_count}" in
      1) duration=2 ;;
      2) duration=4 ;;
      3) duration=2 ;;
      *) echo "the sampler cadence fixture ran an unexpected tick" >&2; return 1 ;;
    esac
    SECONDS=$((SECONDS + duration))
    if ((tick_count == 3)); then
      : >"${G6RD_STATE}/sampler-stop"
    fi
  }
  sleep() {
    local duration="${1:?mock sleep duration is required}"
    [[ "${duration}" =~ ^[1-9][0-9]*$ ]] || return 1
    printf '%s\n' "${duration}" >>"${sampler_cadence_fixture}/sleeps"
    SECONDS=$((SECONDS + duration))
  }
  eval "${sampler_loop}"
  g6rd_sampler_loop
  starts=()
  while IFS= read -r start; do
    starts[${#starts[@]}]="${start}"
  done <"${sampler_cadence_fixture}/starts"
  [[ "${#starts[@]}" == 3 ]] || {
    echo "the sampler cadence fixture did not execute three ticks" >&2
    exit 1
  }
  max_gap=0
  for ((index = 1; index < ${#starts[@]}; index++)); do
    gap=$((starts[index] - starts[index - 1]))
    ((gap > max_gap)) && max_gap="${gap}"
  done
  if ((max_gap > $(slo_limit stability_max_sample_gap_seconds))); then
    echo "deadline-anchored sampler fixture exceeded the max sample gap: ${max_gap}s" >&2
    exit 1
  fi
  if [[ -s "${sampler_cadence_fixture}/sleeps" ]] \
    && grep -qx '3' "${sampler_cadence_fixture}/sleeps"; then
    echo "the sampler cadence fixture observed a fixed post-work delay" >&2
    exit 1
  fi
)
rm -rf -- "${sampler_cadence_fixture}"
if command -v setsid >/dev/null 2>&1; then
  sampler_stop_fixture="$(mktemp -d)"
  (
    export G6RD_STATE="${sampler_stop_fixture}/graceful"
    mkdir -p "${G6RD_STATE}"
    : >"${G6RD_STATE}/sampler-started-at"
    setsid bash -c '
      while [[ ! -e "${G6RD_STATE}/sampler-stop" ]]; do sleep 0.05; done
      printf "%s\n" "2026-08-19T00:00:08.000001Z" \
        >"${G6RD_STATE}/sampler-complete-at"
    ' &
    sampler_pid=$!
    printf '%s\n' "${sampler_pid}" >"${G6RD_STATE}/sampler.pid"
    g6rd_stop_sampler || {
      echo "a graceful sampler completion was rejected" >&2
      exit 1
    }
    [[ -s "${G6RD_STATE}/sampler-complete-at" \
      && ! -e "${G6RD_STATE}/sampler-forced-at" \
      && ! -e "${G6RD_STATE}/sampler.pid" \
      && ! -e "${G6RD_STATE}/sampler-started-at" ]] || {
      echo "graceful sampler stop did not reap and clear its lifecycle state" >&2
      exit 1
    }
    if kill -0 -- "-${sampler_pid}" 2>/dev/null; then
      echo "graceful sampler stop left its process group alive" >&2
      exit 1
    fi
  )
  (
    export G6RD_STATE="${sampler_stop_fixture}/forced"
    mkdir -p "${G6RD_STATE}"
    : >"${G6RD_STATE}/sampler-started-at"
    setsid bash -c '
      trap "" TERM
      : >"${G6RD_STATE}/child-ready"
      while :; do sleep 1; done
    ' &
    sampler_pid=$!
    printf '%s\n' "${sampler_pid}" >"${G6RD_STATE}/sampler.pid"
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      [[ -e "${G6RD_STATE}/child-ready" ]] && break
      sleep 0.05
    done
    [[ -e "${G6RD_STATE}/child-ready" ]] || {
      echo "forced sampler fixture did not become ready" >&2
      exit 1
    }
    sleep() { :; }
    if g6rd_stop_sampler >/dev/null 2>&1; then
      echo "sampler stop accepted a forced process-group termination" >&2
      exit 1
    fi
    unset -f sleep
    [[ -s "${G6RD_STATE}/sampler-forced-at" \
      && ! -e "${G6RD_STATE}/sampler-complete-at" \
      && ! -e "${G6RD_STATE}/sampler.pid" ]] || {
      echo "forced sampler stop did not fail closed with diagnostic state" >&2
      exit 1
    }
    if kill -0 -- "-${sampler_pid}" 2>/dev/null; then
      echo "forced sampler stop returned with a live process group" >&2
      exit 1
    fi
  )
  rm -rf -- "${sampler_stop_fixture}"
fi
for metric in stability_sample_span_seconds stability_max_sample_gap_seconds \
  stability_valid_sample_count authorized_real_agents \
  max_production_command_inflight database_rpo_seconds database_rto_seconds; do
  grep -A2 "^  ${metric}:" "${SLO}" | grep -q 'limit:' || {
    echo "g6-slo.yaml is missing the ${metric} limit" >&2
    exit 1
  }
done

# Harness scripts must not copy frozen SLO thresholds into shell code; the
# verifier recomputes every metric from g6-slo.yaml at evaluation time.
if grep -nE '\b(900|7200)\b' "${LIB}" "${FD_A}" "${FD_B}"; then
  echo "harness scripts must not hardcode g6-slo.yaml limits" >&2
  exit 1
fi
if grep -n '0\.999' "${LIB}" "${FD_A}" "${FD_B}"; then
  echo "harness scripts must not hardcode availability ratios" >&2
  exit 1
fi

# The authority and identity fences: the lib validates the enum and the
# frozen environment id pattern, and the workflow binds both failure
# domains to the dispatched authority and candidate commit.
grep -q 'G6_AUTHORITY must be engineering or production_readiness' "${LIB}" || {
  echo "the shared lib must validate the authority enum" >&2
  exit 1
}
grep -q 'g6-\[a-z0-9\]{8,32}' "${LIB}" || {
  echo "the shared lib must validate the frozen environment id pattern" >&2
  exit 1
}
grep -qF 'shared_run="${RUN_ID%-fd-[a-b]}"' "${LIB}" || {
  echo "the environment id must derive from the shared run identity" >&2
  exit 1
}
authority_bindings="$(grep -c 'G6_AUTHORITY: ${{ inputs.authority }}' "${WORKFLOW}")"
if [[ "${authority_bindings}" -ne 3 ]]; then
  echo "both failure-domain jobs and the verifier must bind the dispatched authority (found ${authority_bindings})" >&2
  exit 1
fi
sha_bindings="$(grep -c 'G6RD_CANDIDATE_SHA: ${{ github.sha }}' "${WORKFLOW}")"
if [[ "${sha_bindings}" -ne 2 ]]; then
  echo "both failure-domain jobs must bind the candidate SHA (found ${sha_bindings})" >&2
  exit 1
fi
if grep -q 'G6RD_FAILURE_DOMAIN_CLASS' "${WORKFLOW}"; then
  echo "the workflow must not override the lib's multi-host failure-domain class" >&2
  exit 1
fi

# Rendezvous order: the load must be active before the PITR markers (RPO),
# the promotion must consume the isolation record, the crash-window and
# fault scenarios must precede the bounded window, and the frozen state
# must be collected before the bundle is built and verified.
order_of() {
  grep -n "$1" "${WORKFLOW}" | head -1 | cut -d: -f1
}
fd_a_load_wait="$(order_of 'wait-download "g6-rd-load-active')"
fd_a_pitr="$(order_of 'fd-a.sh pitr-prepare')"
if [[ -z "${fd_a_load_wait}" || -z "${fd_a_pitr}" || "${fd_a_load_wait}" -ge "${fd_a_pitr}" ]]; then
  echo "fd-a must wait for the active load before recording PITR markers" >&2
  exit 1
fi
fd_a_tunnel_up="$(order_of 'fd-a.sh tunnel-up')"
fd_a_shared_stage="$(order_of 'fd-a.sh publish-shared-secrets')"
fd_a_shared_upload="$(order_of 'name: g6-rd-shared-')"
if [[ -z "${fd_a_tunnel_up}" || -z "${fd_a_shared_stage}" || -z "${fd_a_shared_upload}" \
  || "${fd_a_tunnel_up}" -ge "${fd_a_shared_stage}" \
  || "${fd_a_shared_stage}" -ge "${fd_a_shared_upload}" ]]; then
  echo "fd-a must stage the shared trust material before publishing its rendezvous" >&2
  exit 1
fi
fd_a_enroll="$(order_of 'fd-a.sh agents-enroll')"
fd_a_peer_enrolled="$(order_of 'wait-download "g6-rd-agents-enrolled-fd-b')"
fd_a_trust_reload="$(order_of 'fd-a.sh transport-trust-reload')"
fd_a_agents_start="$(order_of 'fd-a.sh agents-start')"
fd_b_enroll="$(order_of 'fd-b.sh agents-enroll')"
fd_b_peer_trust="$(order_of 'wait-download "g6-rd-trust-ready')"
fd_b_agents_start="$(order_of 'fd-b.sh agents-start')"
if [[ -z "${fd_a_enroll}" || -z "${fd_a_peer_enrolled}" || -z "${fd_a_trust_reload}" \
  || -z "${fd_a_agents_start}" || "${fd_a_enroll}" -ge "${fd_a_peer_enrolled}" \
  || "${fd_a_peer_enrolled}" -ge "${fd_a_trust_reload}" \
  || "${fd_a_trust_reload}" -ge "${fd_a_agents_start}" ]]; then
  echo "fd-a must reload the complete cross-domain trust snapshot before starting agents" >&2
  exit 1
fi
if [[ -z "${fd_b_enroll}" || -z "${fd_b_peer_trust}" || -z "${fd_b_agents_start}" \
  || "${fd_b_enroll}" -ge "${fd_b_peer_trust}" \
  || "${fd_b_peer_trust}" -ge "${fd_b_agents_start}" ]]; then
  echo "fd-b must wait for the complete transport trust snapshot before starting agents" >&2
  exit 1
fi
fd_b_load="$(order_of 'fd-b.sh load-start')"
fd_b_isolation_wait="$(order_of 'wait-download "g6-rd-isolation')"
fd_b_promote="$(order_of 'fd-b.sh promote ')"
if [[ -z "${fd_b_load}" || -z "${fd_b_promote}" || "${fd_b_load}" -ge "${fd_b_promote}" ]]; then
  echo "the load must start before the promotion consumes the isolation record" >&2
  exit 1
fi
if [[ -z "${fd_b_isolation_wait}" || "${fd_b_isolation_wait}" -ge "${fd_b_promote}" ]]; then
  echo "the promotion must wait for the isolation record" >&2
  exit 1
fi
fd_b_window="$(order_of 'fd-b.sh window')"
fd_b_crash="$(order_of 'outbox-result-before-commit')"
fd_b_collect="$(order_of 'fd-b.sh evidence-collect')"
fd_b_build="$(order_of 'fd-b.sh evidence-build')"
fd_b_verify="$(order_of 'fd-b.sh evidence-verify')"
if [[ -z "${fd_b_crash}" || -z "${fd_b_window}" || "${fd_b_crash}" -ge "${fd_b_window}" ]]; then
  echo "every fault scenario must precede the bounded observation window" >&2
  exit 1
fi
if [[ -z "${fd_b_window}" || -z "${fd_b_collect}" || "${fd_b_window}" -ge "${fd_b_collect}" ]]; then
  echo "the bounded window must close before the evidence state is frozen" >&2
  exit 1
fi
if [[ -z "${fd_b_collect}" || -z "${fd_b_build}" || "${fd_b_collect}" -ge "${fd_b_build}" ]]; then
  echo "the frozen state must be collected before the bundle is built" >&2
  exit 1
fi
if [[ -z "${fd_b_verify}" || "${fd_b_build}" -ge "${fd_b_verify}" ]]; then
  echo "the frozen bundle must be built before its producer verification" >&2
  exit 1
fi
fd_a_ready="$(order_of 'fd-a.sh ready')"
fd_a_freeze_wait="$(order_of 'wait-download "g6-rd-final-freeze')"
fd_a_collect="$(order_of 'fd-a.sh evidence "${RUNNER_TEMP}/g6-rd-final-freeze')"
fd_b_ready_wait="$(order_of 'wait-download "g6-rd-fd-a-ready')"
fd_b_scenario="$(order_of 'fd-b.sh scenario-scheduler')"
fd_b_freeze="$(order_of 'fd-b.sh final-freeze')"
fd_b_final_wait="$(order_of 'wait-download "g6-rd-fd-a-evidence')"
fd_b_final_merge="$(order_of 'fd-b.sh merge-peer-final-evidence')"
if [[ -z "${fd_a_ready}" || -z "${fd_a_freeze_wait}" || -z "${fd_a_collect}" \
  || "${fd_a_ready}" -ge "${fd_a_freeze_wait}" || "${fd_a_freeze_wait}" -ge "${fd_a_collect}" ]]; then
  echo "fd-a must stay live until fd-b requests the final freeze" >&2
  exit 1
fi
if [[ -z "${fd_b_ready_wait}" || -z "${fd_b_scenario}" || "${fd_b_ready_wait}" -ge "${fd_b_scenario}" ]]; then
  echo "fd-b scenarios must wait for fd-a readiness" >&2
  exit 1
fi
if [[ -z "${fd_b_freeze}" || -z "${fd_b_final_wait}" || -z "${fd_b_final_merge}" \
  || "${fd_b_collect}" -ge "${fd_b_freeze}" || "${fd_b_freeze}" -ge "${fd_b_final_wait}" \
  || "${fd_b_final_wait}" -ge "${fd_b_final_merge}" || "${fd_b_final_merge}" -ge "${fd_b_build}" \
  || "${fd_b_build}" -ge "${fd_b_verify}" ]]; then
  echo "fd-b must collect final sessions, request freeze, then merge fd-a final journals before building" >&2
  exit 1
fi

# fd-b assembles the bundle from both failure domains: the builder and the
# verifier must both run there, the peer evidence must gate the build, and
# the relay scenario must consume the peer's relay-a failure stamp.
grep -q 'build-g6-evidence.mjs' "${FD_B}" || {
  echo "fd-b must run the evidence builder" >&2
  exit 1
}
grep -q 'verify-g6-evidence.mjs' "${FD_B}" || {
  echo "fd-b must verify the bundle before publishing it" >&2
  exit 1
}
grep -q 'phase_evidence_build() {' "${FD_B}" || {
  echo "fd-b must define the evidence build phase" >&2
  exit 1
}
grep -q 'phase_evidence_verify() {' "${FD_B}" || {
  echo "fd-b must define a separate evidence verification phase" >&2
  exit 1
}
grep -q 'require_file "${peer}/evidence/instances.tsv"' "${FD_B}" || {
  echo "the evidence build must require the peer container inventory" >&2
  exit 1
}
grep -q -- '--run-dir "${G6RD_WORK}"' "${FD_B}" || {
  echo "the shell producer must pass the harness run root to the evidence builder" >&2
  exit 1
}
if grep -q -- '--state-dir' "${FD_B}"; then
  echo "the evidence builder does not accept --state-dir" >&2
  exit 1
fi

# Deterministic producer wiring and causality guards found during the first
# independent harness review.
basebackup_line="$(grep -n 'pg_basebackup -h 127.0.0.1' "${FD_A}" | head -1 | cut -d: -f1)"
marker_a_line="$(grep -n "pitr-marker-a',txid_current" "${FD_A}" | head -1 | cut -d: -f1)"
if [[ -z "${basebackup_line}" || -z "${marker_a_line}" || "${basebackup_line}" -ge "${marker_a_line}" ]]; then
  echo "the PITR base backup must predate marker A and the restore target" >&2
  exit 1
fi
grep -q 'RETURNING txid.*written_at' "${FD_A}" || {
  echo "PITR markers must return their own transaction id and timestamp" >&2
  exit 1
}
source "${LIB}"
workspace_id="$(g6rd_uuidv7)"
if [[ ! "${workspace_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
  echo "the G6 workspace generator did not produce a canonical UUIDv7" >&2
  exit 1
fi
if grep -q 'uuidgen' "${FD_A}"; then
  echo "the G6 workspace must not use the UUIDv4-oriented system generator" >&2
  exit 1
fi
marker_with_tag=$'781:2026-08-17T01:02:03.123456Z\nINSERT 0 1'
if [[ "$(g6rd_extract_pitr_marker_row "${marker_with_tag}")" != "781:2026-08-17T01:02:03.123456Z" ]]; then
  echo "PITR marker parsing must discard the psql command tag" >&2
  exit 1
fi
if g6rd_extract_pitr_marker_row $'781:2026-08-17T01:02:03Z\nINSERT 0 1' >/dev/null 2>&1; then
  echo "PITR marker parsing must require exact microsecond RFC3339 shape" >&2
  exit 1
fi
marker_b_line="$(grep -n "pitr-marker-b',txid_current" "${FD_A}" | head -1 | cut -d: -f1)"
wal_switch_line="$(grep -n 'pg_walfile_name(pg_switch_wal())' "${FD_A}" | head -1 | cut -d: -f1)"
archive_wait_line="$(grep -n 'PITR target WAL archived' "${FD_A}" | head -1 | cut -d: -f1)"
if [[ -z "${marker_b_line}" || -z "${wal_switch_line}" || -z "${archive_wait_line}" \
  || "${marker_b_line}" -ge "${wal_switch_line}" || "${wal_switch_line}" -ge "${archive_wait_line}" ]]; then
  echo "PITR must switch WAL after marker B and wait for that segment in the archive" >&2
  exit 1
fi
grep -qF "sh -c 'pg_rewind -D /data --source-server=\"host=host.docker.internal port=15432 user=ocservia_replication dbname=ocservia password=\$PGPASSWORD\"" "${FD_A}" || {
  echo "the rejoin password must remain single-quoted for container expansion" >&2
  exit 1
}
rejoin_phase="$(sed -n '/^phase_rejoin() {/,/^}/p' "${FD_A}")"
grep -q -- '--add-host host.docker.internal:host-gateway' <<<"${rejoin_phase}" || {
  echo "the raw rejoin container must resolve the runner-hosted tunnel on Linux" >&2
  exit 1
}
grep -q -- '--user 999:999' <<<"${rejoin_phase}" || {
  echo "pg_rewind and pg_basebackup must run as the PostgreSQL service user" >&2
  exit 1
}
promoted_wait="$(order_of 'wait-download "g6-rd-new-primary')"
post_promotion_probe="$(order_of 'dual-primary-probes "${RUNNER_TEMP}/g6-rd-new-primary')"
if [[ -z "${promoted_wait}" || -z "${post_promotion_probe}" || "${promoted_wait}" -ge "${post_promotion_probe}" ]]; then
  echo "former-primary probes must run after the replacement promotion" >&2
  exit 1
fi
dual_primary_phase="$(sed -n '/^phase_dual_primary_probes() {/,/^}/p' "${FD_A}")"
if [[ "$(grep -c 'jq -s -e' <<<"${dual_primary_phase}")" != 2 ]] \
  || ! grep -qF 'all(.[]; .accepted == false)' <<<"${dual_primary_phase}" \
  || ! grep -qF 'all(.[]; (.at | stamp_key)' <<<"${dual_primary_phase}" \
  || ! grep -qF '[0-9]{1,9}' <<<"${dual_primary_phase}" \
  || grep -qF 'fromdateiso8601' <<<"${dual_primary_phase}"; then
  echo "former-primary JSONL probes must be slurped and fail closed" >&2
  exit 1
fi
dual_primary_precision_fixture="$(mktemp -d)"
mkdir -p "${dual_primary_precision_fixture}/outbox/isolation" \
  "${dual_primary_precision_fixture}/promoted" \
  "${dual_primary_precision_fixture}/state"
printf '%s\n' 0000000000000000000000000000000000000000000000000000000000000001 \
  >"${dual_primary_precision_fixture}/state/peer-pg-b-node-id"
printf '%s\n' '2026-08-19T10:00:00.499999999Z' \
  >"${dual_primary_precision_fixture}/promoted/promoted-at"
(
  G6RD_OUTBOX="${dual_primary_precision_fixture}/outbox"
  G6RD_STATE="${dual_primary_precision_fixture}/state"
  FD_ID="fd-a"
  export MARKER_TABLE=g6_readiness_markers
  require_file() { [[ -s "${1:?path is required}" ]]; }
  g6rd_tunnel_forward() { :; }
  g6rd_psql() { printf '%s\n' '2026-08-19T10:00:00.500000000Z'; }
  g6rd_secret() { printf '%s\n' fixture; }
  docker() { return 1; }
  sleep() { :; }
  eval "${dual_primary_phase}"
  phase_dual_primary_probes "${dual_primary_precision_fixture}/promoted"
  printf '%s\n' '2026-08-19T10:00:00.500000001Z' \
    >"${dual_primary_precision_fixture}/promoted/promoted-at"
  if phase_dual_primary_probes "${dual_primary_precision_fixture}/promoted" \
    >/dev/null 2>&1; then
    echo "a same-second dual-primary probe before promotion was accepted" >&2
    exit 1
  fi
)
rm -rf -- "${dual_primary_precision_fixture}"
control_evidence_copy="$(sed -n '/^copy_control_evidence() {/,/^}/p' "${FD_A}")"
if ! grep -q 'isolation/outage-declared-at' <<<"${control_evidence_copy}" \
  || ! grep -q 'isolation/isolated-at' <<<"${control_evidence_copy}" \
  || grep -q '\*\.at' <<<"${control_evidence_copy}"; then
  echo "fd-a control evidence must copy the exact isolation boundary files" >&2
  exit 1
fi
grep -q 'post-rejoin-probes.jsonl' "${FD_A}" || {
  echo "the rejoined former primary needs explicit read-only probes" >&2
  exit 1
}
relay_isolation_line="$(grep -n 'docker network connect --alias relay-b' \
  "${FD_B}" | cut -d: -f1)"
direct_cut_line="$(grep -n 'network disconnect "${COMPOSE_PROJECT}_default"' \
  "${FD_B}" | cut -d: -f1)"
relay_restore_line="$(grep -n 'network disconnect "${isolated_network}" "${COMPOSE_PROJECT}-relay-1"' \
  "${FD_B}" | cut -d: -f1)"
[[ -n "${relay_isolation_line}" && -n "${direct_cut_line}" \
  && -n "${relay_restore_line}" && "${relay_isolation_line}" -lt "${direct_cut_line}" \
  && "${direct_cut_line}" -lt "${relay_restore_line}" ]] || {
  echo "the path scenario must preserve relay-b while isolating only the direct bridge" >&2
  exit 1
}
grep -q 'OCSERV_TEST_RESULT_COMMIT_BARRIER_DIR' "${COMPOSE_FILE}" || {
  echo "the worker must expose the result-before-commit ingress barrier" >&2
  exit 1
}
grep -q 'agent_command_results.*== 0' "${FD_B}" || {
  echo "the result-before-commit scenario must prove the transaction stayed uncommitted" >&2
  exit 1
}
grep -q 'queued_outbox_count' "${FD_A}" || {
  echo "failure injection must freeze the due outbox population" >&2
  exit 1
}
isolate_phase="$(sed -n '/^phase_isolate() {/,/^}/p' "${FD_A}")"
if [[ "$(grep -c 'clock_timestamp()' <<<"${isolate_phase}")" -lt 3 ]] \
  || ! grep -q 'outage-declared-at' <<<"${isolate_phase}" \
  || ! grep -qF 'g6rd_tunnel_forward pg-b-forward' <<<"${isolate_phase}" \
  || ! grep -qF 'G6_DB_PORT=15432 G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql' <<<"${isolate_phase}" \
  || ! grep -qF 'isolation/rto-started-at' <<<"${isolate_phase}"; then
  echo "database RPO and RTO boundaries must retain their distinct authoritative PostgreSQL clocks" >&2
  exit 1
fi
rto_start_line="$(grep -nF 'isolation/rto-started-at' <<<"${isolate_phase}" | head -1 | cut -d: -f1)"
isolation_stop_line="$(grep -nF 'g6rd_compose stop scheduler api worker transportd' <<<"${isolate_phase}" | cut -d: -f1)"
[[ -n "${rto_start_line}" && -n "${isolation_stop_line}" \
  && "${rto_start_line}" -lt "${isolation_stop_line}" ]] || {
  echo "the same-clock RTO start must be frozen immediately before failure injection" >&2
  exit 1
}
standby_phase="$(sed -n '/^phase_standby_bootstrap() {/,/^}/p' "${FD_B}")"
grep -qF 'g6rd_tunnel_serve pg-b' <<<"${standby_phase}" || {
  echo "the standby must expose its database clock to the fault injector" >&2
  exit 1
}
grep -qF 'capture_local_database_clock >"${G6RD_STATE}/promoted-at"' "${FD_B}" || {
  echo "the RTO restoration boundary must use the promoted FD-B database clock" >&2
  exit 1
}
grep -qF 'readText(peerDir, "isolation", "rto-started-at")' "${BUILDER}" || {
  echo "the builder must trust the fault-adjacent standby-clock RTO start" >&2
  exit 1
}
grep -q 'normalizePreciseStamp' "${BUILDER}" || {
  echo "the evidence builder must preserve precise causal boundary timestamps" >&2
  exit 1
}
grep -q 'psql_primary -Atc "SELECT pg_advisory_lock' "${FD_B}" || {
  echo "the outbox dispatch barrier must lock the writable primary" >&2
  exit 1
}
grep -q 'synthetic-barrier-file' "${SUPERVISOR}" || {
  echo "real Agent execution must hold the active failure-boundary population" >&2
  exit 1
}
if grep -qE 'pg_stat_activity.*\|\| echo 0|outbox_events.*\|\| echo 0|g6rd_sampler_tick .*\|\| true' "${LIB}"; then
  echo "resource sampling must fail closed instead of substituting zero" >&2
  exit 1
fi
grep -q 'peer}/evidence/effects/' "${FD_B}" || {
  echo "fd-b must merge fd-a durable effects into the full command population" >&2
  exit 1
}
grep -q 'successful synthetic command.*has no durable effect' "${BUILDER}" || {
  echo "the builder must reject successful commands without one durable effect" >&2
  exit 1
}
grep -q 'runCommandByHex' "${BUILDER}" || {
  echo "durable effects must be checked against the run-wide synthetic population" >&2
  exit 1
}
grep -q 'does not cover exactly all managed Agents' "${BUILDER}" || {
  echo "the final effect freeze must cover every managed Agent" >&2
  exit 1
}
grep -q 'fd-a final evidence was not frozen after the bounded window' "${BUILDER}" || {
  echo "the builder must enforce final-freeze causality" >&2
  exit 1
}

# Execute the real shell evidence-build phase with a recording Node shim. This
# pins the producer-to-builder CLI boundary rather than only unit-testing the
# builder with a hand-written invocation.
producer_test="$(mktemp -d)"
trap 'rm -rf "${producer_test}"' EXIT
run_id=producer-contract-fd-b
run_dir="${producer_test}/g6-readiness-${run_id}"
peer_dir="${producer_test}/peer"
mkdir -p "${run_dir}/state/evidence" "${peer_dir}/evidence"
printf '{}\n' >"${run_dir}/state/evidence/commands.jsonl"
printf 'peer\n' >"${peer_dir}/evidence/instances.tsv"
node_shim="${producer_test}/node-shim"
cat >"${node_shim}" <<'SHIM'
#!/usr/bin/env bash
set -Eeuo pipefail
script="$1"
shift
if [[ "${script}" == */build-g6-evidence.mjs ]]; then
  if [[ "${G6RD_SHIM_BUILD_FAIL:-0}" == 1 ]]; then
    echo 'Error: fixture evidence build failed' >&2
    exit 17
  fi
  printf '%s\n' "$@" >"${G6RD_PRODUCER_ARGS}"
  while (($#)); do
    if [[ "$1" == --out-dir ]]; then
      out="$2"
      mkdir -p "${out}"
      printf '{}\n' >"${out}/evidence.json"
      printf '{}\n' >"${out}/topology.json"
      printf '{}\n' >"${out}/release-manifest.json"
      break
    fi
    shift
  done
  exit 0
fi
result=""
while (($#)); do
  if [[ "$1" == --result ]]; then
    result="$2"
    break
  fi
  shift
done
case "${G6RD_SHIM_VERIFY_MODE:-passed}" in
passed)
  [[ -z "${result}" ]] || printf '{"schema_version":"ocservia.g6-evidence-phase-result.v1","phase":"verify","status":"passed","verdict_passed":true,"exit_code":0,"failure_reasons":[]}\n' >"${result}"
  printf '{"schema_version":"ocservia.g6-verdict.v2","passed":true,"failure_reasons":[],"measurement_results":{},"observation_results":{}}\n'
  ;;
accepted_non_final)
  [[ -z "${result}" ]] || printf '{"schema_version":"ocservia.g6-evidence-phase-result.v1","phase":"verify","status":"accepted_non_final","verdict_passed":false,"exit_code":1,"failure_reasons":["final pass requires production_readiness authority"]}\n' >"${result}"
  printf '{"schema_version":"ocservia.g6-verdict.v2","passed":false,"failure_reasons":["final pass requires production_readiness authority"],"measurement_results":{},"observation_results":{}}\n'
  exit 1
  ;;
failed)
  [[ -z "${result}" ]] || printf '{"schema_version":"ocservia.g6-evidence-phase-result.v1","phase":"verify","status":"failed","verdict_passed":false,"exit_code":1,"reason":"fixture schema rejection"}\n' >"${result}"
  echo 'G6 evidence rejected: fixture schema rejection' >&2
  exit 1
  ;;
*)
  exit 99
  ;;
esac
SHIM
chmod +x "${node_shim}"
RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=production_readiness G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_PRODUCER_ARGS="${producer_test}/builder-args" \
  "${FD_B}" evidence-build "${peer_dir}"
RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=production_readiness G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_PRODUCER_ARGS="${producer_test}/builder-args" \
  "${FD_B}" evidence-verify
if [[ "$(<"${run_dir}/outbox/evidence-bundle/evidence-build-exit-code.txt")" != 0 \
  || "$(<"${run_dir}/outbox/evidence-bundle/evidence-verify-exit-code.txt")" != 0 ]]; then
  echo "the evidence producer must preserve separate successful phase exit codes" >&2
  exit 1
fi
RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=engineering G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_SHIM_VERIFY_MODE=accepted_non_final \
  "${FD_B}" evidence-verify
if [[ "$(<"${run_dir}/outbox/evidence-bundle/evidence-verify-exit-code.txt")" != 1 ]] \
  || ! jq -e '.status == "accepted_non_final"' \
    "${run_dir}/outbox/evidence-bundle/verification-result.json" >/dev/null \
  || ! jq -e '.failure_reasons == ["final pass requires production_readiness authority"]' \
    "${run_dir}/outbox/evidence-bundle/verdict.json" >/dev/null; then
  echo "the engineering authority fence was not accepted as non-final evidence" >&2
  exit 1
fi
verify_failure_log="${producer_test}/verify-failure.log"
if RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=engineering G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_SHIM_VERIFY_MODE=failed \
  "${FD_B}" evidence-verify >"${verify_failure_log}" 2>&1; then
  echo "the engineering schema-failure fixture unexpectedly passed" >&2
  exit 1
fi
if ! jq -e '.status == "failed" and .reason == "fixture schema rejection"' \
    "${run_dir}/outbox/evidence-bundle/verification-result.json" >/dev/null \
  || [[ -e "${run_dir}/outbox/evidence-bundle/verdict.json" ]] \
  || grep -Eq 'jq:|verdict.json.*(No such file|Could not open)' "${verify_failure_log}"; then
  echo "the verifier schema failure did not remain structured and noise-free" >&2
  exit 1
fi
rm -rf "${run_dir}/outbox/evidence-bundle"
if RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=production_readiness G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_PRODUCER_ARGS="${producer_test}/builder-args" \
  G6RD_SHIM_BUILD_FAIL=1 "${FD_B}" evidence-build "${peer_dir}" \
  >/dev/null 2>&1; then
  echo "the evidence build failure fixture unexpectedly passed" >&2
  exit 1
fi
if [[ "$(<"${run_dir}/outbox/evidence-bundle/evidence-build-exit-code.txt")" != 17 ]] \
  || ! jq -e '.phase == "build" and .status == "failed" and .exit_code == 17 and .reason == "fixture evidence build failed"' \
    "${run_dir}/outbox/evidence-bundle/verification-result.json" >/dev/null \
  || [[ ! -s "${run_dir}/outbox/evidence-bundle/build.stderr.log" ]]; then
  echo "the failed evidence build did not preserve its exact structured diagnostics" >&2
  exit 1
fi
previous=""
producer_run_dir=""
while IFS= read -r argument; do
  [[ "${previous}" == --run-dir ]] && producer_run_dir="${argument}"
  previous="${argument}"
done <"${producer_test}/builder-args"
if [[ "${producer_run_dir}" != "${run_dir}" ]]; then
  echo "the executed shell producer did not pass --run-dir <work-root>" >&2
  exit 1
fi
grep -q 'scenario-relay "${RUNNER_TEMP}/g6-rd-fd-a-ready"' "${WORKFLOW}" || {
  echo "the relay scenario must consume the peer readiness evidence" >&2
  exit 1
}

# The independent verifier recomputes the environment identity from the run
# identity itself, evaluates against the dispatched authority, and must not
# drift from the verdict published with the bundle.
grep -q 'g6-rd-verifier' "${WORKFLOW}" || {
  echo "the workflow must run the independent verifier job" >&2
  exit 1
}
grep -qF 'printf '\''%s'\'' "${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" | openssl dgst -sha256 -r | cut -c1-16' "${WORKFLOW}" || {
  echo "the verifier job must recompute the environment id from the run identity" >&2
  exit 1
}
grep -q -- '--expected-authority "${G6_AUTHORITY}"' "${WORKFLOW}" || {
  echo "the verifier job must evaluate the dispatched authority" >&2
  exit 1
}
grep -q 'the independent verdict differs from the bundle verdict' "${WORKFLOW}" || {
  echo "the verifier job must compare its verdict with the bundle verdict" >&2
  exit 1
}
grep -q 'the independent verifier rejected the production-readiness bundle' "${WORKFLOW}" || {
  echo "the verifier job must fail closed on a rejected production bundle" >&2
  exit 1
}

# The secret scan is an independent job over the published evidence, run
# with the pinned redacting scanner rather than the git-history scan.
grep -q 'gitleaks dir --no-banner --redact --no-color "${RUNNER_TEMP}/g6-rd-evidence-bundle"' "${WORKFLOW}" || {
  echo "the secret-scan job must scan the published bundle with redaction" >&2
  exit 1
}
grep -q 'gitleaks dir --no-banner --redact --no-color "${RUNNER_TEMP}/g6-rd-fd-a-evidence"' "${WORKFLOW}" || {
  echo "the secret-scan job must scan the published failure-domain evidence" >&2
  exit 1
}

# The evidence builder is under the same test regime as the verifier: the
# contracts job must exercise it on every pull request.
grep -q 'test-g6-evidence-builder.mjs' "${ROOT}/scripts/docs-check.sh" || {
  echo "docs-check.sh must run the evidence builder test" >&2
  exit 1
}

# The builder itself must stay a pure transcriber bound to the frozen
# contract library: no local metric thresholds, no authority shortcuts.
if grep -nE '\b(900|7200)\b|0\.999' "${BUILDER}"; then
  echo "the evidence builder must not embed SLO thresholds" >&2
  exit 1
fi
grep -q 'from "./g6-contract-lib.mjs"' "${BUILDER}" || {
  echo "the builder must derive measurements through the shared contract library" >&2
  exit 1
}
for token in \
  'scheduler-maintenance-observation.json' \
  'scheduler-replacement-term' \
  'scheduler maintenance marker completed after it was observed committed' \
  'schedulerMaintenanceCommittedObservedAtMicros' \
  'marker_completed_at: normalizedCompletedAt'; do
  grep -qF "${token}" "${BUILDER}" || {
    echo "the evidence builder does not bind the committed scheduler observation: ${token}" >&2
    exit 1
  }
done
for token in \
  '"marker_completed_at"' \
  'markerCompletedAtNs > parsedNs' \
  'completion.markerCompletedAtNs ==='; do
  grep -qF "${token}" "${CONTRACT}" || {
    echo "the G6 contract does not preserve the scheduler marker/observation boundary: ${token}" >&2
    exit 1
  }
done

# The era model keeps one controller identity across the promotion: fd-b
# must import the peer controller key, never generate its own, and fd-a must
# include it in the short-lived shared-trust handoff consumed before startup.
grep -q 'fd-b never generates its own controller key' "${FD_B}" || {
  echo "fd-b must document the controller key handover" >&2
  exit 1
}
sed -n '/^phase_publish_shared_secrets() {/,/^}/p' "${FD_A}" \
  | grep -q 'cp -f "${G6RD_SECRETS}/controller.key" "${G6RD_OUTBOX}/shared/"' || {
  echo "fd-a must hand the controller key through the shared-trust rendezvous" >&2
  exit 1
}

# The evidence bundle published with long retention must stay
# credential-free: the fd-a final bundle draws only from the isolation
# record, the PITR stamps, and container inventory.
grep -q 'phase_evidence() {' "${FD_A}" || {
  echo "fd-a must define the evidence phase" >&2
  exit 1
}
grep -q 'No credentials enter this bundle' "${FD_A}" || {
  echo "the fd-a evidence bundle must document its credential-free scope" >&2
  exit 1
}

# Every artifact name the workflow waits on must pass the shared artifact
# helper's allowlist.
while IFS= read -r wait_name; do
  concrete="${wait_name//\$\{GITHUB_RUN_ID\}/424242}"
  concrete="${concrete//\$\{GITHUB_RUN_ATTEMPT\}/1}"
  GITHUB_RUN_ID=424242 GITHUB_RUN_ATTEMPT=1 \
    "${ROOT}/scripts/real-e2e-artifact.sh" validate-name "${concrete}" || {
      echo "workflow artifact name is rejected by the shared validator: ${concrete}" >&2
      exit 1
    }
done < <(grep -oE 'wait-download "[^"]+"' "${WORKFLOW}" | sed 's/^wait-download "//; s/"$//' | sort -u)

# Every pinned action must reuse an exact `uses: action@sha` line already
# proven by a merged workflow with hosted execution.
proven_pins="$(grep -hoE 'uses: [^@]+@[0-9a-f]{40}' \
  "${ROOT}/.github/workflows/ci.yml" \
  "${ROOT}/.github/workflows/p1-capacity.yml" \
  "${ROOT}/.github/workflows/real-e2e.yml" | sort -u)"
while IFS= read -r pin; do
  grep -qxF "${pin}" <<<"${proven_pins}" || {
    echo "pinned action ${pin} is not proven by any existing hosted workflow" >&2
    exit 1
  }
done < <(grep -hoE 'uses: [^@]+@[0-9a-f]{40}' "${WORKFLOW}" | sort -u)

# A policy failure can invoke cleanup before prepare has created any secrets.
# Exercise that path with a recording Docker shim: cleanup must use inert
# placeholders, remain scoped, and remove its run directory successfully.
cleanup_test="$(mktemp -d)"
mkdir -p "${cleanup_test}/bin" "${cleanup_test}/runner-temp"
cat >"${cleanup_test}/bin/docker" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "compose "*)
    for name in G6_FD_ID G6_OWNER_PASSWORD G6_APP_PASSWORD \
      G6_REPLICATION_PASSWORD G6_DEV_AUTH_TOKEN G6_SIGNING_DIR G6_RELAY_DIR; do
      [[ -n "${!name:-}" ]] || {
        echo "cleanup compose environment is missing ${name}" >&2
        exit 1
      }
    done
    if [[ " $* " == *" agents-fd-a.yaml "* ]]; then
      for index in 01 02; do
        for dir in identity journal privd secrets state; do
          [[ -d "${G6RD_TEST_AGENT_ROOT}/agent-fd-a-${index}/${dir}" ]] || {
            echo "agent cleanup bind directory is missing: agent-fd-a-${index}/${dir}" >&2
            exit 1
          }
        done
      done
    fi
    ;;
  "volume inspect" | "network inspect") exit 1 ;;
  "images --format") exit 0 ;;
  "run --rm")
    [[ " $* " == *" --pull=never "* ]] || {
      echo "cleanup attempted a network-capable docker run" >&2
      exit 1
    }
    exit 0
    ;;
esac
exit 0
SHIM
chmod +x "${cleanup_test}/bin/docker"
(
  export PATH="${cleanup_test}/bin:${PATH}"
  export RUNNER_TEMP="${cleanup_test}/runner-temp"
  export RUN_ID=cleanup-before-prepare-fd-a
  export FD_ID=fd-a
  export FD_ALIAS=fd-alpha
  export G6_AUTHORITY=engineering
  export G6RD_CANDIDATE_SHA=5f9a2a943d7aa38224bc3266b7176f0a061a6b6c
  export G6_AGENTS_A=2
  source "${LIB}"
  g6rd_init_environment
  work="${G6RD_WORK}"
  g6rd_placeholder_env
  g6rd_write_agent_overlay "$(g6rd_agent_count)"
  unset G6_FD_ID G6_OWNER_PASSWORD G6_APP_PASSWORD G6_REPLICATION_PASSWORD \
    G6_DEV_AUTH_TOKEN G6_SIGNING_DIR G6_RELAY_DIR
  rm -rf "${G6RD_AGENTS}"
  export G6RD_TEST_AGENT_ROOT="${G6RD_AGENTS}"
  g6rd_cleanup
  [[ ! -e "${work}" ]]
)
rm -rf "${cleanup_test}"

"${ROOT}/scripts/test-real-e2e-artifact.sh"
shellcheck "${LIB}" "${FD_A}" "${FD_B}" "${SUPERVISOR}" \
  "${ROOT}/scripts/real-e2e-artifact.sh" \
  "${ROOT}/scripts/test-real-e2e-artifact.sh" "${POSTGRES_INIT}"
echo "g6-readiness policy checks passed"
