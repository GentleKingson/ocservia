# Controller lifecycle

Run all explicit Controller upgrades, rollbacks, and uninstallations through
the protected lifecycle script, keeping the configured database backend,
authentication environment, secrets, and clean release source available on the
host. Replacing images or invoking Docker Compose directly bypasses these
checks.

In v1.1.0, the lifecycle removes admission gates for source-version windows,
version ordering, and descriptor equality, so running it across incompatible
versions can fail or damage state. The tooling does not convert historical
deployments, and older scripts already installed on a host retain their
original behavior; see [support policy](../reference/support-policy.md).

## Upgrade

Before starting an upgrade, verify current service health, confirm that you
have a fresh backup you can restore, and review the target release's
persistent-state requirements and the
[backend recovery procedure](../operations/incident-recovery.md#database-recovery).
Because the lifecycle script checks only that the database backup service (along
with PostgreSQL in bundled deployments) is running and healthy, you must verify
on your own that a recent backup file exists and is restorable. Run the upgrade
from a clean Git clone with no staged, unstaged, or untracked changes, using a
protected release bundle that matches the Docker daemon architecture. When the
working tree is not already checked out at the target `source_commit`, that
commit must exist in the local repository so the lifecycle can maintain a
separate clean clone under the protected state root.

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

While the script validates the target manifest and records the pending state,
the current release stays running. Always deploy the exact images declared in
the release manifest instead of substituting caller-selected tags. Running
`upgrade` with a release file identical to the active release is a no-op. Once
image pulling and container activation start, a failure can leave the host with
partially replaced containers: the confirmed release state on disk stays
unchanged, even though the running services are no longer guaranteed to match
it (see [Verify and recover](#verify-and-recover)).

### Runtime database grants

Before starting the Controller, the upgrade workflow runs the target release's
one-shot `migrate` service with `--migrate-only`, mounted database owner
credentials, and `OCSERV_RUNTIME_DATABASE_ROLE`. That step reapplies runtime
grants even when no new schema migration is needed, while the long-running
Controller containers retain only runtime credentials. Keep your
authentication configuration, session and audit keys, database TLS files, and
backend/role settings intact across upgrades.

Only a new, verifiably empty database is initialized from the backend's current
`schema.sql`. Existing databases are validated and advanced strictly through
unapplied forward migrations and revisions; an updated snapshot file in a new
release is never a reason to overwrite an existing schema. If schema provenance
is missing or inconsistent, or if an earlier MySQL initialization was
interrupted, normal startup stops. Do not erase metadata tables or alter
checksums to force startup past those checks.

Policy cleanup requires `DELETE` privilege on `user_policy_enforcements`,
granted to the configured role on PostgreSQL or to the explicit `user@host`
account on MySQL. Although the grant itself is at the table level, store
predicates restrict deletion to eligible unfinished records, and other table or
DDL privileges remain unchanged. Replacing binaries without running the
migration step will not repair missing grants.

After the upgrade completes, confirm that migrations and background maintenance
run without logging `user_operations.cleanup_failed`. When that error occurs,
the maintenance worker halts the remaining steps for that pass while the
Controller process stays alive and retries on its normal tick. Fix any
persistent permission or storage failures, noting that this maintenance pass
does not perform a full historical cleanup of the table.

## Rollback

Rolling back reverts only the Controller application package (container images
and the Compose project). Because rollback never restores database contents or
downgrades the schema, reverting after an upgrade that applied forward
migrations leaves the older images running against the already-migrated
database, which may not be compatible. Reverting database state requires the
separate [database restore](../operations/incident-recovery.md#database-recovery)
procedure.

Before rolling back, stop new writes if the incident requires it, reconcile
every Unknown operation, and evaluate the risks across the current and previous
database state, configuration, and backups. The host must have the protected
`previous-release.json` and its exact source commit available; a fresh
installation has no previous release record, and rollback refuses to run
without one. Like `upgrade`, `rollback` first runs the database and backup
service health check.

```bash
deploy/production/controller.sh rollback
```

The `rollback` command always targets the protected `previous-release.json` on
disk and never takes an operator-supplied manifest. It uses that target
release's exact source commit for Docker Compose and smoke checks, maintaining
a separate clean checkout under the protected state root whenever needed so
bind-mounted files remain available. Missing source commits, dirty checkouts,
invalid platforms, and configuration or runtime failures remain hard errors.

When a rollback succeeds, the restored release becomes current and the replaced
release becomes `previous`, so running `rollback` a second time returns to the
newer release. If the target release is identical to the active one, the
command exits as a no-op (matching version strings alone do not establish
artifact identity). During startup, the target Compose graph runs forward
initialization where required, and it never reverses migrations, restores a
database, resets Signer identity, or converts legacy Docker networks.

## Verify and recover

After every upgrade or rollback, require the built-in lifecycle smoke check to
pass, then verify `/api/v1/readyz`, the expected version and source metadata at
`/api/v1/version`, authenticated application reads, and the node inventory.

If activation fails, the script preserves the confirmed release state and
leaves `pending-release.json` on disk as evidence. Until a retry or recovery
pass completes the smoke check, treat the running containers as unconfirmed, as
they may be a mix of old and new images. Fix the underlying cause and retry the
exact same target release; do not run `install` on an existing deployment,
delete the pending state files, or manually force old images into the stack.

When recovering to the same target is impossible, apply a forward fix or run an
isolated [database restore](../operations/incident-recovery.md#database-recovery).
Before reopening production traffic or issuing commands, fence off old database
writers, verify schema and runtime permissions and the audit chain with the
real keys, and reconcile all pending or Unknown work. A verified backup on its
own does not authorize reopening production.

## Uninstall

Before uninstalling, make sure no install, upgrade, or rollback transaction is
pending, and retain all required backups, incident evidence, current release
files, and production secrets.

### Retain data

```bash
deploy/production/controller.sh uninstall
```

Running `uninstall` removes the containers and project networks while keeping
the bundled database, transport and trust volumes, external databases,
lifecycle state files, backups, and secrets intact. The Controller remains
offline and managed nodes lose their Controller connection until you run
`start`. Restart the same confirmed release with:

```bash
deploy/production/controller.sh start
```

### Purge local data

Pass `--purge-data` only when you intend to delete local deployment data:

```bash
deploy/production/controller.sh uninstall --purge-data
```

In addition to removing containers and networks, `--purge-data` deletes the
production project volumes (including bundled database data, which cannot be
recovered afterwards without an off-host backup) and the lifecycle state files
(`current-release.json`, `previous-release.json`, `pending-release.json`, and
the deployment profile). External databases, protected secrets, off-host
backups, source checkouts (including retained source clones under the state
root), and unrelated Docker volumes are left untouched. If Docker fails to
remove a volume, the purge remains partial and the lifecycle state files are
kept; if volume removal succeeds and only state-file cleanup fails, the script
reports the leftover paths and exits non-zero. Note that `--purge-data` does
not perform a cryptographic secure erase or a restore, and Integrated mode
refuses the operation because disposing of Signer and identity material
requires separate reconciliation.

Verify that the uninstall command exits zero. After a data-retaining uninstall,
confirm that the project containers are gone and that `start` restores the
confirmed release; after a purge, verify that your off-host recovery material
remains available. See
[production deployment](../operations/production-deployment.md) for the full
filesystem, state, and failure contracts.
