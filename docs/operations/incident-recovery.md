# Production incident recovery

## Authentication outage

Incident handling depends on the configured [authentication mode](authentication.md#choose-a-mode):

- Local-only deployments run without OIDC configuration, client secrets, or an external IdP.
- Local + OIDC deployments can continue signing in with Local accounts that were already enabled and initialized before the outage. Those logins still go through the full password, account-state, session, and authorization checks; Local login does not act as a verification bypass or an automatic fallback for failed OIDC requests.
- OIDC-only deployments must never fall back to Local login during an outage, nor may they skip issuer, signature, or callback checks.

When an IdP is unreachable or failing validation, reject any new SSO login that
cannot complete full verification. Already established sessions continue under
their normal expiration, revocation, and account-state rules; do not extend
session lifetimes to mask the outage. Before closing an OIDC incident, restore
connectivity to the issuer and confirm that discovery, signing-key refresh, and
login callbacks succeed.

## Revocation committed with synchronization pending

The revocation endpoint's HTTP `503` with title `Revocation committed` means
the database transaction committed: node trust is revoked and the convergence
intent was queued. It does not mean the transaction rolled back, or that both
the transport trust update and connection close were acknowledged. Other `503`
responses, such as database errors, need their own investigation; retain the
response body and Request ID.

Treat these as separate states:

| State | Evidence and limit |
| --- | --- |
| Controller trust | Database node/endpoint revocation and authorization revision block new handshakes and reject events from lingering revoked sessions. |
| Transport trust | transportd's in-memory policy applies exact trust revisions and reloads the database TrustSnapshot at startup. A missing RPC acknowledgement leaves its result uncertain to the caller. |
| Connection owner | The owner lease/epoch authorizes mutations. Offline status or a missing connection/fence is a current snapshot, not proof of a previous close. |
| Queue convergence | `update_applied`, `close_required` and `close_applied` record progress under the existing RPC contracts. In particular, CloseNode `NOT_FOUND` satisfies current absence, not historical execution. |

1. Preserve the original Request ID, approval ID, node ID, EndpointID, reason,
   authorization revision, response and Controller/transportd errors. Use
   `GET /api/v1/nodes/{node_id}` with the original authorized workspace to check
   `trust_status` and `connection_state`; an offline result alone is insufficient.
2. Through an authorized read-only database connection, inspect the exact node
   and queued intent. For PostgreSQL, replace the UUID placeholder:

   ```sql
   SELECT n.id, n.status, n.authorization_revision,
          e.state AS endpoint_state, encode(e.endpoint_id, 'hex') AS endpoint_id,
          q.desired_state, q.revision, q.reason, q.update_applied,
          q.close_required, q.close_applied, q.attempts, q.available_at,
          q.locked_by, q.locked_until, q.last_error
   FROM nodes n
   LEFT JOIN node_endpoint_keys e ON e.node_id = n.id
   LEFT JOIN node_trust_convergence q ON q.node_id = n.id
   WHERE n.id = '<node-uuidv7>'::uuid;
   ```

   For MySQL 8.4, UUIDs use binary storage and timestamps below are raw logical
   microseconds from `2000-01-01`, with sentinel values for infinities:

   ```sql
   SELECT HEX(n.id) AS node_id, n.status, n.authorization_revision,
          e.state AS endpoint_state, HEX(e.endpoint_id) AS endpoint_id,
          q.desired_state, q.revision, q.reason, q.update_applied,
          q.close_required, q.close_applied, q.attempts, q.available_at,
          HEX(q.locked_by) AS locked_by, q.locked_until, q.last_error
   FROM nodes n
   LEFT JOIN node_endpoint_keys e ON e.node_id = n.id
   LEFT JOIN node_trust_convergence q ON q.node_id = n.id
   WHERE n.id = UNHEX(REPLACE('<node-uuidv7>', '-', ''));
   ```

3. Correlate the exact intent with logs for `converge node trust` and
   `record synchronous revocation convergence`. A successful transport mutation
   followed by failed bookkeeping can still leave the queue pending. Attempts
   and retry backoff do not prove delivery; there is no retry count that turns
   failure into success.
4. Restore a diagnosed Controller/transportd or database outage through the
   existing recovery procedure. The worker attempts only mutations permitted
   by the current owner fence and exact revision. If ownership has ended,
   evidence conflicts, or the outcome remains uncertain, keep the revocation
   and pending intent intact and escalate for engineering review. There is no
   guaranteed automatic recovery interval. A restart can restore revoked trust
   admission checks without proving that an earlier close completed.

Do not blindly resubmit an uncertain request, create another approval, replace
the revoked Agent identity, bypass fencing, or directly update/delete convergence
records. The original consumed approval permits only the existing authorized
replay rules; it does not grant new authority or repair missing transport evidence.

## Lost Local administration authority

When a deployment loses its last working administrator and independent approver
pair, halt application writes and preserve the incident evidence. Recover to a
verified state that has independently controlled credentials by following the
authorized [backend-specific recovery procedure](#database-recovery),
leaving all initialization markers and audit history intact. Before reopening
traffic, use the protected administrative database connection to revoke all
active restored sessions. On PostgreSQL, run:

```sql
UPDATE auth_sessions SET revoked_at=now() WHERE revoked_at IS NULL;
```

The current MySQL schema stores session timestamps as a BIGINT microsecond epoch
counted from 2000-01-01 UTC:

```sql
UPDATE auth_sessions
SET revoked_at=TIMESTAMPDIFF(MICROSECOND, '2000-01-01 00:00:00', UTC_TIMESTAMP(6))
WHERE revoked_at IS NULL;
```

Log the offline recovery in the incident and change record, confirm that both
administrative logins work with intact approval separation, and rotate any
exposed credentials through standard rotation flows. Ocservia provides no
generic account-unlock CLI or unauthenticated HTTP recovery endpoint, and
operators must never delete or edit the Bootstrap marker to force
reinitialization.

## OIDC issuer correction

Before editing OIDC settings, confirm the provider's authoritative issuer string
and inspect existing records in `identities`. Failed Discovery or token
validation never creates an identity row, so there is nothing to migrate from
failed login attempts alone. Because each identity is keyed by `(issuer, subject)`,
changing the issuer string alters that unique key; an issuer with a trailing
slash and one without it represent distinct accounts that the application never
merges or migrates during login.

When an existing deployment requires a separately approved issuer migration:

1. Open a maintenance window and stop login and session writes on the affected
   deployment. Record the change ticket, operator, approver, exact old and new
   issuer strings, and the explicit list of identity IDs and subjects in scope.
   Gather independent IdP evidence proving that each subject still belongs to
   the same person; a matching email address or display name is not sufficient
   proof of subject ownership.
2. Take a restorable database backup and export the targeted `identities` rows,
   `role_bindings` (including workspace and resource scopes), active sessions,
   and any other tables referencing those identity IDs. Store the prior
   configuration and the before-and-after identity manifest in the restricted
   change record, omitting raw tokens and client secrets.
3. Query `identities` across both exact issuer strings, including disabled
   accounts, to check for target-key collisions on any approved subject, and
   review `role_bindings` by identity ID with the security owner. If a target
   `(issuer, subject)` pair already exists, halt the migration; a collision
   never authorizes merging accounts, deleting the target row, or moving roles.
4. Rehearse the change on an isolated database restore first. During execution,
   open an explicit transaction, lock `identities` against concurrent writes,
   re-run the collision and ownership checks, and update only the approved IDs
   whose current issuer and subject still match the manifest. Assert that the
   affected row count equals the approved manifest count, and roll back on any
   mismatch. Leave identity IDs, subjects, disabled flags, and role bindings
   unchanged, revoke active sessions for the migrated users so they must sign in
   again, and log the committed before-and-after mapping through the operational
   audit process without altering historical audit rows. Roll out the new issuer
   setting through normal change control.
5. Before reopening login traffic, confirm fresh Discovery and token validation,
   unchanged role ownership, and the absence of duplicate identities. If a
   rollback is required, stop writes again, check the manifest for old-key
   conflicts, and transactionally restore only the mapped issuer values and
   prior configuration. Revoke any sessions created during the migration window
   without reactivating previously revoked sessions. If identities or role
   bindings were modified after the cutover, stop for manual review instead of
   overwriting the newer records, and attach all rollback evidence to the
   original change ticket.

## Signer unavailable

An unexpected ledger write failure latches the Signer closed: `/healthz` and
every authenticated signing, revocation and sealing request return `503` until
the process restarts. This is deliberate; do not bypass it.

1. Pause certificate issuance, revocation and P12 export approvals while the
   Signer is down.
2. Read the Signer logs and repair the cause on the state volume: free space,
   ownership by the runtime UID, mode `0700` on the state directory and `0600`
   on `ledger.db`, and the health of the underlying disk.
3. Restart the Signer. Never run `init` or replace `ledger.db` to clear the
   fault; startup refuses a missing, corrupt or foreign ledger, and that refusal
   means a [ledger restore](../development/certificates-and-signer.md#state-and-recovery),
   not a re-initialization.
4. Confirm `health --url https://signer:9443/healthz` succeeds, then retry the
   affected Controller operations. Signer writes are idempotent by certificate
   ID, so a retry returns the original result for any request that had already
   committed.

## Transport and credentials

Production deployments support only a single dedicated Relay and reject any
nonempty `RELAY_URL_B` or `OCSERV_RELAY_URL_B`, so no standby Relay exists for
failover. Recovering from a Relay outage requires bringing that same Relay back
online at its original address with the existing TLS certificate and
credentials, then confirming fresh Agent heartbeats and reconciled command
outcomes. Nodes that depend on the Relay remain unreachable for management
until the original path returns. Do not point production traffic at public
relays, reset node identities, or re-enroll nodes to work around a Relay outage.
During testing, direct peer connectivity can mask a broken Relay path.

To recover a lost Controller endpoint key, restore the encrypted offline backup
to `controller-iroh.key` inside `OCSERV_SECRET_DIR` with owner `65532:65532` and
mode `0400`, then derive and verify the Controller EndpointID before starting
transportd. Never generate a replacement key silently. If the key backup is
unavailable or fails the EndpointID check, leave transport offline, revoke trust
in the old EndpointID, generate a new protected key, and re-enroll every node
through the standard approval workflow. Record both the old and new EndpointIDs
along with the trust transition in the incident record.

### PostgreSQL credential rotation

The rotation script below applies only to bundled PostgreSQL deployments;
external PostgreSQL and MySQL credentials must be rotated through their own
database administration workflows. Editing `postgres-app-password`,
`postgres-backup-password`, `database-app-url`, or `postgres.pgpass` on disk
does not update the password verifier stored inside PostgreSQL. Against
`deploy/production/compose.yaml` (or the file named by
`OCSERV_ROTATION_COMPOSE_FILE`), the script invokes `docker compose` directly
instead of calling `compose.sh`, and requires that file to define the `postgres`
and backup services. Test the procedure on a non-production copy first. If the
script reports a missing or unknown service, or fails its initial credential
check, stop immediately and preserve its output without retrying blindly.

To rotate both runtime roles, create two single-link, launcher-owned password
files (mode `0400` or `0600`) inside a launcher-owned mode-`0700` directory
outside `OCSERV_SECRET_DIR`, and run:

```bash
export OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE=/protected/new-app-password
export OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE=/protected/new-backup-password
export OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS=true # incident rotations only
deploy/production/rotate-postgres-credentials.sh
```

For the duration of the rotation, the script holds an exclusive mode-`0600` lock
inside the private secret directory; any concurrent invocation waits for that
lock before reading its baseline state. Once locked, the workflow checks the
existing credentials, runs `ALTER ROLE` over the local administrative
connection, verifies that new connections succeed with the new passwords and
fail with the old ones, atomically updates all four Compose secret files, and
recreates the Control Plane and backup containers to verify their fresh
connections. If an error triggers rollback, the script restores the prior
verifiers and secret files only if the database and on-disk files still match
either the initial baseline or the values written during that run, never
overwriting unexpected concurrent changes. Keep any reported recovery snapshot
protected and leave services stopped until recovery finishes. Passwords are read
only from files and are never accepted on the command line or printed to logs.

## Deployment rollback

Before rolling back, stop new writes and reconcile any operation in the Unknown
state. Run the guarded [Controller rollback](../how-to/controller-lifecycle.md#rollback)
or [Agent rollback](../how-to/agent-lifecycle.md#rollback) while satisfying the
target release's protocol, configuration, and persistent-state requirements. In
v1.1.0, version ordering, schema ranges, and descriptor equality are not
enforced as admission gates and do not guarantee cross-version safety. Rolling
back the Controller swaps only the installed images and Compose project without
running down migrations or restoring database state; rolling back an Agent
similarly restores only local node files without modifying the Controller.
Neither action substitutes for the other or for a database restore. When
same-target upgrade recovery and package rollback cannot bring the deployment
back, proceed to database recovery below, and record the exact release tag,
source SHA, migration state, backup identifier, and audit checkpoint.

## Database recovery

Start by checking `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` in
the active configuration against the
[production support matrix](production-deployment.md#database-support):

- PostgreSQL: for a transient outage, restart the existing instance; after data
  loss, follow [verified backup and isolated restore](database-backup-restore.md#postgresql).
  Clustered databases, automatic failover, and PITR readiness fall outside the
  supported single-instance deployment.
- MySQL: follow the [logical backup and isolated restore](database-backup-restore.md#mysql)
  procedure for external MySQL. This workflow does not provide PITR, failover,
  storage snapshots, or cross-engine migration, and PostgreSQL recovery or
  credential-rotation scripts must never be run against MySQL backends.

Always restore into a separate, isolated target instance without overwriting the
remaining copy of the damaged database. Fence old writers immediately and keep
command execution blocked on the restored instance. Passing a backup checksum or
restore verifier check is only the first step: bring up an isolated Controller
with the production audit keys to validate the schema, runtime database grants,
and audit chain, then inspect the restored scheduler and owner state and
reconcile all in-flight operations and commands, including any Unknown outcomes,
and complete the [security state reconciliation](#security-state-reconciliation-before-cutover).
Repoint client traffic and re-enable command dispatch only after every check
succeeds and a single authoritative instance is established. Keep all original
incident evidence and initialization markers intact.

### Security state reconciliation before cutover

Any restore from a backup rolls back security changes committed after the
snapshot: revoked sessions, disabled identities, and removed role bindings come
back, so a stolen session cookie that has not yet expired is accepted again.
Before reopening user traffic or command dispatch on any restored database, on
PostgreSQL and MySQL alike:

1. On the isolated target, through the protected administrative database
   connection, revoke every unrevoked session with the backend-specific
   statement shown in [Lost Local administration authority](#lost-local-administration-authority).
   Keep the rows and their audit links.
2. Reapply every identity disable, workspace or resource role removal, and
   security-relevant credential rotation made after the snapshot, using change
   records, audit evidence and IdP state from outside the restored database, and
   following [Revoking an OIDC identity or role](authentication.md#revoking-an-oidc-identity-or-role).
   Revoking sessions does not replace this step: a principal that can still
   sign in at the IdP regains every role binding the snapshot holds. Keep at
   least one working administrator and an independent approver.
3. Keep the current Signer ledger; never roll it back to match an older
   Controller database. If the ledger itself must be restored, follow the
   [Signer state and recovery](../development/certificates-and-signer.md#state-and-recovery)
   procedure with an independently reconciled `--minimum-revision`, and
   account for every issuance, revocation, import and disable after the
   snapshot. When the evidence for any of these changes is incomplete, keep the
   affected function closed and reconcile manually; never derive a high-water
   mark from the same old snapshot.
4. Keep node fence floors, effect receipts and execution history; never lower
   a floor or clear a ledger to let the restored Controller connect.
5. Record the snapshot, the reconciliation cut-off, the changes reapplied and
   the operator in the incident record.

Never guess the result of an Unknown remote write. Retain logs and scoped
diagnostics, rotate any credentials that may have been exposed, and coordinate
with the external OIDC, PKI/HSM, relay, or telemetry operators whenever a failure
crosses an external trust boundary.
