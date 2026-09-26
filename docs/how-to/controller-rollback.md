# Roll back the Controller

Roll back to the last confirmed Controller release recorded by the guarded
lifecycle.

## Before you begin

- The current deployment is stopped or no longer accepting new writes if the
  incident requires it.
- The protected `previous-release.json` exists in the Controller state root.
- You have reconciled every `Unknown` operation and assessed the database,
  configuration and backup risks for the incident. The lifecycle no longer
  certifies cross-version compatibility.
- Confirm the database backend/deployment and the actual current/previous
  production descriptors, not just the release version numbers.
- The target `source_commit` is available locally or in its retained clean source checkout.

## Command

```bash
deploy/production/controller.sh rollback
```

The command selects only the protected previous release. It does not accept an
operator-selected manifest and does not run a database down migration or
restore.

The v1.1.0 lifecycle does not compare software versions, migration numbers or
deployment descriptors to decide whether rollback is permitted. It uses the
target manifest's exact source commit for Compose and smoke, not the caller's
deployment files with another release's images. A different source is retained
as a clean Git checkout under the protected state root so bind-mounted files
remain available for later start/uninstall operations. Missing source, dirty
checkout, invalid platform or actual configuration/runtime failure still fails.
An identical target is a no-op; a different artifact is not treated as installed
merely because its version string matches.

The normal target Compose graph runs forward initialization where required.
It does not reverse migrations, restore a database, reset Signer identity or
automatically convert legacy networks. Cross-version operations can fail or
damage state despite the absence of a compatibility rejection. Old installed
scripts retain their old behavior. For a failed operation, preserve pending
evidence and retry the identical target, or use
[backend-specific recovery](../operations/incident-recovery.md#database-recovery)
in an isolated deployment.

## Verify

Wait for the rollback smoke check to succeed. Confirm the expected version and
source commit at `/api/v1/version`, readiness at `/api/v1/readyz`, and the
authenticated application and node paths.

## If it fails

The confirmed release state remains unchanged and pending failure evidence is
retained for a same-target retry. Do not redeploy old images manually. If the
target cannot run against the existing state, select recovery by backend:
PostgreSQL [backup](../operations/postgres-backup.md) or
[PITR](../operations/postgres-pitr-restore.md) within the documented scope;
MySQL/MariaDB [logical restore](../operations/mysql-backup.md), which is not
PITR, failover, snapshots or cross-engine migration. Fence old writers, verify
the restored database and real audit keys, and reconcile pending/Unknown work
before restoring traffic or command authority. Backup verification alone is
not a production reopening gate.

See [Production deployment reference](../operations/production-deployment.md)
for the target integrity and filesystem contracts.
