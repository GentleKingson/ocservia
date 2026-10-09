# Troubleshooting

Use the task guide first, then check the matching boundary below. Keep the
exact release, architecture, command output, and redacted logs with the
incident record.

## Controller does not start

Run the read-only configuration check:

```bash
deploy/production/compose.sh config --quiet
```

This check validates only the environment you export; it does not read the
release manifest or install state, and it changes nothing. Check that the
six image variables (`OCSERV_GATEWAY_IMAGE`, `OCSERV_CONTROL_IMAGE`,
`OCSERV_TRANSPORT_IMAGE`, `OCSERV_BACKUP_IMAGE`, `OCSERV_POSTGRES_IMAGE`,
`OCSERV_OTEL_IMAGE`; plus `OCSERV_EDGE_IMAGE`, `OCSERV_RELAY_IMAGE` and
`OCSERV_SIGNER_IMAGE` for Integrated, and `OCSERV_DATABASE_BACKUP_IMAGE` for
MySQL) are each exported as a version-tagged (`:vX.Y.Z` or `:vX.Y.Z-rc.N`) or
`@sha256:` reference, taking the values from the release manifest in use,
not an unreviewed tag. Check the protected
secret directory and backup directory meet their ownership/mode contracts, and
the required PKI, Controller EndpointID, and relay settings are present.
Check authentication against the selected
[mode](../operations/authentication.md#choose-a-mode): Local-only needs no
OIDC settings or client secret; OIDC-only and dual-auth need a complete OIDC
configuration and protected client secret. Partial OIDC configuration is
invalid even with Local enabled. Do not bypass `controller.sh` with direct Compose.

## Identity provider is unavailable

Local-only is independent of the IdP. In dual-auth, already enabled and
initialized Local accounts remain usable; new SSO logins that cannot complete
verification fail closed. OIDC-only must not automatically switch authentication
methods or bypass verification. Existing valid sessions retain their normal
expiry and revocation checks. Follow [authentication incident recovery](../operations/incident-recovery.md#authentication-outage).

## Optional traces are missing

OTEL is off when `OCSERV_OTEL_BACKEND_ENDPOINT` is unset or empty. With the same
exported configuration used by the lifecycle command, inspect enabled services
and running containers without dumping secrets:

```bash
deploy/production/compose.sh config --services
deploy/production/compose.sh ps
```

`otel-collector` should appear only when an endpoint is configured. The launcher
enables its profile automatically and ignores inherited `COMPOSE_PROFILES`.
When enabled, check `otel-client.crt`, `otel-client.key`, and `otel-ca.crt` for
launcher ownership and mode `0444`, then check Collector logs and backend mTLS
connectivity. Missing or incorrectly permissioned TLS files block startup.

## Deployment configuration is rejected

Use the JSON configuration matching the Docker daemon architecture and a clean
checkout of its `source_commit`. Check version, topology, image references and
root-controlled file ancestry. Follow the reported prerequisite; do not bypass
lifecycle validation or overwrite confirmed/pending state.

## Agent cannot enroll

Confirm the expected EndpointID is the one printed from the same persistent
identity directory, and that the Controller EndpointID and Relay URL pins have
not changed. Tokens are single-purpose: after a failed or unknown enrollment
response, first query the Controller by that EndpointID (see
[Enroll a node](enroll-node.md)) before requesting a new token; a new token
alone does not prove the earlier attempt failed. Enrollment only creates a
pending node: it must be approved by a different authorized principal before it
can use a normal mutation-capable session, and a locally `ENROLLED_LOCAL` or
active service is not evidence of that.

## Agent will not start after an upgrade

Check `/etc/ocservia-agent/agent.env`, the independently provisioned command
verification key, the two distinct sealing keys, `relays.env` (a literal HTTPS
`RELAY_URL_A`; a nonempty `RELAY_URL_B` is rejected) and the relay access token
and CA files it references; `journalctl -u ocservia-privd -u ocservia-agent`
shows which one failed. A failed upgrade that stopped before changing files
needs only the reported prerequisite fixed. If the installed pair
must be restored, use [Agent rollback](agent-lifecycle.md#rollback); do not copy binaries
from an unverified directory.

## Relay is unavailable

Only one dedicated Relay is supported (a nonempty `RELAY_URL_B` is rejected), so
restore the same address, certificate and credentials and verify fresh
heartbeats and command results. Communication that depends on that Relay is
interrupted until recovery; there is no standby failover or second Relay to
fall back to, and rebuilding the Relay host is original-Relay recovery, not
redundancy. Check DNS, HTTPS egress and authenticated relay connections: a
healthy container or local socket is not that evidence.
Do not fall back to a public relay or
replace the Controller or Agent identity key.

## Database recovery is needed

Confirm `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` from the
effective deployment configuration before choosing a procedure:

- PostgreSQL: restart the same instance for transient outages, or use
  [verified backup and isolated restore](../operations/database-backup-restore.md#postgresql).
  Cluster failover and PITR readiness are outside the supported deployment.
- MySQL: [isolated logical restore](../operations/database-backup-restore.md#mysql),
  not PostgreSQL commands. This does not provide PITR, failover, storage
  snapshots or cross-engine migration.

Do not treat application rollback or backup checksum verification as database
recovery. Follow the [reopening conditions](../operations/incident-recovery.md#database-recovery)
before restoring traffic or command authority.
