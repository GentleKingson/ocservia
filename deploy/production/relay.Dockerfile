FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS toolchain
WORKDIR /build
FROM toolchain AS inputs
RUN --mount=type=bind,source=toolchains.lock,target=/tmp/toolchains.lock \
    --mount=type=bind,source=scripts/checksums.txt,target=/tmp/checksums.txt \
    mkdir scripts && awk -F= '$1 == "iroh"' /tmp/toolchains.lock > toolchains.lock \
    && awk '$2 ~ /^iroh-relay-.*\.crate$/' /tmp/checksums.txt > scripts/checksums.txt

FROM toolchain AS build
COPY --from=inputs /build/ ./
COPY scripts/build-relay.sh ./scripts/
COPY deploy/production/relay.Cargo.lock ./deploy/production/
RUN bash scripts/build-relay.sh /usr/local/cargo

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS relay-runtime
RUN apt-get -o APT::Update::Error-Mode=any update \
    && apt-get -o Acquire::Retries=0 install -y --no-install-recommends --only-upgrade apt libapt-pkg6.0 \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' apt)" ge 2.6.2 \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get install -y --no-install-recommends --only-upgrade perl-base libpcre2-8-0 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 relay \
    && useradd --system --uid 65532 --gid relay --home-dir /nonexistent --shell /usr/sbin/nologin relay \
    && install -d -o relay -g relay -m 0700 /var/lib/iroh-relay
COPY --from=build /usr/local/cargo/bin/iroh-relay /usr/local/bin/iroh-relay
COPY deploy/production/relay-entrypoint.sh /usr/local/bin/relay-entrypoint
COPY deploy/production/relay-healthcheck.sh /usr/local/bin/relay-healthcheck
USER relay:relay
ENTRYPOINT ["/usr/local/bin/relay-entrypoint"]
