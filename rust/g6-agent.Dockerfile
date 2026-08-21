# G6 readiness managed-node image: real Agent and privd plus the fixed-path
# read fixtures they snapshot. One container represents one production
# managed node in the G6 topology; the supervisor starts privd as root and
# the Agent as the unprivileged ocservia-agent account, exactly as the
# deployed systemd units split the two principals.
FROM rust:1.97.1-bookworm@sha256:14bc9c5966e7b3a385794b3d5389a8765668342025fbcc7b2e3d2866ac4bd8c3 AS build
WORKDIR /src
COPY rust/Cargo.toml rust/Cargo.lock rust/rust-toolchain.toml ./
COPY rust/.cargo ./.cargo
COPY rust/vendor ./vendor
COPY rust/crates ./crates
RUN cargo build --locked --release --package ocservia-agent --package ocservia-privd

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241
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
COPY --from=build /src/target/release/ocservia-agent /usr/local/bin/ocservia-agent
COPY --from=build /src/target/release/ocservia-privd /usr/local/bin/ocservia-privd
COPY --chmod=0555 deploy/g6-readiness/agent-supervisor.sh /usr/local/libexec/ocservia-agent-supervisor
# The adapter executes exactly these fixed paths; the shims answer the seven
# read-only snapshot probes with healthy, parseable output.
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/systemctl /usr/bin/systemctl
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/ocserv /usr/sbin/ocserv
COPY --chmod=0555 deploy/g6-readiness/fake-ocserv/shims/occtl /usr/bin/occtl
COPY --chmod=0444 deploy/g6-readiness/fake-ocserv/ocserv.conf /etc/ocserv/ocserv.conf
ENV OCSERVIA_NODE_ROOT=/var/lib/ocservia-agent
ENTRYPOINT ["/usr/local/libexec/ocservia-agent-supervisor"]
