# Database backup and restore

## PostgreSQL

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

### Retention and connection credentials

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
the read-only Compose secret into a private mode-0600 passfile.

### Deployment boundaries

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

To restore a PostgreSQL backup:

1. Stop all application writers and log the incident timestamp.
2. Choose a verified base backup along with the streamed WAL required for consistency.
3. Extract the backup into a fresh, empty PostgreSQL 18 data directory without overwriting the existing copy. On the official container layout, mount the volume at `/var/lib/postgresql` and restore into `/var/lib/postgresql/18/docker`, because the parent mount point is not `PGDATA`.
4. Bring PostgreSQL up in isolation to check schema provenance (see
   [Schema provenance after restore](#schema-provenance-after-restore)),
   audit-chain checkpoints, table row counts, and a read-only application smoke test.
5. Complete the [security state reconciliation](incident-recovery.md#security-state-reconciliation-before-cutover).
6. Repoint the control plane only after those checks pass, and keep the previous database intact until the rollback window closes.

Two smoke scripts exercise these mechanics in CI or disposable environments.
Neither substitutes for testing an actual production archive, and a completed
backup job alone is never recovery proof without a verified restore.

- `scripts/i18-backup-restore-smoke.sh` exercises bundled base backup and
  restore by confirming that a marker row survives recovery, without checking
  schema receipts.
- `scripts/i18-external-postgres-backup-restore-smoke.sh` covers the external
  PostgreSQL contract. It runs a TLS server outside the Controller network,
  verifies routing and hostname rejection, applies migrations prior to backup,
  confirms that `pg_verifybackup` catches corruption, restores into an isolated
  server, compares the `schema_revisions` journal across a repeat owner
  migration, and tests the restored runtime account's read and privilege
  boundaries.

## MySQL

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
lease and leaves command execution blocked. Before cutover, validate the
restored instance against the matching Controller build:

1. Run the owner `--migrate-only` step to verify receipts, checksums, and schema
   admission. This step may also grant privileges and provision telemetry, so it
   modifies state.
2. Start a Controller with the production audit keys to verify the audit chain
   and runtime privileges. This does not re-check migration receipts.

Redirect traffic and re-enable commands only after old writers are fenced, both
Controller checks succeed, pending work is reconciled, the
[security state reconciliation](incident-recovery.md#security-state-reconciliation-before-cutover)
is complete, and authority ownership is unambiguous.

This logical restore tooling covers recovery of the ocservia schema only; PITR,
database failover, storage snapshots, and cross-engine migration require
separate procedures and independent verification evidence.

## Schema provenance after restore

Every backup must preserve the full `schema_revisions` table, including
checksums, state, step progress, and timestamps. The runtime Controller and
`/readyz` do not re-evaluate migration receipts, so a restored database is
admitted only when the matching build's owner migration run validates it and
confirms there are no unknown epochs, revisions, checksums, or schema drift. On
a restored database, owner migration runs only supported forward SQL without
replaying `schema.sql`. A fresh initialization records its actual baseline
execution. Checkpoint upgrades leave backend-specific journal histories:
MySQL retains the epoch-1 receipt alongside the verified transition and cleanup
receipts in the same table, while PostgreSQL deletes the epoch-1 row inside the
transition transaction and leaves a single epoch-2 / revision-0 receipt. Freshly
initialized and upgraded databases must produce equivalent schemas even though
their receipt histories differ; never hand-edit the journal to force identical
rows.

The only supported prior upgrade checkpoint is v1.2.0
(`169102557cd610847c9f6ac2083336cdcf82c483`, epoch 1 / revision 0). Keep a
verified backup from that release before starting a major upgrade. Databases on
older releases must first be upgraded to v1.2.0; restoring a pre-checkpoint
backup does not allow adopting or force-cleaning the schema. Likewise, any
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
journals have been removed in this major release and cannot replace the current
artifact checksum verification.
