#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CALLER_CARGO_HOME="${CARGO_HOME:-}"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
if [[ -n "${CALLER_CARGO_HOME}" ]]; then
  export CARGO_HOME="${CALLER_CARGO_HOME}"
fi

SCOPE="${1:-all}"
if (($# > 1)); then
  echo "usage: $0 [all|go|rust|web|bundle]" >&2
  exit 2
fi
case "${SCOPE}" in
  all | go | rust | web | bundle) ;;
  *)
    echo "unsupported benchmark scope: ${SCOPE} (expected all|go|rust|web|bundle)" >&2
    exit 2
    ;;
esac

OUTPUT_DIR="${OUTPUT_DIR:-}"
GO_BENCH_COUNT="${GO_BENCH_COUNT:-3}"
GO_BENCH_TIME="${GO_BENCH_TIME:-500ms}"

if [[ -n "${OUTPUT_DIR}" ]]; then
  mkdir -p "${OUTPUT_DIR}"
fi

emit_section() {
  local name=$1
  shift
  echo "=== perf-bench: ${name} ==="
  if [[ -n "${OUTPUT_DIR}" ]]; then
    "$@" | tee "${OUTPUT_DIR}/${name}.txt"
  else
    "$@"
  fi
}

run_go_bench() (
  local tmpdir
  mkdir -p "${ROOT}/.cache"
  tmpdir="$(mktemp -d "${ROOT}/.cache/go-bench-tmp.XXXXXX")"
  trap 'rm -rf -- "${tmpdir}"' EXIT
  export TMPDIR="${tmpdir}"
  (
    cd "${ROOT}/control-plane"
    go test -run='^$' -bench=. -benchmem -count="${GO_BENCH_COUNT}" -benchtime="${GO_BENCH_TIME}" \
      ./internal/database/value \
      ./internal/database/postgres \
      ./internal/operations \
      ./internal/audit \
      ./internal/eventstream \
      ./internal/telemetry
  )
  (
    cd "${ROOT}/signer"
    go test -run='^$' -bench=. -benchmem -count="${GO_BENCH_COUNT}" -benchtime="${GO_BENCH_TIME}" .
  )
)

run_rust_bench() {
  (
    cd "${ROOT}/rust"
    cargo test --release -p ocservia-contracts --test perf_bench -- --ignored --nocapture --test-threads=1
    cargo test --release -p ocservia-command-authorization --test perf_bench -- --ignored --nocapture --test-threads=1
    cargo test --release -p ocservia-command-journal --test perf_bench -- --ignored --nocapture --test-threads=1
  )
}

run_web_bench() {
  (
    cd "${ROOT}/web"
    npx vitest bench --run test/perf.bench.ts
  )
}

run_web_bundle() {
  (
    cd "${ROOT}/web"
    npm run build
  )
}

case "${SCOPE}" in
  all)
    emit_section go_bench run_go_bench
    emit_section rust_bench run_rust_bench
    emit_section web_bench run_web_bench
    emit_section web_bundle run_web_bundle
    ;;
  go)
    emit_section go_bench run_go_bench
    ;;
  rust)
    emit_section rust_bench run_rust_bench
    ;;
  web)
    emit_section web_bench run_web_bench
    ;;
  bundle)
    emit_section web_bundle run_web_bundle
    ;;
esac
