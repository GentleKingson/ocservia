# Shared G6 harness release builder for every first-party Rust image. The
# three release compile partitions below run as three separate Cargo
# invocations in one builder container: a single workspace-level invocation
# would unify dependency features across packages and silently change the
# transportd and probe binaries G6 is supposed to verify against production.
# The groups share this builder and its target/ cache sequentially — same
# resolved features per package, no duplicated common dependencies, and no
# three-way Docker builder contention — and each group's binaries are frozen
# to /out immediately after it links. The transportd runtime stages mirror
# rust/transportd.Dockerfile exactly; scripts/test-g6-runtime-adapters.sh
# rejects any drift between the two definitions and any future attempt to
# merge the compile partitions back together.
FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS g6-rust-builder
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-transportd \
    && mkdir -p /out/transportd \
    && cp target/release/ocservia-transportd /out/transportd/
RUN cargo build --locked --release \
    --package ocservia-g6-probe \
    --package ocservia-g6-tunnel \
    && mkdir -p /out/probe \
    && cp target/release/ocservia-g6-probe target/release/ocservia-g6-tunnel /out/probe/
RUN cargo build --locked --release \
    --package ocservia-agent \
    --package ocservia-privd \
    && mkdir -p /out/agent \
    && cp target/release/ocservia-agent target/release/ocservia-privd /out/agent/

# Mirrors the runtime-base stage of rust/transportd.Dockerfile.
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS transportd-runtime-base
RUN groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia transportd \
    && install -d -o transportd -g ocservia -m 0750 /run/ocserv-platform \
    && install -d -o 65534 -g ocservia -m 0750 /run/ocserv-trust
COPY --chmod=0555 deploy/prepare-transport-runtime.sh /usr/local/libexec/ocservia-prepare-transport-runtime
USER transportd:ocservia

FROM transportd-runtime-base AS transportd-runtime
COPY --from=g6-rust-builder /out/transportd/ocservia-transportd /usr/local/bin/ocservia-transportd
ENTRYPOINT ["/usr/local/bin/ocservia-transportd"]
CMD ["--socket", "/run/ocserv-platform/transportd.sock", "--key-file", "/run/secrets/controller-iroh.key", "--control-plane-uid", "65534", "--control-plane-gid", "65532"]

# G6 readiness probe image: the stale-owner fencing probe binary with the
# transport UDS and controller key material mounted at runtime. The probe
# runs as the control-plane account so transportd's peer-credential check
# accepts it exactly like a real worker dispatch.
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS g6-probe-runtime
RUN groupadd --system --gid 65532 ocservia \
    && usermod --gid ocservia nobody
COPY --from=g6-rust-builder /out/probe/ocservia-g6-probe /usr/local/bin/ocservia-g6-probe
USER nobody:ocservia
ENTRYPOINT ["/usr/local/bin/ocservia-g6-probe"]

# The release-image producer exports this target once and binds its checksum
# to the candidate commit. Failure-domain runners consume those frozen bytes
# instead of rebuilding the host-side tunnel independently.
FROM scratch AS g6-tunnel-artifact
COPY --from=g6-rust-builder /out/probe/ocservia-g6-tunnel /ocservia-g6-tunnel

# G6 readiness managed-node image: real Agent and privd plus the fixed-path
# read fixtures they snapshot. One container represents one production
# managed node in the G6 topology; the supervisor starts privd as root and
# the Agent as the unprivileged ocservia-agent account, exactly as the
# deployed systemd units split the two principals.
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS g6-agent-runtime
# sqlite3 lets the harness read the durable command journal live from inside
# the container; the journal bind is owned by the agent uid, so the host
# runner cannot read it directly.
RUN apt-get update \
    && apt-get install -y --no-install-recommends sqlite3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 ocservia \
    && useradd --system --uid 65532 --gid ocservia --home-dir /var/lib/ocservia-agent --shell /usr/sbin/nologin ocservia-agent \
    && install -d -o root -g ocservia -m 0750 /run/ocserv-platform \
    && install -d -o root -g root -m 0755 /etc/ocserv /etc/ocservia /var/lib/ocservia-privd \
    && install -d -o ocservia-agent -g ocservia -m 0700 /var/lib/ocservia-agent/identity /var/lib/ocservia-agent/journal
COPY --from=g6-rust-builder /out/agent/ocservia-agent /usr/local/bin/ocservia-agent
COPY --from=g6-rust-builder /out/agent/ocservia-privd /usr/local/bin/ocservia-privd
COPY --chmod=0555 deploy/g6-readiness/agent-supervisor.sh /usr/local/libexec/ocservia-agent-supervisor
# The adapter executes exactly these fixed paths; the shims answer the seven
# read-only snapshot probes with healthy, parseable output.
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/systemctl /usr/bin/systemctl
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/ocserv /usr/sbin/ocserv
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/occtl /usr/bin/occtl
COPY --chmod=0444 deploy/g6-readiness/fake-ocserv/ocserv.conf /etc/ocserv/ocserv.conf
ENV OCSERVIA_NODE_ROOT=/var/lib/ocservia-agent
ENTRYPOINT ["/usr/local/libexec/ocservia-agent-supervisor"]
