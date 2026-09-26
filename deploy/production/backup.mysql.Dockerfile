FROM mysql:8.4.11@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d
RUN microdnf upgrade -y curl libcurl && microdnf clean all
COPY --chmod=0755 scripts/mysql-backup.sh /usr/local/bin/ocservia-mysql-backup
COPY --chmod=0755 scripts/mysql-restore-verify.sh /usr/local/bin/ocservia-mysql-restore-verify
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER 999:999
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
