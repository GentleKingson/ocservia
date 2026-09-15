# Production incident recovery

## Authentication outage

Follow the configured [authentication mode](authentication.md#choose-a-mode):

- Local-only requires no OIDC configuration, client secret or available IdP.
- Local + OIDC can use Local accounts that were enabled and initialized before
  the incident. Local authentication still requires normal password, account,
  session and authorization checks; it is not a bypass or automatic fallback.
- OIDC-only must not automatically enable Local authentication or bypass issuer,
  signature or callback verification during an outage.

Reject new SSO logins that cannot complete verification. Existing valid sessions
retain normal expiry, revocation and account checks; never extend them to work
around the outage. Restore issuer connectivity and verify discovery,
signing-key refresh and callback behavior before closing an OIDC incident.

## Transport and credentials

For a relay outage, leave the healthy dedicated relay configured, repair the failed relay, and verify an Agent can reconnect through each URL independently. Do not switch production to public relays.

For Controller endpoint-key recovery, restore the encrypted offline backup to the configured secret path as UID 65532 with mode `0400`, then derive and compare the Controller EndpointID before starting transportd. Never silently generate a replacement key. If the backup or its identity check fails, keep transport offline, revoke trust in the old EndpointID, generate a new protected key, and re-enroll every node through the normal approval path. Record both EndpointIDs and the trust transition in the incident record.

For PostgreSQL credential exposure, use `deploy/production/rotate-postgres-credentials.sh` with new protected application and backup password files and set `OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS=true`. Replacing Compose secret files alone does not change PostgreSQL role verifiers. Confirm that each new credential opens a connection, each old credential is rejected for a new connection, and both Control Plane and backup clients reconnect before closing the incident.

## Deployment rollback

Stop new writes and reconcile every Unknown operation. Use the guarded
[Controller rollback](../how-to/controller-rollback.md) or
[Agent rollback](../how-to/agent-rollback.md), respecting the selected artifact's
protocol, schema and deployment-descriptor compatibility. Controller rollback
does not run a down migration or restore a database. If same-target upgrade
recovery and compatible rollback are impossible, use database recovery below.
Record the exact release, source SHA, migration, backup and audit checkpoint.

## Database recovery

First identify `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` from
the effective configuration and check the
[production support matrix](production-deployment.md#database-support).

- PostgreSQL: use [backup and restore](postgres-backup.md),
  [PITR](postgres-pitr-restore.md), or [failover](postgres-failover.md) only
  within their documented topology and WAL/standby prerequisites. External
  PostgreSQL's supplied backup coverage does not itself certify HA or PITR.
- MySQL/MariaDB: use the matching backend's
  [logical backup and isolated restore](mysql-backup.md). It does not supply
  PITR, failover, storage snapshots or cross-engine migration. Do not run
  PostgreSQL recovery or credential-rotation scripts against these backends.

Restore into an isolated target, not over the only existing copy. Fence old
writers and keep restored command authority blocked. A successful checksum or
restore verifier is not permission to return to production: verify the schema,
runtime permissions and audit chain with the real audit keys using an isolated
Controller, inspect restored scheduler/owner state, and reconcile unfinished
operations and commands, including Unknown outcomes. Redirect traffic and
enable command execution only when those checks pass and the authority source
is unambiguous. Preserve the original evidence and initialization markers.

Never guess an Unknown remote-write outcome. Preserve logs and scoped diagnostics, rotate exposed credentials, and involve the external OIDC, PKI/HSM, relay, or telemetry owner when the failing trust boundary is outside Ocservia.
