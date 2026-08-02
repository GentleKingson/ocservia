#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if (cd "${ROOT}/control-plane" && go list -deps ./...) | grep -Eiq '(^|/)iroh($|/)'; then
  echo "Go dependency graph must not contain Iroh" >&2
  exit 1
fi

transport_tree="$(cd "${ROOT}/rust" && cargo tree --locked --package ocservia-transportd --prefix none)"
if grep -Eiq '^(postgres|postgres-types|tokio-postgres|sqlx|diesel|sea-orm|rusqlite) ' <<<"${transport_tree}"; then
  echo "transportd dependency graph must not contain a database client" >&2
  exit 1
fi

if grep -R --line-number --include='*.rs' -E '(postgres://|postgresql://|DATABASE_URL)' \
  "${ROOT}/rust/crates/transportd"; then
  echo "transportd source must not contain database configuration" >&2
  exit 1
fi
