#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/real-e2e.yml"
COMPOSE_FILE="${ROOT}/deploy/real-e2e/controller.compose.yaml"

ruby -r yaml - "${WORKFLOW}" "${COMPOSE_FILE}" <<'RUBY'
workflow_path, compose_path = ARGV
workflow = YAML.safe_load(File.read(workflow_path), aliases: true)
compose = YAML.safe_load(File.read(compose_path), aliases: true)

def reject(message)
  warn message
  exit 1
end

trigger = workflow.fetch(true)
reject("Real E2E must remain workflow_dispatch-only") unless trigger.keys == ["workflow_dispatch"]
reject("Real E2E permissions must be read-only") unless workflow.fetch("permissions") == {"contents" => "read", "actions" => "read"}
jobs = workflow.fetch("jobs")
reject("Real E2E must contain exactly two execution jobs") unless jobs.keys.sort == %w[real-controller real-node]
jobs.each do |job_id, job|
  reject("#{job_id} must use ubuntu-24.04") unless job.fetch("runs-on") == "ubuntu-24.04"
  reject("#{job_id} timeout must remain 30 minutes") unless job.fetch("timeout-minutes") == 30
  reject("#{job_id} must run concurrently") if job.key?("needs")
  Array(job.fetch("steps")).select { |step| step.key?("uses") }.each do |step|
    reject("#{job_id} Action is not pinned to a full SHA") unless step.fetch("uses").match?(/@[0-9a-f]{40}\z/)
  end
end

node_steps = jobs.fetch("real-node").fetch("steps")
node_step_names = node_steps.map { |step| step["name"] }.compact
build_index = node_step_names.index("Build Agent")
wait_index = node_step_names.index("Wait for Controller EndpointID")
prepare_index = node_step_names.index("Prepare persistent Agent EndpointID")
reject("Real E2E node must build the Agent") unless build_index
reject("Real E2E node must wait for the Controller EndpointID") unless wait_index
reject("Real E2E node must prepare a persistent Agent EndpointID") unless prepare_index
reject("Agent build must run before the Controller rendezvous") unless build_index < wait_index
reject("Agent identity preparation must follow the Controller rendezvous") unless wait_index < prepare_index
reject("Agent build step must only invoke the node build phase") unless node_steps.find { |step| step["name"] == "Build Agent" }.fetch("run").include?("real-e2e-node.sh build")
reject("Agent prepare step must consume the Controller artifact") unless node_steps.find { |step| step["name"] == "Prepare persistent Agent EndpointID" }.fetch("run").include?("real-e2e-node.sh prepare")

required_services = %w[postgres migrate control-plane transportd transport-runtime-init controller-key-init]
reject("Real E2E Controller service set is incomplete") unless (required_services - compose.fetch("services").keys).empty?
transport = compose.fetch("services").fetch("transportd")
reject("transportd must use the production Dockerfile") unless transport.fetch("build").fetch("dockerfile") == "rust/transportd.Dockerfile"
reject("transportd must use default Internet relay discovery") unless transport.fetch("command").each_slice(2).to_a.include?(["--relay-mode", "default"])
control = compose.fetch("services").fetch("control-plane")
reject("test-only development auth must remain loopback-only") unless control.fetch("environment").fetch("OCSERV_HTTP_ADDRESS") == "127.0.0.1:8080"
reject("Control Plane HTTP must not be published from the Controller VM") if control.key?("ports")
RUBY

if rg -n 'transportd-stub|OCSERV_LOCAL_SIMULATOR' "${WORKFLOW}" "${COMPOSE_FILE}" \
  "${ROOT}/scripts/real-e2e-controller.sh" "${ROOT}/scripts/real-e2e-node.sh"; then
  echo "Real E2E must not use the transport stub or local simulator" >&2
  exit 1
fi

for script in real-e2e-artifact.sh real-e2e-controller.sh real-e2e-node.sh; do
  bash -n "${ROOT}/scripts/${script}"
done
if command -v shellcheck >/dev/null; then
  shellcheck "${ROOT}/scripts/real-e2e-artifact.sh" \
    "${ROOT}/scripts/real-e2e-controller.sh" \
    "${ROOT}/scripts/real-e2e-node.sh" \
    "${ROOT}/scripts/test-real-e2e-workflow.sh"
fi

GITHUB_RUN_ID=123456 GITHUB_RUN_ATTEMPT=2 \
  "${ROOT}/scripts/real-e2e-artifact.sh" validate-name real-e2e-controller-ready-123456-2
if GITHUB_RUN_ID=123456 GITHUB_RUN_ATTEMPT=2 \
  "${ROOT}/scripts/real-e2e-artifact.sh" validate-name real-e2e-controller-ready-123456-1 >/dev/null 2>&1; then
  echo "artifact helper accepted a stale run attempt" >&2
  exit 1
fi

OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '0%.0s' {1..64})" \
  docker compose --project-name ocservia-real-e2e-policy --file "${COMPOSE_FILE}" config --quiet

echo "Cross-VM Real E2E workflow policy passed"
