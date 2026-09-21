# ocservia documentation

Use these documents to deploy, operate, and understand ocservia. Start with the installation guides, use the operational runbooks for maintenance, and consult the technical reference for implementation contracts.

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
- [Validate MySQL/MariaDB backup and restore](operations/mysql-backup.md)
- [Fail over PostgreSQL](operations/postgres-failover.md)
- [Recover PostgreSQL to a point in time](operations/postgres-pitr-restore.md)
- [Recover from an incident](operations/incident-recovery.md)

## Understand the system

- [Architecture](architecture.md)
- [Security policy](../SECURITY.md)
- [1.0 support and versioning policy](reference/support-policy.md)
- [Technical reference](reference/README.md)

## Deployment reference

- [Production configuration and Controller lifecycle](operations/production-deployment.md)
- [Authentication modes and first Local administrators](operations/authentication.md)
- [Agent package lifecycle and fleet upgrades](operations/agent-lifecycle.md)
- [Dedicated relay configuration](operations/dedicated-relays.md)
- [Bootstrap endpoint hosting and trust](operations/bootstrap-hosting.md)

## Development and validation

- [Validate a change](development/testing.md)
- [Control-plane development](development/control-plane.md)
- [Contracts and toolchains](development/contracts.md)
- [GitHub Actions validation](development/github-actions.md)
- [Native release upgrade validation](development/release-upgrade-validation.md)
- [Readiness harness contracts](acceptance/README.md)

## Documentation scope

Keep deployment guides, architecture and security contracts, operational
runbooks, and instructions needed to maintain or validate the project here.
Historical release verdicts and one-off task closeout reports do not belong
in the maintained documentation; use Git history for removed records.
Existing historical records retained for traceability carry a dated source
snapshot notice and links to current guides. Their Draft status and original
PASS/FAIL/NOT RUN results describe only their recorded revisions, not current
support or release acceptance. They are not part of the operating-guide index.

The machine-readable files in `acceptance/` are harness inputs, not disposable
reports. The files in `upstream/` support attribution and backport validation
and remain available through the technical reference.
