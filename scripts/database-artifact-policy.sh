#!/usr/bin/env bash
# Two executable artifacts per engine, anchored to the released checkpoint.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
case "${1:-all}" in
  postgres) package=./migrations; test=TestPostgreSQLArtifactCatalog ;;
  mysql) package=./internal/database/mysql; test=TestMySQLArtifactCatalog ;;
  all) package='./migrations ./internal/database/mysql'; test='Test(PostgreSQL|MySQL)ArtifactCatalog' ;;
  *) echo 'usage: database-artifact-policy.sh [postgres|mysql|all]' >&2; exit 2 ;;
esac
for directory in control-plane/migrations control-plane/internal/database/mysql/mysql; do
  actual="$(rg --files --hidden "${ROOT}/${directory}" -g "*.sql" -g "*.json" | sort)"
  expected="$(printf '%s\n' "${ROOT}/${directory}/schema.sql" "${ROOT}/${directory}/upgrade.sql")"
  [[ "$actual" == "$expected" ]] || { echo "unexpected migration artifacts in ${directory}" >&2; exit 1; }
done
[[ ! -d "${ROOT}/control-plane/internal/database/mysql/history" ]] || { echo 'legacy MySQL manifests remain active' >&2; exit 1; }
# shellcheck disable=SC2086 # Intentional fixed package list, never user input.
go -C "${ROOT}/control-plane" test ${package} -run "^${test}$" -count=1
