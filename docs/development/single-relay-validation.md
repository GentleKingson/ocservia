# Single Dedicated Relay Validation

This is functional, single-host validation, not the formal multi-host G6 gate.
Run project code and all checks on `BuildServer`, in a task-owned checkout,
temporary directory and Docker projects. Never inject faults into an existing
deployment. The implementation baseline is
`594415068c00d1ef1bcf07427558cb9af7dc1c3f`; the Draft PR records the final tested
commit, commands, timings, evidence directory and any incomplete acceptance.

## Lightweight Checks

Run the existing Rust builder on BuildServer, with the task checkout mounted at
the same absolute path, then source `scripts/env.sh`:

```sh
cd rust
cargo test --locked -p ocservia-agent -p ocservia-transportd relay -- --nocapture
cargo test --locked -p ocservia-agent fenced_agent_supervisor_redials_over_hot_standby_and_advances_epoch -- --nocapture
cargo fmt --all -- --check
```

The Relay filter covers 0/1/2/8/9 URLs, normalized duplicates, forbidden URLs,
token files and the unchanged default/disabled behavior on both endpoints.
It also runs the existing multi-member connection and surviving-Relay tests.
The separate fenced supervisor test verifies dual-Relay recovery with the same
Endpoint and an advanced owner epoch. Public-Relay tests remain ignored; they
are not evidence for an authenticated dedicated deployment.

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
No G6 threshold or database runtime matrix is relaxed or expanded.

## Network Probe

Build the current transportd runtime image including its new launcher, and use
the real authenticated iroh-relay image built by `deploy/production/relay.Dockerfile`. On BuildServer:

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

`scripts/single-relay-integration.sh` reuses the existing G6 engineering setup,
database, browser-session fixtures, enrollment and independent approval APIs.
Its test sessions have the existing SecurityAdmin and Operator roles. It uses
real Agent, privd, Controller and transportd binaries, a real dedicated Relay,
and ocserv 1.3.0. No command signature, approval, identity binding, fencing,
root receipt verification or Relay authentication is disabled.

Build the disposable node image with
`docker build -f scripts/single-relay-node.Dockerfile -t "$SINGLE_NODE_IMAGE" .`.
Set the following inputs before running the script:

```sh
export RUN_ID=single-final RUNNER_TEMP=/root/task-owned-directory
export ARTIFACT_DIR="$RUNNER_TEMP/evidence/chain"
export G6RD_CONTROL_PLANE_IMAGE=your-current-local-control-image
export G6RD_TRANSPORTD_IMAGE=your-current-local-transport-image
export G6RD_RELAY_IMAGE=your-local-authenticated-relay-image
export G6RD_PROBE_IMAGE=your-local-image-containing-ocservia-g6-probe
export SINGLE_NODE_IMAGE=your-local-single-relay-node-image
export SINGLE_AGENT_ARCHIVE=/root/task-owned-directory/package.tar.gz
export SINGLE_AGENT_PUBLIC_KEY="$SINGLE_AGENT_ARCHIVE.sha256.pub.pem"
export SINGLE_AGENT_KEY_SHA256=your-test-release-public-key-der-sha256
bash scripts/single-relay-integration.sh
```

Use the adjacent checksum and detached signature from `package-agent.sh`; this
is a test signing identity, never a published release. The archive is verified
and installed through its shipped lifecycle scripts. The existing one-shot
privd credential API provisions the real root-owned attestation key.

The fixture uses a private CA through the existing explicit CA options. Because
the packaged launcher intentionally has no general extra-command facility, a
test-only copy and systemd drop-in add that CA option. The original packaged
launcher is separately tested unchanged. Backend engineering fixtures are not
an external OIDC or complete production Compose certification. Initial trust
snapshot synchronization restarts transportd after approval and receipt-key
provisioning, before the first Agent start, as in the existing local G6 setup.
This setup action must not be mistaken for automatic fault recovery.

Both actual process argv contain exactly one custom Relay URL. Separate bridges
prevent direct UDP connectivity; connection probes must report the Relay path.
The chain checks enrollment, approval, online/fresh heartbeat, read-only ocserv
telemetry, an independently approved real reload, and its verified result.
It then queues another approved reload while the only Relay is stopped. The
native business probe first waits for the old owner lease to become invalid,
then records that the queued command has no published outbox entry, sent attempt,
Agent journal entry or root effect before restoring the Relay. Stopping the
container alone does not prove the buffered QUIC connection is gone. This
offline-queue case does not promise automatic recovery of an uncertain in-flight
reload without a root receipt; that case must remain fail closed. After
restoration, the same operation must succeed, have one Agent journal entry with
a root receipt, and produce exactly one additional ocserv reload. Replaying the
API idempotency key must return the same operation and command without another
reload. Cold-starting both communication processes with the Relay unavailable
must recover after restoring that Relay. The fixed recovery limit is 120 seconds
(Agent backoff capped at 30 seconds, handshake timeout 10 seconds). Process start
timestamps and identity hashes must remain unchanged after each restoration;
no re-enrollment, reapproval, public fallback or claimed standby switch is allowed.

## Package Lifecycle And Cleanup

Reuse `i18-agent-package-smoke.sh`, `release-native-package-smoke.sh` and
`release-baseline-upgrade-smoke.sh` in disposable systemd/packaging containers.
The baseline upgrade uses the verified published v0.5.2 assets; it does not
publish anything. For native tests that launch sibling Docker containers,
`RUNNER_TEMP` must be mounted at the same absolute path on BuildServer and in
the parent container. Test ARM64 deb and Rocky Linux 9 rpm on this ARM64 host;
do not report an unexecuted architecture matrix as passing.

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
