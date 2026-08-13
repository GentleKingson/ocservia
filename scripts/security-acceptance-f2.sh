#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"

case "${RUN_ID}" in
  *[!a-zA-Z0-9._-]*) echo "RUN_ID contains unsafe characters" >&2; exit 2 ;;
esac

mkdir -p "${ARTIFACT_DIR}"
summaries=()
for scale in 1 10 100; do
  scale_artifacts="${ARTIFACT_DIR}/sse-${scale}"
  scale_run_id="${RUN_ID}-sse-${scale}"
  mkdir -p "${scale_artifacts}"
  P1_PROFILE=full \
  AGENT_COUNT=24 \
  HEARTBEAT_COUNT=2 \
  HEARTBEAT_INTERVAL_MS=500 \
  REQUEST_CONCURRENCY=8 \
  QUEUE_CAPACITY=256 \
  SSE_VIEWERS="${scale}" \
  MINIMUM_RESOURCE_SAMPLES=8 \
  RUN_ID="${scale_run_id}" \
  COMPOSE_PROJECT="ocservia-${scale_run_id}" \
  ARTIFACT_DIR="${scale_artifacts}" \
    "${ROOT}/scripts/p1-resilience-capacity.sh"

  grep -Fxq "sse_viewers=${scale}" "${scale_artifacts}/run-parameters.txt"
  grep -Fq 'Last-Event-ID reconnect:' "${scale_artifacts}/p1-metrics.txt"
  grep -Fxq 'sse released active: 0' "${scale_artifacts}/p1-metrics.txt"
  grep -Fxq 'sse released watchers: 0' "${scale_artifacts}/p1-metrics.txt"
  grep -Fxq 'test_exit=0 sampler_exit=0 trap_exit=0 cleanup_exit=0' \
    "${scale_artifacts}/p1-exit-status.log"
  jq -e --argjson scale "${scale}" '
    .max_sse_active_streams >= $scale and
    .max_sse_watchers == 1 and
    .sse_rejected_streams == 0 and
    .sampler_exit == 0 and
    .max_control_rss_kib > 0 and
    .max_control_fd > 0
  ' "${scale_artifacts}/p1-summary.json" >/dev/null
  summaries+=("${scale_artifacts}/p1-summary.json")
done

jq -s '
  [.[0], .[1], .[2]] |
  {
    F2: "PASS",
    scales: [1, 10, 100],
    sustained_push: "PASS",
    reconnect: "PASS",
    last_event_id: "PASS",
    release: "PASS",
    resources: map({
      max_sse_active_streams,
      max_sse_watchers,
      max_control_rss_kib,
      max_control_fd,
      max_goroutines,
      max_db_total
    })
  }
' "${summaries[@]}" >"${ARTIFACT_DIR}/f2-summary.json"
echo "F2 SSE live capacity acceptance passed at 1, 10, and 100 connections"
