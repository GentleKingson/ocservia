FROM mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc
COPY --chmod=0755 scripts/mysql-backup.sh /usr/local/bin/ocservia-mysql-backup
COPY --chmod=0755 scripts/mysql-restore-verify.sh /usr/local/bin/ocservia-mysql-restore-verify
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER 999:999
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
