#!/usr/bin/env bash
# Phase A: two active SQL files per engine; legacy assets are bridge-only.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
case "${1:-all}" in
  postgres) package=./migrations; test=TestPostgreSQLArtifactCatalog ;;
  mysql) package=./internal/database/mysql; test=TestMySQLArtifactCatalog ;;
  all) package='./migrations ./internal/database/mysql'; test='Test(PostgreSQL|MySQL)ArtifactCatalog' ;;
  *) echo 'usage: database-artifact-policy.sh [postgres|mysql|all]' >&2; exit 2 ;;
esac
(cd "${ROOT}" && sha256sum -c docs/database-migrations.sha256 >/dev/null)
# shellcheck disable=SC2086 # Intentional fixed package list, never user input.
go -C "${ROOT}/control-plane" test ${package} -run "^${test}$" -count=1
