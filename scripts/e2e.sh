#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT="${COMPOSE_PROJECT:-ocservia-i03-e2e}"
COMPOSE=(docker compose -p "${PROJECT}" -f "${ROOT}/deploy/compose/compose.yaml")

cleanup() {
  "${COMPOSE[@]}" --profile e2e down --volumes --remove-orphans
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
"${COMPOSE[@]}" --profile e2e run --rm e2e
