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
The lifecycle itself only checks that the database backup service (and, for
bundled PostgreSQL, PostgreSQL) is running and healthy; it does not check that
a recent backup exists or can be restored, so verify that yourself first.
Use a clean checkout (no staged, unstaged or untracked changes) from a Git
clone, and a protected release bundle matching the Docker daemon architecture.
If the checkout is not already at the target `source_commit`, the lifecycle
needs that commit to be present locally and keeps a separate clean clone of it
under the protected state root.

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

The current release remains running during target validation and pending-state
recording. Use the manifest's images, not caller-selected tags. Re-running with
a release file identical to the current one is a no-op. Once image pull and
activation begin, containers may be partly replaced even if the command then
fails: the confirmed release state stays unchanged, but the running services
are not guaranteed to match it (see [Verify and recover](#verify-and-recover)).

### Runtime database grants

Upgrade runs the target's one-shot `migrate` service with `--migrate-only`,
mounted owner credentials and `OCSERV_RUNTIME_DATABASE_ROLE` before Controller
startup. It reapplies runtime grants even when no new migration is needed.
Keep authentication, session/audit keys, database TLS and backend/role settings
intact. Long-running Controllers retain runtime credentials only.

A new, proven-empty database is initialized from the backend's current
`schema.sql`. Existing databases are validated and advanced only through
unapplied forward migrations/revisions. An updated snapshot file is never a
reason to overwrite an existing schema. Missing or inconsistent provenance and
interrupted MySQL initialization stop ordinary startup; do not erase metadata
or change checksums to make it proceed.

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

This rolls back the Controller package (images and Compose project) only. It is
not a database restore and never downgrades the schema: after an upgrade that
applied forward migrations, the previous images run against the already-migrated
database and may not work with it. Restoring data is the separate
[database restore](../operations/incident-recovery.md#database-recovery).

Stop new writes as the incident requires and reconcile every Unknown operation.
Assess current/previous database, configuration and backup risks. The protected
`previous-release.json` and its exact source commit must be available; a fresh
install has no previous release, and rollback refuses to run without one. The
same database and backup health check as upgrade applies first.

```bash
deploy/production/controller.sh rollback
```

Rollback selects only the protected previous release, not an operator manifest.
It uses that target's exact source for Compose and smoke. A different source is
retained in a clean checkout under the protected state root so bind-mounted
files remain available. Missing source, dirty checkout, invalid platform and
actual configuration/runtime failures remain errors.

A successful rollback makes the old release current and the replaced release
`previous`, so running it again returns to the newer release. An identical
target is a no-op; matching version strings alone do not establish
artifact identity. The target Compose graph runs forward initialization where
required. It never reverses migrations, restores a database, resets Signer
identity or converts legacy networks.

## Verify and recover

After upgrade or rollback, require the lifecycle smoke check to pass. Check
`/api/v1/readyz`, expected version/source at `/api/v1/version`, authenticated
application reads and the node inventory.

Failure preserves confirmed state and `pending-release.json` evidence. After a
failed activation, treat the running containers as unconfirmed (possibly a mix
of old and new images) until a retry or recovery passes the smoke check.
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
The Controller is offline, and nodes lose their Controller connection, until
`start`.
Restart the same confirmed release with:

```bash
deploy/production/controller.sh start
```

### Purge local data

Only when local deletion is intentional:

```bash
deploy/production/controller.sh uninstall --purge-data
```

This removes production project volumes (including bundled database data,
which cannot be recovered afterwards unless an off-host backup exists) and the
lifecycle state files (`current-release.json`, `previous-release.json`,
`pending-release.json` and the deployment profile), not external
databases, protected secrets, off-host backups, source (including retained
source clones under the state root) or unrelated volumes. If volume removal
fails, the purge is partial and the lifecycle state is kept; if only the state
cleanup fails, the command reports the residual paths and exits non-zero.
It is neither secure erase nor restore. Integrated mode refuses this operation;
Signer/identity disposal requires separate reconciliation.

Require command success. For retained data, check containers are removed and
`start` restores the confirmed release. For purge, verify off-host recovery
material remains. See [production deployment](../operations/production-deployment.md)
for filesystem, state and failure contracts.
