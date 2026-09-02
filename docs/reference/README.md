# Technical reference

These documents describe how ocservia works, its exact contracts, and its
validation evidence. They are not the first reading path for installing or
operating the system.

## System and security

- [Architecture and trust boundaries](../architecture.md)
- [Production deployment and Controller lifecycle](../operations/production-deployment.md)
- [Agent package lifecycle](../operations/agent-lifecycle.md)
- [Agent and privd boundary](../development/agent-privd.md)
- [Node enrollment and trust](../development/enrollment.md)
- [Iroh transport](../development/transportd.md)
- [Certificate and secret lifecycle](../development/certificate-secret-lifecycle.md)
- [Identity, authorization, approval, and audit](../development/identity-authorization-audit.md)

## Protocols and state

- [Controller command authorization v1](../development/command-authorization-v1.md)
- [Command semantic hash v1](../development/command-semantic-hash-v1.md) and [v2](../development/command-semantic-hash-v2.md)
- [Contracts and toolchains](../development/contracts.md)
- [Configuration planning](../development/config-plan.md)
- [Configuration apply and rollback](../development/config-apply.md)
- [Operations and transactional outbox](../development/operations-outbox.md)
- [Agent command journal](../development/agent-command-journal.md)
- [Controlled session and service operations](../development/session-operations.md)
- [User and group state](../development/user-group-state.md)
- [Quota, expiry, and batch operations](../development/quota-expiry-batch.md)
- [Telemetry and read-only fleet views](../development/telemetry.md)

## Validation and release engineering

- [GitHub Actions validation](../development/github-actions.md)
- [Cross-VM real E2E validation](../development/real-e2e.md)
- [P1 resilience and initial capacity validation](../development/p1-resilience-capacity.md)
- [G6 readiness harness](../development/g6-readiness.md)
- [G6 HA/PITR cross-VM harness](../development/g6-ha-pitr-topology.md)

## API and provenance

- HTTP API: [`openapi/openapi.yaml`](../../openapi/openapi.yaml)
- Protobuf contracts: [`proto/`](../../proto/)
- Generated Web client: [`web/src/api/generated/`](../../web/src/api/generated/)
- [Acceptance records](../acceptance/README.md)
- [Upstream provenance records](../upstream/v4.9-post1.md)

Generated artifacts are replaced by `make generate` and must not be edited
manually. Historical acceptance and upstream records are preserved as
evidence; this index only changes their navigation context.
