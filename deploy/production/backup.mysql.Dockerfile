FROM mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6
COPY --chmod=0755 scripts/mysql-backup.sh /usr/local/bin/ocservia-mysql-backup
COPY --chmod=0755 scripts/mysql-restore-verify.sh /usr/local/bin/ocservia-mysql-restore-verify
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER 999:999
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
