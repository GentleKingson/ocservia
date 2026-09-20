# Web API boundaries

The handwritten Web API layer uses the generated local `@ocservia/api-client`
package. Domain modules import one `api/transport.ts` configuration; they do not
instantiate authentication state, select a Workspace, navigate to a view, or
decide operation terminal states.

## Ownership

- `api/transport.ts` owns the authenticated fetch, `/api/v1` base path,
  `same-origin` credentials, development-only bearer token, optional request
  signal and idempotency key generation. It forwards 401 responses to the
  session coordinator, then returns the original response. It does not retry.
- `shared/session.ts` owns the redirect guard and login return path. The
  transport requests login navigation; `App.vue` consumes the saved path after
  successful Workspace discovery. Neither imports a view. The existing
  `shared/login.ts` keeps path validation and OIDC attempt helpers.
- `api/workspace.ts` is the sole owner of selected/authorized Workspaces,
  in-flight discovery and generation. Generated API instances are stateless
  clients sharing the transport configuration, not additional state owners.
- `api/platform.ts` owns the independent, in-flight-only authentication probe.
  Probing `/workspaces` must not update the Workspace cache or selection.
- Domain modules adapt parameters, scopes, pagination and mutation policies.
  Callers import their actual owner directly; there is no `api/client.ts`
  compatibility barrel or temporary re-export.
- Fleet and local-slice stores still own their EventSource, cancellation,
  listeners and timers. Moving imports adds no module-load subscriptions and
  does not unify their intentionally different terminal-state definitions.

## Static import boundaries

The existing [`eslint.config.ts`](../../web/eslint.config.ts) uses ESLint's core
`no-restricted-imports` rule for handwritten `src/api/**/*.ts` and the
configuration/certificate feature directories. These modules must not directly
import views, `vue-router`, `shared/router` or the concrete `shared/fleet` and
`shared/localSlice` stores. API modules must not import feature workflows either.
Relative imports at different depths, static re-exports and type-only imports
are subject to the same restrictions. There are currently no source aliases in
the Web TypeScript/Vite configuration.

Generated clients/types, domain APIs and Workspace remain legitimate
dependencies. In particular, `api/transport.ts` may call `shared/session.ts`;
this is not a ban on `shared/**`. Features may use Vue and `features/node-workflow.ts`
while receiving context getters and tracking callbacks from their caller.

Run `npm run lint` from `web` on BuildServer; its existing prelint builds the
generated client. For a configuration-only edit, also run
`npx --no-install prettier --check eslint.config.ts`. Other changes should select
checks from [Validate a change](testing.md), rather than running every command
below by default.

This is a static import/re-export check, not dynamic-import or constructed-string
analysis, a transitive dependency graph, runtime state-ownership enforcement or
a complete security proof. Generated sources retain their existing lint ignore;
tests/fixtures outside these source directories and other features receive no
new restriction. Vue SFC parsing is unchanged, so this does not claim equivalent
coverage for every `.vue` file.

## Export And Caller Inventory

Paths in this table are relative to `web/src`. It records every former
`api/client.ts` export and its production callers after PR-06/07. Existing wrappers
are retained because these callers use them; they are domain entry points, not
compatibility aliases. Remove an entry point if its last consumer disappears,
rather than retaining an unused forwarding export.

| Owner | Exports | Production callers | Preserved semantics |
| --- | --- | --- | --- |
| `shared/session.ts` | `consumeLoginReturnPath` | `App.vue` | Consume once; clear OIDC attempt; validate internal return path |
| `api/workspace.ts` | `listAuthorizedWorkspaces` | `App.vue`, `shared/localSlice.ts` | Cache and coalesce discovery; explicit refresh; remembered selection |
| `api/workspace.ts` | `getWorkspace` | `App.vue`, `shared/{fleet,localSlice,overview}.ts`, `views/{OperationsView,SettingsView}.vue` | Use selected Workspace, otherwise discover; fail on empty authorization |
| `api/workspace.ts` | `selectWorkspace` | `App.vue` | Authorized selection only; persist and notify only on changed ID |
| `api/workspace.ts` | `workspaceContext`, `WorkspaceContext` | `shared/{fleet,localSlice,overview}.ts`, `views/{NodeDetailView,OperationsView}.vue`; type-only in `features/node-workflow.ts` and configuration/certificate features | Snapshot of ID and generation; features receive the page's getter |
| `api/workspace.ts` | `workspaceChangedEvent` | `shared/{fleet,localSlice,overview}.ts`, `views/{NodeDetailView,OperationsView,SettingsView}.vue` | Existing event name and ID detail |
| `api/platform.ts` | `getReadiness`, `getVersion` | `shared/readiness.ts`, `views/SettingsView.vue`, respectively | Same generated requests through shared transport |
| `api/platform.ts` | `probeAuthentication` | `shared/{fleet,localSlice}.ts` | Coalesce only concurrent probes; keep Workspace state independent |
| `api/events.ts` | `eventStreamPath` | `shared/{fleet,localSlice}.ts` | Encode `after` and `workspace_id`; existing development-token fallback; no token in URL |
| `api/events.ts` | `listEvents` | `shared/{localSlice,overview}.ts` | Workspace header, page size 200, optional cursor/order/signal |
| `api/events.ts` | `platformEventsEvent` | `shared/{fleet,overview}.ts` | Existing event name; stores retain dispatch/subscription ownership |
| `api/nodes.ts` | `listNodes`, `getNode`, `listNodeSessions`, `listNodeIpBans`, `listNodeUserGroupState` | `shared/fleet.ts` | Workspace-scoped list; node-specific reads; pagination and signals |
| `api/operations.ts` | `listOperations` | `shared/{localSlice,overview}.ts`, `views/OperationsView.vue` | Workspace header, page size 200, optional cursor/signal |
| `api/operations.ts` | `operationSummary` | `shared/overview.ts` | Workspace header and signal |
| `api/operations.ts` | `getOperation` | `shared/{fleet,localSlice}.ts`, `views/OperationsView.vue`, configuration/certificate features | Operation ID and signal; no terminal-state interpretation |
| `api/operations.ts` | `createLocalSimulation` | `shared/localSlice.ts` | Existing development endpoint, scenario and signal |
| `api/operations.ts` | `disconnectSession`, `terminateSession`, `removeIpBan`, `reloadService` | `shared/fleet.ts` | Revision If-Match, unique idempotency key, 60-second TTL; session boot binding; reload approval header |
| `api/users.ts` | `createUser`, `disableUser`, `enableUser`, `rotateUserPassword`, `applyGroup` | `shared/fleet.ts` | Revision If-Match, unique idempotency key, 86400-second TTL; sealed-password envelope; member deduplication |
| `api/users.ts` | `getUserPolicy`, `setUserPolicy` | `adapters/user-policy.ts` | Node/username and signals; mutation idempotency; no added If-Match |
| `api/configuration.ts` | `createConfigPlan`, `getConfigPlan`, `applyConfigPlan` | `features/configuration/useNodeConfiguration.ts` | Request revision/approval unchanged; idempotency on create/apply; signals |
| `api/certificates.ts` | `createCertificate`, `getCertificate`, `listNodeCertificates`, `issueCertificate`, `createCertificateP12`, `revokeCertificate` | `features/certificates/useNodeCertificates.ts` | List item extraction; signals; idempotency on create/P12/revoke, not issue |
| `api/certificates.ts` | `downloadCertificateArtifact` | `features/certificates/useNodeCertificates.ts` | Encoded artifact ID; grant token plus optional development bearer; same-origin credentials; no Workspace header; Blob/error handling |
| `api/agents.ts` | `upgradeNodeAgent` | `shared/fleet.ts` | Trusted target version only; revision If-Match, idempotency, approval in body |
| `api/agents.ts` | `createAgentRollout` | `views/NodesView.vue` | Workspace header, idempotency and unchanged target/node/batch/approval body |
| `api/agents.ts` | `listAgentRollouts` | `views/OperationsView.vue` | Workspace header and optional limit/signal |
| `api/agents.ts` | `getAgentRollout`, `resumeAgentRollout` | `views/RolloutDetailView.vue` | Workspace header and signal; resume idempotency |

`workspaceID` is now an internal cross-module helper exported by the Workspace
owner. `configuration`, `authenticatedFetch`, `devAuthToken`, `requestInit` and
`newIdempotencyKey` are transport exports used by the domain modules above.
`redirectToLogin` is the session coordinator entry point used by transport.
None is a new public HTTP contract.

## Verification

Run in the authorized BuildServer checkout, from `web`:

```sh
npm ci
npm run typecheck
npm run test:generated-auth
npm test
npm run lint
npm run format:check
npm run build
node test/run-auth-browser.mjs
```

`test/api-client.test.ts` exercises real handwritten modules and generated
serialization against a stub fetch, including 401 coordination, Workspace
authority, headers, signals, mutation fences and artifact downloads. The
existing browser runner covers 12 focused login/Workspace/SSE regressions
against the production build, including late responses and rapid switches.
It is not a full E2E or database validation. Generated sources and HTTP schemas
are unchanged by this responsibility split.
