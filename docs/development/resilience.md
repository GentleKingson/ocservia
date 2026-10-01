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

The checks below reuse the existing Business and database owners. Manual Release Check
requires `run-resilience=true` for Integrated Business;
there is no separate G6 workflow or evidence framework.

| Check | Owner and entrypoint | Required observation |
| --- | --- | --- |
| R1 Controller restart | Existing Business Smoke / Integration environment, optional `run-resilience` | Original state and completed operation remain verifiable; expiration of the former owner lease triggers a fresh fenced Agent session, then new authorized business succeeds. |
| R2 Agent / privd / transport restart | Same Business environment; reuse extended Agent / privd checks once | Identity, journal and receipt survive; replacement connections advance the owner fence; confirmed effects are not repeated. |
| R3 Database interruption | Existing database smoke jobs and `test-enrollment-restart.sh`; reference Business environment for API outage behavior | Original storage and pool recover; confirmed state survives; unavailable dependencies cannot produce false success. Full CI keeps PG17/PG18, MySQL and MariaDB smoke coverage. |
| R4 Sole Relay interruption | Shared strict Business single-Relay scenario, once per run | Prove Relay dependence and that an offline queued command was never sent; recover the same Relay and command with exactly one additional real effect. |

Use the Business job status and actual recovery checkpoints in `result.json`.
Sanitized failure diagnostics use seven-day Actions artifacts. Selected checks must actually finish successfully;
missing fields, failure, cancellation and unexpected skips block acceptance.
Unselected resilience is explicitly `SKIPPED`; install-only diagnostics cannot
pass recovery acceptance. No separate fault matrix or evidence framework is
required.

## Retired scope and retained tools

G6 workflows, Go harness, dual-domain HA/PITR orchestration, schemas, verdicts,
checkpoints, rendezvous and dedicated tests are retired. Business builds its own native Relay image. The independent token-authenticated TCP/QUIC
network probe remains a small executable; existing startup/role/Rebind tests
retain only their scheduler completion fixture. Build/cache tooling, full-history
secret scans, test runtime and Relay fixture have neutral paths. Necessary
historical secret-scan allowlists remain.

Production installation and launch entrypoints require Relay A and reject
nonempty B before side effects. Existing A/B deployments require an explicit
operator change to one Relay; installer reruns preserve identity and trust state.

Owner/epoch/fence/lease, transactional outbox, authorization, approval, signing,
idempotent command identities, Agent SQLite journal and privd receipts remain
required. Unknown outcomes use read-only reconciliation and the existing
explicit retry after proven effect absence. Never treat a failed, provably
unsent offline command as expected Unknown. These checks do not promise general
exactly-once execution or production RTO/RPO.
