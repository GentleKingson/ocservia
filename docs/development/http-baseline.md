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
otherwise identical intent hash. `ApplyInput` now reuses the committed revision
only for the same Plan and workspace-scoped idempotency key. New keys retain
the existing allocation path. ConfigPlan prechecks, Operations' complete intent
comparison, transactional revision checks and approval consumption remain in
their original layers; neither the global hash format nor Schema changes.

A 202 means an asynchronous operation intent was committed, not that a real
Rust Agent applied configuration. F-2, broader architecture work and release
readiness are not covered by this fix.
