#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/g6-readiness.yml"
CI_WORKFLOW="${ROOT}/.github/workflows/ci.yml"
ARTIFACT_HELPER="${ROOT}/scripts/real-e2e-artifact.sh"
POSTGRES_INIT="${ROOT}/deploy/g6-readiness/postgres-init/001-g6-readiness.sh"
LIB="${ROOT}/scripts/g6-readiness-lib.sh"
FD_A="${ROOT}/scripts/g6-readiness-fd-a.sh"
FD_B="${ROOT}/scripts/g6-readiness-fd-b.sh"

ruby -r yaml - "${WORKFLOW}" "${CI_WORKFLOW}" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
ci_workflow = YAML.safe_load(File.read(ARGV.fetch(1)), aliases: true)

def reject(message)
  warn message
  exit 1
end

concurrency = workflow.fetch("concurrency")
reject("G6 readiness must queue formal runs without cancelling active evidence") unless
  concurrency.fetch("queue") == "max" && !concurrency.key?("cancel-in-progress")
for token in ["github.workflow", "github.ref", "inputs.authority"]
  reject("the G6 concurrency group must include #{token}") unless concurrency.fetch("group").include?(token)
end

jobs = workflow.fetch("jobs")
hang_guard_command = "scripts/test-g6-readiness-hang-guards.sh"
ci_jobs = ci_workflow.fetch("jobs")
ci_count = ci_jobs.values.sum do |job|
  Array(job.fetch("steps", [])).sum do |step|
    step.fetch("run", "").lines.count { |line| line.strip == hang_guard_command }
  end
end
reject("the hang-guard regression test must run exactly once in ordinary required CI") unless ci_count == 1
contracts_steps = Array(ci_jobs.fetch("contracts-policy").fetch("steps"))
reject("Contracts and Policy CI must own the hang-guard regression test") unless contracts_steps.any? do |step|
  step.fetch("run", "").lines.any? { |line| line.strip == hang_guard_command }
end
reject("Contracts and Policy must remain in the required quality aggregate") unless
  Array(ci_jobs.fetch("quality-security-native").fetch("needs")).include?("contracts-policy")
%w[g6-rd-fd-a g6-rd-fd-b].each do |job_id|
  steps = jobs.fetch(job_id).fetch("steps")
  reject("#{job_id} must not repeat the ordinary-CI hang-guard regression test") if steps.any? do |step|
    step.fetch("run", "").lines.any? { |line| line.strip == hang_guard_command }
  end

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
  "g6-rd-release-image" => {
    "Build and freeze the release images" => 30,
    "Clean release-image resources" => 5
  },
  "g6-rd-fd-a" => {
    "Prepare failure domain A images" => 35,
    "Enroll the failure domain A fleet" => 25,
    "Verify the PITR restore point" => 15,
    "Rejoin the former primary as standby" => 15
  },
  "g6-rd-fd-b" => {
    "Prepare failure domain B images" => 35,
    "Bootstrap the streaming standby" => 15,
    "Enroll the failure domain B fleet" => 25,
    "Promote the standby under load" => 8,
    "Capture the pre-fault relay-a session" => 8,
    "Outbox crash window after claim" => 10,
    "Outbox crash window after transport send" => 10,
    "Outbox crash window before result commit" => 10,
    "Preflight bounded resource evidence" => 3,
    "Run the bounded observation window" => 11
  },
  "g6-rd-assemble" => {
    "Assemble the evidence bundle" => 15
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

fd_b_steps = jobs.fetch("g6-rd-fd-b").fetch("steps")
preflight = fd_b_steps.find { |step| step["name"] == "Preflight bounded resource evidence" }
window = fd_b_steps.find { |step| step["name"] == "Run the bounded observation window" }
reject("resource preflight must use its 120-second hard process limit") unless
  preflight&.fetch("run") == "timeout --signal=TERM --kill-after=15s 120s scripts/g6-readiness-fd-b.sh resource-preflight"
reject("resource preflight must precede the full observation window") unless
  preflight && window && fd_b_steps.index(preflight) < fd_b_steps.index(window)

bundle_upload = jobs.fetch("g6-rd-assemble").fetch("steps").find do |step|
  step["name"] == "Publish the partial or complete evidence bundle"
end
reject("the evidence bundle diagnostics upload is missing") unless bundle_upload
reject("the evidence bundle diagnostics must upload after failure") unless bundle_upload.fetch("if") == "always()"
reject("assembly must always publish a structured partial or complete bundle") unless bundle_upload.fetch("with").fetch("if-no-files-found") == "error"

fd_a_steps = jobs.fetch("g6-rd-fd-a").fetch("steps")
promoted_wait = fd_a_steps.find { |step| step["name"] == "Wait for the promoted primary" }
reject("fd-a must wait for the promoted-primary artifact") unless promoted_wait
wait_seconds = promoted_wait.fetch("run")[/g6-rd-new-primary[^\n]*\s(\d+)\s+"G6 Readiness Failure Domain B"/, 1]&.to_i
producer_minutes = critical_timeouts.fetch("g6-rd-fd-b").fetch("Promote the standby under load")
reject("the promoted-primary artifact wait must outlive its producer timeout") unless wait_seconds && wait_seconds > producer_minutes * 60
RUBY

relay_topology_docker="$(sed -n '/^relay_topology_docker() {/,/^}/p' "${FD_A}")"
grep -qF 'timeout --signal=TERM --kill-after=5s 20s docker "$@"' \
  <<<"${relay_topology_docker}" || {
  echo "relay topology Docker operations lack a hard process-group timeout" >&2
  exit 1
}
relay_ready_phase="$(sed -n '/^phase_relay_rejoin_ready() {/,/^}/p' "${FD_A}")"
relay_stop_phase="$(sed -n '/^phase_relay_a_stop() {/,/^}/p' "${FD_A}")"
for phase in "${relay_ready_phase}" "${relay_stop_phase}"; do
  grep -qF 'relay_a_only_topology_restore' <<<"${phase}" || {
    echo "relay controlled-topology phase lacks bounded failure restoration" >&2
    exit 1
  }
done
cleanup_lib="$(sed -n '/^g6rd_cleanup() {/,/^}/p' "${LIB}")"
# shellcheck disable=SC2016  # assert literal cleanup source
if ! grep -qF 'docker network rm "${relay_topology_network}"' <<<"${cleanup_lib}" \
  || ! grep -qF 'for network in agent-shared agent-isolated relay-a-only' \
    <<<"${cleanup_lib}"; then
  echo "bounded cleanup does not remove and gate the run-scoped relay topology" >&2
  exit 1
fi
relay_up_phase="$(sed -n '/^phase_relay_up() {/,/^}/p' "${FD_B}")"
relay_health_probe="$(sed -n '/^relay_b_healthy() {/,/^}/p' "${FD_B}")"
if ! grep -qF 'g6rd_wait_until_deadline 120 2' <<<"${relay_up_phase}" \
  || ! grep -qF 'G6RD_COMPOSE_TIMEOUT_SECONDS=5' <<<"${relay_health_probe}"; then
  echo "relay-b startup is not deadline/per-probe bounded" >&2
  exit 1
fi

# The owner failover must leave the frozen database leases untouched until
# their natural deadline. A graceful worker stop could run authority-release
# cleanup, so use a scoped hard-kill and then wait on the database-clock proof.
owner_phase="$(sed -n '/^phase_scenario_owner() {/,/^}/p' "${FD_B}")"
grep -qF 'G6RD_COMPOSE_TIMEOUT_SECONDS=15 g6rd_compose kill --signal KILL worker' \
  <<<"${owner_phase}" || {
  echo "the connection-owner injection lacks a scoped worker crash" >&2
  exit 1
}
if grep -qF 'g6rd_compose stop worker' <<<"${owner_phase}"; then
  echo "the connection-owner injection must not gracefully release frozen leases" >&2
  exit 1
fi

# The deterministic send-before-MarkSent point uses exact pre/post Worker
# hooks. It must not depend on an advisory-lock holder or backend inference.
post_arm="$(sed -n '/^post_send_barrier_arm() {/,/^}/p' "${FD_B}")"
post_reached="$(sed -n '/^post_send_barrier_reached() {/,/^}/p' "${FD_B}")"
post_release="$(sed -n '/^post_send_barrier_release() {/,/^}/p' "${FD_B}")"
post_attempt="$(sed -n '/^exact_post_send_attempt_id() {/,/^}/p' "${FD_B}")"
post_proof="$(sed -n '/^exact_post_send_attempt_proof() {/,/^}/p' "${FD_B}")"
post_report="$(sed -n '/^report_exact_post_send_attempt_failure() {/,/^}/p' "${FD_B}")"
crash2_phase="$(sed -n '/^phase_outbox_send_before_mark() {/,/^}/p' "${FD_B}")"
cleanup_phase="$(sed -n '/^phase_cleanup() {/,/^}/p' "${FD_B}")"
cleanup_prelude="$(sed -n '/^phase_cleanup_prelude() {/,/^}/p' "${FD_B}")"
for helper in "${post_arm}" "${post_reached}" "${post_release}"; do
  # shellcheck disable=SC2016  # assert literal helper source
  grep -qF '"${command_id}"' <<<"${helper}" || {
    echo "the post-Send barrier is not bound to the exact command id" >&2
    exit 1
  }
done
for token in \
  'attempt.attempt_number=outbox.attempts' \
  "attempt.state='sending' AND attempt.finished_at IS NULL" \
  'command.state,operation.state,outbox.published_at IS NULL' \
  'COALESCE(outbox.locked_until>clock_timestamp(),false)' \
  'COALESCE(lease.leased_until>clock_timestamp(),false)' \
  "attempt.id='\${attempt_id}'" \
  'FROM agent_command_results AS result'; do
  grep -qF "${token}" <<<"${post_attempt}${post_proof}" || {
    echo "the post-Send barrier lacks exact attempt proof: ${token}" >&2
    exit 1
  }
done
for token in \
  "SELECT 'target'" \
  "SELECT 'attempt'" \
  "SELECT 'result'" \
  'lease.worker_id,lease.leased_until'; do
  grep -qF "${token}" <<<"${post_report}" || {
    echo "the post-Send failure matrix lacks diagnostic field: ${token}" >&2
    exit 1
  }
done
# shellcheck disable=SC2016  # assert literal exact-attempt shell expressions
for token in \
  'attempt_id="$(exact_post_send_attempt_id "${command_id}")"' \
  'attempt_proof="$(exact_post_send_attempt_proof "${command_id}" "${attempt_id}")"' \
  'report_exact_post_send_attempt_failure "${command_id}" "${attempt_id}"'; do
  grep -qF "${token}" <<<"${crash2_phase}" || {
    echo "the send-before-MarkSent phase does not retain its exact attempt: ${token}" >&2
    exit 1
  }
done
if grep -qF 'ORDER BY attempt_number LIMIT 1' <<<"${crash2_phase}"; then
  echo "the send-before-MarkSent phase still infers the dispatch from attempt one" >&2
  exit 1
fi
if grep -qE '^send_before_mark_|^SEND_BEFORE_MARK|start_send_before_mark|stop_send_before_mark' "${FD_B}"; then
  echo "the obsolete generic send-before-MarkSent holder is still present" >&2
  exit 1
fi
grep -qF 'timeout --foreground --signal=TERM --kill-after=5s 45s' \
  <<<"${cleanup_phase}" || {
  echo "the failure-domain cleanup prelude lacks an overall hard timeout" >&2
  exit 1
}
# shellcheck disable=SC2016  # assert literal cleanup expressions
for cleanup_token in \
  'stop_watchers' \
  'release_armed_pre_send_barrier' \
  'release_armed_post_send_barrier' \
  'release_armed_result_commit_barrier' \
  'g6rd_release_synthetic_barriers' \
  'for service in transportd api scheduler worker' \
  'unpause_scoped_container "${COMPOSE_PROJECT}-${service}-1"'; do
  grep -qF "${cleanup_token}" <<<"${cleanup_prelude}" || {
    echo "failure-domain cleanup is missing timeout recovery: ${cleanup_token}" >&2
    exit 1
  }
done

# Detached watchers and the sampler own process groups, so stopping the wrapper
# also terminates an in-flight Docker/psql descendant. Each watcher query has
# its own hard attempt timeout before the bounded cleanup removes run state.
window_phase="$(sed -n '/^phase_window() (/,/^)/p' "${FD_B}")"
if grep -qE '^[[:space:]]*wait[[:space:]]*$' <<<"${window_phase}"; then
  echo "the observation window uses a bare wait that can deadlock on the sampler" >&2
  exit 1
fi
# shellcheck disable=SC2016  # assert literal PID-scoped wait expressions
for token in 'enqueue_pids+=("$!")' \
  'wait_for_window_enqueue_wave "${enqueue_pids[@]}"'; do
  grep -qF "${token}" <<<"${window_phase}" || {
    echo "the observation window is missing PID-scoped enqueue waiting: ${token}" >&2
    exit 1
  }
done

# The 305-second load window, every wall-clock wait, the maximum predicate and
# driver overruns, one bounded diagnostic probe, and sampler shutdown must all
# finish before Actions' ten-minute hard kill with a full minute of margin.
if grep -qE 'g6rd_wait_until[[:space:]]' <<<"${window_phase}"; then
  echo "the observation window uses an attempt-count wait" >&2
  exit 1
fi
# shellcheck disable=SC2016  # assert literal deadline and diagnostic expressions
for token in \
  'g6rd_wait_until_deadline "${WINDOW_API_READY_TIMEOUT_SECONDS}"' \
  'g6rd_wait_until_deadline "${WINDOW_PRE_DRAIN_TIMEOUT_SECONDS}"' \
  'g6rd_wait_until_deadline "${WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS}"' \
  'g6rd_wait_until_deadline "${WINDOW_POST_DRAIN_TIMEOUT_SECONDS}"' \
  'report_window_command_timeout "${CLAIM_KEY}"' \
  'report_window_outbox_timeout' \
  'validate_window_timeout_budget'; do
  grep -qF "${token}" <<<"${window_phase}" || {
    echo "the observation window is missing bounded timeout behavior: ${token}" >&2
    exit 1
  }
done
grep -qF 'G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary' "${FD_B}" || {
  echo "observation-window SQL predicates lack their ten-second hard bound" >&2
  exit 1
}
[[ "$(grep -cF '((SECONDS < window_deadline)) || break' <<<"${window_phase}")" == 4 ]] || {
  echo "the observation-window driver does not bound every request boundary" >&2
  exit 1
}

window_constant() {
  local name="${1:?constant name is required}" value
  value="$(sed -nE "s/^${name}=([0-9]+)$/\\1/p" "${FD_B}")"
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || {
    echo "invalid observation-window budget constant ${name}" >&2
    return 1
  }
  printf '%s\n' "${value}"
}
window_default="$(grep -oE 'G6RD_WINDOW_SECONDS:-[0-9]+' "${FD_B}" | head -1 | cut -d: -f2- | tr -d ':-')"
api_ready_budget="$(window_constant WINDOW_API_READY_TIMEOUT_SECONDS)"
pre_drain_budget="$(window_constant WINDOW_PRE_DRAIN_TIMEOUT_SECONDS)"
settle_budget="$(window_constant WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS)"
post_drain_budget="$(window_constant WINDOW_POST_DRAIN_TIMEOUT_SECONDS)"
api_overrun="$(window_constant WINDOW_API_PREDICATE_OVERRUN_SECONDS)"
sql_overrun="$(window_constant WINDOW_SQL_PREDICATE_OVERRUN_SECONDS)"
driver_overrun="$(window_constant WINDOW_DRIVER_OVERRUN_SECONDS)"
diagnostic_budget="$(window_constant WINDOW_DIAGNOSTIC_MAX_SECONDS)"
sampler_stop_budget="$(window_constant WINDOW_SAMPLER_STOP_MAX_SECONDS)"
declared_outer="$(window_constant WINDOW_WORKFLOW_TIMEOUT_SECONDS)"
minimum_margin="$(window_constant WINDOW_MINIMUM_OUTER_MARGIN_SECONDS)"
((minimum_margin >= 60)) || {
  echo "the observation window must retain at least one minute below its outer timeout" >&2
  exit 1
}
for helper_name in g6rd_api_curl g6rd_enqueue_command g6rd_read_nodes; do
  helper_body="$(sed -n "/^${helper_name}() {/,/^}/p" "${LIB}")"
  grep -qF -- '--max-time 10' <<<"${helper_body}" || {
    echo "${helper_name} no longer matches the declared observation-window HTTP bound" >&2
    exit 1
  }
done
((api_overrun >= 10 && sql_overrun >= 10 && driver_overrun >= 21 \
  && diagnostic_budget >= 10 && sampler_stop_budget >= 8)) || {
  echo "the observation-window overrun constants understate their process bounds" >&2
  exit 1
}
workflow_window_minutes="$(ruby -r yaml -e '
  workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
  step = workflow.fetch("jobs").fetch("g6-rd-fd-b").fetch("steps")
    .find { |candidate| candidate["name"] == "Run the bounded observation window" }
  puts step.fetch("timeout-minutes")
' "${WORKFLOW}")"
workflow_outer=$((workflow_window_minutes * 60))
[[ "${declared_outer}" == "${workflow_outer}" ]] || {
  echo "the observation-window script and workflow disagree on the outer timeout" >&2
  exit 1
}
inner_budget=$((window_default + api_ready_budget + pre_drain_budget + settle_budget
  + post_drain_budget + api_overrun + (3 * sql_overrun) + driver_overrun
  + diagnostic_budget + sampler_stop_budget))
if ((inner_budget + minimum_margin >= workflow_outer)); then
  echo "observation-window inner budget ${inner_budget}s does not leave the required ${minimum_margin}s margin inside ${workflow_outer}s" >&2
  exit 1
fi

# shellcheck disable=SC2016  # assert literal process-group source expressions
for token in \
  'nohup setsid env' \
  'kill -TERM -- "-${pid}"' \
  'kill -KILL -- "-${pid}"' \
  'G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql' \
  '--label "ocservia.g6.run-id=${RUN_ID:?}"' \
  'PGOPTIONS="-c statement_timeout=$((timeout_seconds * 1000))"' \
  'docker ps --all --quiet --filter "label=ocservia.g6.run-id=${RUN_ID}"'; do
  grep -qF -- "${token}" "${LIB}" || {
    echo "background harness loop cleanup is not process-group bounded: ${token}" >&2
    exit 1
  }
done
# shellcheck disable=SC2016  # assert the literal watcher pid-file expression
grep -qF 'g6rd_stop_harness_loop "${G6RD_STATE}/${name}-watcher.pid"' "${FD_B}" || {
  echo "watcher cleanup does not terminate each scoped process group" >&2
  exit 1
}

# Every external predicate used by the new crash-window deadline loops has a
# per-attempt process timeout; the deadline helper itself intentionally only
# controls when another attempt may start.
for token in \
  'G6RD_PSQL_TIMEOUT_SECONDS=10 psql_primary' \
  'G6RD_COMPOSE_TIMEOUT_SECONDS=10 g6rd_compose'; do
  grep -qF "${token}" "${FD_B}" || {
    echo "crash-window external predicate is not per-attempt bounded: ${token}" >&2
    exit 1
  }
done
# shellcheck disable=SC2016  # assert the literal per-attempt timeout handoff
grep -qF 'G6RD_COMPOSE_TIMEOUT_SECONDS="${timeout_seconds}"' "${LIB}" || {
  echo "Agent journal observation is not per-attempt bounded" >&2
  exit 1
}

# Each crash-window wait stops starting attempts at its declared deadline, and
# its aggregate declared budget leaves at least one minute for bounded
# Docker/SQL overhead inside the ten-minute workflow step.
replacement_helper="$(sed -n '/^restart_worker_transport_unit() {/,/^}/p' "${FD_B}")"
deadline_budget() {
  local body="${1:?function body is required}"
  grep -oE '(g6rd_wait_until_deadline|wait_for_journal_command)[[:space:]]+[0-9]+' <<<"${body}" \
    | awk '{ total += $2 } END { print total + 0 }'
}
replacement_budget="$(deadline_budget "${replacement_helper}")"
for phase_name in phase_outbox_claim_before_send phase_outbox_send_before_mark \
  phase_outbox_result_before_commit phase_relay_pre_fault phase_scenario_relay \
  phase_scenario_path; do
  phase_body="$(sed -n "/^${phase_name}() {/,/^}/p" "${FD_B}")"
  if grep -qE 'g6rd_wait_until[[:space:]]' <<<"${phase_body}"; then
    echo "${phase_name} uses an attempt-count wait instead of a wall-clock deadline" >&2
    exit 1
  fi
  phase_budget="$(deadline_budget "${phase_body}")"
  nested_budget=0
  [[ "${phase_name}" == phase_outbox_* ]] && nested_budget="${replacement_budget}"
  if ((phase_budget + nested_budget > 540)); then
    echo "${phase_name} declares more than nine minutes of nested waits" >&2
    exit 1
  fi
  if [[ "${phase_name}" == phase_relay_pre_fault \
    || "${phase_name}" == phase_scenario_relay \
    || "${phase_name}" == phase_scenario_path ]]; then
    grep -qF 'G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5' <<<"${phase_body}" || {
      echo "${phase_name} lacks a short per-probe transport timeout" >&2
      exit 1
    }
  fi
done

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

# shellcheck source=scripts/g6-readiness-lib.sh disable=SC1091
source "${LIB}"
never_ready() { return 1; }
started="${SECONDS}"
if g6rd_wait_until_deadline 1 5 "deadline regression fixture" never_ready \
  >"${tmp}/deadline-stdout" 2>"${tmp}/deadline-stderr"; then
  echo "the deadline wait accepted a predicate that never became ready" >&2
  exit 1
fi
((SECONDS - started < 3)) || {
  echo "the deadline wait slept past its wall-clock budget" >&2
  exit 1
}
grep -qF 'timed out waiting for deadline regression fixture' \
  "${tmp}/deadline-stderr" || {
  echo "the deadline wait did not identify the timed-out condition" >&2
  exit 1
}

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
