# Database backup and restore

## PostgreSQL

The production backup worker runs `scripts/postgres-backup.sh`. It creates a
checksummed `pg_basebackup`, streams the WAL required to make that base backup
consistent, validates it with `pg_verifybackup`, atomically updates `LATEST`,
and retains a bounded number of base backups. Mount `OCSERV_BACKUP_DIR` from
separately protected storage; copying the database volume itself is not a
backup.

The production initializer creates the separate `ocservia_backup` login with replication permission; it is neither the database owner nor the application role. Encrypt backup storage, monitor age and verification failures, and copy backups off the application host according to the deployment retention policy.

### Deployment boundaries

Bundled PostgreSQL 18 configures `archive_mode` and an `archive_command` that
writes continuous WAL into the backup mount. These existing backup and
retention settings remain available; they do not certify PITR readiness.

External PostgreSQL support is limited to major version 18. Owner, runtime, and
backup connections require `sslmode=verify-full` with the launcher-validated
`database-ca.pem`. The backup worker verifies the server major before writing a
backup. It guarantees a verified base backup only: the external server is not
configured by ocservia, so continuous WAL archive, PITR, replication failover,
and their retention are the external database operator's responsibility and
require independent evidence.

Restore procedure:

1. Stop application writers and record the incident time.
2. Select a verified base backup, including its streamed WAL needed for consistency.
3. Restore into a new empty PostgreSQL 18 data directory, never over the only existing copy. For the official container layout, mount the volume at `/var/lib/postgresql` and restore into `/var/lib/postgresql/18/docker`; the parent mount itself is not `PGDATA`.
4. Start PostgreSQL in isolation and verify migrations, audit-chain checkpoints, row counts, and a read-only application smoke test.
5. Redirect the control plane only after verification; retain the previous database until the rollback window closes.

Run `scripts/i18-backup-restore-smoke.sh` in CI or a disposable environment to exercise base backup and restore. A successful backup command without a successful restore test is not recovery evidence.
Run `scripts/i18-external-postgres-backup-restore-smoke.sh` for the external
PostgreSQL contract. It places the TLS server outside the Controller network,
checks verified routing and hostname rejection, runs migrations before backup,
rejects corruption, and restores into an isolated server.

## MySQL

This production operations contract covers external MySQL 8.4 LTS only. It does not cover bundled deployments, other server
versions, HA, PITR, storage snapshots, or cross-engine migration.

The image built from `backup.mysql.Dockerfile` uses the native MySQL client. The worker creates
a single-transaction logical dump with triggers, routines, events, binary data,
and explicit database creation, records server flavor/version metadata, writes
SHA-256 checksums, atomically advances `LATEST`, and bounds retention. It does
not copy a database volume. Database accounts and server-global grants are
provisioning state and are not reconstructed from a schema dump; recreate them
from the protected provisioning source and re-run least-privilege checks before
cutover.

`database-backup.cnf` is a mode-`0444` Compose secret inside the private
launcher-owned secret directory. Its `[client]` section contains only the
dedicated backup account, TLS settings, and external endpoint. The entrypoint
copies it to a mode-`0600` temporary file before invoking the client. The backup
account needs the minimum privileges required to read tables and views and dump
triggers, routines, and events; it must not own the schema or migration tables.
For the pinned clients, grant `SELECT`, `SHOW VIEW`, `TRIGGER`, and `EVENT` on
`ocservia.*`. MySQL 8.4 additionally needs the global `SHOW_ROUTINE` dynamic
privilege. The dump uses
`--no-tablespaces`, so the account does not need `PROCESS`.

Restore only into a new isolated server. Run the matching image with
`scripts/mysql-restore-verify.sh`, an absolute completed backup directory, and
a mode-`0600` target administrator client file. The verifier rejects checksum
damage, backend mismatch, unsafe artifacts, and a pre-existing target database,
then checks the restored fixture's metadata, audit authentication shape, identities,
scheduler epoch, idempotency rows, and unfinished operations/commands.

The restore tool never redirects the Controller or grants command authority.
Without `--old-writers-fenced` it exits with status 3 after verification. Even
with that acknowledgement, a live restored scheduler lease is rejected and the
tool keeps command authority blocked. Before cutover, an operator must also run
an isolated Controller with the real audit keys so its startup verifies the
audit chain and schema/runtime permissions. Only after old writers are fenced,
that verification passes, pending work is reconciled, and the authority source
is unambiguous may the operator redirect traffic and allow commands.

Logical restore is not PITR, database failover, a storage snapshot, or
cross-engine migration. Those procedures require independent evidence and are
not supplied by this tooling.

## Schema provenance after restore

Backups must include `schema_revisions` and every retained legacy receipt,
including timestamps, checksums, progress, and initialization comments. Validate
them with the matching Controller/backend before allowing writes. Owner migration
on a valid restored database applies only unapplied forward SQL; it does not
replay `schema.sql`. A fresh database contains one verified schema checkpoint
and no fabricated historical execution. Phase A retains old receipt tables for
bridge recovery and provenance; a verified new journal is the execution authority.

An interrupted MySQL SQL artifact refuses ordinary migration. In a controlled
test/development recovery environment, obtain the checksum from the exact
matching build with `ocserv-db-foundation --mode schema-artifact-checksum` for
fresh initialization, or `--mode upgrade-artifact-checksum` for the latest
running upgrade revision. Use `--revision N` to select its exact logical block
when the file also contains later revisions; revision zero selects the declared
previous-checkpoint transition. Supply that reviewed checksum with
`--mode repair --repair-checksum <checksum>`. If a later snapshot has changed,
recover with the original schema SQL first, then upgrade. DDL recovery accepts
only a pinned before/after fingerprint; data and progress commit in one InnoDB
transaction. Foreign objects and partial postconditions require investigation.

The bounded legacy bridge retains `--mode snapshot-checksum` for an interrupted
old snapshot and `--mode manifest-checksum` for an old revision. These checksums
identify different artifacts and cannot substitute for a new running journal's
SQL checksum. Never mark a running operation verified by editing its journal.
The foundation tool's existing production restriction remains in place. Runtime
and maintenance accounts can read checkpoint evidence but cannot write journals
or execute arbitrary DDL.
