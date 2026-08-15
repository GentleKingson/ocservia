# PostgreSQL point-in-time recovery

This runbook restores the database to a declared restore point from a verified
base backup plus archived WAL. It is the recovery path when the primary cannot
be failed over cleanly, when data must be rolled back to a known-good point,
or after an operator error that replication has already propagated.

Trigger: declared data corruption, failed failover, or a mandated rollback to
a restore point. Risk: PITR discards every transaction after the recovery
target on the restored cluster. Never run it over the only copy of the
database.

## Preconditions

- A verified base backup exists and passes `pg_verifybackup` (the production
  backup worker handles this; see `postgres-backup.md`).
- Continuous WAL archiving was on: `archive_mode = on` with an `archive_command`
  that stores segments in the protected backup location, and
  `archive_timeout` bounds the gap for an idle database.
- A named restore point exists for the target: `SELECT pg_create_restore_point('...');`

## Restore steps

1. Stop application writers and record the incident time and the target
   restore point. Record a before/after marker transaction pair around every
   declared restore point at creation time, so the restore can be verified:
   the "before" marker must be present after recovery and the "after" marker
   must be absent.
2. Restore the base backup into a fresh empty data directory on an isolated
   host. Do not overwrite the existing (possibly still useful) cluster.
3. Configure recovery in the restored cluster:
   `restore_command` reading the WAL archive, `recovery_target_name` set to
   the restore point, `recovery_target_action = 'pause'`, then create
   `recovery.signal` and start PostgreSQL.
4. Wait for recovery to reach `PAUSED` at the target
   (`pg_is_wal_replay_paused()`), then verify the before/after markers: the
   before-marker row exists, the after-marker row does not. Any other result
   means the wrong target or a broken archive; stop and re-verify.
5. Verify schema consistency (migrations match the expected
   `database_schema_version`), audit-chain checkpoints, and run a read-only
   application smoke test while still paused.
6. Only after verification, promote out of recovery (`pg_wal_replay_resume()`
   followed by the planned promotion, or `pg_promote()` depending on state),
   redirect the control plane to the restored cluster, and keep the previous
   cluster until the rollback window closes.

## Abort and rollback

If verification fails while paused, stop the restored cluster, discard nothing
else, and restart from step 2 with a different backup or target. The original
cluster is untouched until step 6, so aborting loses nothing.

## Evidence collection

`scripts/g6-ha-pitr-fd-a.sh` executes this runbook end to end against real
base backups and archived WAL: it writes the marker pair, creates the restore
point, forces a WAL switch, waits for the segment to reach the archive,
restores into a separate instance, pauses at the target, and checks both
markers. The result is published as the `pitr-report.json` evidence artifact
(see `docs/development/g6-ha-pitr-topology.md`).
