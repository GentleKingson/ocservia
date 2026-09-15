# Roll back the Controller

Roll back to the last confirmed Controller release recorded by the guarded
lifecycle.

## Before you begin

- The current deployment is stopped or no longer accepting new writes if the
  incident requires it.
- The protected `previous-release.json` exists in the Controller state root.
- You have reconciled every `Unknown` operation and confirmed the database
  compatibility and backup boundary for the incident.
- Confirm the database backend/deployment and the actual current/previous
  production descriptors, not just the release version numbers.

## Command

```bash
deploy/production/controller.sh rollback
```

The command selects only the protected previous release. It does not accept an
operator-selected manifest and does not run a database down migration or
restore.

The optional-relay launcher and dedicated transport egress network change the
production deployment descriptors. This transition is also **forward-only**
through the guarded rollback entry point: old images do not contain the new
launcher. Restoring two valid HTTPS relay URLs and both relay services is
necessary before using any old version that requires two, but does not make
the descriptor mismatch safe or bypass its guard. Recover forward or use the
documented isolated recovery procedure; do not substitute old images into the
new Compose deployment. Historical scripts and direct image replacement are
outside the current preflight's control.

Rollback also requires an unchanged production deployment contract. The static
gateway/application IPAM and security transition from v0.4.0 is a historical
**forward-only deployment change**; this command intentionally refuses that
rollback. Later releases are checked against their own descriptors and schema
compatibility, not only this historical boundary. For a failed upgrade, first retry the identical target through
the guarded lifecycle. If it cannot be recovered, preserve the evidence and
follow [backend-specific recovery](../operations/incident-recovery.md#database-recovery)
in an isolated deployment, rather than bypassing the deployment-contract guard.

## Verify

Wait for the rollback smoke check to succeed. Confirm the expected version and
source commit at `/api/v1/version`, readiness at `/api/v1/readyz`, and the
authenticated application and node paths.

## If it fails

The confirmed release state remains unchanged and pending failure evidence is
retained for a same-target retry. Do not redeploy old images manually. If the
database cannot satisfy the compatibility contract, select recovery by backend:
PostgreSQL [backup](../operations/postgres-backup.md) or
[PITR](../operations/postgres-pitr-restore.md) within the documented scope;
MySQL/MariaDB [logical restore](../operations/mysql-backup.md), which is not
PITR, failover, snapshots or cross-engine migration. Fence old writers, verify
the restored database and real audit keys, and reconcile pending/Unknown work
before restoring traffic or command authority. Backup verification alone is
not a production reopening gate.

See [Production deployment reference](../operations/production-deployment.md)
for the compatibility and filesystem contracts.
