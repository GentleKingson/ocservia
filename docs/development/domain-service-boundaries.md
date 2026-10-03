# Consumer service boundaries

Consumers depend on narrow method sets while sharing domain values/errors,
database Backends and cross-Store transactions. These are package boundaries,
not runtime authorization boundaries.

| Consumer | Consumer-owned capability | Transaction owner |
| --- | --- | --- |
| `configplan.Service` | `operationCreator.CreateSynthetic` for Create/Apply | ConfigPlan owns pre-read transactions; Operations commits command/Plan/Outbox/audit and consumes approval |
| `useroperations.Service` | `userMutator.Mutate` for monthly reset, enforcement and batch submission | UserOperations owns policy/claim/receipt; UserState commits desired state/command/Outbox/audit and checks fencing |
| `configplanhttp.Handler` | `Plans.Create/Get/Apply` | Existing domain services |
| Parent authorization/approval HTTP | `configPlanLookup.ApprovalBinding/Resource` | Approval creation/decision stays in the parent; consumption stays in Operations |

`platform/app` passes the existing configured instances and original request
or scheduler context, including its fence. Read/validation needs no writer;
nil writers do not add a fallback or make writes succeed. Production supplies
the shared instances. Constructors retain concurrency defaults.

`NewServer` derives both ConfigPlan views from one input and normalizes concrete
typed nils. Its request adapter supplies actor/identity/session/request/trace
values and a Secret-use check after Certificates/RBAC/devAuth are fixed.
Resource and permission lookups remain live. Shared parsing belongs to
`api/httpx`; see [HTTP contracts](http-baseline.md#configplan-http-module-r2-02)
for validation/error order, module limits and lifecycle.

## Policy cleanup

Runtime needs DELETE on `user_policy_enforcements` through the owner migration
authorization path, including existing installations with current schema.
This table-level grant does not replace Store predicates for node/user/policy,
cause/period, unfinished operation and required source version.

`EnforcementCleanupError` preserves cleanup failures and stops the current
maintenance pass before later work or completion is recorded. Scheduler logs
the failure and retries on its normal next tick. Leadership loss, cancellation,
fatal errors and rollout handling retain their classifications.
See [Controller upgrade](../how-to/controller-lifecycle.md#upgrade) for reauthorization.

## Upgrade preparation

`operations.Service.PrepareAgentUpgrade` and `AgentUpgradeApprovalBinding`
resolve targets through `RBACStore.UpgradeNode` and the operator-provisioned
release catalog. Assembly configures that shared catalog before consumers start.
HTTP owns decoding, session/resource authorization and its distinct Problems,
not package identity construction; missing Operations disables preparation.

Preparation does not replace `CreateSynthetic` locking, version/replay checks,
capability/attestation checks, observed-version recording, approval consumption
or active-upgrade guard. Approval binds a trusted target without promising it
is newer or executable. Single-node execution can queue offline work; rollout
admission remains separate.

## ConfigPlan approval binding

`configplan.Service.ApprovalBinding` uses interpreted `Get`, not Store
`Proof`, checks validity/expiry with the service clock and returns workspace,
node, candidate hash and safe summary bytes. HTTP retains selected-workspace
and node `config.apply` checks. Approval detail/decision checks every saved
authority before legacy `Resource` fallback. Apply prechecks, desired-revision
allocation and transactional consumption remain with their existing owners.

## Verification

Select checks through [Validate a change](testing.md). Ordinary Go tests cover
constructor/interface boundaries, no concrete-Service bypass or reverse imports,
scheduler identities, batch hashes and canonical approval/upgrade bindings.

Existing `backend-policy-config`, `backend-policy-useroperations`,
`backend-policy-api` and `regression-auth` groups cover signed intent,
approval consumption/rollback, cleanup failures, fencing and HTTP behavior.
MySQL's `TestRealConfigurationReadAndIntent/consumer-service-chain`
checks the actual ConfigPlan-to-Operations path separately from PostgreSQL's
historical service test. Use restricted runtime connections; owner access is
only for setup/fault injection. Required cases must run, and ordinary database
skips do not count as acceptance.
