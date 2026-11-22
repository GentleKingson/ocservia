#!/usr/bin/env bash
# Embedded PostgreSQL goose SQL migrations live under control-plane/migrations/sql/.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
case "${1:-all}" in
  postgres|all) ;;
  *) echo 'usage: database-artifact-policy.sh [postgres|all]' >&2; exit 2 ;;
esac
directory="${ROOT}/control-plane/migrations"
stray="$(find "${directory}" -maxdepth 1 -type f \( -name "*.sql" -o -name "*.json" \) | sort)"
[[ -z "${stray}" ]] || { echo "unexpected top-level migration artifacts in ${directory}: ${stray}" >&2; exit 1; }
[[ -f "${directory}/sql/0001_initial_schema.sql" ]] || { echo "missing baseline migration ${directory}/sql/0001_initial_schema.sql" >&2; exit 1; }
[[ ! -e "${ROOT}/control-plane/internal/database/mysql" ]] || { echo 'retired MySQL database package remains present' >&2; exit 1; }
go -C "${ROOT}/control-plane" test ./migrations -run '^TestEmbeddedGooseMigrationsParseCleanly$' -count=1
