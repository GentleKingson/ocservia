FROM postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af AS runtime
RUN apt-get -o APT::Update::Error-Mode=any update \
    && apt-get -o Acquire::Retries=0 install -y --no-install-recommends --only-upgrade apt libapt-pkg6.0 \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' apt)" ge 2.6.2 \
    && apt-get install -y --no-install-recommends --only-upgrade libssl3 openssl libpcre2-8-0 \
        libperl5.36 perl perl-base perl-modules-5.36 libsasl2-2 libsasl2-modules-db \
    && rm -rf /var/lib/apt/lists/*
COPY --chmod=0755 scripts/postgres-backup.sh /usr/local/bin/ocservia-postgres-backup
COPY --chmod=0755 deploy/production/backup-entrypoint.sh /usr/local/bin/ocservia-backup-entrypoint
USER postgres:postgres
ENTRYPOINT ["/usr/local/bin/ocservia-backup-entrypoint"]
