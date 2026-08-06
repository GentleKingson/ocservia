#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT="${COMPOSE_PROJECT:-ocservia-e2e-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
COMPOSE=(docker compose -p "${PROJECT}" -f "${ROOT}/deploy/compose/compose.yaml")

cleanup() {
  local test_exit=$? cleanup_exit=0 leftovers
  trap - EXIT INT TERM
  set +e
  if [[ -n "${ARTIFACT_DIR}" ]]; then
    mkdir -p "${ARTIFACT_DIR}" || cleanup_exit=1
    "${COMPOSE[@]}" ps --all >"${ARTIFACT_DIR}/docker-ps.txt" 2>&1 || true
    "${COMPOSE[@]}" logs --no-color >"${ARTIFACT_DIR}/docker-compose.log" 2>&1 || true
  fi
  "${COMPOSE[@]}" --profile e2e down --volumes --remove-orphans --rmi local || cleanup_exit=1
  leftovers="$(docker ps -a --filter "label=com.docker.compose.project=${PROJECT}" -q)"
  leftovers+="$(docker volume ls --filter "label=com.docker.compose.project=${PROJECT}" -q)"
  leftovers+="$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" -q)"
  if [[ -n "${leftovers}" ]]; then
    echo "scoped cleanup left Compose resources for ${PROJECT}" >&2
    cleanup_exit=1
  fi
  if ((test_exit != 0)); then
    exit "${test_exit}"
  fi
  exit "${cleanup_exit}"
}
trap cleanup EXIT INT TERM

"${COMPOSE[@]}" up --build -d postgres otel-collector transportd-stub migrate control-plane web
for _ in $(seq 1 60); do
  if "${COMPOSE[@]}" exec -T control-plane /usr/bin/curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null; then
    break
  fi
  sleep 2
done
"${COMPOSE[@]}" exec -T control-plane /usr/bin/curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null
if [[ -n "${ARTIFACT_DIR}" ]]; then
  mkdir -p "${ARTIFACT_DIR}"
  "${COMPOSE[@]}" --profile e2e run --rm \
    -v "${ARTIFACT_DIR}:/artifacts" \
    -e PLAYWRIGHT_HTML_OUTPUT_DIR=/artifacts/playwright-report \
    -e PLAYWRIGHT_OUTPUT_DIR=/artifacts/test-results \
    e2e
else
  "${COMPOSE[@]}" --profile e2e run --rm e2e
fi
