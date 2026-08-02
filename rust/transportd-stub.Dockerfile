FROM rust:1.97.1-bookworm AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd-stub

FROM debian:bookworm-slim
RUN groupadd --system --gid 65534 nogroup 2>/dev/null || true \
    && useradd --system --uid 65534 --gid 65534 nobody 2>/dev/null || true \
    && install -d -o nobody -g nogroup -m 0750 /run/ocserv-platform
COPY --from=build /src/target/release/ocservia-transportd-stub /usr/local/bin/ocservia-transportd-stub
USER nobody:nogroup
ENTRYPOINT ["/usr/local/bin/ocservia-transportd-stub"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--queue-capacity", "256"]
