# HTTP foundation baseline (PR-02)

Baseline: `de26ea4b93bbedaafff670d0f77d02132a71f08d` (PR-01, #216).
The inventory in `control-plane/internal/api/routes_baseline_test.go` records
all 72 registrations, their handler/wrapper expressions, representative path
substitutions, method rules and permissions. It is not production route metadata.

The preserved execution order is `otelhttp -> requestContext -> limitBody ->
timeout -> trackRequests -> routeErrors -> ServeMux -> route wrapper -> handler`.
Ordinary requests run inside `http.TimeoutHandler`; SSE bypasses that wrapper,
not request tracking. Shutdown still closes admission and event hubs, shuts down
HTTP, closes connections on deadline, and waits for inner work. Closed hubs are
retained to prevent late watcher creation.

Mechanical moves, without signature or visibility changes:

- `server.go`: registrations to `routes.go`, method/path checks to `routing.go`,
  generic wrappers to `middleware.go`, response writers to `response.go`;
  authentication wrappers stay with `authorization.go`.
- `enrollment.go`: `decodeStrictJSON`, `parseUUIDv7`, `requestID` to `request.go`.
  Callers include auth, enrollment, configuration, certificate and user handlers.
- `local_slice.go`: pagination, cursor and trace correlation helpers to
  `request.go`; callers include node reads, events and operation writes.
- ConfigPlan create/apply share `requireIdempotencyKey`: trim the header and
  write the identical missing-key Problem. UUID checks still precede it, JSON
  decoding follows it, and their different success Locations remain unchanged.

Historical PR-02 observations:

- ConfigPlan apply was registered, but `routeMethod` rejected its path with 404
  before authentication for every method. PR-02 froze that defect as
  `unreachable`; the F-1 functional fix below supersedes only this expectation.
  Supplemental direct-handler tests still cover idempotency/body error priority.
- HEAD is rejected even on GET routes. Unknown paths return Problem 404;
  a trailing slash on a parameter path can instead reach ServeMux's plain-text
  404. With the pinned Go 1.26.6 toolchain, a doubled leading slash on the node
  detail path receives a 307 HTML redirect. These differences are intentional
  baseline observations, not newly unified error handling.

Key tests run through `NewBackend().http.Handler`. Read fixtures use real Local
sessions, restricted database connections and separate workspaces. Connection
tests cover flushed/resumed SSE, normal timeout and inner-handler draining.
Existing Local/OIDC, Origin, JSON and PR-01 shutdown cases remain in use.
`regression-auth` selects the node/read baselines on PostgreSQL 17/18, MySQL and
MariaDB; PostgreSQL additionally selects the Local/OIDC and ConfigPlan HTTP
response baselines. Required-test guards reject missing or skipped cases.

PR-02 kept `api`, concrete service fields, `Server`, `NewBackend`, `EnableXXX`,
`routeMethod`, `routeAction` and resource authorization. Node/telemetry handler
extraction and fixing the apply-route omission were separate follow-ups.
Final candidate SHA and command results belong in the Draft PR evidence.

## Node read module (PR-03)

Base: `b34b91df057a2dd824fc0a0c2e3326c4a1da7819` (PR-02, #217).
Only the following five registrations move from `api/routes.go` to
`api/nodehttp/routes.go`; the inventory still covers all 72 registrations.

| GET path | Module handler |
| --- | --- |
| `/api/v1/nodes` | `listNodes` |
| `/api/v1/nodes/{node_id}` | `getNode` |
| `/api/v1/nodes/{node_id}/sessions` | `listNodeSessions` |
| `/api/v1/nodes/{node_id}/ip-bans` | `listNodeIPBans` |
| `/api/v1/nodes/{node_id}/telemetry` | `listNodeTelemetry` |

Each registration declares `guard("node.read", handler)` on the same root
ServeMux. The parent supplies `requireActionAuth`, reusing authentication,
browser checks, resource resolution, RBAC and authorized request context.
Only a missing explicit action falls back to `routeAction` for legacy routes.
The module obtains the workspace through the existing context accessor,
never from a client header. A missing guard fails at registration.

`nodehttp.Reader` exposes exactly `ListNodesInWorkspace`, `GetNode`,
`ListSessions`, `ListIPBans`, and `HistoryFrom`, with the original signatures.
In particular, `ListSessions` returns `[]telemetryread.Session`, not the
ingestion model `[]telemetry.Session`. Shared telemetry read models, errors
and `value.Timestamp` stay in their existing packages. No database/store,
Transport, concrete Service or parent Server is retained by the module.

`NewBackend` creates the stable module before registering routes.
`EnableTelemetry` is a startup-only adapter that explicitly converts a nil
`*telemetry.Service` to a nil Reader and otherwise injects the same configured
service, preserving its recommendation and release catalog. There is no second
Service field, replacement Service, runtime swapping mechanism or old Server
handler. Application assembly and the HTTP/lifecycle wrapper order are unchanged.

`httpx` owns the single JSON/Problem encoders, page-size parser and optional
UUIDv7 parser. Parent functions are thin compatibility adapters; strict JSON,
idempotency and command trace helpers remain in `api`.

Module tests use a handwritten Reader without a database or Transport.
The real Local-session baseline additionally checks nil/late injection,
Reader exclusion on denial, node-scoped access, counterfactual explicit actions,
adjacent write routes and configured service fields. These run through the
existing four-backend `regression-auth` selection, with required subtests for
explicit actions and configuration. Full-chain cancellation tests ensure node
reads keep the original deadline/cancellation and remain part of Shutdown
draining. Small AST/type checks protect these package boundaries.

PR-03 left other handlers, centralized resource resolution, `routeMethod`, and
the historical ConfigPlan apply omission unchanged. This was not full API
modularization or the PR-04 service-interface work.

## ConfigPlan Apply reachability (F-1)

The existing `POST /api/v1/config-plans/{plan_id}/apply` registration now passes
the segmented method check. Only the exact five-part shape with a nonempty ID
and `apply` action is recognized; UUID validation remains downstream. The root
ServeMux, `requireOperationAuth`, `config.apply`, request format and 72-route
inventory are unchanged. The obsolete `unreachable` exception is removed.

`TestConfigPlanApplyHTTPRoute` first reproduced the old 404, then verifies the
unauthenticated POST's 401/challenge and other methods' Problem 405/Allow.
Path tests preserve current trailing-slash, doubled-slash and escaped-segment
behavior rather than normalizing the entire router. Connection tests disable
redirect following and assert that malformed paths create no Apply intent.

`TestConfigPlanApplyBackendHTTPIntegration` is selected and required by
`regression-auth` on PostgreSQL 17/18, MySQL and MariaDB. It uses real Local
login cookies, trusted Origin, node-scoped roles, and restricted runtime
connections for all services. Owner access is limited to fixture setup and
isolated fault/state injection. Plans are created via HTTP and validated through
`localslice.Ingest` with controlled signed result fixtures. Independent approval
also uses HTTP, including self-approval rejection. The tests cover input and
resource denials, exact conflict Problems, persisted Operation/Plan/approval/
command/Outbox/audit associations, replay, and rollback after approval consumption.

The HTTP regression also exposed an immediate-replay defect: the first Apply
advances the desired revision, so recomputing the next revision changed the
otherwise identical intent hash. Background transport activity can likewise
advance the node version between identical requests. `ApplyInput` now reuses
both the committed revision and the original command's `expected_version`
only for the same Plan and workspace-scoped idempotency key. The required
`node-version-drift-replay` HTTP regression ingests a normal disconnect through
the runtime connection, asserts the version increment, and verifies replay
without another Operation, command, Outbox or success audit. New keys retain
the live node version and existing allocation path. ConfigPlan prechecks,
Operations' complete intent comparison, transactional revision checks and
approval consumption remain in their original layers; neither the global hash
format nor Schema changes.

A 202 means an asynchronous operation intent was committed, not that a real
Rust Agent applied configuration. F-2, broader architecture work and release
readiness are not covered by this fix.

## Explicit business route actions (R2-01)

Base: `d8b9cdc3883896cc22dc1ccb9c47d00c1beecaf2` (#221).
The F-1 wrapper description above records that fix's historical state. R2-01
subsequently replaces `requireOperationAuth(handler)` with
`requireActionAuth("fixed.action", handler)` at these eight registrations:

| Method and path | Action |
| --- | --- |
| `POST /api/v1/nodes/{node_id}/config-plans` | `config.plan` |
| `GET /api/v1/config-plans/{plan_id}` | `config.review` |
| `POST /api/v1/config-plans/{plan_id}/apply` | `config.apply` |
| `GET /api/v1/nodes/{node_id}/users/{username}/policy` | `node.read` |
| `PUT /api/v1/nodes/{node_id}/users/{username}/policy` | `user.manage` |
| `POST /api/v1/user-batches` | `user.manage` |
| `GET /api/v1/user-batches/{batch_id}` | `operation.read` |
| `GET /api/v1/user-operations/metrics` | `operation.read` |

Their seven dedicated `routeAction` branches (including the combined policy
GET/PUT branch) are removed. Shared node reads, Operations, Events, rollouts,
certificates, approvals and dynamic actions still use the existing inference.
`requireOperationAuth`, `authorizeRoute` (including SSE revalidation), and the
unchanged `routeMethod` compatibility layer remain. There are still 72 routes,
including the five explicit `nodehttp` guards.

The unified guard still authenticates, checks browser mutations, resolves the
actual resource/workspace, authorizes the supplied action and writes context.
Plan/approval node ownership and batch workspace selection are unchanged, as
are handler-level per-item checks, batch reader restrictions, Secret usage and
independent Apply approval. No handlers or service dependencies move modules.

The inventory now recognizes only the two explicit wrapper forms and compares
their literal action and original handler against independent expectations.
`TestBusinessRouteActionsBackendHTTPIntegration` reuses the Local-session Apply
fixture and real `NewBackend().http.Handler` for all eight routes. It complements
the existing Apply success/replay/approval tests with denial intent counts,
workspace and per-item boundaries, and counterfactual actions through the real
guard. `regression-auth` requires it on all four backends. The old PostgreSQL-only
batch resource test now supplies `user.manage` explicitly; it is not the HTTP
acceptance evidence. Candidate SHA and executed results belong in the Draft PR.

## ConfigPlan HTTP module (R2-02)

Base: `3266e8fb1ec8248e9f94ed0f0dd0f47a08ac2515` (R2-01, #222).
`api/configplanhttp` now owns Create, Get and Apply, their private request types
and ConfigPlan error mapping. Its `routes.go` registers the same three complete
paths on the root ServeMux with `config.plan`, `config.review`, and `config.apply`.
The independent inventory scans `api`, `nodehttp` and `configplanhttp`: still
72 routes and 13 explicit actions. The other five R2-01 business actions and the
removed inference branches are unchanged.

The module consumes only `Plans.Create/Get/Apply`, authenticated request values
(actor, identity, session, request ID and traceparent), and one Secret-use check.
It retains no concrete Service, database/Store, Transport or parent Server.
`api/configplan_access.go` supplies values using the existing context accessors.
Its Secret adapter resolves the final Certificates/RBAC configuration, checks
each reference's real workspace against the authorized context and applies the
existing `s.devAuth` exception, not an issuer-based exception. No references means
no Certificates dependency; a missing adapter or Certificates service fails with
503 only when a reference is used. Query/ownership/permission failures still deny
with 403 and stop before Create. The domain transaction still validates and
resolves the stored reference's state, version and node workspace.

`NewBackend` creates the stable Handler before registration. Startup-only
`EnableConfigPlans` supplies two views of the same instance: the module's business
methods and the parent's `configPlanLookup.Get/Resource`. Explicit nil clears
both. The parent no longer stores `*configplan.Service`. Plan authorization and
legacy approval fallback use Resource; approval creation still uses the fully
interpreted Get result, its validity/expiry, workspace, candidate/current hashes,
revision and redacted diff. Saved approval authority resources are all checked
before considering any legacy fallback. Approvals and consumption remain in
their original layers.

Strict JSON, UUIDv7 parsing and idempotency-key checks join the existing helpers
in `httpx`, with thin parent adapters and no business/auth imports. Media types,
UTF-8, unknown-field and single-value checks are unchanged, as is the shared body
limit. The historical direct Apply validation test now exercises the full HTTP
chain. Create still checks availability before its ID; Apply checks ID, key,
JSON and approval ID before its business call. Success remains Plan versus
Operation with different Locations and unchanged replay headers.

One narrowly accompanying defect fix covers previously reachable development
requests with no ConfigPlan service: Get and Apply used to dereference nil after
their input checks. They now return the existing service-unavailable Problem at
that point. Ordinary authentication/Origin/resource guards still run first,
including 401 and missing-lookup 404; there is no blanket pre-guard 503.

Handwritten module tests use `ServeMux.ServeHTTP`, not direct Handler dispatch.
Boundary tests prohibit capability recovery and old Server handlers. Full-chain
tests cover all three requests' cancellation, deadline and Shutdown draining.
`regression-auth` additionally requires lookup/approval compatibility, nil/late
injection, Secret denials and a valid signed Plan submission/replay on all four
backends using the existing restricted-runtime Local-login fixture. The older
PostgreSQL response/approval tests remain PostgreSQL-only evidence. R2-01 and
F-1/F-2 regressions remain selected. No Agent execution or release readiness is
implied. The following R2-03 section covers user-policy/batch extraction.

## R2-03: User Policy and Batch HTTP

`api/useroperationshttp` owns these five implementations, their private HTTP
request types and their original Problem mappings. They register directly on
the same root ServeMux with a required, explicit Guard:

| Method and path | Action |
| --- | --- |
| GET `/api/v1/nodes/{node_id}/users/{username}/policy` | `node.read` |
| PUT `/api/v1/nodes/{node_id}/users/{username}/policy` | `user.manage` |
| POST `/api/v1/user-batches` | `user.manage` |
| GET `/api/v1/user-batches/{batch_id}` | `operation.read` |
| GET `/api/v1/user-operations/metrics` | `operation.read` |

The inventory now also scans `useroperationshttp`, retaining 72 registrations
and 13 explicit actions. No route inference, method handling, authentication,
Origin, workspace selection, SSE or shared lifecycle behavior changes.

The consuming module defines two interfaces: `Operations` has only GetPolicy,
SetPolicy, CreateBatch, GetBatch and Metrics; `Authorizer` has only Node and
Authorize. It shares domain values/errors, `auth.Principal` and `rbac.Resource`,
not concrete services, stores, a parent Server or sibling HTTP modules.
`NewBackend` constructs the stable Handler before route registration.
`EnableUserOperations` and `EnableAuthorization` inject the same configured
business/RBAC instances at startup and explicitly convert typed nil. The parent
stores only the module object, not the full UserOperations Service. The app
continues using its original service for scheduling. No parent lookup is needed:
approval HTTP still uses the domain batch types, validation and hash functions.

`useroperations_access.go` obtains Principal and workspace from the authorized
context, plus the original actor, request ID and traceparent. `X-Approval-ID`
remains a client reference parsed by `approvalID()`: missing, malformed and
non-v7 values become nil. It is not a trusted approval decision or a new JSON
field. Binding, saved item order/versions, independent approval and consumption
remain within the existing domain transaction.

Policy writes retain key-before-JSON validation, RFC3339 parsing with a trailing
Z, domain validation of fractional seconds, body `expected_version`, trimmed
reason, 200, revision ETag and replay headers. Replay still reads current policy
state. Batch creation retains 202 and Location. A missing or foreign node rejects
the entire batch; an unauthorized same-workspace item persists as forbidden,
while an authorized missing user persists as failed/not_found. Client-supplied
`authorized` is rejected by strict JSON; items are not sorted or deduplicated.
Development per-item bypass depends on the Principal issuer, not Server devAuth.

Batch reads keep their special lookup/workspace 404 before the additional read
check. Creators skip only that additional workspace-wide check, never the outer
session/permission guard. Other readers need workspace `operation.read`; node
managers do not gain workspace metrics. Missing module capabilities fail closed
after the public guard. A standalone noncreator read without an Authorizer now
returns service-unavailable rather than dereferencing nil; configured requests
retain the original authorization error mapping.

Handwritten module tests exercise real ServeMux dispatch, request values,
context identity, validation order, response headers and missing capabilities.
The full-chain lifecycle tests cover cancellation, deadlines and Shutdown for
all five methods. The shared multi-backend Local-login fixture optionally creates
a private PostgreSQL database for scheduler tests; MySQL/MariaDB already do so.
Normal HTTP and scheduling calls use runtime, not owner. A scoped audit constraint
forces failure after approval consumption and batch/item insertion, proving
transaction rollback before the same HTTP request succeeds.

New approval/rollback, policy, per-item and assembly tests are required in both
`regression-auth` and Full's `backend-policy-api`; PostgreSQL Full now executes
that existing policy HTTP group as well as MySQL/MariaDB. R2-01/R2-02 tests and
F-1/F-2 coverage remain intact. HTTP assertions prove saved intent only. A
separate original Service RunOnce path verifies signed child Command, Outbox,
idempotency and actor/session/request/audit linkage, not real Agent execution.

The three second-round HTTP modules do not make all APIs modular or create a
runtime security boundary. Central resource authorization, other concrete Server
services, shared domain types and existing transactions intentionally remain.

## Single-pass HTTP assembly (S-01)

Base: `533f7b41e0e46ed81068b1bbc6fb8165ec8e0d27` (#224).
The late-injection descriptions above record their respective historical
baselines. S-01 replaces those assembly tests with construction scenarios; it
does not classify all historical startup setters as vulnerabilities.

`api.NewServer(HTTPConfig, backend, build, logger, Modules, Authorization)` is
the single core constructor. The compatibility `NewBackend` delegates with
explicit default SSE values and disabled optional modules/authentication; it
cannot be completed through business setters. Deployment assembly uses
`newHTTPServer`, not that compatibility wrapper.

`runRoles` still creates and configures the shared business instances, Transport
and Owner Fencing and starts only the selected Worker/Scheduler tasks. Non-API
roles return before authentication or HTTP construction. For API roles:

1. Validate the final SSE configuration and construct authentication.
2. Establish parent RBAC, approvals, audit, Certificates and Plan lookup.
3. Construct node, ConfigPlan and UserOperations handlers with final narrow
   interfaces and authenticated request accessors.
4. Create one admission manager and one watcher budget shared by the platform
   and operation hubs, then register the original routes and middleware.
5. Transfer Shutdown ownership to lifecycle immediately, finish the remaining
   non-module startup adapters, and return the complete Server for listening.

`Modules.ConfigPlans` is one combined construction capability, split into
`Plans.Create/Get/Apply` and `configPlanLookup.Get/Resource`; consumers cannot
receive different instances. `Authorization.RBAC` supplies both the parent
guard and the batch module's `Node/Authorize` view. Known concrete typed-nil
Services are normalized at this boundary. Nil means disabled, not a substitute
Service or a constructor error. Request accessors and registration Guards
retain their required-capability contracts. Modules and Server do not retain
the assembly input structures.

The Secret adapter is bound after Certificates, RBAC and devAuth are fixed.
It still queries current resources and permissions on every request, including
the historical `s.devAuth` exception. No SecretRef needs no Certificates service;
missing services return 503, while lookup, workspace and permission failures
retain their denial order. Browser Origin normalization and trusted-proxy slice
copying are unchanged. Telemetry recommendation/catalog, UserOperations
concurrency, scheduler sharing and Operations/ConfigPlan sharing are preserved.

SSE requires an explicit valid `eventstream.Config`; callers choosing defaults
use `DefaultConfig()`. Invalid or partial configuration returns an error and no
Server, never a silent fallback. There is no public SSE reconfiguration or
request-time lazy constructor. Hubs start polling only upon subscription.
Static constructor-site checks complement component identity, nondefault limit,
shared-budget recovery, independent-Server and closed-subscription tests.

All reachable configuration failures occur before allocating HTTP resources.
The SSE constructor still closes any already-created manager/hub on an error.
Successful construction transfers ownership before listening, so later startup
failures and normal shutdown use the existing lifecycle. HTTP owns only its
listener, tracked handlers and SSE, not the shared database or domain services.
Closed objects remain installed; Shutdown still drains inner TimeoutHandler
work and closes connections on deadline. Synchronization and SSE admission,
backoff, cursor, revalidation and response-header order remain unchanged.

The ordinary Go group covers construction, typed nil, copied inputs, boundaries,
SSE and failure ownership. The existing four-backend Controller startup group
now includes `HTTP-assembly`, `SSE-configuration` and
`authentication-configuration`. The production `newHTTPServer` test uses a real
Local login and restricted runtime, checks all three modules on their first
requests, nondefault SSE admission/Retry-After, live role revocation and stream
revalidation. It stays inside the isolated startup database. Existing HTTP
fixtures construct separate Servers over their persisted identities/business
objects instead of swapping dependencies, retaining F-1/F-2, approval, rollback,
replay, scheduling and cancellation assertions.

The inventory remains 72 routes and 13 explicit actions. Other startup adapters
and LocalSlice runtime state remain intentional. S-02 method rules, new handler
extraction, domain refactors, generated contracts and release readiness are not
part of this change. Executed SHA-bound evidence belongs in the Draft PR.
