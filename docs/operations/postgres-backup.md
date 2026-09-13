# PostgreSQL backup and restore

The production backup worker runs `scripts/postgres-backup.sh`. It creates a
checksummed `pg_basebackup`, streams the WAL required to make that base backup
consistent, validates it with `pg_verifybackup`, atomically updates `LATEST`,
and retains a bounded number of base backups. Mount `OCSERV_BACKUP_DIR` from
separately protected storage; copying the database volume itself is not a
backup.

The production initializer creates the separate `ocservia_backup` login with replication permission; it is neither the database owner nor the application role. Encrypt backup storage, monitor age and verification failures, and copy backups off the application host according to the deployment retention policy.

## Deployment boundaries

Bundled PostgreSQL 17 configures `archive_mode` and an `archive_command` that
writes continuous WAL into the backup mount. Its retained base backup plus WAL
contract supports the separate PITR procedure.

External PostgreSQL support is limited to major version 17. Owner, runtime, and
backup connections require `sslmode=verify-full` with the launcher-validated
`database-ca.pem`. The backup worker verifies the server major before writing a
backup. It guarantees a verified base backup only: the external server is not
configured by ocservia, so continuous WAL archive, PITR, replication failover,
and their retention are the external database operator's responsibility and
require independent evidence.

Restore procedure:

1. Stop application writers and record the incident time.
2. Select a verified backup and, for bundled PITR, the required WAL for the recovery target.
3. Restore into a new empty PostgreSQL data directory, never over the only existing copy.
4. Start PostgreSQL in isolation and verify migrations, audit-chain checkpoints, row counts, and a read-only application smoke test.
5. Redirect the control plane only after verification; retain the previous database until the rollback window closes.

Run `scripts/i18-backup-restore-smoke.sh` in CI or a disposable environment to exercise base backup and restore. A successful backup command without a successful restore test is not recovery evidence.
Run `scripts/i18-external-postgres-backup-restore-smoke.sh` for the external
PostgreSQL contract. It places the TLS server outside the Controller network,
checks verified routing and hostname rejection, runs migrations before backup,
rejects corruption, and restores into an isolated server.
