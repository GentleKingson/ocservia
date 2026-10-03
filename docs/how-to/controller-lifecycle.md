# Controller lifecycle

Use the protected lifecycle for explicit upgrade, rollback and uninstall.
Keep the selected backend/authentication environment, secrets and clean release
source available. Direct Compose or image replacement bypasses these checks.

The v1.1.0 lifecycle has no source-version window, version-order or
descriptor-equality admission gate. Cross-version execution can fail or damage
state. Historical deployment conversion is not provided; installed old scripts
retain their behavior. See [support policy](../reference/support-policy.md).

## Upgrade

Before upgrading, confirm current health, a fresh restorable backup, target
persistent-state requirements and the
[backend recovery procedure](../operations/incident-recovery.md#database-recovery).
Use a clean checkout at the target `source_commit` and a protected release
bundle matching the Docker daemon architecture.

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

The current release remains running during target validation and pending-state
recording. Use the manifest's images, not caller-selected tags.

### Runtime database grants

Upgrade runs the target's one-shot `migrate` service with `--migrate-only`,
mounted owner credentials and `OCSERV_RUNTIME_DATABASE_ROLE` before Controller
startup. It reapplies runtime grants even when no new migration is needed.
Keep authentication, session/audit keys, database TLS and backend/role settings
intact. Long-running Controllers retain runtime credentials only.

Policy cleanup needs DELETE on `user_policy_enforcements`: PostgreSQL uses its
configured role; MySQL uses explicit `user@host`. Store predicates, not
this table-level grant, restrict which unfinished records can be removed.
Other table/DDL privileges do not change. A binary replacement alone cannot
repair missing grants.

Confirm successful migration and maintenance without
`user_operations.cleanup_failed`. That error stops the remaining maintenance
steps; the process stays alive and retries at its normal tick. Repair persistent
permission/storage failures. This does not promise historical full-table cleanup.

## Rollback

Stop new writes as the incident requires and reconcile every Unknown operation.
Assess current/previous database, configuration and backup risks. The protected
`previous-release.json` and its exact source commit must be available.

```bash
deploy/production/controller.sh rollback
```

Rollback selects only the protected previous release, not an operator manifest.
It uses that target's exact source for Compose and smoke. A different source is
retained in a clean checkout under the protected state root so bind-mounted
files remain available. Missing source, dirty checkout, invalid platform and
actual configuration/runtime failures remain errors.

An identical target is a no-op; matching version strings alone do not establish
artifact identity. The target Compose graph runs forward initialization where
required. It never reverses migrations, restores a database, resets Signer
identity or converts legacy networks.

## Verify and recover

After upgrade or rollback, require the lifecycle smoke check to pass. Check
`/api/v1/readyz`, expected version/source at `/api/v1/version`, authenticated
application reads and the node inventory.

Failure preserves confirmed state and `pending-release.json` evidence.
Correct the cause and retry the identical target; do not use `install` on an
existing deployment, delete pending state or force old images into it.

If same-target recovery is impossible, use a forward fix or an isolated
[database restore](../operations/incident-recovery.md#database-recovery).
Fence old writers, verify schema/runtime permissions and the audit chain with
real keys, and reconcile pending/Unknown work before traffic or commands resume.
A verified backup alone does not authorize reopening production.

## Uninstall

Require no pending install, upgrade or rollback transaction. Retain necessary
backups and incident evidence, current release and production secrets.

### Retain data

```bash
deploy/production/controller.sh uninstall
```

This removes containers and project networks, retaining bundled database,
transport/trust volumes, external databases, lifecycle state, backups and secrets.
Restart the same confirmed release with:

```bash
deploy/production/controller.sh start
```

### Purge local data

Only when local deletion is intentional:

```bash
deploy/production/controller.sh uninstall --purge-data
```

This removes production project volumes and local lifecycle state, not external
databases, protected secrets, off-host backups, source or unrelated volumes.
It is neither secure erase nor restore. Integrated mode refuses this operation;
Signer/identity disposal requires separate reconciliation.

Require command success. For retained data, check containers are removed and
`start` restores the confirmed release. For purge, verify off-host recovery
material remains. See [production deployment](../operations/production-deployment.md)
for filesystem, state and failure contracts.
