# Upgrade the Controller

Upgrade an installed Controller to a newer signed release using the guarded
lifecycle. The lifecycle keeps the current release running while it validates
the target and records pending evidence.

## Before you begin

- The current Controller is healthy and its backup is fresh.
- The checkout is clean and matches the target release's `source_commit`.
- The release bundle for the host architecture is in one protected directory.
- The required production environment and secrets are still available.

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

For state transitions, source matching, migration compatibility, and failure
semantics, see [Production deployment reference](../operations/production-deployment.md).
