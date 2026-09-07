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
the required OIDC, PKI, Controller EndpointID, and relay settings are
present. Do not bypass `controller.sh` with direct Compose.

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

Keep the healthy dedicated relay configured, repair the failed relay, and
verify both relay URLs independently. Do not fall back to a public relay or
replace the Controller or Agent identity key.

## PostgreSQL recovery is needed

Use [PostgreSQL backup and restore](../operations/postgres-backup.md),
[PostgreSQL failover](../operations/postgres-failover.md), or [point-in-time
recovery](../operations/postgres-pitr-restore.md). Do not treat application
rollback as a database restore.
