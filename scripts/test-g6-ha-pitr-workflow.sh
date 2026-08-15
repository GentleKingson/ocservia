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

# Required observation events from g6-slo.yaml must all be produced by the
# harness timeline or its phase scripts.
for event in load_started primary_failure_injected new_primary_writable \
  api_recovered worker_recovered load_stopped old_primary_isolated \
  new_primary_promoted old_primary_write_rejected marker_a_written \
  restore_point_created marker_b_written restore_verified; do
  grep -q "${event}" "${FD_B}" || {
    echo "timeline event ${event} is not produced by the harness" >&2
    exit 1
  }
done

shellcheck "${LIB}" "${FD_A}" "${FD_B}"
echo "g6-ha-pitr policy checks passed"
