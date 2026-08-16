#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-readiness.yml"
COMPOSE_FILE="${ROOT}/deploy/g6-readiness/compose.yaml"
SUPERVISOR="${ROOT}/deploy/g6-readiness/agent-supervisor.sh"
LIB="${ROOT}/scripts/g6-readiness-lib.sh"
FD_A="${ROOT}/scripts/g6-readiness-fd-a.sh"
FD_B="${ROOT}/scripts/g6-readiness-fd-b.sh"
BUILDER="${ROOT}/scripts/build-g6-evidence.mjs"
SLO="${ROOT}/docs/acceptance/g6-slo.yaml"

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

jobs = workflow.fetch("jobs")
expected = %w[g6-rd-fd-a g6-rd-fd-b g6-rd-secret-scan g6-rd-verifier]
reject("G6 readiness must contain exactly the four harness jobs") unless jobs.keys.sort == expected.sort
jobs.each do |job_id, job|
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} must stay within the bounded window") unless job.fetch("timeout-minutes") <= (job_id.start_with?("g6-rd-fd-") ? 90 : 20)
  reject("#{job_id} Action is not pinned to a full SHA") if Array(job.fetch("steps")).any? { |step| step.key?("uses") && !step.fetch("uses").match?(/@[0-9a-f]{40}\z/) }
  reject("#{job_id} must not force a failing check green") if Array(job.fetch("steps")).any? { |step| step.key?("run") && step.fetch("run").include?("continue-on-error") }
  environment = job.fetch("environment").fetch("name")
  reject("#{job_id} must gate both authorities through GitHub environments") unless environment.include?("g6-production-readiness") && environment.include?("g6-engineering-rehearsal") && environment.include?("inputs.authority")
end
%w[g6-rd-fd-a g6-rd-fd-b].each do |job_id|
  reject("#{job_id} must run concurrently with its peer") if jobs.fetch(job_id).key?("needs")
  names = Array(jobs.fetch(job_id).fetch("steps")).map { |step| step["name"] }.compact
  reject("#{job_id} must collect diagnostics before cleanup") unless names.index { |n| n.include?("diagnostics") }.to_i < names.index { |n| n.include?("Clean") }.to_i
end
%w[g6-rd-secret-scan g6-rd-verifier].each do |job_id|
  reject("#{job_id} must depend on the bundle publisher") unless jobs.fetch(job_id).fetch("needs") == ["g6-rd-fd-b"]
end

services = compose.fetch("services")
required = %w[postgres migrate api worker scheduler transportd
              controller-key-init transport-runtime-init transport-endpoint-bootstrap
              relay g6-probe]
reject("G6 readiness compose service set is incomplete") unless (required - services.keys).empty?
reject("postgres must receive stop signals directly so fencing leaves a clean data directory") unless services.fetch("postgres").fetch("init") == false
roles = %w[api worker scheduler].to_h { |role| [role, services.fetch(role).fetch("command").fetch(0)] }
reject("control-plane roles must be split") unless roles == {"api" => "--role=api", "worker" => "--role=worker", "scheduler" => "--role=scheduler"}
reject("worker must own the trust socket") unless services.fetch("worker").fetch("environment").key?("OCSERV_TRUST_SOCKET")
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

# The fault-free observation window and the sampler cadence are harness
# margins over the frozen SLO limits, so they must demonstrably clear them.
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
  || "${fd_b_final_wait}" -ge "${fd_b_final_merge}" || "${fd_b_final_merge}" -ge "${fd_b_build}" ]]; then
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
promoted_wait="$(order_of 'wait-download "g6-rd-new-primary')"
post_promotion_probe="$(order_of 'dual-primary-probes "${RUNNER_TEMP}/g6-rd-new-primary')"
if [[ -z "${promoted_wait}" || -z "${post_promotion_probe}" || "${promoted_wait}" -ge "${post_promotion_probe}" ]]; then
  echo "former-primary probes must run after the replacement promotion" >&2
  exit 1
fi
grep -q 'post-rejoin-probes.jsonl' "${FD_A}" || {
  echo "the rejoined former primary needs explicit read-only probes" >&2
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
  printf '%s\n' "$@" >"${G6RD_PRODUCER_ARGS}"
  while (($#)); do
    if [[ "$1" == --out-dir ]]; then
      out="$2"
      mkdir -p "${out}"
      : >"${out}/evidence.json"
      : >"${out}/topology.json"
      : >"${out}/release-manifest.json"
      break
    fi
    shift
  done
  exit 0
fi
printf '{"schema_version":"ocservia.g6-verdict.v2"}\n'
SHIM
chmod +x "${node_shim}"
RUNNER_TEMP="${producer_test}" RUN_ID="${run_id}" FD_ID=fd-b FD_ALIAS=fd-beta \
  G6_AUTHORITY=production_readiness G6RD_CANDIDATE_SHA="$(printf 'a%.0s' {1..40})" \
  G6RD_NODE_BIN="${node_shim}" G6RD_PRODUCER_ARGS="${producer_test}/builder-args" \
  "${FD_B}" evidence-build "${peer_dir}"
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
# must import the peer controller key, never generate its own, and fd-a
# hands it over only through the 1-day primary rendezvous artifact.
grep -q 'fd-b never generates its own controller key' "${FD_B}" || {
  echo "fd-b must document the controller key handover" >&2
  exit 1
}
grep -q 'cp -f "${G6RD_SECRETS}/controller.key" "${G6RD_OUTBOX}/primary-up/controller.key"' "${FD_A}" || {
  echo "fd-a must hand the controller key through the primary rendezvous" >&2
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
    ;;
  "volume inspect" | "network inspect") exit 1 ;;
  "images --format") exit 0 ;;
  "run --rm") exit 0 ;;
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
  source "${LIB}"
  g6rd_init_environment
  work="${G6RD_WORK}"
  g6rd_cleanup
  [[ ! -e "${work}" ]]
)
rm -rf "${cleanup_test}"

shellcheck "${LIB}" "${FD_A}" "${FD_B}" "${SUPERVISOR}" "${ROOT}/scripts/real-e2e-artifact.sh"
echo "g6-readiness policy checks passed"
