# Roll back the Controller

Roll back to the last confirmed Controller release recorded by the guarded
lifecycle.

## Before you begin

- The current deployment is stopped or no longer accepting new writes if the
  incident requires it.
- The protected `previous-release.json` exists in the Controller state root.
- You have reconciled every `Unknown` operation and confirmed the database
  compatibility and backup boundary for the incident.

## Command

```bash
deploy/production/controller.sh rollback
```

The command selects only the protected previous release. It does not accept an
operator-selected manifest and does not run a database down migration or
restore.

Rollback also requires an unchanged production deployment contract. The release
introducing the static gateway/application IPAM and security configuration is a
**forward-only deployment change** from v0.4.0; this command intentionally refuses
that rollback. For a failed upgrade, first retry the identical target through
the guarded lifecycle. If it cannot be recovered, preserve the evidence and
follow the [backup/PITR recovery procedure](../operations/postgres-pitr-restore.md)
in an isolated deployment before redirecting traffic, rather than bypassing the
deployment-contract guard.

## Verify

Wait for the rollback smoke check to succeed. Confirm the expected version and
source commit at `/api/v1/version`, readiness at `/api/v1/readyz`, and the
authenticated application and node paths.

## If it fails

The confirmed release state remains unchanged and pending failure evidence is
retained for a same-target retry. Do not redeploy old images manually. If the
database cannot satisfy the compatibility contract, use the verified
PostgreSQL backup/PITR procedure instead.

See [Production deployment reference](../operations/production-deployment.md)
for the compatibility and filesystem contracts.
