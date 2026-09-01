# Documentation index

Reference documentation for [ocservia](../README.md), a self-hosted management plane for ocserv / OpenConnect VPN fleets. This index links to the authoritative documents; each topic is documented once in its own file.

## Getting started

- [Control-plane development](development/control-plane.md) — run the local development stack (simulated Agents, transport stub) and the browser E2E harness.
- [Production deployment](operations/production-deployment.md) — prerequisites and the guarded Controller install, upgrade, rollback, and uninstall lifecycle.

## Architecture and security

- [Architecture and trust boundaries](architecture.md) — components, data flow, and the full trust-boundary reference.
- [Node enrollment development](development/enrollment.md)
- [Controller command authorization v1](development/command-authorization-v1.md)
- [Command semantic hash v1](development/command-semantic-hash-v1.md) · [v2](development/command-semantic-hash-v2.md)
- [Agent and privd](development/agent-privd.md)
- [Identity, authorization, approval, and audit operations](development/identity-authorization-audit.md)
- [Certificate and secret lifecycle](development/certificate-secret-lifecycle.md)
- Vulnerability reporting policy: [`SECURITY.md`](../SECURITY.md)

## Operations

- [Production deployment](operations/production-deployment.md)
- [Agent package lifecycle](operations/agent-lifecycle.md) — signed package installation, upgrades, rollback, and Controller-driven rollouts.
- [Dedicated relays](operations/dedicated-relays.md)
- [PostgreSQL backup and restore](operations/postgres-backup.md)
- [PostgreSQL failover, former-primary fencing, and rejoin](operations/postgres-failover.md)
- [PostgreSQL point-in-time recovery](operations/postgres-pitr-restore.md)
- [Production incident recovery](operations/incident-recovery.md)

## Development

- [Control-plane development](development/control-plane.md)
- [Contracts and toolchains](development/contracts.md)
- [GitHub Actions validation](development/github-actions.md)
- [Iroh transport development](development/transportd.md)
- [Telemetry and read-only fleet views](development/telemetry.md)
- [Controlled session and service operations](development/session-operations.md)
- [User and group state](development/user-group-state.md)
- [Quota, expiry, and batch operations](development/quota-expiry-batch.md)
- [Configuration planning](development/config-plan.md)
- [Configuration apply and rollback](development/config-apply.md)
- [Operations and transactional outbox](development/operations-outbox.md)
- [Agent command journal](development/agent-command-journal.md)
- [P1 resilience and initial capacity validation](development/p1-resilience-capacity.md)
- [Cross-VM real E2E validation](development/real-e2e.md)
- [G6 readiness harness](development/g6-readiness.md) · [G6 HA/PITR cross-VM harness](development/g6-ha-pitr-topology.md)

## API and contracts

- HTTP API: [`openapi/openapi.yaml`](../openapi/openapi.yaml)
- Protobuf contracts: [`proto/`](../proto/)
- Generated Web client: [`web/src/api/generated/`](../web/src/api/generated/)

Generated artifacts are replaced by `make generate` and must not be edited manually; see [Contracts and toolchains](development/contracts.md).

## Acceptance and upstream records

- [`acceptance/`](acceptance/README.md) — machine-readable G6 acceptance contracts and historical release-readiness records, kept as evidence rather than deployment instructions.
- [`upstream/`](upstream/v4.9-post1.md) — upstream review and backport provenance records.
