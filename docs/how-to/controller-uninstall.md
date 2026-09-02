# Uninstall the Controller

Remove the production Controller runtime while choosing whether to retain its
persistent data.

## Before you begin

- No install, upgrade, or rollback transaction is pending.
- The current release and production secret environment are available.
- You have preserved any database backup or incident evidence you need.

## Stop the runtime and retain data

```bash
deploy/production/controller.sh uninstall
```

This removes the Controller containers and project networks but retains
PostgreSQL data, transport and trust volumes, lifecycle state, backups, and
secrets. The same release can be started again with:

```bash
deploy/production/controller.sh start
```

## Purge Controller-owned local data

Only when local deletion is intentional:

```bash
deploy/production/controller.sh uninstall --purge-data
```

This removes the production project volumes and local lifecycle state. It does
not delete protected secrets, off-host backups, the checkout, or unrelated
Docker volumes. It is not secure erase and it is not a PostgreSQL restore.

## Verify

The command must report success. For the retained-data path, confirm the
containers are gone and `start` can restore the confirmed release. For purge,
confirm the required off-host recovery material remains available.

See [Production deployment reference](../operations/production-deployment.md)
for failure behavior and state preservation.
