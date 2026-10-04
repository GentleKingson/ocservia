# Web boundaries and node workflows

## API boundaries

The handwritten Web API layer uses the generated local `@ocservia/api-client`
package. Domain modules import one `api/transport.ts` configuration; they do not
instantiate authentication state, select a Workspace, navigate to a view, or
decide operation terminal states.

### Ownership

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

### Static import boundaries

The existing [`eslint.config.ts`](../../web/eslint.config.ts) uses ESLint's core
`no-restricted-imports` rule for handwritten `src/api/**/*.ts` and the
configuration/certificate feature directories. These modules must not directly
import views, `vue-router`, `shared/router` or the concrete `shared/fleet` and
`shared/localSlice` stores. API modules must not import feature workflows either.
Relative imports at different depths, `@/` alias imports, static re-exports and
type-only imports are subject to the same restrictions. The `@/` alias maps to
`web/src` in both `tsconfig.json` and `vite.config.ts` (Vitest reuses the Vite
configuration); keep the two definitions identical.

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

### Export And Caller Inventory

Paths are relative to `web/src`. Remove an entry point when its last consumer
disappears; do not retain unused forwarding exports.

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

### Verification

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
It is not full E2E or database validation.

## UI components and styles

The console is migrating to [shadcn-vue](https://www.shadcn-vue.com) primitives
built on [Reka UI](https://reka-ui.com) and Tailwind CSS v4. Legacy pages keep
their existing markup until they are migrated one consumer at a time.

### Cascade

`web/src/main.css` is the only CSS entry and owns the layer order
`theme, legacy, ui-base, utilities`:

- `styles.css` is imported into the `legacy` layer, so utilities win over
  legacy rules by layer order rather than specificity or `!important`.
  Unlayered Vue scoped styles (`LoginView`, `ApprovalsView`) still win over
  every layer.
- Tailwind's global Preflight is not imported. `ui-base` applies the reset the
  primitives need only to elements carrying the `data-slot` marker, which also
  covers content a primitive teleports to `<body>`. Do not render primitives
  inside legacy containers whose descendant selectors would still style them.
- Utilities are generated only from `web/src/components` (`source(none)` plus
  `@source`), so legacy class names never turn into utilities. Before adding
  another `@source` path, check its class names against the generated
  utilities.
- Theme tokens are plain custom properties on `:root`, mapped through
  `@theme inline`. The `dark` variant only matches an explicit `.dark` class;
  dark mode is not a product capability and must not follow the OS setting.

Tailwind v4 output targets Chrome 111, Safari 16.4 and Firefox 128. Production
pages do not consume primitives yet; decide the supported browser range before
the first production consumer.

### Component sources

`web/components.json` records the shadcn-vue CLI settings. Primitives are
project-owned source in `web/src/components/ui`; a CLI run is not an upgrade.
Compare upstream changes by hand (keyboard behavior, ARIA, Portal and props)
and keep the local modifications below. When the CLI generates icon imports
from `lucide-vue-next`, rewrite them to the existing `@lucide/vue` package.

| Component | Upstream | Local modifications |
| --- | --- | --- |
| `button` | `apps/v4/registry/new-york-v4/ui/button` | Prettier; `asChild` defaults to `false` for `exactOptionalPropertyTypes` |
| `badge` | `apps/v4/registry/new-york-v4/ui/badge` | Prettier; renders `span` by default; binds `as`/`asChild` instead of `reactiveOmit` from `@vueuse/core` |
| `input` | `apps/v4/registry/new-york-v4/ui/input` | Prettier; `defineModel` replaces `useVModel` from `@vueuse/core`; `defaultValue` prop removed |
| `table` | `apps/v4/registry/new-york-v4/ui/table` | Prettier; `TableEmpty` and `TableFooter` not imported |
| `lib/utils.ts` | `apps/v4/registry/new-york-v4/lib/utils.ts` | Prettier |

All sources were taken on 2026-10-04 from `unovue/shadcn-vue` commit
`b251d9fd92aa496495e127137a7734704fb34a29` (CLI 2.8.2) and are MIT licensed;
the notice is kept in `web/src/components/ui/LICENSE.shadcn-vue`. Runtime
dependencies are pinned in `web/package.json`: `reka-ui`,
`class-variance-authority`, `clsx` and `tailwind-merge`; build-time
`tailwindcss` and `@tailwindcss/vite`. Primitives do not call APIs, read the
Workspace or decide permissions.

The development-only `/dev` route renders `components/dev/UiPreview.vue` to
check primitives next to legacy styles.

## Node detail workflows

NodeDetail composes configuration and certificate workflows during setup.
User-policy mapping lives in `adapters/user-policy.ts`; other desired-state
and controlled-action handlers remain in the page.

### Boundaries

Paths below are relative to `web/src`.

| Owner | Responsibilities |
| --- | --- |
| `views/NodeDetailView.vue` | Route ID, Fleet selection, authorized-read readiness, Workspace listener, closing dialogs on navigation, and template composition |
| `features/configuration/useNodeConfiguration.ts` | Configuration form, captured revision, Plan/Apply requests, Plan polling, receipt recovery, errors/loading and dialog cancellation |
| `features/certificates/useNodeCertificates.ts` | Certificate form, CSR polling, issue/P12/download/revoke requests, receipt/grant recovery, errors/loading and dialog cancellation |
| `features/node-workflow.ts` | Existing shared context fence, cancellable wait, pending-mutation tickets, identifier receipts and expiring in-memory grants |
| `shared/fleet.ts` | Shared operation tracking and telemetry; feature disposal does not stop Fleet tracking |
| `api/workspace.ts` | Sole Workspace authority; features receive its context getter, not a second Workspace store |

Each feature receives only a readonly node ref, readonly successful-read flag,
Workspace context getter, operation-tracking callback and translator. Neither
imports the router, Fleet, a view or the other feature. The page constructs each
feature once during setup, passes the route-matched authorized node and computes
readiness from detail loading, Fleet selection and selection error.

### Preserved Behavior

- ConfigPlan captures only a known, nonnegative JavaScript-safe configuration
  revision. It does not use node version or rebase/retry a stale revision.
- Closing a dialog synchronously cancels reads and polling and clears its
  transient state. The feature's scope disposal also performs this cleanup;
  route and Workspace changes still close dialogs in the page.
- Mutations are not aborted or automatically resent on teardown. Late accepted
  IDs remain recoverable through the existing receipt/ticket owner. Reopening
  waits for acknowledgement and re-reads authorized server state.
- ConfigPlan terminal states and the certificate `csr_pending` polling rule
  remain different. Both keep their existing 30 attempts and 500 ms delay.
- Apply closes its dialog before handing off operation tracking. P12/revoke
  hand off tracking without closing the certificate dialog. Disposing a
  feature only detaches its own reads, not accepted server work.
- Artifact credentials remain memory-only, scoped to Workspace/node/certificate
  and bounded by expiry. An already requested one-time download still completes
  after dialog closure. No credential is added to persistent receipts; a full
  browser refresh, page termination or another tab cannot recover these secrets.
- A certificate receipt's operation is restored only when its resource ID matches
  the selected certificate. Falling back to another certificate, or finding no
  active certificate, does not fetch or display the old receipt's operation.

### Verification

Run validation in the authorized BuildServer checkout. Existing entry points:

```sh
cd web
npm ci
npm run typecheck
npm test
npm run lint
npm run format:check
npm run build
```

`configuration-feature.test.ts` and `certificates-feature.test.ts` exercise the
features in independent Vue effect scopes without a page, router or store.
The existing `node-config-plan.test.ts` and `node-workflows.test.ts` continue to
mount the real NodeDetail setup for PR-01/02 integration regressions.

Serve the production build on an available BuildServer port, set
`PLAYWRIGHT_BASE_URL`, then run the existing focused browser smoke:

```sh
npx playwright test config-plan.spec.ts certificate-lifecycle.spec.ts --project=desktop --project=mobile
```

This checks Plan/Apply and certificate/P12 UI interactions, not a live
Controller, database recovery or installation.

### Advisory action availability

The authorized node detail read includes `effective_actions`. Telemetry derives
node trust and approved capabilities through the existing Operations Store;
Node HTTP asks its parent-supplied callback to apply the same resource-scoped
RBAC checks as writes. The callback carries the original request context and
adds no authorization store or permission engine to the module. Reads fail
closed on lookup errors, and the caller-specific detail response is not cached.
List reads do not perform these additional lookups.

NodeDetail disables the relevant actions and explains missing capability,
trust or role permissions before a form is filled. Certificate browsing uses
its separate read permission. This remains advisory: writes still enforce
capabilities, role checks, sealing keys, versions, approvals, secret references
and command-specific constraints. Configuration forms identify their template
source and captured revision; generated results identify the full redacted
candidate without inventing current field values. Existing desired/observed
fields distinguish unmanaged resources and missing observations in the display,
without changing convergence records or taking over existing accounts.
