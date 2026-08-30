#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-ha-pitr.yml"
COMPOSE_FILE="${ROOT}/deploy/g6-ha-pitr/compose.yaml"
LIB="${ROOT}/scripts/g6-ha-pitr-lib.sh"
FD_A="${ROOT}/scripts/g6-ha-pitr-fd-a.sh"
FD_B="${ROOT}/scripts/g6-ha-pitr-fd-b.sh"

if [[ ! -e "${WORKFLOW}" ]]; then
  test -f "${ROOT}/.github/workflows/g6-readiness.yml"
  test -f "${FD_A}"
  test -f "${FD_B}"
  echo "legacy G6 HA/PITR workflow retired; historical harness fixtures retained"
  exit 0
fi
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

  # Timing telemetry is non-authoritative: it must identify the run, never be
  # able to fail an authoritative step, cover the required measurement points,
  # preserve rendezvous wait results, and upload as a warn-only artifact.
  env = job.fetch("env", {})
  reject("#{job_id} must not make timing authoritative") if env.key?("G6_TIMING_REQUIRED")
  env.each_value do |value|
    reject("#{job_id} job env must not use runner context: it is unavailable at job scope") if value.include?("${{ runner.")
  end
  run_steps = Array(job.fetch("steps")).select { |step| step.key?("run") }
  run_steps.each do |step|
    reject("#{job_id} must not make timing authoritative") if step.fetch("env", {}).key?("G6_TIMING_REQUIRED")
  end
  run_text = run_steps.map { |step| step.fetch("run") }.join("\n")
  init_step = run_steps.find { |step| step.fetch("run").include?("g6-timing.sh init") }
  reject("#{job_id} must initialize timing diagnostics") if init_step.nil?
  init_text = init_step.fetch("run")
  reject("#{job_id} timing init must bind the per-domain timing file") unless init_text.include?("G6HA_TIMING_FILE=\"${RUNNER_TEMP}/artifacts/timing/#{job_id}.json\"")
  reject("#{job_id} timing init must publish the binding for later steps") unless init_text.include?(">>\"${GITHUB_ENV}\"")
  reject("#{job_id} timing init must bind the job identity") unless init_text.include?(job_id)
  reject("#{job_id} timing init must bind the ha-pitr profile") unless init_text.include?("ha-pitr")
  %w[${GITHUB_SHA} ${GITHUB_RUN_ID} ${GITHUB_RUN_ATTEMPT}].each do |binding|
    reject("#{job_id} timing init must bind #{binding}") unless init_text.include?(binding)
  end
  run_text.lines chomp: true do |line|
    next unless line.include?("scripts/g6-timing.sh")
    reject("#{job_id} timing call must be non-authoritative: #{line}") unless line.include?("|| true")
  end
  %w[runner_preparation toolchain_bootstrap rendezvous_wait_ diagnostics_collection cleanup artifact_upload].each do |stage|
    reject("#{job_id} must time #{stage}") unless run_text.include?(stage)
  end
  run_steps.select { |step| step.fetch("run").include?("real-e2e-artifact.sh wait-download") }.each do |step|
    text = step.fetch("run")
    preserved = text.include?("status=0") && text.include?("|| status=$?") && text.include?('exit "${status}"')
    reject("#{job_id} rendezvous timing must preserve the wait result") unless preserved
  end
  record_step = Array(job.fetch("steps")).find { |step| (step["name"] || "").include?("Record") && step["name"].include?("timing") }
  reject("#{job_id} must aggregate its timing diagnostics") if record_step.nil?
  record_text = record_step.fetch("run")
  reject("#{job_id} must run in all outcomes to record timing") unless record_step.fetch("if") == "always()"
  reject("#{job_id} must aggregate rendezvous wait metrics") unless record_text.include?("g6-timing.sh rendezvous") && record_text.include?('startswith("rendezvous_wait_")')
  reject("#{job_id} must publish a timing summary") unless record_text.include?("g6-timing.sh summary")
  timing_upload = Array(job.fetch("steps")).find { |step| (step["name"] || "").include?("Upload") && step["name"].include?("timing") }
  reject("#{job_id} must upload its timing diagnostics") if timing_upload.nil?
  timing_upload.fetch("with").tap do |with|
    reject("#{job_id} timing upload must use the per-run timing name") unless with.fetch("name").start_with?("g6-timing-ha-")
    reject("#{job_id} timing upload must stay non-authoritative") unless with.fetch("if-no-files-found") == "warn"
    reject("#{job_id} timing upload must stay short-lived") unless with.fetch("retention-days") == 5
  end
  # if-no-files-found: warn only covers a missing local timing file; an
  # artifact-service failure would otherwise fail the failure-domain job
  # after all authoritative work and evidence already completed. Only the
  # telemetry upload is exempt — every evidence and rendezvous artifact
  # upload stays fail-closed.
  reject("#{job_id} timing diagnostics upload must be continue-on-error") unless timing_upload["continue-on-error"] == true
  Array(job.fetch("steps")).select { |step| step["uses"].to_s.start_with?("actions/upload-artifact@") }.each do |step|
    next if step.equal?(timing_upload)
    reject("#{job_id} authoritative artifact upload must stay fail-closed") if step["continue-on-error"]
  end
end

services = compose.fetch("services")
required = %w[postgres migrate api worker scheduler transportd
              controller-key-init transport-runtime-init transport-endpoint-bootstrap]
reject("G6 HA compose service set is incomplete") unless (required - services.keys).empty?
reject("postgres must receive stop signals directly so fencing leaves a clean data directory") unless services.fetch("postgres").fetch("init") == false
roles = %w[api worker scheduler].to_h { |role| [role, services.fetch(role).fetch("command").fetch(0)] }
reject("control-plane roles must be split") unless roles == {"api" => "--role=api", "worker" => "--role=worker", "scheduler" => "--role=scheduler"}
reject("worker must own the trust socket") unless services.fetch("worker").fetch("environment").key?("OCSERV_TRUST_SOCKET")
reject("postgres must publish only loopback") unless services.fetch("postgres").fetch("ports") == ["127.0.0.1:5432:5432"]
reject("postgres must run data checksums") unless services.fetch("postgres").fetch("environment").fetch("POSTGRES_INITDB_ARGS").include?("data-checksums")
pg_caps = services.fetch("postgres").fetch("cap_add")
%w[CHOWN FOWNER DAC_OVERRIDE SETUID SETGID].each do |cap|
  reject("postgres must keep #{cap} for its root entrypoint phase under cap_drop ALL") unless pg_caps.include?(cap)
end
api_env = services.fetch("api").fetch("environment")
reject("api must bind loopback: dev auth rejects non-loopback HTTP addresses") unless api_env.fetch("OCSERV_HTTP_ADDRESS").start_with?("127.0.0.1:")
RUBY

# Script-level timing telemetry must stay non-authoritative: the helpers no-op
# without the workflow-provided file, metadata calls log and swallow failures,
# and the run wrapper never changes how a phase executes.
grep -q "g6_ha_timing()" "${LIB}" || {
  echo "the shared lib must define the timing shim" >&2
  exit 1
}
grep -q "g6_ha_timing_run()" "${LIB}" || {
  echo "the shared lib must define the timed phase wrapper" >&2
  exit 1
}
grep -q "g6_ha_timing_record_images()" "${LIB}" || {
  echo "the shared lib must define the image recorder" >&2
  exit 1
}
grep -q "g6_ha_timing_record_storage_footprints()" "${LIB}" || {
  echo "the shared lib must define the storage footprint recorder" >&2
  exit 1
}
grep -qF '[[ -n "${G6HA_TIMING_FILE:-}" ]] || return 0' "${LIB}" || {
  echo "timing helpers must no-op without the workflow timing file" >&2
  exit 1
}
grep -qF '>>"${G6HA_LOGS}/timing.log" 2>&1 || true' "${LIB}" || {
  echo "timing metadata calls must never fail a phase" >&2
  exit 1
}
if grep -qF '|| status=$?' "${LIB}"; then
  echo "the timed phase wrapper must not run a phase in a condition context:" >&2
  echo "errexit is suppressed for the whole phase body, so a mid-phase failure would keep executing and could still return success" >&2
  exit 1
fi
images_body="$(sed -n '/^g6_ha_timing_record_images()/,/^}/p' "${LIB}")"
if grep -q 'wal_archive_bytes\|basebackup_bytes' <<<"${images_body}"; then
  echo "image recording must not sample storage footprints: the trees are empty before the scenario runs" >&2
  exit 1
fi
if ! grep -q 'g6_ha_timing_record_storage_footprints' \
  <<<"$(sed -n '/^g6_ha_diagnostics()/,/^}/p' "${LIB}")"; then
  echo "storage footprints must be sampled during diagnostics, after the scenario and before cleanup" >&2
  exit 1
fi
fd_a_stages="prepare compose_image_build control_plane_build transportd_build \
tunnel_build tunnel_up primary_bootstrap basebackup pitr_restore failover \
post_promotion_probes recover_roles rejoin"
for stage in ${fd_a_stages}; do
  grep -q "g6_ha_timing_run ${stage} " "${FD_A}" || {
    echo "fd-a must time the ${stage} stage" >&2
    exit 1
  }
done
fd_b_stages="prepare compose_image_build control_plane_build transportd_build \
tunnel_build tunnel_up standby_bootstrap roles_up failover_ready promotion \
evidence_collection rejoin_confirm"
for stage in ${fd_b_stages}; do
  grep -q "g6_ha_timing_run ${stage} " "${FD_B}" || {
    echo "fd-b must time the ${stage} stage" >&2
    exit 1
  }
done
for fd_script in "${FD_A}" "${FD_B}"; do
  grep -q "g6_ha_timing_record_images" "${fd_script}" || {
    echo "${fd_script} must record image identities after building" >&2
    exit 1
  }
done

# The wrapper's fail-fast semantics are executed, not just grepped: a timed
# phase running in a || context would keep executing after a mid-phase
# failure (errexit is suppressed for the whole callee body) and could still
# return success. The child shell must die at the failing command, never
# reach the marker write, and must have entered the timed stage first.
test_timing_run_preserves_fail_fast() (
  local temporary child
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  mkdir -p "${temporary}/logs"
  child="${temporary}/child.sh"
  cat >"${child}" <<CHILD
set -Eeuo pipefail
source "${LIB}"
G6HA_ROOT="${ROOT}"
G6HA_TIMING_FILE="${temporary}/timing.json"
G6HA_LOGS="${temporary}/logs"
failing_phase() {
  false
  touch "${temporary}/must-not-exist"
}
g6_ha_timing_run regression_stage failing_phase
CHILD
  if bash "${child}" >"${temporary}/child.log" 2>&1; then
    echo "a mid-phase failure must fail the phase shell" >&2
    return 1
  fi
  if [[ -e "${temporary}/must-not-exist" ]]; then
    echo "execution continued past a failed command inside a timed phase" >&2
    return 1
  fi
  if ! grep -q 'regression_stage' "${temporary}/timing.json.tsv" 2>/dev/null; then
    echo "the regression child never reached the timed phase:" >&2
    cat "${temporary}/child.log" >&2
    return 1
  fi
)
test_timing_run_preserves_fail_fast

# The storage footprint sampler is executed under a failing helper, not just
# grepped: the sampler is pure telemetry that runs inside the set -e
# diagnostics script, so a filesystem failure (mktemp, du, awk) inside it must
# never abort the shell — that would flip an authoritative green scenario
# red. The mocked mktemp fails every sampler call; the child must still exit
# zero and reach the authoritative continuation marker.
test_storage_footprints_fail_open() (
  local temporary child
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  mkdir -p "${temporary}/logs" "${temporary}/pgarchive" "${temporary}/basebackup"
  child="${temporary}/child.sh"
  cat >"${child}" <<CHILD
set -Eeuo pipefail
source "${LIB}"
G6HA_ROOT="${ROOT}"
G6HA_TIMING_FILE="${temporary}/timing.json"
G6HA_LOGS="${temporary}/logs"
G6HA_ARCHIVE="${temporary}/pgarchive"
G6HA_BASEBACKUP="${temporary}/basebackup"
mktemp() { return 1; }
g6_ha_compose() { return 1; }
g6_ha_timing_record_storage_footprints
touch "${temporary}/authoritative-continued"
CHILD
  if ! bash "${child}" >"${temporary}/child.log" 2>&1; then
    echo "a telemetry failure inside the storage sampler must not fail the shell:" >&2
    cat "${temporary}/child.log" >&2
    return 1
  fi
  if [[ ! -e "${temporary}/authoritative-continued" ]]; then
    echo "execution stopped at a telemetry failure before the next command" >&2
    return 1
  fi
)
test_storage_footprints_fail_open

# Secret export is exercised, not just grepped: every missing or zero-byte
# cluster credential must fail both the reader and the aggregate export. A
# compose call from a fresh diagnostics/cleanup process must then use the
# complete placeholder environment rather than carrying an empty value into
# Compose interpolation.
test_secret_export_fail_closed() (
  local temporary capture scenario mode secret actual
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM

  # shellcheck source=scripts/g6-ha-pitr-lib.sh
  source "${LIB}"
  G6HA_SECRETS="${temporary}/secrets"
  G6HA_STATE="${temporary}/state"
  G6HA_ARCHIVE="${temporary}/archive"
  G6HA_BASEBACKUP="${temporary}/basebackup"
  FD_ID=fd-a
  COMPOSE_PROJECT=g6-secret-policy-test
  COMPOSE_FILE="${temporary}/compose.yaml"
  capture="${temporary}/compose-env"
  mkdir -p "${G6HA_SECRETS}" "${G6HA_STATE}"

  docker() {
    [[ "${1:-}" == compose ]] || return 2
    printf '%s\n' "${G6_FD_ID}|${G6_OWNER_PASSWORD}|${G6_APP_PASSWORD}|${G6_REPLICATION_PASSWORD}" \
      >"${capture}"
  }

  write_valid_secrets() {
    printf '%s\n' owner-value >"${G6HA_SECRETS}/owner-password"
    printf '%s\n' app-value >"${G6HA_SECRETS}/app-password"
    printf '%s\n' replication-value >"${G6HA_SECRETS}/replication-password"
  }

  for scenario in \
    missing:owner-password missing:app-password missing:replication-password \
    empty:owner-password empty:app-password empty:replication-password; do
    write_valid_secrets
    mode="${scenario%%:*}"
    secret="${scenario#*:}"
    if [[ "${mode}" == missing ]]; then
      rm -f "${G6HA_SECRETS}/${secret}"
    else
      : >"${G6HA_SECRETS}/${secret}"
    fi

    if g6_ha_secret "${secret}" >/dev/null 2>&1; then
      echo "g6_ha_secret accepted ${scenario}" >&2
      return 1
    fi
    if g6_ha_export_common_env; then
      echo "g6_ha_export_common_env accepted ${scenario}" >&2
      return 1
    fi

    unset G6_FD_ID G6_OWNER_PASSWORD G6_APP_PASSWORD G6_REPLICATION_PASSWORD
    g6_ha_compose config
    actual="$(<"${capture}")"
    [[ "${actual}" == 'fd-a|harness-placeholder|harness-placeholder|harness-placeholder' ]] || {
      echo "g6_ha_compose did not use the complete placeholder environment for ${scenario}: ${actual}" >&2
      return 1
    }
  done

  write_valid_secrets
  unset G6_FD_ID G6_OWNER_PASSWORD G6_APP_PASSWORD G6_REPLICATION_PASSWORD
  g6_ha_export_common_env
  [[ "${G6_FD_ID}|${G6_OWNER_PASSWORD}|${G6_APP_PASSWORD}|${G6_REPLICATION_PASSWORD}" == \
    'fd-a|owner-value|app-value|replication-value' ]] || {
    echo "g6_ha_export_common_env rejected a complete secret set" >&2
    return 1
  }

  # A partially inherited process environment must be refreshed as one
  # credential set; checking only G6_APP_PASSWORD would leave owner or
  # replication empty and defer the failure to Compose parsing.
  G6_FD_ID=fd-a
  G6_APP_PASSWORD=stale-app-value
  unset G6_OWNER_PASSWORD G6_REPLICATION_PASSWORD
  g6_ha_compose config
  actual="$(<"${capture}")"
  [[ "${actual}" == 'fd-a|owner-value|app-value|replication-value' ]] || {
    echo "g6_ha_compose did not refresh a partial credential environment: ${actual}" >&2
    return 1
  }
)
test_secret_export_fail_closed

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

# The prepare phase derives the tunnel node id for the rendezvous artifact,
# so the tunnel binary must be built there (via the shared helper, with the
# pinned toolchain on PATH from env.sh); the compose interpolation may only
# reference variables the harness env actually exports, and per-role
# application names arrive through PGAPPNAME (pgx libpq fallback), never a
# shell G6_ROLE_NAME.
if grep -q 'G6_ROLE_NAME' "${COMPOSE_FILE}"; then
  echo "compose must not reference G6_ROLE_NAME; role names arrive via PGAPPNAME" >&2
  exit 1
fi
pgappname_count="$(grep -c 'PGAPPNAME: ${G6_FD_ID:?}-' "${COMPOSE_FILE}")"
if [[ "${pgappname_count}" -ne 3 ]]; then
  echo "each role service must set its PGAPPNAME from G6_FD_ID (found ${pgappname_count})" >&2
  exit 1
fi
grep -q 'source "${G6HA_ROOT}/scripts/env.sh"' "${LIB}" || {
  echo "the shared lib must source env.sh so cargo is on PATH in every phase" >&2
  exit 1
}
grep -q "g6_ha_build_tunnel()" "${LIB}" || {
  echo "the shared lib must define the tunnel build helper" >&2
  exit 1
}
for fd_script in "${FD_A}" "${FD_B}"; do
  grep -q "g6_ha_build_tunnel" "${fd_script}" || {
    echo "${fd_script} must build the tunnel binary in its prepare phase" >&2
    exit 1
  }
done

# Exactly one tunnel process runs per failure domain per era: fd-a serves
# and fd-b forwards before the failover, and the promote/recover phases flip
# the roles. One key must never back two live endpoints — the relay drops
# the second with the same endpoint id.
grep -qF 'g6_ha_tunnel_serve "$(peer_node_id)"' "${FD_A}" || {
  echo "fd-a must serve its primary through the pinned tunnel" >&2
  exit 1
}
grep -qF 'g6_ha_tunnel_forward "$(peer_node_id)"' "${FD_A}" || {
  echo "fd-a must flip to forwarding when it recovers against the promoted primary" >&2
  exit 1
}
grep -qF 'g6_ha_tunnel_forward "$(peer_node_id)"' "${FD_B}" || {
  echo "fd-b must forward to the peer primary through the pinned tunnel" >&2
  exit 1
}
grep -qF 'g6_ha_tunnel_serve "$(peer_node_id)"' "${FD_B}" || {
  echo "fd-b must flip to serving when it promotes" >&2
  exit 1
}
if grep -qF "g6_ha_tunnel_start" "${LIB}" "${FD_A}" "${FD_B}"; then
  echo "the harness must not start serve and forward from one key simultaneously" >&2
  exit 1
fi
grep -qF 'for log in "${G6HA_LOGS}"/*.log' "${LIB}" || {
  echo "diagnostics must include the harness phase logs so redirected failures stay visible" >&2
  exit 1
}

# psql -v variables do not survive `docker compose exec` argument passing on
# every hosted compose version; an undefined reference reaches the server raw
# and fails with a syntax error at the colon. SQL literals must be inlined
# behind charset guards instead.
for fd_script in "${FD_A}" "${FD_B}"; do
  if grep -qF ":'" "${fd_script}"; then
    echo "psql variable interpolation is forbidden in ${fd_script}; inline guarded literals" >&2
    exit 1
  fi
done

# The role DSN must take its port from G6_DB_PORT: fd-b roles before promotion
# and fd-a roles after recovery dial the forwarded tunnel on 15432, while a
# locally served primary answers on 5432. A hardcoded port makes the first
# cross-domain role dial refuse connections.
grep -qF ':${G6_DB_PORT:?}/ocservia' "${COMPOSE_FILE}" || {
  echo "role DSN must interpolate G6_DB_PORT" >&2
  exit 1
}
grep -qF 'export G6_DB_PORT=15432' "${FD_B}" || {
  echo "fd-b must dial the forwarded tunnel port for its pre-promotion roles" >&2
  exit 1
}
grep -qF 'export G6_DB_PORT=5432' "${FD_B}" || {
  echo "fd-b must reset the direct port when it becomes the primary" >&2
  exit 1
}
grep -qF 'export G6_DB_PORT=15432' "${FD_A}" || {
  echo "fd-a must dial the forwarded tunnel port after losing the primary role" >&2
  exit 1
}
db_port_defaults="$(grep -cF 'export G6_DB_PORT="${G6_DB_PORT:-5432}"' "${LIB}")"
if [[ "${db_port_defaults}" -ne 2 ]]; then
  echo "both compose env helpers must default G6_DB_PORT (found ${db_port_defaults})" >&2
  exit 1
fi

# Query output crosses the compose exec transport before it reaches evidence
# JSON: CR stripping lives in the shared psql wrapper, marker rows are shape-
# guarded with their bytes dumped on mismatch, and the load JSON is assembled
# by jq itself. Hand-built printf JSON once smuggled a control character past
# every grep-based check and died only at strict jq validation.
if ! grep -qF 'tr -d ' "${LIB}" || ! grep -qF "'\r'" "${LIB}"; then
  echo "g6_ha_psql must strip carriage returns from query output" >&2
  exit 1
fi
grep -qF 'load-markers.ndjson' "${FD_A}" || {
  echo "load markers must be assembled through jq from ndjson rows" >&2
  exit 1
}
if grep -qF '"marker_id": "' "${FD_A}"; then
  echo "hand-built marker JSON is forbidden; jq must assemble the load evidence" >&2
  exit 1
fi
grep -qF 'g6_ha_reclaim_directory "${pg_dir}"' "${LIB}" || {
  echo "cleanup must reclaim postgres-owned bind mounts before rm" >&2
  exit 1
}
grep -qF 'for pg_dir in "${G6HA_ARCHIVE}" "${G6HA_BASEBACKUP}" "${G6HA_RESTORE}"' "${LIB}" || {
  echo "cleanup must reclaim every postgres-owned bind mount" >&2
  exit 1
}

# psql -At still prints the INSERT command tag on its own line after a
# RETURNING tuple; the load marker, PITR marker, and outage declaration row
# readers must keep only the tuple before the shape guard, or the tag reaches
# the evidence JSON as a raw newline.
tag_truncations="$(grep -cF "\$'\n'*" "${FD_A}")"
if [[ "${tag_truncations}" -ne 3 ]]; then
  echo "all RETURNING row readers must truncate at the first newline (found ${tag_truncations})" >&2
  exit 1
fi

# pg_hba admits replication connections for ocservia_replication only, so
# every pg_basebackup must run as that role; the owner login is rejected
# with no pg_hba entry before any byte is copied.
for fd_script in "${FD_A}" "${FD_B}"; do
  grep -qF 'pg_basebackup' "${fd_script}" || continue
  if ! grep -qF -- '-U ocservia_replication' "${fd_script}"; then
    echo "pg_basebackup in ${fd_script} must authenticate as ocservia_replication" >&2
    exit 1
  fi
done

# The PITR restore listens on PITR_RESTORE_PORT, not the libpq default 5432;
# every psql into the pitr container must set that port or it probes a
# nonexistent socket while the restore sits paused at the restore point.
pitr_psql_calls="$(grep -cF 'docker exec "${pitr_container}" psql' "${FD_A}")"
pitr_port_args="$(grep -cF -- '-p "${PITR_RESTORE_PORT}" -Atc' "${FD_A}")"
if [[ "${pitr_psql_calls}" -ne "${pitr_port_args}" ]]; then
  echo "every psql into the pitr container must set PITR_RESTORE_PORT (calls ${pitr_psql_calls}, port args ${pitr_port_args})" >&2
  exit 1
fi

# Marker A, the restore point, and marker B are created back-to-back. Whole-
# second timestamps collapse their order and make a valid restore fail the
# frozen strict A < restore point < B contract, so both SQL producers and the
# marker shape guard must retain fixed-width microseconds.
pitr_microsecond_formats="$(grep -cF 'HH24:MI:SS.US\"Z\"' "${FD_A}")"
if [[ "${pitr_microsecond_formats}" -ne 2 ]]; then
  echo "PITR marker and restore-point timestamps must retain microseconds (found ${pitr_microsecond_formats})" >&2
  exit 1
fi
grep -qF '[0-9]{2}\.[0-9]{6}Z$' "${FD_A}" || {
  echo "PITR marker row guard must require six fractional digits" >&2
  exit 1
}

# The former-primary sanity probe must authenticate through the exact path
# later used for fencing. Without the owner password every probe is rejected
# by pg_hba, making a healthy writable primary look fenced.
grep -qF -- '-e PGPASSWORD="$(g6_ha_secret owner-password)"' "${FD_A}" || {
  echo "former-primary write probes must authenticate as ocservia_owner" >&2
  exit 1
}

# Every artifact name the workflow waits on must pass the shared artifact
# helper's allowlist; the validator once rejected the whole g6-ha family and
# both jobs died at their first rendezvous.
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
