FROM rust:1.97.1-bookworm AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd-stub

FROM debian:bookworm-slim
RUN groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody \
    && install -d -o nobody -g ocservia -m 0770 /run/ocserv-platform
COPY --from=build /src/target/release/ocservia-transportd-stub /usr/local/bin/ocservia-transportd-stub
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-transportd-stub"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--queue-capacity", "256"]
