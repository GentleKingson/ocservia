# ocservia documentation

This documentation defines the technical specifications and operational protocols for ocservia. It provides deployment guidelines, maintenance procedures, and architectural references. We recommend consulting the installation guides for initial setup, the operational runbooks for routine maintenance, and the technical reference for system contracts.

## Start here

- [Project overview](../README.md)
- [Try ocservia locally](getting-started/local-development.md)
- [Deploy the Controller](getting-started/production.md)
- [Install a managed node](getting-started/managed-node.md)
- [Enroll a node](how-to/enroll-node.md)
- [Troubleshooting](how-to/troubleshooting.md)

## Day-to-day operations

- [Controller upgrade, rollback and uninstall](how-to/controller-lifecycle.md)
- [Agent upgrade and rollback](how-to/agent-lifecycle.md)
- [Configure dedicated relays](how-to/dedicated-relays.md)
- [Database backup and restore](operations/database-backup-restore.md)
- [Recover from an incident](operations/incident-recovery.md)

## Understand the system

- [Architecture](architecture.md)
- [Security policy](../SECURITY.md)
- [Current support and versioning policy](reference/support-policy.md)

## Deployment reference

- [Production configuration and Controller lifecycle](operations/production-deployment.md)
- [Authentication modes and first Local administrators](operations/authentication.md)
- [Agent package lifecycle and fleet upgrades](operations/agent-lifecycle.md)
- [Bootstrap endpoint hosting and trust](operations/bootstrap-hosting.md)

## Development and validation

- [Validate a change](development/testing.md)
- [Control-plane development](development/control-plane.md)
- [Contracts and toolchains](development/contracts.md)
- [GitHub Actions validation](development/github-actions.md)
- [Release policy and package validation](development/release-checks.md)

## Technical reference

- [Agent and privd boundary](development/agent-privd.md)
- [Node enrollment and trust](development/enrollment.md)
- [Iroh transport](development/transportd.md)
- [Certificates, secrets and Signer](development/certificates-and-signer.md)
- [Identity, authorization, approval and audit](development/identity-authorization-audit.md)
- [Current contracts and validation](reference/stable-contracts.md)
- [Controller command authorization v1](development/command-authorization-v1.md)
- [Command semantic hash v1](development/command-semantic-hash-v1.md) and [v2](development/command-semantic-hash-v2.md)
- [Configuration planning, apply and node-local TLS](development/configuration.md)
- [Command delivery and recovery](development/command-reliability.md)
- [Controlled session and service operations](development/session-operations.md)
- [Users, groups, quotas and expiry](development/user-management.md)
- [Web API and node workflows](development/web.md)
- [Telemetry and read-only fleet views](development/telemetry.md)
- HTTP API: [OpenAPI schema](../openapi/openapi.yaml)
- Protobuf contracts: [proto/](../proto/)
- Generated Web client: [web/src/api/generated/](../web/src/api/generated/)
- [Upstream provenance records](upstream/v4.9-post1.md)

The build system overwrites generated artifacts during `make generate`. Manual modifications to these files will be lost. For testing, the [validation guide](development/testing.md) indexes the specialized end-to-end, resilience, and capacity checks.

## Documentation scope

This documentation focuses on active procedures and system contracts. We restrict each topic to a single authoritative source to prevent redundancy. Historical data, including release decisions, implementation timelines, and isolated task reports, belong in the Git version history rather than these active documents.

Files within the `upstream/` directory support attribution and backport validation. They remain accessible via the technical reference section.
