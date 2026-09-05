# ocservia documentation

Use these documents to install, operate, and understand ocservia without reading the internal implementation notes first. Detailed contracts remain available in the reference and development sections when needed.

## Start here

- [Project overview](../README.md)
- [Try ocservia locally](getting-started/local-development.md)
- [Deploy the Controller](getting-started/production.md)
- [Install a managed node](getting-started/managed-node.md)
- [Enroll a node](how-to/enroll-node.md)
- [Troubleshooting](how-to/troubleshooting.md)

## Day-to-day operations

- [Upgrade the Controller](how-to/controller-upgrade.md)
- [Roll back the Controller](how-to/controller-rollback.md)
- [Uninstall the Controller](how-to/controller-uninstall.md)
- [Upgrade the Agent](how-to/agent-upgrade.md)
- [Roll back the Agent](how-to/agent-rollback.md)
- [Configure dedicated relays](how-to/dedicated-relays.md)
- [Back up and restore PostgreSQL](operations/postgres-backup.md)
- [Fail over PostgreSQL](operations/postgres-failover.md)
- [Recover PostgreSQL to a point in time](operations/postgres-pitr-restore.md)
- [Recover from an incident](operations/incident-recovery.md)

## Understand the system

- [Architecture](architecture.md)
- [Security policy](../SECURITY.md)
- [Technical reference](reference/README.md)

## Development and release evidence

- [Validate a change](development/testing.md)
- [Control-plane development](development/control-plane.md)
- [Contracts and toolchains](development/contracts.md)
- [GitHub Actions validation](development/github-actions.md)
- [Acceptance records](acceptance/README.md)
- [Upstream records](upstream/v4.9-post1.md)

The acceptance and upstream records are retained as release evidence and are not intended as first-read operator documentation.
