#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/resilience-capacity-lib.sh
source "${ROOT}/scripts/resilience-capacity-lib.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf -- "${TMP_DIR}"' EXIT

# 1. validate_capacity_settings accepts in-envelope settings and rejects over-envelope / invalid settings.
validate_capacity_settings 500 2 30000 32 2048
if validate_capacity_settings 501 2 30000 32 2048 2>/dev/null; then
  echo "expected AGENT_COUNT > 500 to fail" >&2
  exit 1
fi
if validate_capacity_settings 0 2 30000 32 2048 2>/dev/null; then
  echo "expected non-positive AGENT_COUNT to fail" >&2
  exit 1
fi

# 2. validate_resource_samples aggregates maxima and validates phase coverage.
SAMPLES="${TMP_DIR}/samples.jsonl"
SUMMARY="${TMP_DIR}/summary.json"
cat >"${SAMPLES}" <<'EOF'
{"at":"2026-01-01T00:00:00Z","epoch_seconds":1000,"phase":"capacity-load","runtime":{"goroutines":64,"db_acquired":4,"db_idle":6,"db_total":10,"sse_active_streams":16,"sse_rejected_streams":0,"sse_watchers":1,"sse_unhealthy_watchers":0,"sse_sql_queries":12,"sse_slow_consumer_disconnects":0,"sse_database_backoff_seconds":0},"stub":{"active_tasks":120,"task_capacity":256},"control_rss_kib":65536,"control_fd":48,"stub_rss_kib":32768,"stub_fd":32,"postgres":{"active":2,"waiting":0,"available":true}}
{"at":"2026-01-01T00:00:10Z","epoch_seconds":1010,"phase":"slow-sse","runtime":{"goroutines":96,"db_acquired":6,"db_idle":4,"db_total":10,"sse_active_streams":100,"sse_rejected_streams":0,"sse_watchers":1,"sse_unhealthy_watchers":0,"sse_sql_queries":18,"sse_slow_consumer_disconnects":1,"sse_database_backoff_seconds":0},"stub":{"active_tasks":180,"task_capacity":256},"control_rss_kib":81920,"control_fd":140,"stub_rss_kib":36864,"stub_fd":36,"postgres":{"active":3,"waiting":0,"available":true}}
EOF
validate_resource_samples "${SAMPLES}" "${SUMMARY}" 2 10 0 "capacity-load,slow-sse" 15
test "$(jq -r .max_goroutines "${SUMMARY}")" = "96"
test "$(jq -r .max_control_rss_kib "${SUMMARY}")" = "81920"

# 3. validate_resource_slos is a no-op when no OCSERVIA_SLO_* variables are configured.
(
  unset OCSERVIA_SLO_MAX_GOROUTINES OCSERVIA_SLO_MAX_TOKIO_TASKS OCSERVIA_SLO_MAX_CONTROL_RSS_KIB \
    OCSERVIA_SLO_MAX_DB_ACQUIRED OCSERVIA_SLO_MAX_SSE_SQL_QUERIES OCSERVIA_SLO_MAX_SSE_UNHEALTHY_WATCHERS
  validate_resource_slos "${SUMMARY}"
)

# 4. validate_resource_slos passes when all configured SLO ceilings are respected.
(
  OCSERVIA_SLO_MAX_GOROUTINES=128 \
  OCSERVIA_SLO_MAX_TOKIO_TASKS=200 \
  OCSERVIA_SLO_MAX_CONTROL_RSS_KIB=131072 \
  OCSERVIA_SLO_MAX_DB_ACQUIRED=16 \
  OCSERVIA_SLO_MAX_SSE_SQL_QUERIES=20 \
  OCSERVIA_SLO_MAX_SSE_UNHEALTHY_WATCHERS=0 \
  validate_resource_slos "${SUMMARY}"
)

# 5. validate_resource_slos fails when any metric exceeds its SLO ceiling.
if (
  OCSERVIA_SLO_MAX_GOROUTINES=64 \
  validate_resource_slos "${SUMMARY}"
) 2>/dev/null; then
  echo "expected max_goroutines SLO breach to fail" >&2
  exit 1
fi

# 6. validate_resource_slos rejects non-integer SLO thresholds with exit code 2.
rc=0
(
  OCSERVIA_SLO_MAX_DB_ACQUIRED=abc \
  validate_resource_slos "${SUMMARY}"
) 2>/dev/null || rc=$?
test "${rc}" -eq 2

echo "resilience-capacity-lib tests passed"
