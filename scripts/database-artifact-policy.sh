#!/usr/bin/env bash
# Two executable PostgreSQL SQL artifacts anchored to the released checkpoint.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
case "${1:-all}" in
  postgres|all) ;;
  *) echo 'usage: database-artifact-policy.sh [postgres|all]' >&2; exit 2 ;;
esac
directory="${ROOT}/control-plane/migrations"
actual="$(find "${directory}" -type f \( -name "*.sql" -o -name "*.json" \) | sort)"
expected="$(printf '%s\n' "${directory}/schema.sql" "${directory}/upgrade.sql")"
[[ "$actual" == "$expected" ]] || { echo "unexpected migration artifacts in ${directory}" >&2; exit 1; }
[[ ! -e "${ROOT}/control-plane/internal/database/mysql" ]] || { echo 'retired MySQL database package remains present' >&2; exit 1; }
go -C "${ROOT}/control-plane" test ./migrations -run '^TestPostgreSQLArtifactCatalog$' -count=1
