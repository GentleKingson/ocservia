FROM rust:1.97.1-bookworm AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd

FROM debian:bookworm-slim
RUN groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia transportd \
    && install -d -o transportd -g ocservia -m 0770 /run/ocserv-platform
COPY --from=build /src/target/release/ocservia-transportd /usr/local/bin/ocservia-transportd
USER transportd:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--key-file", "/run/secrets/controller-iroh.key"]
