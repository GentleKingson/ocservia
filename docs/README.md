# ocservia documentation

These guides cover deploying and operating ocservia and document its internal contracts. New operators should start with the installation walkthroughs.

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
- [Configure a dedicated Relay](how-to/dedicated-relay.md)
- [Rebind an Agent to another Controller](how-to/rebind-controller.md)
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
- [Schema sources and generation](reference/stable-contracts.md#schema-sources-and-generation)
- [GitHub Actions validation](development/github-actions.md)
- [Release policy and package validation](development/release-checks.md)
- [Business Smoke and Integration](development/real-business-validation.md)
- [Cross-VM enrollment validation](development/cross-vm-enrollment-validation.md)
- [Single-instance resilience](development/resilience.md)
- [Single dedicated Relay validation](development/single-relay-validation.md)
- [Simulated-Agent resilience and capacity](development/p1-resilience-capacity.md)
- [Database migration contract](development/control-plane.md#current-sql-artifacts-and-bounded-upgrades)

## Technical reference

- [Consumer service boundaries](development/domain-service-boundaries.md)
- [HTTP contracts](development/http-baseline.md)
- [Integrated deployment contract](development/integrated-deployment-contract.md)
- [Agent and privd boundary](development/agent-privd.md)
- [Node enrollment and trust](development/enrollment.md)
- [Controller rebind and retention](development/controller-rebind.md)
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
- Upstream provenance: [v4.9 post-release backport](upstream/v4.9-post1.md) and [restore semantics and Ocserv compatibility](upstream/v4.9-post2.md)

Running `make generate` overwrites all generated artifacts, so do not edit those output directories by hand. Module-level checks along with end-to-end, resilience, and capacity test suites are indexed in the [validation guide](development/testing.md).

## Documentation scope

Each topic in `docs/` has a single authoritative home and covers current operational procedures and runtime contracts. Historical release decisions, rollout timelines, and one-off task reports live in Git history; the records under `upstream/` are kept for provenance attribution and backport verification.
