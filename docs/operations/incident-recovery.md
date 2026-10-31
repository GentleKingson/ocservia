# Production incident recovery

This is the single backup and recovery manual for database, Signer, identity,
credential and Unknown command incidents. Supported deployments run a single
Controller, a single database instance, and one dedicated Relay. The supported
PostgreSQL and MySQL versions and deployments are defined in
[Database support](production-deployment.md#database-support). Both Integrated
and standalone topologies remain supported, as do bundled and external
databases.

Out of scope are Controller high availability, multi-Relay failover, database
replication clusters, PostgreSQL automatic failover, and point-in-time recovery
(PITR) readiness. Standard backups, backup verification, and isolated restores
remain supported. Across all recovery scenarios, the command pipeline still
enforces owner/epoch/fence/lease validation, the transactional outbox,
authorization, independent two-principal approvals, cryptographic signatures,
idempotent command IDs, the Agent SQLite journal, and privd receipts. Passing
recovery checks does not imply general exactly-once execution or production
RTO/RPO guarantees.

The Signer ledger, its CA and its revision floor are recovered independently of
the Controller database. Application rollback never runs reverse database
migrations.

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

### Break-glass credential after use

A break-glass login consumes the configured token digest, so the credential
cannot be used again until the operator rotates the offline secret and updates
the digest. The session rules are in
[Break-glass](../reference/stable-contracts.md#break-glass). Generate the
replacement secret and configure its digest as described in
[Break-glass and deployment checks](authentication.md#break-glass-and-deployment-checks),
and record who holds custody.

## OIDC issuer correction

Before editing OIDC settings, confirm the provider's authoritative issuer string
and inspect existing records in `identities`. Failed Discovery or token
validation never creates an identity row, so there is nothing to migrate from
failed login attempts alone. Each identity is keyed by `(issuer, subject)`,
so changing the issuer string alters that unique key. An issuer with a trailing
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
   unchanged. Revoke active sessions for the migrated users so they must sign in
   again. Log the committed before-and-after mapping through the operational
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
   means a [ledger restore](#signer-backup-and-restore), not a
   re-initialization.
4. Confirm `health --url https://signer:9443/healthz` succeeds, then retry the
   affected Controller operations. Signer writes are idempotent by certificate
   ID, so a retry returns the original result for any request that had already
   committed. While the Signer is unreachable, certificate issuance moves to
   `signer_unavailable` (resumed by retrying under the same approval) and
   revocation moves to `revocation_unknown` (a retry may submit it again). Both
   return a service-unavailable problem response without marking the operation
   successful.

## Signer backup and restore

The Signer ledger, its issuer CA and its audit revision floor are backed up and
restored independently of the Controller database. Recovery across the two
services is still coupled: restoring the Signer ledger without the matching
issuer CA, or without reconciling Controller certificate state and all
subsequent issuances, revocations, key imports, and key disables, leaves the
deployment inconsistent. Restoring the Controller database and the Signer ledger
independently does not by itself prove a consistent recovery point.

### Ledger state

The ledger format, startup checks and `init` refusal rules are in
[Signer custody and idempotency](../reference/stable-contracts.md#signer-custody-and-idempotency)
and [Policy and durable effects](../reference/stable-contracts.md#policy-and-durable-effects).
First-time `init`, the offline root CA custody export and the Quick materials
checks are in
[Signer runtime contract](production-deployment.md#signer-runtime-contract) and
[Quick install materials](production-deployment.md#quick-install-materials).

### Back up the ledger

Every administrative CLI subcommand acquires the same exclusive bbolt lock as
the server, so stop `serve` before running `import`, `disable`, `inspect`,
`backup`, `restore`, or `crl`. Running `inspect` prints the state version,
issuer fingerprint, policy, and latest audit revision; record this audit
high-water mark outside the state volume alongside your backup manifest and CA
fingerprint. Running `backup --output /protected/new-snapshot.db` opens a
consistent read transaction, creates the destination file exclusively, and
fsyncs both the file and its parent directory without ever overwriting an
existing snapshot. A partial file left by an interrupted backup is unusable, so
always validate a new snapshot by running `inspect --state` against it.

For exclusive Signer maintenance in an Integrated deployment, quiesce mutations
and reconcile in-flight results first. The existing non-purging uninstall is a
full maintenance stop; it preserves database volumes, Secrets, ledger and
lifecycle evidence. This also interrupts the public Controller and Relay, not
just signing. The snapshot commands below use the same `backup` and `inspect`
subcommands for a standalone Signer, with the CA and state flags configured for
that Signer.

```bash
deploy/production/controller.sh uninstall
SIGNER_IMAGE="$(jq -er '.images.signer' \
  "$OCSERV_CONTROLLER_STATE_ROOT/current-release.json")"
# BACKUP_DIR is a new, canonical, protected directory, owned 65532:65532 mode 0700.
signer_admin() {
  docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true \
    -v "$OCSERV_SIGNER_SECRET_DIR:/run/secrets:ro" \
    -v "$OCSERV_SIGNER_STATE_DIR:/var/lib/ocservia-signer" \
    -v "$BACKUP_DIR:/backup" "$SIGNER_IMAGE" "$@"
}
umask 077
signer_admin inspect > "$BACKUP_DIR/ledger-inspect.json"
signer_admin backup --output /backup/snapshot.db
signer_admin inspect --state /backup/snapshot.db > "$BACKUP_DIR/snapshot-inspect.json"
```

The image reference above comes from the already verified protected lifecycle
manifest; do not replace it with a mutable tag. Keep the CA fingerprint,
snapshot checksum and inspection record outside the ledger volume. Back up
the issuing key separately under its existing custody policy, and take the
corresponding supported [database backup](#database-backup-and-restore). Never
upload private material as CI evidence. Failed or uninspected snapshots are not
accepted backups.

### Restore the ledger

When recovering, `restore --state /protected/snapshot.db --output /protected/new-ledger.db
--minimum-revision N` verifies the snapshot and refuses to write if its audit
revision falls below the independently reconciled high-water mark `N` or if the
output path already exists. Place the restored ledger in a mode `0700`
directory with runtime ownership and permissions, paired with the original
issuer CA (whose private key custody and backup are managed separately). Put
the restored ledger into service only after reconciling Controller state and
accounting for all certificate issuances, revocations, imports, and disables
that occurred after the snapshot. Never take `--minimum-revision` solely from
the old snapshot itself. If both the ledger and independent audit evidence are
lost, no local CLI can prove state freshness; halt automated recovery and
reconcile manually instead of passing a zero floor, wiping state, swapping CAs,
or rolling back container images automatically.

To rehearse recovery while still stopped, set `MINIMUM_REVISION` from the
independently reconciled live high-water mark and later issuance/revocation,
import and disable evidence, not merely from the snapshot being restored:

```bash
signer_admin restore --state /backup/snapshot.db \
  --output /var/lib/ocservia-signer/restored.db \
  --minimum-revision "$MINIMUM_REVISION"
signer_admin inspect --state /var/lib/ocservia-signer/restored.db \
  > "$BACKUP_DIR/restored-inspect.json"
```

All destination files must be new. Compare issuer, policy, state version and
revision with independent evidence before activating the restored file. Keep
the original ledger intact until verification succeeds. Preserve UID/GID
65532:65532 and mode 0600, and the same issuer CA and node identities. Do not
lower `signer-checkpoint.json` to make an old snapshot start. If freshness
cannot be established, leave the deployment recoverably stopped.

Only after those comparisons succeed, activate within the same stopped state
directory, keeping the original under a new, non-overwriting recovery name:

```bash
test ! -e "$OCSERV_SIGNER_STATE_DIR/ledger.before-restore.db"
test "$(stat -c '%u:%g:%a' "$OCSERV_SIGNER_STATE_DIR/restored.db")" = 65532:65532:600
mv -T --no-clobber "$OCSERV_SIGNER_STATE_DIR/ledger.db" \
  "$OCSERV_SIGNER_STATE_DIR/ledger.before-restore.db"
mv -T --no-clobber "$OCSERV_SIGNER_STATE_DIR/restored.db" \
  "$OCSERV_SIGNER_STATE_DIR/ledger.db"
sync -f "$OCSERV_SIGNER_STATE_DIR"
signer_admin inspect > "$BACKUP_DIR/activated-inspect.json"
```

An interruption between these moves leaves the original recoverable; do not
run `init`. Retain the old ledger under the backup custody policy, not as a
second live replica.

After backup/restore or CRL maintenance, run
`deploy/production/controller.sh start` with the unchanged
configuration and current exact source. Check readiness/version, real login,
node reconnect and a newly approved operation. Missing/incompatible ledger or
lower revision must fail closed.

### Sealing key bindings

The ledger holds the node sealing-key bindings, so a restored ledger can lack
imports made after the snapshot and can show a retired node as bound if the
disable came later. Reconcile both against Controller state before activation,
and repeat the missing import or disable with the procedure in
[Trusted public-key transfer](production-deployment.md#trusted-public-key-transfer).
The sealing handler uses only mappings created by `import`; see
[Sealing contract](../reference/stable-contracts.md#sealing-contract).

### CRL refresh

Operators run `crl` through the protected maintenance workflow and publish the
output atomically to the target verifier. The CRL format and what a revoke
response does and does not do are in
[Policy and durable effects](../reference/stable-contracts.md#policy-and-durable-effects).

While stopped, `signer_admin crl > "$BACKUP_DIR/issuer.crl.pem"` creates a new
signed CRL and advances its number. Verify its issuer/signature, number and
next-update, transfer only this public CRL to the intended ocserv node, install
it atomically at that node's configured CRL path and send SIGHUP to the correct
ocserv service. Prove a revoked certificate can no longer establish a login
and an unrevoked control still can. Distribution and refresh remain explicit
operator responsibilities.

## Transport and credentials

Production deployments support only a single dedicated Relay and reject any
nonempty `RELAY_URL_B` or `OCSERV_RELAY_URL_B`, so no standby Relay exists for
failover. Recovering from a Relay outage requires bringing that same Relay back
online at its original address with the existing TLS certificate and
credentials, then confirming fresh Agent heartbeats and reconciled command
outcomes. Nodes that depend on the Relay remain unreachable for management
until the original path returns. Do not point production traffic at public
relays, reset node identities, or re-enroll nodes to work around a Relay outage.
During testing, direct peer connectivity can mask a broken Relay path. Upgrading
an older A/B deployment requires the operator to switch the configuration to a
single Relay explicitly, after which installer reruns preserve existing node
identity and trust state.

To recover a lost Controller endpoint key, restore the encrypted offline backup
to `controller-iroh.key` inside `OCSERV_SECRET_DIR` with owner `65532:65532` and
mode `0400`, then derive and verify the Controller EndpointID before starting
transportd. Never generate a replacement key silently. If the key backup is
unavailable or fails the EndpointID check, leave transport offline, revoke trust
in the old EndpointID, generate a new protected key, and re-enroll every node
through the standard approval workflow. Record both the old and new EndpointIDs
along with the trust transition in the incident record.

The session encryption key, audit checkpoint key, and audit event key (paired
with its stable key ID) are protected secret files. Keep them in separate
protected backups, and verify a restored database with the production audit keys. An
unknown signing key halts audit compaction for the whole batch, so never
replace an audit key silently.

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
inside the private secret directory. Any concurrent invocation waits for that
lock before reading its baseline state. Once locked, the workflow:

1. Checks the existing credentials.
2. Runs `ALTER ROLE` over the local administrative connection.
3. Verifies that new connections succeed with the new passwords and fail with
   the old ones.
4. Atomically updates all four Compose secret files.
5. Recreates the Control Plane and backup containers to verify their fresh
   connections.

If an error triggers rollback, the script restores the prior verifiers and
secret files only if the database and on-disk files still match either the
initial baseline or the values written during that run. It never overwrites
unexpected concurrent changes. Keep any reported recovery snapshot
protected and leave services stopped until recovery finishes. Passwords are read
only from files and are never accepted on the command line or printed to logs.

## Unknown command reconciliation

The rules for `Unknown` operations are in the
[Matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary)
and [Controlled session and service operations](../reference/stable-contracts.md#controlled-session-and-service-operations).
Use them with the steps below.

1. Stop affected writers and command dispatch, and pause conflicting writes to
   the affected node or resource while an operation is `Unknown`. Stop new
   configuration plan and apply requests, reconcile or expire pending plans,
   and reconcile every nonterminal `config_apply` operation. Find open
   operations through `/api/v1/operations/queue-metrics`, or follow one
   operation over REST or its resumable
   `/api/v1/operations/{operation_id}/events` SSE stream (see
   [Controller delivery](../reference/stable-contracts.md#controller-delivery)).
2. Preserve the Agent's SQLite journal, privd's root effect store, and the
   Controller's operation, command, and result tables. Never delete the local
   journal or clear replay protection to force an older binary to start.
3. Let reconciliation run read-only. The Controller issues a re-signed
   `RECONCILE_ONLY` follow-up that queries the Agent journal, as described in
   [Controller delivery](../reference/stable-contracts.md#controller-delivery).
4. Accept only authenticated durable evidence, as listed in the
   [Matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
   Telemetry, a fresh session, a healthy `ocserv` process, missing error logs
   and an unsigned `effect_absent` report are not evidence.
5. Permit `RETRY_IF_EFFECT_ABSENT` only after reconciliation has durably recorded
   proof that the effect is absent, as described in
   [Agent journal](../reference/stable-contracts.md#agent-journal). The
   session and service commands listed there are never retried on that path and
   need manual reconciliation.
6. Expect the signed `expires_at` of a command envelope to stay the immutable
   execution deadline, as described in
   [Controller delivery](../reference/stable-contracts.md#controller-delivery).
   Deploy matched Controller/transportd and Agent/privd packages, because
   upgrading the node package does not reconcile pending commands.

An Unknown write is never replayed after a restore either; see
[Security state reconciliation before cutover](#security-state-reconciliation-before-cutover).

## Deployment rollback

Before rolling back, stop new writes and reconcile any operation in the Unknown
state (see [Unknown command reconciliation](#unknown-command-reconciliation)).
Run the guarded [Controller rollback](production-deployment.md#rollback)
or [Agent rollback](agent-lifecycle.md#rollback) while satisfying the
target release's protocol, configuration, and persistent-state requirements.
Version ordering, schema ranges, and descriptor equality are not
enforced as admission gates and do not guarantee cross-version safety. Rolling
back the Controller swaps only the installed images and Compose project without
running down migrations or restoring database state. Rolling back an Agent
similarly restores only local node files without modifying the Controller.
Neither action substitutes for the other or for a database restore. When
same-target upgrade recovery and package rollback cannot bring the deployment
back, proceed to [database recovery](#database-recovery), and record the exact
release tag, source SHA, migration state, backup identifier, and audit
checkpoint.

Before rolling back the Controller binary, stop new API writes first and keep the
audit, identity, session, approval, and alert tables intact. Deploy the older
binary only after active OIDC sessions have been revoked or allowed to expire.
Retain all database records. The limits on rolling back across a schema change
are in [Database and recovery matrix](../reference/stable-contracts.md#database-and-recovery-matrix).

Rolling back to the previous slot requires state that the target binaries can
read under the same Signer identity and a monotonic ledger revision. Lifecycle
scripts never automatically reverse schema or state after a partial activation,
reset node or Signer identity, or lower a recorded checkpoint to make an older
binary start. A failed activation retains current and pending evidence. Diagnose
the failure before retrying the same verified target; do not delete pending
state, replay an Unknown operation, initialize another ledger, or automatically
roll back potentially committed effects. A changed database or deployment
contract, or insufficient backup or recovery state, can refuse rollback.

Enrollment possession proofs, purpose-separated keys, ALPNs, and real command
capability checks remain mandatory across upgrades and rollbacks. Unresolved
Unknown effects block conflicting work on the affected resource and require
reconciliation. No high availability, process merger, HSM/KMS integration,
automatic CRL distribution, cross-version rollback safety, or guaranteed
automatic recovery of every Unknown mutation is promised.

## Database backup and restore

Every backup must preserve the full `schema_revisions` table (see
[Schema provenance after restore](#schema-provenance-after-restore)) and the
Local bootstrap marker table. The PostgreSQL 18 base backup and the external MySQL 8.4
logical restore are separate procedures with different tooling. Never run
PostgreSQL recovery or credential-rotation scripts against MySQL backends. A
completed backup job alone is never recovery proof without a verified restore.
The checks that must pass before cutover are the same for both backends and are
listed once in [Database recovery](#database-recovery).

### PostgreSQL

The production backup worker runs `scripts/postgres-backup.sh` to take a
checksummed `pg_basebackup` and stream the WAL segments needed to make that
snapshot consistent. Once `pg_verifybackup` succeeds, the script atomically
updates `LATEST` and prunes older base backups to the configured retention
bound. Mount `OCSERV_BACKUP_DIR` from separately protected storage; copying the
database volume directly does not produce a usable backup.

During initialization, the production setup creates a dedicated
`ocservia_backup` login granted replication permission, distinct from both the
database owner and the application role. Encrypt the backup storage and monitor
backup age as well as verification failures. Completed backups must also be
copied off the application host under the deployment retention policy.

#### Retention and connection credentials

Bundled PostgreSQL deployments keep the configured number of verified base
backups on disk. WAL cleanup is anchored to the oldest retained base backup,
preserving the archived WAL stream from that point forward while bounding local
disk growth. Keeping those WAL files does not establish point-in-time recovery
(PITR) readiness, and no PITR procedure is documented or exercised here.
Operators should monitor backup-worker health and the `LATEST` timestamp, copy
each finished base backup with its required WAL range to protected off-host
storage, and verify the remote copy before lowering local retention.

Connection credentials live in `postgres.pgpass`. On bundled PostgreSQL, set
this file to `postgres:5432:*:ocservia_backup:<password>` with the protected
backup-role password from initialization. The wildcard database field covers
both the server-major check and the replication stream. External PostgreSQL
deployments must specify the actual server hostname and port, with entries for
both `ocservia` (used by the server-major preflight) and `replication` (used by
`pg_basebackup`). Before invoking libpq tools, the backup entrypoint copies
the read-only Compose secret into a private mode-0600 passfile. To change these
passwords, see [PostgreSQL credential rotation](#postgresql-credential-rotation).

#### Deployment boundaries

Bundled PostgreSQL 18 enables `archive_mode` with an `archive_command` that
writes continuous WAL into the backup mount. These archive and retention
settings do not certify PITR readiness, even while they remain active.

External PostgreSQL support is restricted to major version 18, and all owner,
runtime, and backup connections must use `sslmode=verify-full` against the
launcher-validated `database-ca.pem`. The backup worker checks the server major
version before taking a snapshot and guarantees only a verified base backup.
ocservia does not manage the external server configuration, so continuous
WAL archiving, PITR, replication failover, and retention policy remain with the
external database operator and require separate verification.

#### Restore a PostgreSQL backup

1. Stop all application writers and log the incident timestamp.
2. Choose a verified base backup along with the streamed WAL required for consistency.
3. Extract the backup into a fresh, empty PostgreSQL 18 data directory without overwriting the existing copy. On the official container layout, mount the volume at `/var/lib/postgresql` and restore into `/var/lib/postgresql/18/docker`, because the parent mount point is not `PGDATA`.
4. Bring PostgreSQL up in isolation. Check schema provenance (see
   [Schema provenance after restore](#schema-provenance-after-restore)),
   audit-chain checkpoints, table row counts, and a read-only application smoke test.
5. Complete the checks in [Database recovery](#database-recovery) before
   repointing the control plane, and keep the previous database intact until the
   rollback window closes.

Two smoke scripts exercise these mechanics in CI or disposable environments.
Neither substitutes for testing an actual production archive.

- `scripts/postgres-backup-restore-smoke.sh` exercises bundled base backup and
  restore by confirming that a marker row survives recovery, without checking
  schema receipts.
- `scripts/external-postgres-backup-restore-smoke.sh` covers the external
  PostgreSQL contract. It runs a TLS server outside the Controller network,
  verifies routing and hostname rejection, applies migrations prior to backup,
  confirms that `pg_verifybackup` catches corruption, restores into an isolated
  server, compares the `schema_revisions` journal across a repeat owner
  migration, and tests the restored runtime account's read and privilege
  boundaries.

### MySQL

Production MySQL operations apply only to external MySQL 8.4 LTS. Bundled
deployments, other server versions, HA, PITR, storage snapshots, and
cross-engine migration are not supported under this contract.

Built from `backup.mysql.Dockerfile`, the backup image uses the native MySQL
client to produce a single-transaction logical dump covering triggers, routines,
events, binary data, and explicit database creation, without copying a database
volume. Along with the dump, the worker records the backend and server version
in metadata, writes SHA-256 checksums, atomically advances `LATEST`, and prunes
old backups. Database accounts and server-global grants belong to provisioning
state and do not travel in a schema dump. Operators must recreate them from
the protected provisioning source and re-run least-privilege checks prior to
cutover.

Connection settings come from `database-backup.cnf`, a mode-`0444` Compose
secret in the private launcher-owned secret directory whose `[client]` section
holds only the dedicated backup user, TLS parameters, and external endpoint.
The entrypoint copies this file to a mode-`0600` temporary path before calling
the client. Grant the backup account only the privileges needed to read tables
and views and to dump triggers, routines, and events; it must never own the
schema or migration tables. On the pinned client versions, grant `SELECT`,
`SHOW VIEW`, `TRIGGER`, and `EVENT` on `ocservia.*`, plus the global
`SHOW_ROUTINE` dynamic privilege required by MySQL 8.4. The `PROCESS` privilege
is unnecessary because the dump runs with `--no-tablespaces`.

Always restore into a new, isolated MySQL server by running the matching backup
image with `scripts/mysql-restore-verify.sh`, an absolute path to a completed
backup directory, and a mode-`0600` target administrator client file. After
rejecting checksum mismatches, wrong backends, unsafe dump artifacts, or an
existing target database, the verifier loads the dump and prints a summary of
the restored state. It checks that `schema_revisions` contains only verified
rows (including an epoch-2 entry) and that no legacy migration tables exist. It
also reports the audit authentication shape, identity counts, scheduler epoch,
idempotency records, and unfinished operations or commands. Passing this script
alone does not prove the restored database is ready for service. The verifier
neither compares receipt checksums against the build's SQL artifacts nor
validates the cryptographic audit chain.

Traffic redirection and command authority remain manual operator gates. Unless
`--old-writers-fenced` is passed, the restore script exits with status 3 after
verification. Even with that flag, it rejects an active restored scheduler
lease and leaves command execution blocked. Then run the matching-build checks
and the reconciliation in [Database recovery](#database-recovery). This logical
restore tooling covers recovery of the ocservia schema only; PITR, database
failover, storage snapshots, and cross-engine migration require separate
procedures and independent verification evidence.

### Schema provenance after restore

Every backup must preserve the full `schema_revisions` table, including
checksums, state, step progress, and timestamps. The runtime Controller and
`/readyz` do not re-evaluate migration receipts, so a restored database is
admitted only when the matching build's owner migration run (`--migrate-only`)
validates it and confirms there are no unknown epochs, revisions, checksums, or
schema drift. On a restored database, owner migration runs only supported
forward SQL without replaying `schema.sql`. The receipt histories of fresh and
checkpoint-upgraded databases, and where to read each backend's current schema
revision, are in [Upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint)
and [Database](../reference/stable-contracts.md#database).

The only supported prior upgrade checkpoint is defined in
[Upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint).
Restoring a pre-checkpoint backup does not allow adopting or force-cleaning the
schema. Likewise, any
PostgreSQL or MySQL instance that already contains user tables or data without a
trusted receipt is rejected outright instead of being initialized in place.
Rolling back across a schema change requires restoring a compatible backup paired
with the matching binary, because reverse SQL migrations are never run against
an upgraded database.

When a MySQL migration artifact is interrupted, normal migration runs will
refuse to proceed. Inside a controlled `test` or `development` recovery
environment, extract the expected checksum from the exact matching build by
running `ocserv-db-foundation --mode schema-artifact-checksum` for an
interrupted initial install or `--mode upgrade-artifact-checksum` for an
in-flight upgrade, passing `--revision N` to select the logical block (where
revision zero targets the previous-checkpoint transition). Once reviewed, pass
that value to `--mode repair --repair-checksum <checksum>`. An interrupted fresh
installation must finish recovery against its original schema bytes before any
newer upgrade artifacts are applied. During repair, data changes and journal
progress commit together in a single InnoDB transaction. DDL steps accept
only pinned before-and-after schema fingerprints (including cleanup DROP steps,
which follow the same protocol). Any unexpected objects or partial
postconditions require manual inspection.

`ocserv-db-foundation` is experimental and refuses to run outside `test` or
`development`, so those repair flags are not a production recovery path. This
guide defines no in-place production schema repair. At runtime, the
application database account can read receipts but lacks permission to modify the
journal or run arbitrary DDL. Never manually update `schema_revisions` to mark
in-flight work as verified. Legacy manifest or snapshot repair modes and their
journals no longer exist and cannot replace the current artifact checksum
verification.

## Database recovery

Start by checking `OCSERV_DATABASE_BACKEND` and `OCSERV_DATABASE_DEPLOYMENT` in
the active configuration against the
[production support matrix](production-deployment.md#database-support):

- PostgreSQL: for a transient outage, restart the existing instance; after data
  loss, follow [verified backup and isolated restore](#restore-a-postgresql-backup).
  Clustered databases, automatic failover, and PITR readiness fall outside the
  supported single-instance deployment.
- MySQL: follow the [logical backup and isolated restore](#mysql)
  procedure for external MySQL. This workflow does not provide PITR, failover,
  storage snapshots, or cross-engine migration.

Before running database recovery, stop certificate and artifact creation,
reconcile all nonterminal certificate commands, and preserve terminal command
history along with root effect evidence.

Always restore into a separate, isolated target instance without overwriting the
remaining copy of the damaged database. Fence old writers immediately and keep
command execution blocked on the restored instance. Passing a backup checksum or
restore verifier check is only the first step. After it, on the isolated
instance:

1. Run the matching Controller build's owner `--migrate-only` step to verify
   receipts, checksums, and schema admission (see
   [Schema provenance after restore](#schema-provenance-after-restore)). This
   step may also grant privileges and provision telemetry, so it modifies state.
2. Start an isolated Controller with the production audit keys to validate the
   audit chain and runtime database grants. This does not re-check migration
   receipts.
3. Inspect the restored scheduler and owner state.
4. Reconcile all in-flight operations and commands, including any Unknown
   outcomes (see [Unknown command reconciliation](#unknown-command-reconciliation)).
5. Complete the [security state reconciliation](#security-state-reconciliation-before-cutover).

Repoint client traffic and re-enable command dispatch only after old writers are
fenced, every check succeeds, and a single authoritative instance is
established, so authority ownership is unambiguous. Keep all original incident
evidence and initialization markers intact.

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
3. Invalidate every restored `pending` or `approved` approval before allowing
   requests or dispatch. A consumed approval can return as approved in the
   snapshot; consuming it again can create a new command ID, so node receipt
   deduplication does not replace this step. Through the protected administrative
   connection, shorten its expiry using the backend-specific statements below.
   Preserve status, approver, consumption and audit evidence: changing an approved
   row to `expired` without removing its approver violates the schema, and removing
   the approver would discard evidence. Record the affected IDs and original
   expiries in the incident record before updating them.

   PostgreSQL (run the whole block in one connection):

   ```sql
   BEGIN;
   UPDATE approval_requests SET expires_at = now()
   WHERE status IN ('pending', 'approved')
     AND created_at < now() AND expires_at > now();
   SELECT count(*) AS approvals_still_live FROM approval_requests
   WHERE status IN ('pending', 'approved') AND expires_at > now();
   COMMIT;
   ```

   MySQL (timestamps are microseconds since 2000; use one connection):

   ```sql
   START TRANSACTION;
   SET @recovery_cutoff = TIMESTAMPDIFF(MICROSECOND, '2000-01-01', UTC_TIMESTAMP(6));
   UPDATE approval_requests SET expires_at = @recovery_cutoff
   WHERE status IN ('pending', 'approved')
     AND created_at < @recovery_cutoff AND expires_at > @recovery_cutoff;
   SELECT count(*) AS approvals_still_live FROM approval_requests
   WHERE status IN ('pending', 'approved') AND expires_at > @recovery_cutoff;
   COMMIT;
   ```

   `approvals_still_live` must be zero. Rows created at or after the cutoff remain
   untouched to preserve `expires_at > created_at`; keep traffic blocked and
   reconcile the database clock and snapshot history if any remain. Fresh work
   requires a new request and independent approval after recovery. Expiring an
   approval does not undo an already-dispatched command: reconcile its result
   separately and never replay an Unknown write.
4. Reapply post-snapshot node and `privd` attestation-key revocations from
   independent evidence, using the normal node-revocation and key-revocation
   workflows on the isolated Controller. Reconcile node authorization revisions,
   endpoint-key state and pending trust convergence, and verify that `transportd`
   has received the revoked trust state before reconnecting nodes. Updating only
   `nodes.status` is insufficient. Keep affected nodes and receipt processing
   isolated when the revocation history cannot be established.
5. Reconcile `local_auth_bootstrap` completion, administrator/independent-approver
   identities, and `break_glass_uses` consumption and `rotation_required` markers
   against records outside the snapshot. Preserve initialization and consumption
   evidence; do not delete markers or reopen bootstrap to regain access. Rotate
   any Break-glass credential consumed after the snapshot, or whose usage cannot
   be established, in the external protected configuration before cutover. Follow
   [Lost Local administration authority](#lost-local-administration-authority)
   for recovery and keep Local/Break-glass access closed until reconciled.
6. Keep the current Signer ledger; never roll it back to match an older
   Controller database. If the ledger itself must be restored, follow the
   [Signer backup and restore](#signer-backup-and-restore)
   procedure with an independently reconciled `--minimum-revision`, and
   account for every issuance, revocation, import and disable after the
   snapshot. When the evidence for any of these changes is incomplete, keep the
   affected function closed and reconcile manually; never derive a high-water
   mark from the same old snapshot.
7. Keep node fence floors, effect receipts and execution history; never lower
   a floor or clear a ledger to let the restored Controller connect.
8. Record the snapshot, the reconciliation cut-off, the changes reapplied and
   the operator in the incident record.

Never guess the result of an Unknown remote write. Retain logs and scoped
diagnostics, rotate any credentials that may have been exposed, and coordinate
with the external OIDC, PKI/HSM, relay, or telemetry operators whenever a failure
crosses an external trust boundary.

## Rebind and retention limits

The steps for moving a node to another Controller are in
[Rebind to another Controller](agent-lifecycle.md#rebind-to-another-controller),
and the retention settings are in
[Independent local retention](agent-lifecycle.md#independent-local-retention).
The invariants are in [Controller rebind](../reference/stable-contracts.md#controller-rebind).
This section lists what rebind does not recover.

Rebinding is not a database recovery path: restore a lost Controller database
with the [database procedure](#database-recovery). Recovery never resets node
identity, the Agent journal, the root effect store, or keys to make a rebind or
restore succeed.

When `commit` publishes the new binding with privileged mutations quarantined,
reconcile the underlying records through
[Unknown command reconciliation](#unknown-command-reconciliation). The
quarantine limits are in
[Resume and inspect](agent-lifecycle.md#resume-and-inspect).
