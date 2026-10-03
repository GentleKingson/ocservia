#!/usr/bin/env bash
set -euo pipefail

if [[ ! "$(postgres --version)" =~ ^postgres\ \(PostgreSQL\)\ 18\. ]]; then
  echo 'ocservia requires a PostgreSQL 18.x server image' >&2
  exit 1
fi

# Keep the existing volume identity when adopting PostgreSQL 18's parent mount.
# A former PG17 volume then has PG_VERSION at the mount root, not at PGDATA.
# Refuse it before upstream initdb can create a second, empty cluster beside it.
if [[ "${PGDATA:-}" != /var/lib/postgresql/18/docker ]]; then
  echo 'ocservia requires PostgreSQL 18 with PGDATA=/var/lib/postgresql/18/docker' >&2
  exit 1
fi
for legacy in /var/lib/postgresql/PG_VERSION /var/lib/postgresql/data/PG_VERSION; do
  if [[ -e "${legacy}" ]]; then
    echo "legacy PostgreSQL data detected at ${legacy}; stop and perform an operator-managed major upgrade or dump/restore into a separate PostgreSQL 18 volume" >&2
    exit 1
  fi
done
if [[ -e "${PGDATA}/PG_VERSION" && "$(cat "${PGDATA}/PG_VERSION")" != 18 ]]; then
  echo 'existing PostgreSQL data is not major 18; automatic major upgrades are forbidden' >&2
  exit 1
fi
exec /usr/local/bin/docker-entrypoint.sh "$@"
