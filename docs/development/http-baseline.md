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

Baseline observations (not fixed here):

- ConfigPlan apply is registered, but `routeMethod` rejects its path with 404
  before authentication for every method. The inventory locks this behavior;
  supplemental direct-handler tests cover its idempotency/body error priority.
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

This keeps `api`, concrete service fields, `Server`, `NewBackend`, `EnableXXX`,
`routeMethod`, `routeAction` and resource authorization. Node/telemetry handler
extraction and fixing the apply-route omission remain separate follow-ups.
Final candidate SHA and command results belong in the Draft PR evidence.
