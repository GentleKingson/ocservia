ARG WORKFLOW_TOOLS=workflow-tools
FROM ocservia-pr02-agent:e2e AS agent
FROM ocservia-pr02-transport:e2e AS transport
FROM ocservia-pr02-relay:e2e AS relay
FROM golang:1.26.9-trixie@sha256:f06b7122f888680048afb247c4e5f49c0069eb6c4c1ccae0f3f4834527a8d22c AS workflow-tools

RUN apt-get update \
    && apt-get install -y --no-install-recommends ocserv openconnect openssl sqlite3 python3 util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 ocservia-control \
    && groupadd --gid 65533 ocserv-agent \
    && useradd --uid 65532 --gid 65532 --no-create-home transportd \
    && useradd --uid 65533 --gid 65533 --no-create-home ocserv-agent \
    && usermod --gid 65532 nobody
COPY --chmod=0555 deploy/database-e2e/systemctl /usr/bin/systemctl
WORKDIR /workspace/control-plane

FROM ${WORKFLOW_TOOLS} AS workflow-base
COPY --from=transport /usr/local/bin/ocservia-transportd /usr/local/bin/ocservia-transportd
COPY --from=relay /usr/local/bin/iroh-relay /usr/local/bin/iroh-relay

FROM workflow-base
COPY --from=agent /usr/local/bin/ocservia-agent /usr/local/bin/ocservia-agent
COPY --from=agent /usr/local/bin/ocservia-privd /usr/local/bin/ocservia-privd

COPY --from=agent /usr/local/bin/ocservia-upgrader /usr/local/bin/ocservia-upgrader
