# Single-instance resilience

Supported deployments run a single Controller, a single database instance, and
one dedicated Relay, with PostgreSQL and MySQL version and deployment support
defined in
[production deployment](../operations/production-deployment.md#database-support).
Both Integrated and standalone topologies as well as bundled and external
databases remain supported, and a deployment can manage multiple Agents whether
the components share a host or run on separate machines.

Out of scope are Controller high availability, multi-Relay failover, database
replication clusters, PostgreSQL automatic failover, and point-in-time recovery
(PITR) readiness. Standard backups, backup verification, and isolated restores
remain supported, and Controller Rebind transfers authority between two
independent Controller deployments without providing live HA failover.

## Recovery coverage and ownership

Single-instance recovery checks run under the existing Business and database
owners. Manual Release Check invokes Integrated Business Smoke with
`run-resilience=true`; the older G6 workflow and its separate evidence
framework have been retired.

| Check | Owner and entrypoint | Required observation |
| --- | --- | --- |
| R1 Controller stack | Business Smoke with `run-resilience` | Restart Controller and transport together; both ready, fresh Agent session and new authorized business succeeds. |
| R2 Agent stack | Same smoke environment | One systemd restart transaction for privd and its dependent Agent; both active, fresh session and unchanged identity. |
| R3 Database interruption | Same smoke environment | Stop the bundled PostgreSQL container, require database-unavailable readiness, start it, recover readiness and Agent connectivity, then complete normal business. No outage mutation. MySQL and external databases have no Business-level recovery check. |
| R4 Sole Relay interruption | Same smoke environment | Stop the only Relay, observe Agent offline with direct UDP blocked, restart Relay, acquire a fresh session and recover authorized business and VPN traffic. |

When running the extended manual integration profile, deeper variants replace
R1 through R4: separate Controller and transport restarts, mutations submitted
during the outage, unsent offline queueing, idempotent replay, exact native
reload counts, and cross-checks across the API, database, Agent journal, and
root receipts. Release Smoke does not require those extended variants. Full CI
separately runs the full database suite alongside the PostgreSQL and MySQL
backup/restore smoke scripts, which validate database-level recovery without
covering Controller, Agent, or Relay restarts.

Acceptance relies on the Business job status together with the scenario
checkpoints recorded in `result.json`, while sanitized failure diagnostics are
retained as seven-day GitHub Actions artifacts. When resilience is enabled,
only `controller`, `agent`, `database`, and `relay` are required scenario
checkpoints (there is no synthetic overall checkpoint or standalone transport
scenario), and each of the four must finish and pass; missing fields, failures,
cancellations, or unexpected skips block acceptance. Runs that do not select
resilience record `SKIPPED`.

## Recovery boundaries

During Business runs, the harness builds the native Relay container image from
the candidate tree. The standalone token-authenticated TCP/QUIC network probe
verifies transport reachability in isolation and does not exercise the
end-to-end business recovery chain.

Production installers and launch scripts require Relay A and reject a non-empty
Relay B before making any host or container changes. Upgrading an older A/B
deployment requires the operator to switch the configuration to a single Relay
explicitly, after which installer reruns preserve existing node identity and
trust state.

Across all recovery scenarios, the command pipeline still enforces
owner/epoch/fence/lease validation, the transactional outbox, authorization,
multi-principal approvals, cryptographic signatures, idempotent command IDs,
the Agent SQLite journal, and privd receipts. Operations that end in an Unknown
state rely on read-only reconciliation followed by the existing explicit retry
once absence of effect is confirmed, whereas a queued offline command that was
provably never sent must never be treated as expected Unknown if it fails.
Passing these checks does not imply general exactly-once execution or
production RTO/RPO guarantees.
