FROM postgres:17.10-bookworm
COPY scripts/postgres-backup.sh /usr/local/bin/ocservia-postgres-backup
USER postgres:postgres
ENTRYPOINT ["/usr/local/bin/ocservia-postgres-backup"]
