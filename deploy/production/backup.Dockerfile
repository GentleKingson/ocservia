FROM postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f
COPY --chmod=0755 scripts/postgres-backup.sh /usr/local/bin/ocservia-postgres-backup
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER postgres:postgres
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
