# Single-instance resilience

Each supported deployment uses one Controller, one database instance and one
dedicated Relay. PostgreSQL, MySQL and MariaDB retain their current versions and
deployment scope in [production deployment](../operations/production-deployment.md#database-support).
Integrated / standalone and bundled / external modes remain available, as does
management of multiple Agents. The components need not share a physical host.

Controller HA, multiple Relay failover, database replication clusters,
PostgreSQL automatic failover and PITR readiness are outside this support scope.
Ordinary backups, verification and isolated restore remain supported. Rebind
uses two independent Controller deployments to transfer authority; it is not HA.

## Recovery coverage and ownership

The following table defines the replacement coverage. Implementation is being
migrated from the G6 workflows; this decision does not claim that these checks
already run. The CI owner switch and rejection of a second Relay address must
land together, after the Business checks are executable.

| Check | Owner and entrypoint | Required observation |
| --- | --- | --- |
| R1 Controller restart | Existing Business Smoke / Integration environment, optional `run-resilience` | Original state and completed operation remain verifiable; new authorized business succeeds. Require a fresh session only when the connection actually broke. |
| R2 Agent / privd / transport restart | Same Business environment; reuse extended Agent / privd checks once | Identity, journal and receipt survive; replacement connections advance the owner fence; confirmed effects are not repeated. |
| R3 Database interruption | Existing database smoke jobs and `test-enrollment-restart.sh`; reference Business environment for API outage behavior | Original storage and pool recover; confirmed state survives; unavailable dependencies cannot produce false success. Full CI keeps PG17/PG18, MySQL and MariaDB smoke coverage. |
| R4 Sole Relay interruption | Shared strict Business single-Relay scenario, once per run | Prove Relay dependence and that an offline queued command was never sent; recover the same Relay and command with exactly one additional real effect. |

Reuse `result.json`, current artifact SHA/version/architecture/digest checks and
seven-day Actions artifacts. Selected checks must actually finish successfully;
missing fields, failure, cancellation and unexpected skips block acceptance.
Unselected resilience is explicitly `SKIPPED`; install-only diagnostics cannot
pass recovery acceptance. No separate fault matrix or evidence framework is
required.

## Migration boundaries

First move the shared build cache, cache-credentials action, full-history secret
scan configuration, database E2E runtime and Relay fixture to neutral locations.
Update all consumers, including temporarily active G6 workflows, before removing
old paths. Preserve justified historical secret-scan fixture allowlists.

Then implement the recovery checks, atomically switch their CI owner and enforce
one Relay in production installation and launch entrypoints. Keep
`OCSERV_RELAY_URL_A`; reject nonempty B before side effects. Finally remove the
unused G6 workflows, harness, dual-domain HA/PITR orchestration and dedicated
tests. Probe/tunnel removal requires confirming there are no retained consumers.

Owner/epoch/fence/lease, transactional outbox, authorization, approval, signing,
idempotent command identities, Agent SQLite journal and privd receipts remain
required. Unknown outcomes use read-only reconciliation and the existing
explicit retry after proven effect absence. Never treat a failed, provably
unsent offline command as expected Unknown. These checks do not promise general
exactly-once execution or production RTO/RPO.
