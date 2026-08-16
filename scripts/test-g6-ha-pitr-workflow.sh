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
pg_caps = services.fetch("postgres").fetch("cap_add")
%w[CHOWN FOWNER DAC_OVERRIDE SETUID SETGID].each do |cap|
  reject("postgres must keep #{cap} for its root entrypoint phase under cap_drop ALL") unless pg_caps.include?(cap)
end
api_env = services.fetch("api").fetch("environment")
reject("api must bind loopback: dev auth rejects non-loopback HTTP addresses") unless api_env.fetch("OCSERV_HTTP_ADDRESS").start_with?("127.0.0.1:")
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
# RETURNING tuple; both marker row readers must keep only the tuple before
# the shape guard, or the tag reaches the evidence JSON as a raw newline.
tag_truncations="$(grep -cF "\$'\n'*" "${FD_A}")"
if [[ "${tag_truncations}" -ne 2 ]]; then
  echo "both marker row readers must truncate at the first newline (found ${tag_truncations})" >&2
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
