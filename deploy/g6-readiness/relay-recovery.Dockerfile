# Diagnostic binaries are built natively on the Debian 13 BuildServer.
# This image is a targeted experiment, not a release artifact.
FROM debian:trixie-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends sqlite3 curl ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia ocservia-agent \
    && usermod --gid ocservia nobody \
    && install -d -o 65532 -g 65532 -m 0750 /run/ocserv-platform \
    && install -d -o 65534 -g 65532 -m 0750 /run/ocserv-trust
COPY bin/ /usr/local/bin/
COPY deploy/prepare-transport-runtime.sh /usr/local/libexec/ocservia-prepare-transport-runtime
COPY deploy/production/relay-entrypoint.sh /usr/local/bin/relay-entrypoint
COPY deploy/production/relay-healthcheck.sh /usr/local/bin/relay-healthcheck
COPY deploy/g6-readiness/agent-supervisor.sh /usr/local/libexec/ocservia-agent-supervisor
COPY deploy/g6-readiness/fake-ocserv/shims/systemctl /usr/bin/systemctl
COPY deploy/g6-readiness/fake-ocserv/shims/ocserv /usr/sbin/ocserv
COPY deploy/g6-readiness/fake-ocserv/shims/occtl /usr/bin/occtl
COPY deploy/g6-readiness/fake-ocserv/ocserv.conf /etc/ocserv/ocserv.conf
RUN chmod 0555 /usr/local/bin/* /usr/local/libexec/* /usr/bin/occtl /usr/sbin/ocserv /usr/bin/systemctl

FROM base AS control
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocserv-control"]

FROM base AS transportd
USER ocservia-agent:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]

FROM base AS probe
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-g6-probe"]

FROM base AS relay
USER ocservia-agent:ocservia
ENTRYPOINT ["/usr/local/bin/relay-entrypoint"]

FROM base AS agent
ENV OCSERVIA_NODE_ROOT=/var/lib/ocservia-agent
ENTRYPOINT ["/usr/local/libexec/ocservia-agent-supervisor"]
