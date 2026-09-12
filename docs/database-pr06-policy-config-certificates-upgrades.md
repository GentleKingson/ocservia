# PR-06 Draft: Policy, Configuration, Certificates and Upgrades

Base: PR-01 through PR-05 are merged, including PR-05 `98c2ef0` (#199).
The plan was reviewed against the common stores and startup wiring before the
acceptance change. PR-02 already moved the mutable workflows in this phase to
backend-owned SQL and common transactions; PR-06 makes their cross-backend
acceptance explicit instead of adding parallel implementations or weakening
their existing transaction boundaries.

## Reviewed Contracts

| Surface | Preserved contract | Required evidence |
| --- | --- | --- |
| Desired users and groups | Node lock, exact expected version, monotonically allocated desired revision, same-input replay, conflicting-key refusal, command/outbox/audit atomicity | `backend-policy-userstate` |
| Usage and policy | Per-session cursor lock, duplicate/stale sample rejection, monthly/lifetime delta accounting, quota and expiry enforcement, stable recovery keys | `backend-policy-useroperations` and native usage tests |
| Batch operations | Approval binding, ordered item identity, bounded submission, per-item leases, expired-claim recovery, owner fencing and child-operation refresh | `backend-policy-useroperations` |
| Configuration | Immutable candidate hash and expected revision, typed plan/apply envelopes, approval consumption, desired-revision CAS, failed-apply recovery and critical rollback evidence | `backend-policy-config` and native configuration tests |
| Certificates and artifacts | Exact CSR/receipt/key binding, binary certificate and sealed-secret storage, one-use download grants, global capacity lock, lease recovery, expiry and audit | `backend-policy-certificates` and native certificate/artifact tests |
| Agent upgrades and rollouts | Trusted catalog lookup, expected node version, exact approval binding, one active upgrade, durable result reconciliation, canary/batch limits, pause/resume and terminal idempotency | `backend-policy-upgrades` and native upgrade/rollout tests |
| HTTP/read paths | Workspace authorization, bounded queries, NULL and logical timestamp responses, upgrade target lookup, readiness failure on unavailable storage | `backend-policy-api` |

The service entry points continue to lock and recheck authoritative state in the
intent transaction. In particular, configuration planning and apply do not
turn a stale caller revision into success by rereading and substituting the
current revision. Certificate sign/seal/fetch calls remain outside database
transactions, while their durable intent, receipt verification, download
consumption and audit transitions retain their original atomic boundaries.
Release manifests remain fixed operator-provisioned files; request bodies cannot
supply package paths or digests.

No schema, historical migration, wire format, signing format, Agent/privd
privilege boundary or runtime grant changes are needed for this phase.
MySQL/MariaDB remain test/development-only.

## Acceptance Wiring

The required-test manifest now names the shared user-state, policy, read API,
configuration, certificate and upgrade/rollout workflows. Both full database
entry points execute the applicable groups and fail if a required top-level
test is absent, skipped or renamed. PostgreSQL keeps isolated user-state and
user-operation database clones because those fixtures deliberately retain
revision-zero commands and singleton lease state. Its full path still runs each
package's complete `Integration` suite; the manifest is an additional guard,
not a narrower selector. The previous approval, audit, attestation and
browser-boundary checks remain in their original runs.

## Validation

Validation ran only through `ssh BuildServer`, in the isolated checkout
`/root/ocservia-pr06`, using Go 1.26.6 and the pinned database images.

| Check | Result |
| --- | --- |
| Controller compile-only `go test -run '^$' ./...` | Passed |
| PostgreSQL 17 shared PR-06 groups | 7/7 required top-level tests passed; user-state, user-operations and read API used `-race` |
| PostgreSQL 18 shared PR-06 groups | 7/7 required top-level tests passed; user-state, user-operations and read API used `-race` |
| MySQL 8.4.10 shared user-state, user-operations and read API groups | 3/3 passed with `-race`, none skipped |
| MariaDB 12.3.2 shared user-state, user-operations and read API groups | 3/3 passed with `-race`, none skipped |
| MySQL 8.4.10 native usage, configuration, certificate issuance, artifact download, upgrade reconciliation and rollout creation | 6/6 passed with `-race` |
| MariaDB 12.3.2 identical native workflow set | 6/6 passed with `-race` |
| Required-test guard, signal/selection fixtures and full all/current/history routing | Passed |
| Database entry-point shell syntax | Passed |

The complete historical migration/rollback matrix, full Controller/transportd/
Agent process matrix and deployment workflows were not rerun for this scoped
acceptance change. Temporary database containers and test binaries were removed
after retaining the results above.

## PR-07 Boundary

PR-07 owns telemetry ingestion/retention and rollups, event and dashboard query
semantics, transport cursor/quarantine completion, the final database-access
inventory and removal of remaining business-layer PostgreSQL compatibility
sites. It also owns role-specific `api`, `worker`, `scheduler` and `all` process
acceptance, startup refusal when a required database capability is absent, and
the agreed query-plan, lock-wait, cleanup-bound and storage-growth measurements.
Those claims are intentionally not made by PR-06.

This change is for a Draft PR only. Do not merge, deploy, publish or mark the
new backends as production-supported.
