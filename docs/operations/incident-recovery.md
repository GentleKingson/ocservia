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

## Lost Local administration authority

If no usable administrator/independent approver pair remains, stop writes,
preserve incident evidence and follow the authorized
[backend-specific recovery procedure](incident-recovery.md#database-recovery) to a verified state
with independently controlled credentials. Keep all initialization markers and
audit history. Before reopening access, revoke restored sessions using the
protected administrative connection. For PostgreSQL:

```sql
UPDATE auth_sessions SET revoked_at=now() WHERE revoked_at IS NULL;
```

For the current MySQL schema, session times use the same BIGINT
microsecond epoch since 2000-01-01 UTC:

```sql
UPDATE auth_sessions
SET revoked_at=TIMESTAMPDIFF(MICROSECOND, '2000-01-01 00:00:00', UTC_TIMESTAMP(6))
WHERE revoked_at IS NULL;
```

Record this offline recovery in the incident/change record, verify both logins
and approval separation, and rotate any exposed credentials through the normal
flows. No generic account-unlock CLI or anonymous HTTP recovery endpoint is
introduced. **Never delete/edit the Bootstrap marker to reinitialize.**

## OIDC issuer correction

Confirm the provider's authoritative issuer and inspect historical identities
before changing configuration. Matching email or username does not prove subject
ownership. Failed Discovery/token validation creates no identity to migrate.

Changing an existing row's issuer changes the unique `(issuer, subject)` key.
The slash and no-slash values are distinct accounts; the application neither
merges nor migrates them. If a separately approved migration is genuinely needed:

1. In a maintenance window, stop login/session writes for the affected deployment.
   Record a change ticket, operator, approver, exact old/new issuers and an explicit
   list of identity IDs and subjects. Obtain independent IdP evidence that each
   subject still identifies the same person; matching email/name is insufficient.
2. Take a restorable database backup and export the selected identity rows,
   role bindings (including workspace/resource scope), active sessions and other
   identity-ID references. Keep the original configuration and a before/after
   manifest in the restricted change record, not tokens or client secrets.
3. Read both issuer populations and check target-key conflicts, including disabled
   identities. For each approved subject, query `identities` for both exact issuer
   values. Review `role_bindings` by identity ID with the security owner. Any
   existing target `(issuer, subject)` is a stop condition, not permission to merge,
   delete the target, or transfer roles.
4. Rehearse on a restored isolated database. In an explicit transaction, lock
   `identities` against concurrent writes, repeat the conflict/ownership checks,
   and update only approved IDs whose old issuer and subject still match the
   manifest. Require exactly the approved row count. Preserve IDs, subjects,
   disabled state and roles; revoke affected active sessions and require fresh
   login. Roll back on any discrepancy. Record the committed before/after mapping
   through the approved operational audit process; do not rewrite historical audit
   events. Apply the exact new issuer configuration through normal change control.
5. Verify fresh Discovery/token validation, unchanged role ownership, and no
   duplicate identities before reopening login. For rollback, stop writes again,
   verify the manifest and absence of old-key conflicts, then transactionally
   restore only the mapped issuers and original configuration. Revoke sessions
   issued during the migration window; never reactivate revoked sessions. Stop for
   manual review if identities/roles have since changed, rather than overwriting
   newer data. Retain rollback evidence with the same change ticket.

These steps require a separately approved migration; login never performs them.

## Transport and credentials

For a single-relay outage, restore that relay at the same address with the same certificate and credentials, then verify fresh Agent heartbeats and reconciled command results. This restores the original path, not standby failover. Do not switch production to public relays, reset identities, or re-enroll nodes. Direct connectivity can mask an outage in a relay test.

For Controller endpoint-key recovery, restore the encrypted offline backup to the configured secret path as UID 65532 with mode `0400`, then derive and compare the Controller EndpointID before starting transportd. Never silently generate a replacement key. If the backup or its identity check fails, keep transport offline, revoke trust in the old EndpointID, generate a new protected key, and re-enroll every node through the normal approval path. Record both EndpointIDs and the trust transition in the incident record.

### PostgreSQL credential rotation

Replacing `postgres-app-password`, `postgres-backup-password`, `database-app-url`, or `postgres.pgpass` by itself does **not** rotate the password verifier already stored by PostgreSQL. To rotate both runtime roles, prepare two single-link, launcher-owned mode-`0400` or `0600` password files in a launcher-owned mode-`0700` directory outside `OCSERV_SECRET_DIR`, then run:

```bash
export OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE=/protected/new-app-password
export OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE=/protected/new-backup-password
export OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS=true # incident rotations only
deploy/production/rotate-postgres-credentials.sh
```

The workflow holds an exclusive mode-`0600` lock in the private secret directory for the complete rotation lifecycle, then verifies the current credentials, executes real `ALTER ROLE` statements through the local administrative connection, verifies both new credentials and rejects both old credentials for new connections, atomically updates the four Compose secret sources, recreates the Control Plane and backup clients, and verifies their new connections. A waiting rotation reads its baseline only after the preceding rotation releases that lock. Recovery restores the previous verifiers and files only when the database and files still match either the baseline or values written by that invocation; unexpected later state is never overwritten. Keep any reported recovery snapshot protected and services stopped until recovery completes. The script never accepts passwords as command-line arguments and does not print them.

## Deployment rollback

Stop new writes and reconcile every Unknown operation. Use the guarded
[Controller rollback](../how-to/controller-lifecycle.md#rollback) or
[Agent rollback](../how-to/agent-lifecycle.md#rollback), respecting the selected artifact's
actual protocol, configuration and persistent-state requirements. Version order,
schema ranges and descriptor equality are not admission gates in v1.1.0 and do
not provide a safety guarantee. Controller rollback
does not run a down migration or restore a database. If same-target upgrade
recovery and explicit rollback cannot restore service, use database recovery below.
Record the exact release, source SHA, migration, backup and audit checkpoint.

## Database recovery

First identify `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` from
the effective configuration and check the
[production support matrix](production-deployment.md#database-support).

- PostgreSQL: restart the same instance for a transient outage, or use
  [verified backup and isolated restore](database-backup-restore.md#postgresql) for data loss.
  Database clusters, automatic failover and PITR readiness are outside the
  supported single-instance deployment.
- MySQL: use the matching backend's
  [logical backup and isolated restore](database-backup-restore.md#mysql). It does not supply
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
