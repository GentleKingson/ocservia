#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
BASELINE_SHA=13705da536d3c335f581601e5878b774a4b2af29
work="${RUNNER_TEMP:-/tmp}/ocservia-f1-${RUN_ID}"
baseline="${work}/baseline"
baseline_target="${work}/baseline-target"
candidate_target="${work}/candidate-target"

case "${RUN_ID}" in
  *[!a-zA-Z0-9._-]*) echo "RUN_ID contains unsafe characters" >&2; exit 2 ;;
esac

cleanup() {
  local status=$?
  git -C "${ROOT}" worktree remove --force "${baseline}" >/dev/null 2>&1 || true
  rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

mkdir -p "${work}" "${ARTIFACT_DIR}"
git -C "${ROOT}" cat-file -e "${BASELINE_SHA}^{commit}"
git -C "${ROOT}" worktree add --detach "${baseline}" "${BASELINE_SHA}" >/dev/null
git -C "${baseline}" apply "${ROOT}/testdata/security-f1-shared-budget-baseline.patch"

set +e
(
  cd "${baseline}/rust"
  CARGO_TARGET_DIR="${baseline_target}" cargo test --locked -p ocservia-transportd \
    shared_budget_baseline_positive_control -- --nocapture
) 2>&1 | tee "${ARTIFACT_DIR}/baseline-shared-budget.log"
baseline_status=${PIPESTATUS[0]}
set -e
if ((baseline_status != 0)) || ! grep -Fq \
  'tests::shared_budget_baseline_positive_control ... ok' \
  "${ARTIFACT_DIR}/baseline-shared-budget.log"; then
  printf 'F1=INVALID\nbaseline_sha=%s\nreason=shared-budget-positive-control-not-reproduced\n' \
    "${BASELINE_SHA}" >"${ARTIFACT_DIR}/f1-summary.txt"
  echo "F1 INVALID: baseline shared-budget positive control was not reproduced" >&2
  exit 1
fi

(
  cd "${ROOT}/rust"
  CARGO_TARGET_DIR="${candidate_target}" cargo test --locked -p ocservia-transportd --lib -- --nocapture
) 2>&1 | tee "${ARTIFACT_DIR}/candidate-live-iroh.log"

for scenario in \
  baseline_budget_parameters_remain_partitioned \
  router_restart_retains_identity_and_direct_connectivity \
  only_multi_member_custom_relays_enable_persistent_connections \
  relay_connections_are_not_persistent_by_default \
  persistent_relay_map_add_remove_reconciles_connections \
  dedicated_relay_failure_accepts_an_immediate_survivor_connection \
  newer_authority_revision_closes_the_retained_session \
  trust_update_reports_stale_and_rejects_tombstone_reactivation \
  enrollment_identity_churn_refuses_new_identity_without_evicting_oldest; do
  grep -Fq "tests::${scenario} ... ok" "${ARTIFACT_DIR}/candidate-live-iroh.log" || {
    printf 'F1=FAIL\nmissing_scenario=%s\n' "${scenario}" >"${ARTIFACT_DIR}/f1-summary.txt"
    echo "F1 candidate scenario did not pass: ${scenario}" >&2
    exit 1
  }
done

printf '%s\n' \
  'F1=PASS' \
  "baseline_sha=${BASELINE_SHA}" \
  'baseline_shared_budget=REPRODUCED' \
  'parameters=600-attempts-per-minute,16-concurrent' \
  'candidate_direct=PASS' \
  'candidate_isolated_relay=PASS' \
  'candidate_revoke=PASS' \
  'candidate_snapshot=PASS' \
  'candidate_restart=PASS' \
  'candidate_identity_churn=PASS' >"${ARTIFACT_DIR}/f1-summary.txt"
echo "F1 live Iroh acceptance passed"
