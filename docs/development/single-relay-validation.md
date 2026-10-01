# Single Dedicated Relay Validation

Run checks only in a task-owned checkout and disposable Linux environment.
Native installation, systemd and network faults belong to the existing
Business runner or a dedicated disposable test container.

## Lightweight Checks

With the existing Rust toolchain in an isolated environment, run:

```sh
cd rust
cargo test --locked -p ocservia-agent -p ocservia-transportd relay -- --nocapture
cargo test --locked -p ocservia-agent fenced_agent_supervisor_redials_over_hot_standby_and_advances_epoch -- --nocapture
cargo fmt --all -- --check
```

The Relay filter covers URL parsing, normalized duplicates, forbidden URLs,
token files and the unchanged default/disabled behavior in the transport library.
Its multi-member tests and the fenced supervisor test exercise retained generic
library behavior; supported installation and launch entrypoints admit one Relay.
These tests do not certify a multiple-Relay deployment or failover. Public-Relay
tests remain ignored; they are not evidence for an authenticated dedicated deployment.

Use a disposable Linux container with Bash, Node, sudo, jq and Python for the
installer contracts. Run installers as a non-root account with passwordless
sudo to exercise controlled elevation, not just the root path:

```sh
bash scripts/test-managed-node-install.sh
bash scripts/test-controller-install.sh
bash scripts/test-controller-bootstrap.sh
bash scripts/test-agent-upgrade-retry.sh
docker run --rm -v "$PWD:/source:ro" node:24.18.0-bookworm \
  python3 /source/scripts/test-relay-launchers.py
```

`test-relay-launchers.py` replaces the fixed binary paths with argv recorders
inside that disposable container only. `test-relay-systemd.sh` must run as root
inside a separate disposable systemd container. It verifies actual unit argv,
service UID/GID, signal delivery and exit status for unset, empty and present B.
Do not run either fixture on an installed node.

For Compose, set unique `RUN_ID`, `ARTIFACT_DIR` and a private, owner-controlled
`RUNNER_TEMP`, then run:

```sh
bash scripts/i18-production-relays.sh --compose-only
bash scripts/test-database-deployment-config.sh
bash scripts/test-production-auth-config.sh
bash scripts/docs-check.sh
```

The focused Compose mode certifies the Relay arguments and merged network
boundary, not reachability. It does not certify the untouched database overlay's
`nofile` limits: the existing full I18 assertion expects limits that the baseline
Postgres and backup services do not declare. The full and `--contract-only` I18
checks retain that assertion and are not reported as passing by this task.
The database support matrix remains unchanged.

## Network Probe

Build the current transportd runtime image including its new launcher, and use
the real authenticated iroh-relay image built by `deploy/production/relay.Dockerfile`. On the disposable test host:

```sh
python3 scripts/single-relay-network-smoke.py \
  --compose-json "$ARTIFACT_DIR/platform-compose-single-empty.json" \
  --transport-image "$TRANSPORT_IMAGE" --relay-image "$RELAY_IMAGE" \
  --artifacts "$NETWORK_EVIDENCE" --cold-start
```

The probe derives transportd's network memberships, UID and hardening from the
merged production configuration. The Relay is on a separate bridge, reached
through a host-bridge TCP publication, never on an application/internal network.
It checks external DNS, strict CA-validated TLS, authenticated iroh connection
events and Pong, cold-start recovery and outage recovery without restarting
transportd. Its 90-second bound is fixed before injection, based on iroh's
10-second connection timeout and retry backoff. It does not start a Controller
backend or authorize an Agent; it is not a command-workflow test.

To reproduce the original isolated topology, remove only `relay-egress` from
the rendered JSON's transportd memberships and networks, and run with
`--expect-isolated` instead of `--cold-start`. Preserve that JSON as evidence.
Do not change live production networks to reproduce this failure.

## Real Agent Chain

The signed Business Smoke / Integration environment owns real single-node
recovery. Use the existing Release Diagnostics workflow with
`run-resilience=true`; see [Resilience](resilience.md) and
[Business validation](real-business-validation.md). The old G6 engineering
entrypoint and its disposable node image are retired.

The sole-Relay scenario proves dependency on that Relay, waits until the old
owner lease is invalid, and verifies that the approved offline command has no
published outbox entry, sent attempt, Agent journal entry or root effect.
Restoring the same Relay must complete the same operation with one journal
entry, a verified root receipt and exactly one additional real reload.
Replaying its API idempotency key returns the same operation and command with
no additional effect. A failed queued command cannot become expected Unknown.

## Package Lifecycle And Cleanup

Reuse `i18-agent-package-smoke.sh` and `release-native-package-smoke.sh` for
current packages in disposable systemd/packaging containers. Historical
baseline upgrades are retired; follow [current package validation](release-upgrade-validation.md).
For native tests that launch sibling Docker containers,
`RUNNER_TEMP` must be mounted at the same absolute path on the disposable test host and in
the parent container. Match the actual architecture for native DEB/RPM checks;
do not report an unexecuted architecture as passing.

Retain logs, rendered topology, timings, checksums and non-secret JSON results.
Do not publish generated enrollment tokens, session cookies, credentials,
private CA or signing keys, or fixture secret directories. Integration scripts
remove only their named containers, volumes and networks in exit traps. After
exporting evidence, remove the task-owned systemd container, test image tags,
test packages and private temporary directories. Never use global Docker prune.

## Design References

- [iroh-relay 1.2.0 RelayMap](https://docs.rs/iroh-relay/1.2.0/iroh_relay/struct.RelayMap.html)
- [Compose services](https://docs.docker.com/reference/compose-file/services/)
- [Compose networks](https://docs.docker.com/reference/compose-file/networks/)
- [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html)
- [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)

The pinned iroh 1.2.0 vendor retains the local lifecycle patch. Multi-Relay persistent
connections still require at least two distinct configured Relays; single-Relay
reconnection reuses the existing Endpoint and supervision, not a new manager.
