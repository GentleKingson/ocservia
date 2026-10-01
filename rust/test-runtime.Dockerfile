# Test helpers compile separately from production products; Cargo feature sets do not unify.
FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS test-rust-source
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates

FROM test-rust-source AS transport-builder
RUN cargo build --locked --release --package ocservia-transportd \
    && mkdir -p /out/transportd \
    && cp target/release/ocservia-transportd /out/transportd/

FROM test-rust-source AS agent-builder
RUN cargo build --locked --release --package ocservia-agent --package ocservia-privd --package ocservia-upgrader \
    && mkdir -p /out/agent \
    && cp target/release/ocservia-agent target/release/ocservia-privd target/release/ocservia-upgrader /out/agent/

# Mirrors the runtime-base stage of rust/transportd.Dockerfile.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS transportd-runtime-base
RUN groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia transportd \
    && install -d -o transportd -g ocservia -m 0750 /run/ocserv-platform \
    && install -d -o 65534 -g ocservia -m 0750 /run/ocserv-trust
COPY --chmod=0555 deploy/prepare-transport-runtime.sh /usr/local/libexec/ocservia-prepare-transport-runtime
COPY --chmod=0555 deploy/production/transportd-relays.sh /usr/local/libexec/ocservia-transportd-relays
USER transportd:ocservia

FROM transportd-runtime-base AS transportd-runtime
COPY --from=transport-builder /out/transportd/ocservia-transportd /usr/local/bin/ocservia-transportd
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--key-file", "/run/secrets/controller-iroh.key", "--control-plane-uid", "65534", "--control-plane-gid", "65532"]

FROM test-rust-source AS relay-network-builder
RUN cargo build --locked --release --package ocservia-relay-network-probe

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS relay-network-probe
COPY --from=relay-network-builder /src/target/release/ocservia-relay-network-probe /usr/local/bin/relay-network
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/relay-network"]

# Real binaries consumed by the disposable database/Rebind workflow image.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS agent-runtime
COPY --from=agent-builder /out/agent/ocservia-agent /usr/local/bin/ocservia-agent
COPY --from=agent-builder /out/agent/ocservia-privd /usr/local/bin/ocservia-privd
COPY --from=agent-builder /out/agent/ocservia-upgrader /usr/local/bin/ocservia-upgrader
