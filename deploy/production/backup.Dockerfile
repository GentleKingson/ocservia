FROM postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af
RUN apt-get update \
    && apt-get install -y --no-install-recommends --only-upgrade libssl3 openssl libpcre2-8-0 \
    && rm -rf /var/lib/apt/lists/*
COPY --chmod=0755 scripts/postgres-backup.sh /usr/local/bin/ocservia-postgres-backup
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER postgres:postgres
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
