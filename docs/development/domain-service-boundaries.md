# Consumer service boundaries

Consumer packages depend on narrow method sets while sharing domain values and errors, database Backends, and cross-Store transactions. These interfaces define Go package boundaries and do not act as runtime authorization boundaries.

| Consumer | Consumer-owned capability | Transaction owner |
| --- | --- | --- |
| `configplan.Service` | `operationCreator.CreateSynthetic` for Create/Apply | ConfigPlan owns pre-read transactions; Operations commits command/Plan/Outbox/audit and consumes approval |
| `useroperations.Service` | `userMutator.Mutate` for monthly reset, enforcement and batch submission | UserOperations owns policy/claim/receipt; UserState commits desired state/command/Outbox/audit and checks fencing |
| `configplanhttp.Handler` | `Plans.Create/Get/Apply` | Existing domain services |
| Parent authorization/approval HTTP | `configPlanLookup.ApprovalBinding/Resource` | Approval creation/decision stays in the parent; consumption stays in Operations |

During wiring, `platform/app` passes the configured service instances along with the caller's original request or scheduler context, preserving any active leadership fence. Read-only and validation paths can run without a writer, and passing a nil writer never triggers a fallback or allows a write to succeed. Production wiring always supplies the shared domain instances, while service constructors keep their built-in concurrency defaults.

`NewServer` derives both ConfigPlan consumer views from a single input and normalizes concrete typed nil pointers into true nil interfaces. Once Certificates, RBAC, and devAuth are initialized, its request adapter populates actor, identity, session, request, and trace fields and attaches the Secret-use callback while keeping resource and permission lookups live on each call. Common request decoding and header helpers live in `api/httpx`; see [HTTP contracts](http-baseline.md#configplan-http-module-r2-02) for validation order, error precedence, module limits, and server lifecycle rules.

## Policy cleanup

At runtime, the database account requires DELETE privileges on `user_policy_enforcements`, which the owner migration path grants both on fresh installs and when reauthorizing existing databases already at the current schema. That table-level privilege only permits the SQL statement to run; Store methods still enforce row predicates for node, user, policy, cause, period, unfinished operations, and the required source version.

When a cleanup delete fails, `EnforcementCleanupError` wraps the failure and aborts the current maintenance pass before downstream work runs or completion is recorded. The scheduler logs the error and retries on its next regular tick, while leadership loss, context cancellation, fatal errors, and rollout handling keep their distinct error classifications. For the operator steps that refresh runtime grants during an upgrade, see [Controller upgrade](../how-to/controller-lifecycle.md#upgrade).

## Upgrade preparation

Both `operations.Service.PrepareAgentUpgrade` and `AgentUpgradeApprovalBinding` resolve their target through `RBACStore.UpgradeNode` and the operator-provisioned release catalog, which application assembly configures before starting any consumer services. The HTTP layer handles request decoding, session and resource authorization, and its dedicated Problem responses, leaving package identity construction to the domain service. If the Operations service is absent, upgrade preparation is disabled.

Target preparation is only the front-end step: `CreateSynthetic` still acquires locks, checks versions and replay state, validates node capabilities and attestation, records the observed version, consumes the approval, and enforces the active-upgrade guard. Approving an upgrade binds a trusted catalog entry without guaranteeing that the target version is newer than the node's current build or ready to execute immediately. Single-node upgrades can queue work for an offline node, whereas rollout admission enforces its own readiness rules separately.

## ConfigPlan approval binding

`configplan.Service.ApprovalBinding` loads the plan through the interpreted `Get` path instead of raw Store `Proof`, validates plan status and expiry against the service clock, and returns the workspace ID, node ID, candidate hash, and redacted summary bytes. At the HTTP boundary, handlers still verify the selected workspace and node `config.apply` permission, and approval detail or decision endpoints check every saved authority resource before falling back to the legacy single `Resource` field. Apply prechecks, desired-revision allocation, and transactional approval consumption remain with their respective domain owners.

## Verification

Choose test targets according to [Validate a change](testing.md). Standard Go unit tests verify constructor and interface boundaries, confirm that packages neither bypass consumer interfaces through concrete Service types nor introduce reverse imports, and check scheduler identities, batch hashes, and canonical approval or upgrade bindings.

For database-backed behavior, the existing `backend-policy-config`, `backend-policy-useroperations`, `backend-policy-api`, and `regression-auth` suites cover signed command intent, approval consumption and rollback, policy cleanup errors, leadership fencing, and HTTP status ordering. On MySQL, `TestRealConfigurationReadAndIntent/consumer-service-chain` exercises the end-to-end ConfigPlan-to-Operations path independently of PostgreSQL's older service test. Run these checks over restricted runtime database connections, reserving owner credentials strictly for schema setup and fault injection. Every required test case must execute; skipped database tests do not satisfy acceptance.
