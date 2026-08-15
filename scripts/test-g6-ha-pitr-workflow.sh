#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-ha-pitr.yml"
COMPOSE_FILE="${ROOT}/deploy/g6-ha-pitr/compose.yaml"
LIB="${ROOT}/scripts/g6-ha-pitr-lib.sh"
FD_A="${ROOT}/scripts/g6-ha-pitr-fd-a.sh"
FD_B="${ROOT}/scripts/g6-ha-pitr-fd-b.sh"
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
reject("G6 HA must remain workflow_dispatch-only") unless trigger.keys == ["workflow_dispatch"]
reject("G6 HA permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read", "actions" => "read"}
jobs = workflow.fetch("jobs")
reject("G6 HA must contain exactly two failure-domain jobs") unless jobs.keys.sort == %w[g6-ha-fd-a g6-ha-fd-b]
jobs.each do |job_id, job|
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} timeout must stay within the bounded window") unless (job.fetch("timeout-minutes") .. 45).cover?(45) && job.fetch("timeout-minutes") <= 45
  reject("#{job_id} must run concurrently with its peer") if job.key?("needs")
  Array(job.fetch("steps")).each do |step|
    if step.key?("uses")
      reject("#{job_id} Action is not pinned to a full SHA") unless step.fetch("uses").match?(/@[0-9a-f]{40}\z/)
    end
    next unless step.key?("run")
    reject("#{job_id} must not force a failing cleanup green") if step.fetch("run").include?("continue-on-error")
  end
  names = Array(job.fetch("steps")).map { |step| step["name"] }.compact
  reject("#{job_id} must collect diagnostics before cleanup") unless names.index { |n| n.include?("diagnostics") }.to_i < names.index { |n| n.include?("Clean") }.to_i
end

services = compose.fetch("services")
required = %w[postgres migrate api worker scheduler transportd
              controller-key-init transport-runtime-init transport-endpoint-bootstrap]
reject("G6 HA compose service set is incomplete") unless (required - services.keys).empty?
roles = %w[api worker scheduler].to_h { |role| [role, services.fetch(role).fetch("command").fetch(0)] }
reject("control-plane roles must be split") unless roles == {"api" => "--role=api", "worker" => "--role=worker", "scheduler" => "--role=scheduler"}
reject("worker must own the trust socket") unless services.fetch("worker").fetch("environment").key?("OCSERV_TRUST_SOCKET")
reject("postgres must publish only loopback") unless services.fetch("postgres").fetch("ports") == ["127.0.0.1:5432:5432"]
reject("postgres must run data checksums") unless services.fetch("postgres").fetch("environment").fetch("POSTGRES_INITDB_ARGS").include?("data-checksums")
RUBY

# Harness scripts must read the frozen limits from g6-slo.yaml instead of
# copying thresholds into shell code.
if grep -nE 'rpo|rtol|rto' "${LIB}" | grep -vE 'g6_ha_slo_limit|database_r(to|po)_seconds'; then
  echo "harness scripts must not embed RPO/RTO thresholds" >&2
  exit 1
fi
if grep -nE '\b(900|7200)\b' "${LIB}" "${FD_A}" "${FD_B}"; then
  echo "harness scripts must not hardcode g6-slo.yaml limits" >&2
  exit 1
fi
for metric in database_rpo_seconds database_rto_seconds; do
  grep -A2 "  ${metric}:" "${SLO}" | grep -q 'limit:' || {
    echo "g6-slo.yaml is missing the ${metric} limit" >&2
    exit 1
  }
done

# Required observation events from g6-slo.yaml that this stage genuinely
# observes must all be produced by the harness timeline or its phase
# scripts. The load bracket (load_started/load_stopped) belongs to the
# later real-agent run: this stage's failover happens after the marker load
# completed, so emitting those events would misrepresent the final
# database_failover_during_load observation.
for event in primary_failure_injected new_primary_writable \
  api_recovered worker_recovered old_primary_isolated \
  new_primary_promoted old_primary_write_rejected marker_a_written \
  restore_point_created marker_b_written restore_verified \
  old_primary_rejoined; do
  grep -q "${event}" "${FD_B}" || {
    echo "timeline event ${event} is not produced by the harness" >&2
    exit 1
  }
done
for forbidden in load_started load_stopped; do
  if grep -q "timeline_event ${forbidden}" "${FD_B}"; then
    echo "stage 6 must not emit ${forbidden}: the failover does not overlap a live load" >&2
    exit 1
  fi
done

# fd-b talks to a clone of fd-a's cluster, so it must import the peer's
# cluster credentials before any local client runs; its own random
# passwords would deterministically fail authentication.
grep -q "g6_ha_import_peer_cluster_credentials \"\${peer}\"" "${FD_B}" || {
  echo "fd-b must import the peer cluster credentials during standby bootstrap" >&2
  exit 1
}
grep -q "g6_ha_import_peer_cluster_credentials()" "${LIB}" || {
  echo "the shared lib must define the peer credential import helper" >&2
  exit 1
}

# The environment binding must satisfy the frozen verifier pattern and be
# derived from the shared run identity, not a per-job constant.
grep -q 'g6-\[a-z0-9\]{8,32}' "${LIB}" || {
  echo "the shared lib must validate the frozen environment id pattern" >&2
  exit 1
}

# Dual-primary evidence must come from probes after the replacement
# promotion, with the true promotion boundary published to the peer.
grep -q "phase_post_promotion_probes" "${FD_A}" || {
  echo "fd-a must probe the fenced former primary after the peer promotion" >&2
  exit 1
}
grep -q "post-rejoin-probes" "${FD_A}" || {
  echo "fd-a must re-verify write rejection after rejoining as a standby" >&2
  exit 1
}
grep -q "G6HA_STATE}/promoted-at" "${FD_B}" || {
  echo "fd-b must record the true promotion boundary" >&2
  exit 1
}
grep -q "g6-ha-post-promotion-\${{ github.run_id }}" "${WORKFLOW}" || {
  echo "the workflow must publish the post-promotion probes artifact" >&2
  exit 1
}
grep -q "post-promotion-probes" "${WORKFLOW}" || {
  echo "fd-a must run the post-promotion probes phase" >&2
  exit 1
}
grep -q "g6-ha-post-promotion-\${GITHUB_RUN_ID}" "${WORKFLOW}" || {
  echo "fd-b must wait for the post-promotion probes before finalizing" >&2
  exit 1
}
grep -q "g6-ha-fd-a-rejoin-\${GITHUB_RUN_ID}" "${WORKFLOW}" || {
  echo "fd-b must consume the rejoin record before finalizing evidence" >&2
  exit 1
}

# Write probes must stay valid against a writable primary: unique run-scoped
# ids with upserts, so a duplicate-key error can never fake a read-only
# rejection, and post-rejoin rejections must be the standby read-only
# SQLSTATE itself rather than any SQL failure.
grep -qF 'ON CONFLICT (id) DO UPDATE' "${FD_A}" || {
  echo "write probes must upsert so only writability decides success" >&2
  exit 1
}
grep -qF 'g6-probe-${RUN_ID}' "${FD_A}" || {
  echo "write probes must carry run-unique probe ids" >&2
  exit 1
}
grep -q "run_readonly_rejection_probes" "${FD_A}" || {
  echo "fd-a must verify read-only rejection by SQLSTATE after rejoin" >&2
  exit 1
}
grep -qF 'cannot execute INSERT in a read-only transaction' "${FD_A}" || {
  echo "post-rejoin probes must accept only the standby read-only SQLSTATE as rejection" >&2
  exit 1
}

# finalize must bind the probe rendezvous to the recorded promotion boundary:
# the echoed promotion record must match byte for byte, and any dual-primary
# probe timestamp that predates the boundary must fail loudly instead of
# silently entering the frozen record.
grep -qF '<"${post_promotion}/promoted-at"' "${FD_B}" || {
  echo "finalize must verify the echoed promotion record matches the recorded boundary" >&2
  exit 1
}
grep -qF 'fromdateiso8601) >= ($promoted | fromdateiso8601)' "${FD_B}" || {
  echo "finalize must reject dual-primary probes that predate the promotion boundary" >&2
  exit 1
}

# fd-b must confirm the rejoined standby is streaming before assembling the
# merged evidence, so the published timeline includes old_primary_rejoined.
rejoin_wait_line="$(grep -n "rejoin-wait" "${WORKFLOW}" | cut -d: -f1 | head -1 || true)"
finalize_line="$(grep -n "fd-b.sh finalize" "${WORKFLOW}" | cut -d: -f1 | head -1 || true)"
if [[ -z "${rejoin_wait_line}" || -z "${finalize_line}" ||
  "${rejoin_wait_line}" -ge "${finalize_line}" ]]; then
  echo "fd-b must run rejoin-wait before finalize so the evidence timeline includes old_primary_rejoined" >&2
  exit 1
fi

# Every pinned action must reuse an exact `uses: action@sha` line already
# proven by a merged workflow with hosted execution. A well-formed but
# nonexistent SHA passes the format check yet fails the dispatch-only run at
# "Set up job" (unresolvable ref), where required PR CI can never catch it.
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

shellcheck "${LIB}" "${FD_A}" "${FD_B}"
echo "g6-ha-pitr policy checks passed"
