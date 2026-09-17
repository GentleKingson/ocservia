# Consumer service boundaries

PR-04 narrows two method sets. It does not isolate all domains or introduce a
runtime authorization boundary. Shared DTOs, error identities, database Backends
and cross-Store transactions remain intentional.

Baseline inspected: `5eb5896dcd1352412d3e1ab3045d6906a1e5861a` (PR-03).

| Consumer | Previous dependency | Consumer-owned port and call sites | Assembly and context | Transaction owner |
| --- | --- | --- | --- | --- |
| `configplan.Service` | `*operations.Service` | `operationCreator.CreateSynthetic`: `Create`, `Apply` | `platform/app` passes its existing configured Operations instance; original request context | ConfigPlan owns pre-read transactions, Operations owns command/Plan/Outbox/audit writes and bound approval consumption |
| `useroperations.Service` | `*userstate.Service` | `userMutator.Mutate`: monthly reset, policy enforcement, batch submission | `platform/app` passes the shared signed UserState instance; original scheduler context including its fence | UserOperations owns policy/claim/receipt transactions; UserState owns desired-state/command/Outbox/audit commit and its fencing check |

The constructors retain their names, delegation and concurrency defaults.
Read/validation paths do not require a writer. Neither consumer checks whether
its writer is nil; converting a typed nil does not add a fallback or make writes
succeed. Production assembly always supplies the original configured instance.

## Verification entry points

- `scripts/go-check.sh standard`: interface validation, minimal constructor and
  field boundaries, no concrete-Service bypass or reverse HTTP/assembly import,
  stable scheduler identities/batch hashes, and the existing nodehttp/httpx and
  lifecycle/HTTP unit baselines.
- PostgreSQL `DATABASE_TEST_SCOPE=regression PG_MAJOR=all
  scripts/database-integration.sh`: `backend-policy-config` exercises recorded
  Create/Apply requests, signed result ingress, approval consumption and rollback;
  `backend-policy-useroperations` exercises all three mutation paths and fencing.
- MySQL/MariaDB `DATABASE_TEST_SCOPE=regression ENGINE=mysql` (or `mariadb`)
  `bash scripts/database-foundation-integration.sh`: the required
  `TestRealConfigurationReadAndIntent/consumer-service-chain` runs the actual
  ConfigPlan-to-Operations chain using the existing restricted-runtime fixture;
  `backend-policy-useroperations` uses the existing multi-backend service fixture.
- The same service tests remain selected by Full. Required-test manifests reject
  missing or skipped service cases; ordinary database skips are not acceptance.

ConfigPlan's PostgreSQL-only historical test is not four-backend evidence. The
MySQL/MariaDB consumer chain is checked separately, not inferred from Store tests.
PR-04 exercised Apply directly at the service boundary, leaving the historical
HTTP `routeMethod` omission unchanged. The subsequent F-1 functional fix restores
that route and adds four-backend HTTP acceptance in `regression-auth` alongside,
not instead of, these service tests. Contracts and generated clients are unchanged.

At the PR-04 baseline, runtime grants omitted `DELETE` on
`user_policy_enforcements` on all backends and four cleanup branches ignored
errors. The F-2 functional fix adds only that table's DELETE grant through the
existing production authorization entry points. This is a table-level database
permission, not row-level authorization: the existing Store predicates still
bind node, user, policy version, cause, period, unfinished operation and, where
required, source user version. Policy error tests now use restricted runtime
connections; owner credentials are confined to fixture setup and fault injection.

Cleanup failures preserve their cause as `EnforcementCleanupError`. They end the
current maintenance pass without running later steps or recording completion.
The Scheduler logs the failure and waits for its existing next tick; persistent
failure keeps those later steps blocked until permissions or storage recover.
Leadership loss, parent cancellation, ordinary fatal errors and rollout handling
retain their existing classification. No new retry timer or background service
is introduced. Existing installations need the owner `--migrate-only`
reauthorization path even when the schema is already current; see
[Controller upgrade](../how-to/controller-upgrade.md).
