FROM rust:1.97.1-bookworm AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd

FROM debian:bookworm-slim
RUN groupadd --system --gid 65532 transportd \
    && useradd --system --uid 65532 --gid transportd transportd \
    && install -d -o transportd -g transportd -m 0750 /run/ocserv-platform
COPY --from=build /src/target/release/ocservia-transportd /usr/local/bin/ocservia-transportd
USER transportd:transportd
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--key-file", "/run/secrets/controller-iroh.key"]
