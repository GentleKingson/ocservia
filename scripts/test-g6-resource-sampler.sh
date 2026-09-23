#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/g6-readiness-lib.sh disable=SC1091
source "${ROOT}/scripts/g6-readiness-lib.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
export FD_ID=fd-b G6RD_ENVIRONMENT_ID=fixture-environment
export G6RD_CANDIDATE_SHA=0123456789abcdef0123456789abcdef01234567
export G6RD_STATE="${fixture}/state" G6RD_SECRETS="${fixture}/secrets"
mkdir -p "${G6RD_STATE}" "${G6RD_SECRETS}"
printf '%s\n' fixture-secret >"${G6RD_SECRETS}/owner-password"
g6rd_now() { printf '2026-08-19T00:00:00.000001Z\n'; }

(
  g6rd_psql() {
    echo 'connection failed: fixture-secret password=fixture-secret' >&2
    return 124
  }
  if g6rd_sampler_tick "${fixture}/samples.csv" 2>"${fixture}/error"; then
    echo 'a failed database counter probe was accepted' >&2
    exit 1
  else
    [[ "$?" == 124 ]] || { echo 'database probe status was rewritten' >&2; exit 1; }
  fi
  grep -qF 'stage=db_counters component=postgres-fd-b timeout=3s elapsed=' "${fixture}/error"
  grep -qF 'status=124 stderr=connection failed: [redacted] password=[redacted]' "${fixture}/error"
  ! grep -qF fixture-secret "${fixture}/error"
  [[ ! -s "${fixture}/samples.csv" ]]
  g6rd_psql() { printf 'failure %1200s\n' x >&2; return 1; }
  if g6rd_sampler_tick "${fixture}/samples.csv" 2>"${fixture}/error"; then
    echo 'an oversized database error was accepted' >&2
    exit 1
  fi
  [[ "$(wc -c <"${fixture}/error")" -lt 1200 ]]
  grep -qF '[truncated]' "${fixture}/error"
  g6rd_psql() {
    printf '%s\n' \
      'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.dynamic.jwt' \
      'Cookie: a=x; b=y' \
      'probe failed' >&2
    return 1
  }
  if g6rd_sampler_tick "${fixture}/samples.csv" 2>"${fixture}/error"; then
    echo 'a failed authenticated database probe was accepted' >&2
    exit 1
  fi
  grep -qF 'Authorization=[redacted]' "${fixture}/error"
  grep -qF 'Cookie=[redacted]' "${fixture}/error"
  grep -qF 'probe failed' "${fixture}/error"
  ! grep -Eq 'Bearer|eyJhbGci|a=x|b=y' "${fixture}/error"
  for invalid in 'bad 2:db_connections' '2 bad:queue_depth' '1 2 3 4:queue_depth'; do
    value="${invalid%%:*}"
    field="${invalid#*:}"
    g6rd_psql() { printf '%s\n' "${value}"; }
    if g6rd_sampler_tick "${fixture}/samples.csv" 2>"${fixture}/error"; then
      echo "an invalid ${field} counter was accepted" >&2
      exit 1
    fi
    grep -qF "invalid ${field}" "${fixture}/error"
  done
)

(
  g6rd_agent_compose() {
    [[ "$1" == exec && "$2" == -T && "$3" == --user && "$4" == 65532:65532 ]]
    echo 'password=fixture-secret' >&2
    return 124
  }
  if G6RD_COMPOSE_TIMEOUT_SECONDS=4 g6rd_sampler_row agent agent-fd-b-01 agent-fd-b-01 \
    'cat /run/ocserv-platform/agent.pid' 'echo 1' 0 '' \
    2026-08-19T00:00:00Z >"${fixture}/row.csv" 2>"${fixture}/error"; then
    echo 'a failed component probe was accepted' >&2
    exit 1
  else
    [[ "$?" == 124 ]] || { echo 'component probe status was rewritten' >&2; exit 1; }
  fi
  grep -qF 'stage=component_probe component=agent-fd-b-01 timeout=4s' "${fixture}/error"
  grep -qF 'status=124 stderr=password=[redacted]' "${fixture}/error"
  [[ ! -s "${fixture}/row.csv" ]]
  [[ -z "$(find "${G6RD_STATE}" -name 'sampler-stderr.*' -print -quit)" ]]
)

(
  g6rd_psql() { printf '1 2\n'; }
  g6rd_sampler_row() { [[ "$1" != agent ]] || return 124; }
  if g6rd_sampler_tick "${fixture}/batch.csv"; then
    echo 'a failed component batch was accepted' >&2
    exit 1
  else
    [[ "$?" == 124 ]] || { echo 'batch status was rewritten' >&2; exit 1; }
  fi
  [[ ! -s "${fixture}/batch.csv" ]]
  [[ -z "$(find "${G6RD_STATE}" -name 'sampler-tick.*' -print -quit)" ]]
)

(
  export G6RD_STATE="${fixture}/loop-success" G6RD_SAMPLER_OUT="${fixture}/batch.csv"
  mkdir -p "${G6RD_STATE}"
  g6rd_sampler_tick() { : >"${G6RD_STATE}/sampler-stop"; }
  sleep() { :; }
  g6rd_sampler_loop
  [[ -s "${G6RD_STATE}/sampler-complete-at" && ! -e "${G6RD_STATE}/sampler-failed-at" ]]
)
(
  export G6RD_STATE="${fixture}/loop-failure" G6RD_SAMPLER_OUT="${fixture}/batch.csv"
  mkdir -p "${G6RD_STATE}"
  g6rd_sampler_tick() { return 124; }
  if g6rd_sampler_loop; then
    echo 'a failed sampler loop was accepted' >&2
    exit 1
  else
    [[ "$?" == 124 ]] || { echo 'sampler loop status was rewritten' >&2; exit 1; }
  fi
  [[ -s "${G6RD_STATE}/sampler-failed-at" && ! -e "${G6RD_STATE}/sampler-complete-at" ]]
)

command -v setsid >/dev/null || { echo 'setsid is required for sampler stop fixtures' >&2; exit 1; }
(
  export G6RD_STATE="${fixture}/graceful"
  mkdir -p "${G6RD_STATE}"
  : >"${G6RD_STATE}/sampler-started-at"
  setsid bash -c '
    while [[ ! -e "${G6RD_STATE}/sampler-stop" ]]; do sleep 0.05; done
    printf "%s\n" "2026-08-19T00:00:08.000001Z" >"${G6RD_STATE}/sampler-complete-at"
  ' &
  pid=$!
  printf '%s\n' "${pid}" >"${G6RD_STATE}/sampler.pid"
  g6rd_stop_sampler
  [[ -s "${G6RD_STATE}/sampler-complete-at" && ! -e "${G6RD_STATE}/sampler.pid" ]]
  ! kill -0 -- "-${pid}" 2>/dev/null
)
(
  export G6RD_STATE="${fixture}/unrecovered"
  mkdir -p "${G6RD_STATE}"
  printf '%s\n' 12345 >"${G6RD_STATE}/sampler.pid"
  kill() { [[ "$1" == -0 ]]; }
  wait() { return 0; }
  sleep() { :; }
  if g6rd_stop_sampler_process "${G6RD_STATE}/sampler.pid" >"${fixture}/stop.log" 2>&1; then
    echo 'a surviving sampler process group was accepted' >&2
    exit 1
  else
    [[ "$?" == 2 ]] || { echo 'unrecovered sampler status was not classified' >&2; exit 1; }
  fi
  grep -qF 'did not terminate' "${fixture}/stop.log"
  [[ -s "${G6RD_STATE}/sampler.pid" ]]
)

run_cleanup_case() (
  scenario="$1"
  dir="${fixture}/cleanup-${scenario}"
  export RUNNER_TEMP="${dir}" RUN_ID=fixture COMPOSE_PROJECT=ocservia-g6-rd-fixture
  export G6RD_STATE="${dir}/state" G6RD_LOGS="${dir}/logs"
  export G6RD_WORK="${dir}/g6-readiness-work"
  export G6RD_ARCHIVE="${G6RD_WORK}/archive"
  export G6RD_BASEBACKUP="${G6RD_WORK}/basebackup"
  export G6RD_RESTORE="${G6RD_WORK}/restore"
  export G6RD_AGENT_COMPOSE="${dir}/missing-agent-overlay"
  mkdir -p "${G6RD_STATE}" "${G6RD_LOGS}" "${G6RD_WORK}"
  unset G6RD_AGENT_IMAGE G6RD_CONTROL_PLANE_IMAGE G6RD_TRANSPORTD_IMAGE \
    G6RD_RELAY_IMAGE G6RD_PROBE_IMAGE
  g6rd_release_synthetic_barriers() { :; }
  g6rd_tunnel_stop() { :; }
  g6rd_reclaim_directory() { :; }
  g6rd_compose() { printf '%s\n' "$*" >"${dir}/compose-call"; }
  docker() {
    case "$1" in
      ps | images) return 0 ;;
      network | volume) return 1 ;;
      *) echo "unexpected cleanup Docker call: $*" >&2; return 1 ;;
    esac
  }
  : >"${G6RD_STATE}/sampler-started-at"
  if [[ "${scenario}" == failed-recovered* ]]; then
    setsid bash -c '
      while [[ ! -e "${G6RD_STATE}/sampler-stop" ]]; do sleep 0.05; done
      printf "%s\n" "2026-08-19T00:00:08.000001Z" >"${G6RD_STATE}/sampler-failed-at"
      exit 124
    ' &
    printf '%s\n' "$!" >"${G6RD_STATE}/sampler.pid"
    expected='sampler_failed=1 sampler_stop_failed=0 sampler_process_cleanup_failed=0 other_resource_cleanup_failed=0'
    if [[ "${scenario}" == failed-recovered-twice ]]; then
      if g6rd_stop_sampler >"${dir}/first-stop.log" 2>&1; then
        echo 'a failed sampler was accepted on first stop' >&2
        exit 1
      fi
      [[ ! -e "${G6RD_STATE}/sampler.pid" && ! -e "${G6RD_STATE}/sampler-started-at" ]]
    fi
  elif [[ "${scenario}" == forced-recovered ]]; then
    printf '%s\n' 12345 >"${G6RD_STATE}/sampler.pid"
    terminated=0
    kill() {
      if [[ "$1" == -0 ]]; then
        ((terminated == 0))
      else
        terminated=1
      fi
    }
    wait() { return 143; }
    sleep() { :; }
    expected='sampler_failed=0 sampler_stop_failed=1 sampler_process_cleanup_failed=0 other_resource_cleanup_failed=0'
  else
    printf '%s\n' invalid >"${G6RD_STATE}/sampler.pid"
    expected='sampler_failed=0 sampler_stop_failed=1 sampler_process_cleanup_failed=1 other_resource_cleanup_failed=0'
  fi
  if g6rd_cleanup 2>"${dir}/cleanup.log"; then
    echo "cleanup accepted ${scenario}" >&2
    exit 1
  fi
  grep -qF "${expected}" "${dir}/cleanup.log"
  grep -qF 'down --volumes --remove-orphans --rmi local' "${dir}/compose-call"
)
run_cleanup_case failed-recovered
run_cleanup_case failed-recovered-twice
run_cleanup_case forced-recovered
run_cleanup_case unrecovered
