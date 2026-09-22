# Upgrade the Controller

Upgrade an installed Controller to a newer signed release using the guarded
lifecycle. The lifecycle keeps the current release running while it validates
the target and records pending evidence.

For formal `1.x`, this procedure starts from `v1.0.0` or a later 1.x release.
`v1.0.0` is a published transitional upgrade source; `v1.0.1` is the first
recommended production stable baseline. Pre-1.0 installations must
[redeploy](../getting-started/production.md), not upgrade in place or use
`v1.0.0` as a bridge. See the [support policy](../reference/support-policy.md).

## Before you begin

- The current Controller is healthy and its backup is fresh.
- The checkout is clean and matches the target release's `source_commit`.
- The release bundle for the host architecture is in one protected directory.
- The required production environment and secrets are still available.
- Confirm the selected database backend/deployment and its
  [recovery procedure](../operations/incident-recovery.md#database-recovery).
- Review the target's production deployment descriptor and schema compatibility;
  neither a newer version number nor an additive migration guarantees rollback.

## Command

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

Use the manifest matching the Docker daemon architecture. Do not supply a
caller-selected image tag or replace the lifecycle with direct Compose.

## Policy cleanup authorization

The F-2 fix grants the configured runtime account `DELETE` on
`user_policy_enforcements` only. PostgreSQL uses the configured role;
MySQL/MariaDB use its explicit `user@host`. This grants table-level deletion;
the Store's conditional DELETE, not the database grant, limits which unfinished
records the policy workflow removes. Other tables and DDL privileges are unchanged.

Replacing only the binary does not repair an existing account. Use the guarded
`controller.sh upgrade` command above with the signed target release. Its target
Compose descriptor runs the existing one-shot `migrate` service with
`--migrate-only`, mounted owner credentials, and `OCSERV_RUNTIME_DATABASE_ROLE`
before the Controller starts. That path reapplies runtime grants even with no
new schema migration and is repeatable. Keep the selected authentication mode,
session and audit secrets (including the audit event key ID/file), database TLS
CA and backend/role settings intact; the production descriptors supply these
to the migration service. Do not replace the runtime database secret with an
owner secret or bypass signature, source, or compatibility checks.

Confirm the migration service completed successfully and subsequent maintenance
completes without `user_operations.cleanup_failed`. That error means cleanup
failed and the remaining maintenance steps were skipped; the process stays alive
and retries at the normal tick, not in a fast loop. Persistent failures require
repairing the authorization or underlying database fault. Long-running
Controllers retain runtime credentials only and never execute GRANT themselves.
This upgrade does not promise a full-table cleanup of historical policy records.

## Verify

Wait for the command and release smoke check to succeed. Then check the public
`/api/v1/readyz` and `/api/v1/version` endpoints, an authenticated read, and
the node inventory.

## If it fails

A failed target remains in `pending-release.json` with evidence and the
confirmed release state is unchanged. Correct the cause and retry the
identical target. Do not use `install` to retry an existing installation.

Network migration can leave the application services stopped or the application
network absent after a failure. Keep the target checkout, environment, signed
bundle and pending state intact; correct the reported cause and rerun the same
`upgrade` command. It inspects live network state and resumes without deleting
volumes or the database network. Do not delete pending evidence or substitute
direct Compose commands.

The v0.4.0 network/security transition is a historical **forward-only deployment
change**, not an upgrade path into `1.x` or the complete rollback rule for later
releases. The lifecycle compares the actual current/previous deployment descriptors and database
compatibility; changed deployment contracts can reject rollback even with a
compatible schema. If same-target recovery is impossible, preserve failure
evidence and select the [backend-specific recovery procedure](../operations/incident-recovery.md#database-recovery):
PostgreSQL backup/PITR within its scope, or MySQL/MariaDB isolated logical
restore, without implying equivalent recovery capabilities. Complete its
fencing, audit and unfinished-work checks before redirecting traffic or enabling
commands. Do not force old images onto the partially upgraded deployment.

For state transitions, source matching, migration compatibility, and failure
semantics, see [Production deployment reference](../operations/production-deployment.md).
