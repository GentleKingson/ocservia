# Single dedicated Relay validation

Run these checks inside a task-owned repository checkout on a disposable Linux
environment. Any step that installs native packages, modifies systemd units, or
injects network faults belongs on the disposable Business runner or inside a
dedicated test container.

## Lightweight Checks

Inside an isolated environment with the repository's Rust toolchain, run:

```sh
cd rust
cargo test --locked -p ocservia-agent -p ocservia-transportd relay -- --nocapture
cargo test --locked -p ocservia-agent fenced_agent_supervisor_redials_over_hot_standby_and_advances_epoch -- --nocapture
cargo fmt --all -- --check
```

The Relay test filter exercises URL parsing, normalized duplicate detection,
forbidden URL rejection, token-file loading, and the default/disabled behavior
of the transport library. Although the transport crate keeps generic
multi-member and fenced-supervisor tests, production installers and launch
entrypoints admit only one Relay, so passing these unit tests does not certify
multi-Relay deployments or failover. Likewise, tests that rely on public Relay
discovery, such as [cross-VM enrollment](cross-vm-enrollment-validation.md), do
not validate an authenticated dedicated Relay deployment.

To check the installer contracts, use a disposable Linux container equipped
with Bash, Node, sudo, jq, and Python. Run the installer scripts as an
unprivileged user with passwordless sudo so that the run exercises controlled
privilege elevation alongside the root execution paths:

```sh
bash scripts/test-managed-node-install.sh
bash scripts/test-controller-install.sh
bash scripts/test-controller-bootstrap.sh
bash scripts/test-agent-upgrade-retry.sh
docker run --rm -v "$PWD:/source:ro" node:24.18.0-bookworm \
  python3 /source/scripts/test-relay-launchers.py
```

Within that disposable container, `test-relay-launchers.py` swaps the fixed
binary paths for argv recorders. The systemd unit check,
`test-relay-systemd.sh`, requires root inside a separate disposable systemd
container, where it checks the rendered unit argv, service UID and GID,
signal delivery, and exit codes when Relay B is unset, empty, or populated.
Never run either fixture on an installed host. In Basic CI, the installer and
launcher self-tests above run only when path routing sets the installer flag
(on installer changes or unrecognized paths; see
[GitHub Actions validation](github-actions.md#path-routing)), while
`test-relay-systemd.sh` remains manual-only with no CI job.

To validate the Compose configuration, export a unique `RUN_ID`,
`ARTIFACT_DIR`, and a private, owner-controlled `RUNNER_TEMP`, then run:

```sh
bash scripts/i18-production-relays.sh --compose-only
bash scripts/test-database-deployment-config.sh
bash scripts/test-production-auth-config.sh
bash scripts/docs-check.sh
```

The focused Compose mode checks the rendered Relay arguments and merged network
boundaries without testing live network reachability, and it leaves the database
support matrix unchanged. Because the baseline Postgres and backup service
definitions do not declare `nofile` limits, this mode does not satisfy
the full I18 `nofile` assertion on the untouched database overlay; the full and
`--contract-only` I18 modes still enforce that check and must be run
separately when needed.

## Network Probe

After building the current transportd runtime image (with its Relay launcher)
and the authenticated iroh-relay image from
`deploy/production/relay.Dockerfile`, run the network smoke probe on the
disposable test host:

```sh
python3 scripts/single-relay-network-smoke.py \
  --compose-json "$ARTIFACT_DIR/platform-compose-single-empty.json" \
  --transport-image "$TRANSPORT_IMAGE" --relay-image "$RELAY_IMAGE" \
  --artifacts "$NETWORK_EVIDENCE" --cold-start
```

Reading the merged production Compose JSON, the probe configures transportd
with the production network memberships, UID, and container hardening while
placing the Relay on a separate bridge network reached over a host-bridge TCP
port (never on the internal application network). Without starting a Controller
backend, authorizing an Agent, or running command workflows, it verifies
external DNS resolution, strict CA-validated TLS, authenticated iroh connection
events and Pong frames, cold-start connection establishment, and recovery from a
Relay outage without restarting transportd. Its 90-second recovery window is
fixed prior to fault injection to match iroh's 10-second connection timeout and
retry backoff.

If you need to reproduce the earlier isolated-network failure mode, strip only
`relay-egress` from transportd's network memberships and top-level networks in
the rendered JSON, invoke the probe with `--expect-isolated` in place of
`--cold-start`, and keep the modified JSON with the test artifacts. Never alter
live production networks to reproduce that failure.

## Real Agent Chain

End-to-end single-node recovery runs inside the Business Smoke and Integration
environment via the Release Diagnostics workflow with `run-resilience=true`;
see [Resilience](resilience.md) and
[Business validation](real-business-validation.md).

In Smoke, the sole-Relay recovery scenario (R4) stops and restarts the Relay
and requires the node to establish a fresh Agent session, complete an
authorized reload, and pass VPN traffic. The manual integration profile extends
this with the offline-queue scenario: while the Relay is stopped, it confirms
that the node depends on that Relay, waits for the prior owner lease to expire,
and verifies that an approved offline command produces no published outbox
record, transmission attempt, Agent journal entry, or root side effect. Once
the same Relay restarts, the queued operation must finish with a single Agent
journal entry, a verified root receipt, and one additional native reload, and
replaying the API idempotency key must return the existing operation and
command without triggering another reload. A queued command that fails in this
flow cannot be accepted as expected Unknown.

## Package Lifecycle And Cleanup

For current DEB and RPM packages, run `i18-agent-package-smoke.sh` and
`release-native-package-smoke.sh` inside disposable systemd and packaging
containers as described in [package validation](release-checks.md). When a
native test container spawns sibling Docker containers via the host socket,
mount `RUNNER_TEMP` at the exact same absolute path on the disposable host and
inside the parent container. Always run native DEB and RPM checks on matching
host and binary architectures, and never report an unexecuted architecture as
passing.

Preserve the test logs, rendered Compose topology, stage timings, checksums,
and non-secret JSON summaries as evidence, while keeping generated enrollment
tokens, session cookies, credentials, private CA or signing keys, and fixture
secret directories out of published artifacts. Each integration script cleans up
its own named containers, volumes, and networks via exit traps; once you have
exported the run evidence, remove the task-owned systemd container, temporary
image tags, built test packages, and private working directories without running
a global Docker prune.

## Design References

- [iroh-relay 1.2.0 RelayMap](https://docs.rs/iroh-relay/1.2.0/iroh_relay/struct.RelayMap.html)
- [Compose services](https://docs.docker.com/reference/compose-file/services/)
- [Compose networks](https://docs.docker.com/reference/compose-file/networks/)
- [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html)
- [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)

The vendored iroh 1.2.0 tree retains the local connection-lifecycle patch:
keeping persistent connections across multiple Relays still requires at least
two distinct Relays in configuration, while single-Relay reconnection reuses
the existing Endpoint and supervisor loop without introducing a separate
connection manager.
