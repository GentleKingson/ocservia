# Troubleshooting

Use the task guide first, then check the matching boundary below. Keep the
exact release, architecture, command output, and redacted logs with the
incident record.

## Controller does not start

Run the read-only configuration check:

```bash
deploy/production/compose.sh config --quiet
```

Check that all six image variables are full SHA-256 digests, the protected
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

## Release verification fails

Keep the selected manifest, its `.sha256`, `SHA256SUMS`, and `SHA256SUMS.sig`
from the same release bundle. The trusted public key must be provisioned
outside that bundle. Use the manifest matching the Docker daemon architecture.

## Agent cannot enroll

Use a fresh token, confirm the expected EndpointID is the one printed from the
same persistent identity directory, and ensure the Controller EndpointID pin
has not changed. A pending node must be approved before it can use a normal
mutation-capable session.

## Agent will not start after an upgrade

Check `/etc/ocservia-agent/agent.env`, the independently provisioned command
verification key, and the two distinct sealing keys. If the installed pair
must be restored, use [Agent rollback](agent-rollback.md); do not copy binaries
from an unverified directory.

## Relay is unavailable

With one relay, restore the same address, certificate and credentials and
verify fresh heartbeats and command results. Communication that depends on
that relay is interrupted until recovery; this is not standby failover.
With two, keep the healthy relay configured, repair the failed relay, and
verify both URLs independently. Check DNS, HTTPS egress and authenticated
relay connections: a healthy container or local socket is not that evidence.
Do not fall back to a public relay or
replace the Controller or Agent identity key.

## Database recovery is needed

Confirm `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` from the
effective deployment configuration before choosing a procedure:

- PostgreSQL: [backup and restore](../operations/postgres-backup.md),
  [failover](../operations/postgres-failover.md), or
  [PITR](../operations/postgres-pitr-restore.md), within each guide's scope.
- MySQL/MariaDB: [isolated logical restore](../operations/mysql-backup.md),
  not PostgreSQL commands. This does not provide PITR, failover, storage
  snapshots or cross-engine migration.

Do not treat application rollback or backup checksum verification as database
recovery. Follow the [reopening conditions](../operations/incident-recovery.md#database-recovery)
before restoring traffic or command authority.
