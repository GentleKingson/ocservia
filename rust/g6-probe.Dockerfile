# G6 readiness probe image: the stale-owner fencing probe binary with the
# transport UDS and controller key material mounted at runtime. The probe
# runs as the control-plane account so transportd's peer-credential check
# accepts it exactly like a real worker dispatch.
FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates
RUN cargo build --locked --release \
    --package ocservia-g6-probe \
    --package ocservia-g6-tunnel

# The release-image producer exports this target once and binds its checksum
# to the candidate commit. Failure-domain runners consume those frozen bytes
# instead of rebuilding the host-side tunnel independently.
FROM scratch AS g6-tunnel-artifact
COPY --from=build /src/target/release/ocservia-g6-tunnel /ocservia-g6-tunnel

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241
RUN groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody
COPY --from=build /src/target/release/ocservia-g6-probe /usr/local/bin/ocservia-g6-probe
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-g6-probe"]
