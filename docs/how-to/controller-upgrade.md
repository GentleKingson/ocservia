# Upgrade the Controller

Upgrade an installed Controller to a newer signed release using the guarded
lifecycle. The lifecycle keeps the current release running while it validates
the target and records pending evidence.

## Before you begin

- The current Controller is healthy and its backup is fresh.
- The checkout is clean and matches the target release's `source_commit`.
- The release bundle for the host architecture is in one protected directory.
- The required production environment and secrets are still available.
- For the legacy v0.4.0 application-network migration, reserve a maintenance
  window and verify a recoverable backup. The guarded upgrade recreates gateway,
  control-plane, transportd and only their application network, preserving named
  volumes, PostgreSQL/backup and lifecycle state. Choose a non-overlapping target
  subnet and update any explicit trusted-proxy CIDR before upgrading.

## Command

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

Use the manifest matching the Docker daemon architecture. Do not supply a
caller-selected image tag or replace the lifecycle with direct Compose.

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

This network/security release is a **forward-only deployment change** relative
to v0.4.0: the changed production deployment contract causes standard `rollback`
to fail closed, even if the database schema is compatible. If same-target
recovery is impossible, preserve failure evidence and use the
[backup/PITR recovery procedure](../operations/postgres-pitr-restore.md)
in an isolated deployment before redirecting traffic. Do not
force old images onto the partially upgraded deployment.

For state transitions, source matching, migration compatibility, and failure
semantics, see [Production deployment reference](../operations/production-deployment.md).
