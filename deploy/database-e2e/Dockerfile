FROM ocservia-pr02-agent:e2e AS agent
FROM ocservia-pr02-transport:e2e AS transport
FROM ocservia-pr02-relay:e2e AS relay
FROM golang:1.26.6-trixie@sha256:b75d466dd608587fd66cca705a307ba65b889827d06ad61d6a75f0482b51b7c7

RUN apt-get update \
    && apt-get install -y --no-install-recommends ocserv openconnect openssl sqlite3 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 ocservia-control \
    && groupadd --gid 65533 ocservia-agent \
    && useradd --uid 65532 --gid 65532 --no-create-home transportd \
    && useradd --uid 65533 --gid 65533 --no-create-home ocservia-agent \
    && usermod --gid 65532 nobody
COPY --from=agent /usr/local/bin/ocservia-agent /usr/local/bin/ocservia-agent
COPY --from=agent /usr/local/bin/ocservia-privd /usr/local/bin/ocservia-privd
COPY --from=transport /usr/local/bin/ocservia-transportd /usr/local/bin/ocservia-transportd
COPY --from=relay /usr/local/bin/iroh-relay /usr/local/bin/iroh-relay
COPY --chmod=0555 deploy/database-e2e/systemctl /usr/bin/systemctl
WORKDIR /workspace/control-plane
