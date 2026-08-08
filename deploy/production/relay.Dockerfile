FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS build
RUN cargo install --locked --version 1.0.3 --features server iroh-relay

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 relay \
    && useradd --system --uid 65532 --gid relay --home-dir /nonexistent --shell /usr/sbin/nologin relay
COPY --from=build /usr/local/cargo/bin/iroh-relay /usr/local/bin/iroh-relay
COPY deploy/production/relay-entrypoint.sh /usr/local/bin/relay-entrypoint
COPY deploy/production/relay-healthcheck.sh /usr/local/bin/relay-healthcheck
USER relay:relay
ENTRYPOINT ["/usr/local/bin/relay-entrypoint"]
