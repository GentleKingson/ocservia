#!/usr/bin/env bash
# shellcheck disable=SC1090,SC2016,SC2030,SC2031,SC2329
# This test sources a path variable, matches literal expansions, and isolates
# environment mutations in subshell fixtures.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-readiness.yml"
COMPOSE_FILE="${ROOT}/deploy/g6-readiness/compose.yaml"
SUPERVISOR="${ROOT}/deploy/g6-readiness/agent-supervisor.sh"
AGENT_MAIN="${ROOT}/rust/crates/agent/src/main.rs"
LIB="${ROOT}/scripts/g6-readiness-lib.sh"
FD_A="${ROOT}/scripts/g6-readiness-fd-a.sh"
FD_B="${ROOT}/scripts/g6-readiness-fd-b.sh"
BUILDER="${ROOT}/scripts/build-g6-evidence.mjs"
SLO="${ROOT}/docs/acceptance/g6-slo.yaml"
PROBE_DOCKERFILE="${ROOT}/rust/g6-probe.Dockerfile"
TRANSPORT_DOCKERFILE="${ROOT}/rust/transportd.Dockerfile"
RELAY_DOCKERFILE="${ROOT}/deploy/production/relay.Dockerfile"
POSTGRES_INIT="${ROOT}/deploy/g6-readiness/postgres-init/001-g6-readiness.sh"
OCSERV_FIXTURE="${ROOT}/deploy/g6-readiness/fake-ocserv/shims/ocserv"

ruby -r yaml - "${WORKFLOW}" "${COMPOSE_FILE}" <<'RUBY'
workflow_path, compose_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
compose = YAML.safe_load(File.read(compose_path), aliases: true)

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
release_job = jobs.fetch("g6-rd-release-image")
release_steps = Array(release_job.fetch("steps"))
release_build = release_steps.find { |step| step["name"] == "Build and freeze the release Agent image" }
release_upload = release_steps.find { |step| step["name"] == "Publish the frozen release Agent image" }
release_cleanup = release_steps.find { |step| step["name"] == "Clean release-image resources" }
reject("the release Agent image must be candidate-labeled and exported once") unless release_build&.fetch("run")&.include?("org.opencontainers.image.revision=${GITHUB_SHA}") && release_build.fetch("run").include?("docker save") && release_build.fetch("run").include?("sha256sum agent-image.tar.gz")
reject("the release Agent image artifact must be run scoped") unless release_upload&.fetch("with")&.fetch("name")&.include?("github.run_id") && release_upload.fetch("with").fetch("name").include?("github.run_attempt")
reject("the release Agent image archive must use the step-scoped runner temp directory") unless release_upload.fetch("with").fetch("path").include?("runner.temp") && release_build.fetch("run").include?("RUNNER_TEMP")
reject("the release Agent image producer must clean its scoped image") unless release_cleanup&.fetch("if") == "always()" && release_cleanup.fetch("timeout-minutes") == 5
release_image = release_job.fetch("env").fetch("G6RD_AGENT_IMAGE")
%w[g6-rd-fd-a g6-rd-fd-b].each do |job_id|
  reject("#{job_id} must depend only on the shared release image") unless jobs.fetch(job_id).fetch("needs") == "g6-rd-release-image"
  reject("#{job_id} must use the producer's exact release image tag") unless jobs.fetch(job_id).fetch("env").fetch("G6RD_AGENT_IMAGE") == release_image
  steps = Array(jobs.fetch(job_id).fetch("steps"))
  names = steps.map { |step| step["name"] }.compact
  download = steps.find { |step| step["name"] == "Download the frozen release Agent image" }
  load = steps.find { |step| step["name"] == "Verify and load the release Agent image" }
  reject("#{job_id} must download the exact run-scoped release image") unless download&.fetch("with")&.fetch("name") == release_upload.fetch("with").fetch("name")
  reject("#{job_id} must verify, load, and candidate-bind the release image") unless load&.fetch("run")&.include?("sha256sum --check") && load.fetch("run").include?("docker load") && load.fetch("run").include?("org.opencontainers.image.revision") && load.fetch("run").include?("GITHUB_SHA")
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
reject("fd-a must use the trust-independent image build phase") unless fd_a_steps.any? { |step| step["run"] == "scripts/g6-readiness-fd-a.sh build-images" }
build_order = [
  "Wait for failure domain A rendezvous",
  "Import the peer tunnel identities",
  "Build failure domain B images and tunnel",
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
  reject("#{job_id} must require both failure-domain conclusions") unless condition.include?("needs.g6-rd-fd-a.result == 'success'") && condition.include?("needs.g6-rd-fd-b.result == 'success'")
end

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
grep -q 'g6rd_install_controller_key' <<<"${promote_phase}" || {
  echo "fd-b must install the handed-over controller key before promotion" >&2
  exit 1
}
node_connection_probe="$(sed -n '/^g6rd_probe_node_connection() {/,/^}/p' "${LIB}")"
grep -q 'timeout --foreground --signal=TERM --kill-after=5s' \
  <<<"${node_connection_probe}" || {
  echo "node connection probes must have a per-attempt hard timeout" >&2
  exit 1
}
relay_connection_probe="$(sed -n '/^relay_probe_relay_b() {/,/^}/p' "${FD_B}")"
grep -q '\.observations\[0\]\.path == "relay"' \
  <<<"${relay_connection_probe}" || {
  echo "relay failover must read the node-connection observations contract" >&2
  exit 1
}

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
  g6rd_node_revision() { printf '7\n'; }
  g6rd_now() { printf '2026-08-17T00:00:00Z\n'; }
  g6rd_secret() { printf 'test-development-token\n'; }
  g6rd_api_port() { printf '18080\n'; }
  curl() {
    local output="" arguments=("$@")
    printf '%s\n' "${arguments[@]}" >"${enqueue_test}/curl.args"
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
    if [[ "${G6RD_TEST_CURL_MODE}" == accepted ]]; then
      printf '%s\n' '{"command_id":"018f2f10-7abc-7def-8abc-3123456789ab"}' >"${output}"
      printf '202 0.125'
    else
      printf '%s\n' '{"type":"https://ocservia.dev/problems/expected-version-required","detail":"provide a quoted revision"}' >"${output}"
      printf '400 0.050'
    fi
  }

  G6RD_TEST_CURL_MODE=accepted
  g6rd_enqueue_command 018f2f10-7abc-7def-8abc-0123456789ab test-key
  grep -Fxq 'If-Match: "revision-7"' "${enqueue_test}/curl.args" || {
    echo "synthetic enqueue did not send the API's quoted revision ETag" >&2
    exit 1
  }
  grep -Fxq '{"kind":"noop"}' "${enqueue_test}/curl.args" || {
    echo "synthetic enqueue did not send the operations API's noop kind" >&2
    exit 1
  }
  jq -e '.status == 202 and .command_id != ""' \
    "${enqueue_test}/enqueue-log.jsonl" >/dev/null

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
grep -A5 '^g6rd_reclaim_directory()' "${LIB}" | grep -q -- '--pull=never' || {
  echo "cleanup ownership reclaim must not pull an image" >&2
  exit 1
}
grep -q '^g6rd_cleanup_bounded()' "${LIB}" || {
  echo "the shared cleanup path must enforce an overall hard timeout" >&2
  exit 1
}
grep -q 'cleanup) g6rd_cleanup_bounded' "${FD_A}" || {
  echo "failure domain A must use the bounded cleanup entry point" >&2
  exit 1
}
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

# A connection fence is bound to the target Agent's authenticated Iroh
# endpoint, not to the controller endpoint the Agent dials. Both stale-owner
# probes must therefore derive the endpoint from the selected node inventory.
owner_phase="$(sed -n '/^phase_scenario_owner() {/,/^}/p' "${FD_B}")"
owner_replaced="$(sed -n '/^owner_replaced() {/,/^}/p' "${FD_B}")"
node_service_helper="$(sed -n '/^node_service() {/,/^}/p' "${FD_B}")"
grep -qF -- '-v id="${node_id}"' <<<"${node_service_helper}" || {
  echo "the owner scenario node-to-service lookup must bind its awk id value" >&2
  exit 1
}
grep -qF 'prefix="g6-${FD_ID}-"' <<<"${owner_phase}" || {
  echo "the owner scenario must select Agents local to the failure-domain runner" >&2
  exit 1
}
grep -qF 'node_hex="${node//-/}"' <<<"${owner_phase}" || {
  echo "the owner scenario must convert UUID text to the bytea fencing identity" >&2
  exit 1
}
grep -qF "WHERE node_id=decode('\${node_hex}','hex') AND lease_until>clock_timestamp()" \
  <<<"${owner_phase}" || {
  echo "the owner scenario must select only current authoritative bytea owner rows" >&2
  exit 1
}
if grep -qF "WHERE node_id='\${node}'" <<<"${owner_phase}"; then
  echo "the owner scenario must not compare a bytea node id with UUID text" >&2
  exit 1
fi
if grep -qF 'selected == 5' <<<"${owner_phase}"; then
  echo "the owner scenario must not truncate local candidates before owner matching" >&2
  exit 1
fi
grep -qF 'if ((sample_count == 5)); then' <<<"${owner_phase}" || {
  echo "the owner scenario must stop only after five current owner rows match" >&2
  exit 1
}
grep -qF 'g6rd_agent_compose restart "${reconnect_services[@]}"' <<<"${owner_phase}" || {
  echo "the replacement owner must be driven by explicit Agent reconnects" >&2
  exit 1
}
owner_worker_line="$(grep -n 'replacement worker trust socket' <<<"${owner_phase}" | cut -d: -f1)"
owner_reconnect_line="$(grep -n 'g6rd_agent_compose restart' <<<"${owner_phase}" | cut -d: -f1)"
owner_epoch_wait_line="$(grep -n 'replacement owner registered higher epochs' <<<"${owner_phase}" | cut -d: -f1)"
[[ -n "${owner_worker_line}" && -n "${owner_reconnect_line}" \
  && -n "${owner_epoch_wait_line}" && "${owner_worker_line}" -lt "${owner_reconnect_line}" \
  && "${owner_reconnect_line}" -lt "${owner_epoch_wait_line}" ]] || {
  echo "Agent reconnects must follow replacement-worker readiness and precede the epoch barrier" >&2
  exit 1
}
if grep -qF 'g6rd_enqueue_command' <<<"${owner_phase}"; then
  echo "enqueueing work must not stand in for an owner-session reconnect" >&2
  exit 1
fi
grep -qF '((current_epoch > old_epoch))' <<<"${owner_replaced}" || {
  echo "owner replacement must advance every sampled node beyond its prior epoch" >&2
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

# Watchers remain live through slow collection. The API is frozen before the
# final observations, and two independently verified transport inventories
# bracket one DB-clock authority cut. Watchers stop at the cut, before the
# second inventory, so post-cut renewals cannot redefine the evidence state.
collect_phase="$(sed -n '/^phase_evidence_collect() {/,/^}/p' "${FD_B}")"
writer_quiesce="$(sed -n '/^quiesce_control_plane_writers() {/,/^}/p' "${FD_B}")"
ingress_quiesce="$(sed -n '/^quiesce_transport_ingress() {/,/^}/p' "${FD_B}")"
renewer_quiesce="$(sed -n '/^quiesce_authority_renewers() {/,/^}/p' "${FD_B}")"
authority_cut="$(sed -n '/^capture_final_authority_cut() {/,/^}/p' "${FD_B}")"
session_assert="$(sed -n '/^assert_final_session_authority() {/,/^}/p' "${FD_B}")"
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
  'HH24:MI:SS.US\"Z\"' \
  'fencing.lease_until>cut.at' \
  'leadership.lease_until>cut.at' \
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
  '.owner_lease_until | epoch_seconds' \
  'cmp -s "${before_terms}" "${after_terms}"' \
  'cmp -s "${session_authority}" "${authority_terms}"'; do
  grep -qF "${token}" <<<"${session_assert}" || {
    echo "final session authority validation is missing: ${token}" >&2
    exit 1
  }
done
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
window_phase="$(sed -n '/^phase_window() {/,/^}/p' "${FD_B}")"
window_wait_helper="$(sed -n '/^wait_for_window_enqueue_wave() {/,/^}/p' "${FD_B}")"
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
window_default="$(grep -oE 'G6RD_WINDOW_SECONDS:-[0-9]+' "${FD_B}" | head -1 | cut -d: -f2- | tr -d ':-')"
if [[ -z "${window_default}" || "${window_default}" -le "$(slo_limit stability_sample_span_seconds)" ]]; then
  echo "the observation window default (${window_default:-unset}) must exceed the SLO span limit" >&2
  exit 1
fi
sampler_sleep="$(grep -oE 'sleep [0-9]+' "${LIB}" | awk '{print $2}' | sort -n | head -1)"
if [[ -z "${sampler_sleep}" || "${sampler_sleep}" -gt "$(slo_limit stability_max_sample_gap_seconds)" ]]; then
  echo "the sampler cadence (${sampler_sleep:-unset}s) violates the SLO max sample gap" >&2
  exit 1
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
  || ! grep -qF 'all(.[]; (.at | fromdateiso8601)' <<<"${dual_primary_phase}"; then
  echo "former-primary JSONL probes must be slurped and fail closed" >&2
  exit 1
fi
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
if [[ "$(grep -c 'clock_timestamp()' <<<"${isolate_phase}")" != 2 ]] \
  || ! grep -q 'outage-declared-at' <<<"${isolate_phase}"; then
  echo "the active-load and outage boundary must use the same precise PostgreSQL clock" >&2
  exit 1
fi
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
  echo "the final effect freeze must cover all 55 managed Agents" >&2
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
[[ -z "${result}" ]] || printf '{"schema_version":"ocservia.g6-evidence-phase-result.v1","phase":"verify","status":"passed","verdict_passed":true,"exit_code":0,"failure_reasons":[]}\n' >"${result}"
printf '{"schema_version":"ocservia.g6-verdict.v2","passed":true,"failure_reasons":[],"measurement_results":{},"observation_results":{}}\n'
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
grep -q 'scenario-relay "${RUNNER_TEMP}/g6-rd-fd-a-ready/relay-a-failed-at"' "${WORKFLOW}" || {
  echo "the relay scenario must consume the peer relay-a failure stamp" >&2
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
