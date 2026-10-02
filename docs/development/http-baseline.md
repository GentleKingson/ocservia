# HTTP contracts

`internal/api` owns HTTP assembly, middleware and resource authorization.
`nodehttp`, `configplanhttp` and `useroperationshttp` own their handlers and
consumer interfaces. Package boundaries do not replace runtime authorization
or split domain transactions.

## Assembly and lifecycle

`api.NewServer(HTTPConfig, backend, build, logger, Modules, Authorization)`
constructs the complete server. Deployment uses `newHTTPServer`;
`NewBackend` is a compatibility constructor with default SSE settings and
disabled optional modules, not a server completed through business setters.
Non-API roles return before authentication or HTTP construction.

Assembly configures shared domain services, authentication, RBAC, approvals,
audit, Certificates and Plan lookup before constructing modules and routes.
`Modules.ConfigPlans` supplies both business and parent lookup capabilities from
one instance; `Authorization.RBAC` supplies both parent and batch guards.
Known concrete typed nils become disabled capabilities, not replacement services.
Modules retain neither assembly inputs, stores, Transport nor a parent Server.

SSE requires explicit valid configuration. Invalid or partial settings fail
construction; allocated resources are closed on failure. Platform and operation
hubs share admission and watcher budgets, start polling on subscription, and
have no runtime reconfiguration or request-time constructor.

Middleware order is `otelhttp -> requestContext -> limitBody -> timeout ->
trackRequests -> routeErrors -> ServeMux -> route wrapper -> handler`.
Ordinary requests use `http.TimeoutHandler`; SSE bypasses timeout but stays
tracked. Lifecycle takes Shutdown ownership before listening. Shutdown closes
admission/hubs, shuts down HTTP, closes connections on deadline and drains inner
handlers. Closed hubs remain installed. HTTP does not own the shared database
or domain services.

## Methods and dispatch

Ordinary `HandleFunc` declarations are the production method source.
The construction-only `methodRegistrar` forwards each registration once to the
root ServeMux before publishing methods. Failed registration publishes no
metadata. Duplicate methods, unsupported/overlapping shapes and mux conflicts
panic. Completed rules are read-only and isolated per Server.

Patterns require explicit methods, hostless paths, literal segments and
whole-segment `{name}` parameters. Subtrees, remainder wildcards, `{$}`, embedded
parameters, escaped literals and dot segments are rejected, as are equivalent
shapes with different parameter names. Identical paths can merge methods while
retaining distinct guards.

Derived lookup precedes `compatibilityRouteMethod`, including method denials.
Compatibility rules retain action-dependent Local-user, session, IP-ban, user,
certificate, SecretRef and approval paths, plus the operation-detail prefix
rule. They cannot override derived `summary` or `queue-metrics` denials.

Static paths compare full strings; parameter paths retain Trim/Split semantics
over `URL.Path`, without further decoding or normalization. ServeMux receives
the original request and parameter names. GET does not imply HEAD. Unknown
paths produce Problem 404; some trailing-slash paths reach plain-text 404 and
doubled leading slashes can redirect with 307. Preserve error/authentication
order and these path differences. The independent inventory in
`control-plane/internal/api/routes_baseline_test.go` checks routes, wrappers,
methods and permissions; historical route counts are not a contract.

## Module authorization

Required explicit guards authenticate, check browser mutations, resolve the
actual resource/workspace, authorize the action and supply trusted context.
Modules never derive workspace authority from an unverified client header.
Other routes retain central action inference, resource checks and SSE
revalidation. Original contexts carry cancellation and deadlines into services.

| Module | Routes and actions | Consumer capability |
| --- | --- | --- |
| `nodehttp` | List/detail, sessions, IP bans, telemetry: `node.read` | `ListNodesInWorkspace`, `GetNode`, `ListSessions`, `ListIPBans`, `HistoryFrom` |
| `configplanhttp` | Create: `config.plan`; Get: `config.review`; Apply: `config.apply` | `Plans.Create/Get/Apply`, authenticated request values, Secret-use check |
| `useroperationshttp` | Policy GET: `node.read`; policy PUT and batch POST: `user.manage`; batch GET and metrics: `operation.read` | `Operations.GetPolicy/SetPolicy/CreateBatch/GetBatch/Metrics`, `Authorizer.Node/Authorize` |

`httpx` owns JSON/Problem encoding, pagination, UUIDv7, strict JSON and
idempotency parsing without auth/business dependencies. Strict JSON retains
media-type, UTF-8, unknown-field, single-value and shared body-size checks.
Node session reads use `[]telemetryread.Session`, not ingestion models.

<a id="configplan-http-module-r2-02"></a>
## ConfigPlan requests

Create checks availability before node ID; Apply checks ID, trimmed idempotency
key, JSON and approval ID before its business call. Disabled services fail at
their existing validation point, after outer authentication, Origin and resource
guards. Create returns a Plan; Apply returns an Operation with its own Location
and replay headers. A 202 proves committed intent, not Agent execution.

Secret checks resolve each reference's workspace and permission on every request,
retaining the `s.devAuth` exception. No references need no Certificates service.
Missing dependencies return 503 when used; lookup/workspace/permission failures
return 403 before Create. The transaction still checks stored state/version
and node ownership.

Node `config_revision` comes from `node_config_state.revision`, independently
of `nodes.version` and `desired_revision`. Missing state means zero; failed reads
remain errors. OpenAPI permits absence for older servers. Web captures a known,
nonnegative JavaScript-safe revision with node/workspace generation and neither
rebases nor automatically retries stale requests.

Same-Plan, same-workspace-key Apply replay reuses the committed desired revision
and original command's `expected_version`; new keys use live state. Complete
intent comparison, revision locking and approval consumption stay transactional.

The parent receives `ApprovalBinding/Resource` from the same ConfigPlan instance.
The domain interprets validity/expiry and returns the candidate hash and safe
summary. Missing service/wrong resource type returns `400 invalid-request`;
read or invalid/expired Plan returns `409 config-plan-not-ready` before
foreign-workspace `400 invalid-request` and node-RBAC `403 forbidden`.
`config_plan_summary` retains `node_id`, `expected_revision`, `candidate_hash`,
`current_hash`, `diff_redacted` and `expires_at`. Detail/decision checks cover
all saved authority resources before legacy fallback.

## User policy and batches

Policy writes check key before JSON, require RFC3339 with trailing Z and domain
validation of fractional seconds, retain body `expected_version`, trim reason,
and return 200 with revision ETag/replay headers. Replay reads current policy.
Batch creation returns 202 and Location. Input order and duplicates are
preserved; strict JSON rejects client-supplied `authorized`.

A missing or foreign node rejects the entire batch. An unauthorized
same-workspace item persists as forbidden; an authorized missing user persists
as failed/not_found. The development per-item exception depends on Principal
issuer, not Server devAuth. Missing, malformed or non-v7 `X-Approval-ID`
becomes nil, never approval authority. Item binding/order/versions and approval
consumption belong to the domain transaction.

Batch reads perform lookup/workspace 404 before the additional read check.
Creators skip only that additional workspace-wide check, never the outer guard;
other readers require workspace `operation.read`. Node managers do not gain
workspace metrics. Missing capabilities fail closed after the public guard.

## Upgrade requests

Operations owns trusted target preparation. Unknown architecture maps to
`release-not-trusted` for execution and `node-not-ready` for approval.
Node read/workspace failures remain `404 not-found` versus `409 node-not-ready`.
Invalid syntax precedes node read, architecture, catalog lookup and the
newer-version check. Approval need not target a newer observed version.
Single-node execution may queue offline work, unlike rollout admission.

Replay still prepares the target: node-version drift does not change its original
intent identity, but an Agent already at the target returns `target-not-newer`.
Missing Operations disables approval preparation. Transactional versions,
capabilities, approvals and active-upgrade guards remain domain responsibilities.

## Verification

Select checks through [validation guidance](testing.md). Ordinary Go tests cover
construction, boundaries, method inventory/path equivalence, failure atomicity,
per-server isolation, SSE budgets and shutdown. Module tests exercise the mux
with narrow handwritten capabilities; full-chain tests cover cancellation,
timeouts and inner-work draining.

Existing `regression-auth`, `backend-policy-api` and startup groups exercise
real Local sessions and restricted runtime connections: Plan Apply/replay,
approval/error order, Secret denial, policy/batch ownership, signed child intent,
upgrade binding and first-request assembly. Owner access is limited to setup
and fault injection. PostgreSQL-only tests are not four-backend evidence;
skipped database tests are not acceptance. HTTP intent tests do not establish
Agent execution, package installation or release readiness.
