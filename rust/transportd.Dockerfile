FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS chef
RUN cargo install --locked --version 0.1.78 cargo-chef
WORKDIR /src

FROM chef AS planner
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS dependencies
COPY --from=planner /src/recipe.json recipe.json
COPY rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
# The patched path dependency has a build script; keep its real source.
COPY rust/vendor ./vendor
RUN cargo chef cook --locked --release --package ocservia-transportd --recipe-path recipe.json

FROM dependencies AS build
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS runtime-base
RUN groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia transportd \
    && install -d -o transportd -g ocservia -m 0750 /run/ocserv-platform \
    && install -d -o 65534 -g ocservia -m 0750 /run/ocserv-trust
COPY --chmod=0555 deploy/prepare-transport-runtime.sh /usr/local/libexec/ocservia-prepare-transport-runtime
COPY --chmod=0555 deploy/production/transportd-relays.sh /usr/local/libexec/ocservia-transportd-relays
USER transportd:ocservia

FROM runtime-base
COPY --from=build /src/target/release/ocservia-transportd /usr/local/bin/ocservia-transportd
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--key-file", "/run/secrets/controller-iroh.key", "--control-plane-uid", "65534", "--control-plane-gid", "65532"]
