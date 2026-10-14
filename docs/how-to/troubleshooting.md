# Troubleshooting

Start with the procedure in the relevant task guide, then check the matching
failure boundary below. Retain the exact release version, host architecture,
command output, and redacted logs with your incident record.

## Controller does not start

Run the read-only Compose configuration check first:

```bash
deploy/production/compose.sh config --quiet
```

Because `compose.sh config --quiet` validates only the environment exported in
your current shell without reading the release manifest, checking installation
state, or modifying disk, verify those settings directly against the active
release manifest:

- All six core image variables (`OCSERV_GATEWAY_IMAGE`, `OCSERV_CONTROL_IMAGE`,
  `OCSERV_TRANSPORT_IMAGE`, `OCSERV_BACKUP_IMAGE`, `OCSERV_POSTGRES_IMAGE`, and
  `OCSERV_OTEL_IMAGE`, plus `OCSERV_EDGE_IMAGE`, `OCSERV_RELAY_IMAGE`, and
  `OCSERV_SIGNER_IMAGE` for Integrated, and `OCSERV_DATABASE_BACKUP_IMAGE` for
  MySQL) must be exported as version-tagged (`:vX.Y.Z` or `:vX.Y.Z-rc.N`) or
  `@sha256:` references taken from the release manifest in use; never
  substitute an unreviewed tag.
- Both the protected secret directory and the backup directory must satisfy
  their ownership and mode contracts, and all required PKI, Controller
  EndpointID, and relay settings must be present.
- Authentication variables must match the selected
  [mode](../operations/authentication.md#choose-a-mode): Local-only requires no
  OIDC settings or client secret, whereas OIDC-only and dual-auth require a
  complete OIDC configuration and protected client secret (a partial OIDC
  configuration is rejected even when Local is enabled).

Always manage the stack through `controller.sh` instead of invoking Docker
Compose directly.

## Identity provider is unavailable

Local-only deployments do not depend on an external IdP. In dual-auth mode,
Local accounts that were already enabled and initialized remain usable during
an IdP outage, while new SSO logins that cannot complete verification fail
closed. An OIDC-only deployment must never switch authentication methods
automatically or bypass token verification. Existing valid sessions continue to
enforce their normal expiry and revocation checks; follow
[authentication incident recovery](../operations/incident-recovery.md#authentication-outage).

## Optional traces are missing

OpenTelemetry export stays disabled whenever `OCSERV_OTEL_BACKEND_ENDPOINT` is
unset or empty. Using the same exported configuration passed to the lifecycle
command, list the enabled services and running containers without dumping
secrets:

```bash
deploy/production/compose.sh config --services
deploy/production/compose.sh ps
```

The `otel-collector` service appears only when an endpoint is configured, as
the launcher enables that profile automatically and ignores any inherited
`COMPOSE_PROFILES`. When tracing is enabled, verify that `otel-client.crt`,
`otel-client.key`, and `otel-ca.crt` are owned by the launcher user with mode
`0444` (missing or mispermissioned TLS files block startup), then inspect the
Collector logs and check backend mTLS connectivity.

## Deployment configuration is rejected

Use the JSON release configuration that matches the Docker daemon architecture
along with a clean Git checkout of its `source_commit`. Verify the release
version, topology, image references, and root-controlled file ancestry, then
fix the reported prerequisite without bypassing lifecycle validation or
overwriting confirmed or pending state files.

## Agent cannot enroll

Confirm that the expected EndpointID matches the one printed from the node's
persistent identity directory and that the Controller EndpointID and Relay URL
pins have not changed. Because enrollment tokens are single-purpose, always
query the Controller by that EndpointID after a failed or unknown enrollment
response (see [Enroll a node](enroll-node.md)) before issuing a new token, as a
replacement token on its own does not prove the earlier attempt failed.
Remember that enrollment only creates a pending node record: a different
authorized principal must approve the node before it can open a normal
mutation-capable session, even if the host already shows `ENROLLED_LOCAL` or
active local services.

## Agent will not start after an upgrade

Inspect `journalctl -u ocservia-privd -u ocservia-agent` to see which check
failed, then verify `/etc/ocservia-agent/agent.env`, the independently
provisioned command verification key, both distinct sealing keys, `relays.env`
(which requires a literal HTTPS `RELAY_URL_A` and rejects any non-empty
`RELAY_URL_B`), and the relay access token and CA files it references. When an
upgrade fails during preflight before modifying files on disk, simply fix the
reported prerequisite and retry. If the new binaries were already installed and
must be reverted, run [Agent rollback](agent-lifecycle.md#rollback) instead of
copying binaries from an unverified directory.

## Relay is unavailable

Because only one dedicated Relay is supported and any non-empty `RELAY_URL_B`
is rejected, there is no standby failover or second Relay to fall back to, and
all Relay-dependent communication pauses until the Relay recovers. Restore the
Relay at the same address with its matching certificate and credentials
(rebuilding the Relay host with those same materials is how you recover the
original Relay), then confirm reconnection, fresh heartbeats, and command
results. Verify DNS resolution, HTTPS egress, and authenticated relay
connections end to end, as a healthy container or local socket on the Relay
host does not prove reachability. Never fall back to a public relay or replace
the Controller or Agent identity key during a Relay outage.

## Database recovery is needed

Check `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` in the
effective deployment configuration before choosing a recovery procedure:

- On PostgreSQL, restart the existing instance after a transient outage, or
  follow
  [verified backup and isolated restore](../operations/database-backup-restore.md#postgresql)
  when data restoration is required. Cluster failover and PITR readiness are
  outside the supported deployment scope.
- On MySQL, follow the
  [isolated logical restore](../operations/database-backup-restore.md#mysql)
  procedure with MySQL tooling (do not run PostgreSQL restore commands against
  MySQL). This workflow does not provide PITR, automatic failover, storage
  snapshots, or cross-engine migration.

Neither rolling back the Controller application package nor verifying a backup
checksum restores database state. Complete the
[reopening conditions](../operations/incident-recovery.md#database-recovery)
before restoring client traffic or command authority.
