FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS build
WORKDIR /build
COPY toolchains.lock ./
COPY scripts/build-relay.sh scripts/checksums.txt ./scripts/
COPY deploy/production/relay.Cargo.lock ./deploy/production/
RUN bash scripts/build-relay.sh /usr/local/cargo

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS relay-runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 relay \
    && useradd --system --uid 65532 --gid relay --home-dir /nonexistent --shell /usr/sbin/nologin relay
COPY --from=build /usr/local/cargo/bin/iroh-relay /usr/local/bin/iroh-relay
COPY deploy/production/relay-entrypoint.sh /usr/local/bin/relay-entrypoint
COPY deploy/production/relay-healthcheck.sh /usr/local/bin/relay-healthcheck
USER relay:relay
ENTRYPOINT ["/usr/local/bin/relay-entrypoint"]
