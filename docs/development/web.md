# Web boundaries and node workflows

## Startup, routing and development server

- [`main.ts`](../../web/src/main.ts) registers Pinia, the router, and i18n, then waits for `router.isReady()` before mounting so `/login` never flashes the application shell.
- [`shared/routes.ts`](../../web/src/shared/routes.ts) defines the console route table. The `/dev` simulator route is registered only in development builds and is omitted from production navigation. The default Overview landing page ships in the entry bundle, while all other views load lazily on first visit. If a lazy route chunk fails to load (for example after a deployment removes old assets or during a network drop), the router keeps the current view and any unsaved form input on screen and prompts the user to reload manually instead of refreshing the page on its own. When the very first navigation hits a chunk load error, the app mounts a minimal reload prompt without rendering the shell or firing API requests. This recovery path handles only Vite `vite:preloadError` events; other router errors are logged to the console. A manual reload always fetches the current build's entry document, because Gateway serves HTML responses with `Cache-Control: no-cache`.
- [`App.vue`](../../web/src/App.vue) coordinates initial Workspace discovery, periodic readiness refreshes, and the one-time post-login redirect described below.
- Running `npm run dev` (which powers the `web` service in the [local stack](../getting-started/local-development.md) on port `4173`) proxies `/api` requests to `VITE_API_TARGET` and injects `VITE_DEV_AUTH_TOKEN` as a bearer header whenever that variable is set. Invoking `npm run dev` directly on the host binds `127.0.0.1` only. The development container passes `--host 0.0.0.0`, and Compose publishes the port on host loopback. [`vite.config.ts`](../../web/vite.config.ts) aborts `vite build` if `VITE_DEV_AUTH_TOKEN` is present in the environment. In production, Gateway serves the static bundle, and its [Caddyfile](../../deploy/production/Caddyfile) proxies `/api/*` directly without using the Vite development server.
- Install the exact Node and npm versions pinned in [`toolchains.lock`](../../toolchains.lock) and run `npm ci` inside `web/`. Verification commands are listed under [Validation](#validation).

## API boundaries

Handwritten modules under `web/src/api` wrap the generated local `@ocservia/api-client` package. Every domain API module imports the shared `api/transport.ts` configuration and stays free of UI state: API modules never instantiate authentication stores, choose a Workspace, trigger router navigation, or classify terminal operation states.

### Ownership

- `api/transport.ts` configures the authenticated fetch wrapper, the `/api/v1` base URL, `same-origin` credentials, the development-only bearer header, optional request signal propagation, and idempotency key generation. When a call returns 401, the transport notifies the session coordinator and returns the original response to the caller without retrying.
- `shared/session.ts` manages the unauthenticated redirect guard and the saved post-login return path. The transport layer asks the coordinator to navigate to login, and `App.vue` consumes the stored return path once Workspace discovery succeeds. Neither module imports Vue route views, while `shared/login.ts` retains internal path validation and OIDC login attempt helpers.
- `api/workspace.ts` is the single source of truth for the active and authorized Workspace list, coalesced in-flight discovery, and the Workspace generation counter. Generated OpenAPI client instances are lightweight stateless wrappers around the shared transport configuration and hold no state of their own.
- `api/platform.ts` provides an independent, in-flight-only authentication probe. Probing `/workspaces` through this helper checks session validity without mutating the cached Workspace list or current selection.
- Domain API modules translate request parameters, Workspace scopes, pagination cursors, and idempotency rules for their specific endpoints. Components and features import the owning domain module directly, with no `api/client.ts` barrel file or compatibility re-exports.
- The Fleet and local-slice Pinia stores manage their own EventSource connections, abort controllers, event listeners, and polling timers. Importing an API module never starts a background subscription, and the two stores intentionally keep their separate terminal-state rules.

### Static import boundaries

[`eslint.config.ts`](../../web/eslint.config.ts) enforces static import and re-export boundaries across handwritten API modules and the configuration and certificate feature composables. Neither layer may import view components, the router, or the concrete Fleet and local-slice stores, and API modules are further barred from importing feature composables. Generated client files are excluded from these lint rules. ESLint inspects only static module specifiers and does not police dynamic imports, runtime references, or arbitrary Vue SFCs outside those directories.

The same ESLint configuration checks `src/**/*.vue` against eslint-plugin-vue's essential rule set (catching invalid directives, prop mutations, combined `v-if` with `v-for` usage, and missing keys) and parses `<script lang="ts">` blocks with the typed TypeScript parser. Single-word component names are permitted only for the shadcn-vue primitives under `components/ui`.

Keep the `@/` path alias mapped to `web/src` identically in both TypeScript and Vite configs. Feature composables receive their Workspace context and operation-tracking callbacks as arguments from the hosting page; they may import Vue reactivity APIs, generated client types, domain API modules, and `features/node-workflow.ts`. The transport module is allowed to call the session coordinator. Delete unused forwarding exports as soon as their last caller is removed, and consult the [API source directory](../../web/src/api) for the current export surface.

<a id="export-and-caller-inventory"></a>
### Request and Workspace invariants

Domain API adapters attach Workspace headers, pagination parameters, abort signals, `If-Match` revisions, approval IDs, command TTLs, and endpoint-specific idempotency keys as required by each route. Never add automatic mutation retries in the client or assume every endpoint accepts the same headers: certificate artifact downloads authenticate with their one-time grant token and same-origin session cookie without a Workspace header, and SSE stream URLs must never carry bearer tokens in query parameters.

Workspace discovery coalesces concurrent requests and caches the result. Selecting a Workspace accepts only authorized IDs and increments the generation counter on every change. Asynchronous callers must fence their responses against both the active Workspace ID and the generation counter so a switch away from and back to the same Workspace still invalidates earlier in-flight reads. Running the independent authentication probe must never alter that Workspace state. Post-login return paths are validated as internal console routes and consumed at most once. Both the remembered Workspace ID and the login return path are stored in `sessionStorage` on a best-effort basis: if storage is unavailable or throws, the console proceeds without a saved preference and never blocks login, Workspace selection, or change notifications. SSE stores start and stop their own subscriptions, timers, and abort controllers explicitly.

<a id="verification"></a>
Verification commands for these boundaries are listed in [Validation](#validation).

## UI components and styles

The console UI is built with [shadcn-vue](https://www.shadcn-vue.com) primitives on top of [Reka UI](https://reka-ui.com) and Tailwind CSS v4. All views compose these primitives and Tailwind utility classes directly without a secondary stylesheet or parallel CSS class hierarchy.

### Cascade

`web/src/main.css` is the sole stylesheet entry point and declares the explicit CSS cascade layer order `theme, base, ui-base, utilities`:

- `base` contains only global document defaults: the font stack, page foreground and background colors bound to theme tokens, `box-sizing`, `body` margin and minimum width, and links inheriting text color. Add rules to `base` only when every screen requires them; component-specific styling belongs in utility classes on the template that renders the element.
- Tailwind's global Preflight reset is intentionally omitted. Instead, `ui-base` scopes the minimal element reset required by primitives to nodes carrying a `data-slot` attribute (and ensures form controls inherit font and text color), which also covers popovers and dialogs teleported to `<body>`. Always use the `components/ui` form primitives in place of raw HTML form tags, which would otherwise retain unstyled browser defaults.
- Utility classes override `base` and `ui-base` through cascade layer precedence without needing `!important`. Vue `<style scoped>` blocks are not used in any component, because unlayered CSS beats every named layer.
- Tailwind scans `web/src` statically for utility class names, so keep complete class strings literal in source files instead of assembling class names dynamically at runtime.

### Tokens

Design tokens are defined as plain CSS custom properties on `:root` in `main.css` and exposed to Tailwind color and border-radius utilities via `@theme inline`: `background`, `foreground`, `card`, `popover`, `primary`, `secondary`, `muted`, `accent` (each paired with a `-foreground` token), `destructive`, `success`, `border`, `input`, `ring`, `sidebar` (with `-foreground`, `-accent`, `-accent-foreground`, and `-border`), and `--radius` (backing `rounded-sm` through `rounded-xl`). The production palette is Apple-inspired Light: a pale gray page and sidebar, white cards, graphite text, and blue interactions, with muted text, form inputs, and focus rings chosen to meet contrast targets. Adjust colors by editing the token definitions in `main.css` instead of overriding individual utility classes. Warning badges and callouts use Tailwind's built-in `amber` scale directly. The `dark` variant is bound to an explicit `.dark` selector; dark mode is not a supported product feature and must never activate automatically from the OS setting.

### Supported browsers

The Web console supports Chrome 111+, Safari 16.4+, Firefox 128+, and Chromium-based browsers built on Chromium 111 or newer (such as Edge 111+). That floor matches Tailwind CSS v4's native CSS requirements (cascade layers, `@property`, and `color-mix()`), and `vite.config.ts` configures the same `build.target`. Update both settings together, and do not lower the browser floor without a reviewed compatibility plan.

This browser list defines the support policy and is distinct from the automated CI matrix. Basic CI's `web` job runs `scripts/web-check.sh basic` without launching a browser. The path-routed `web-browser` workflow ([GitHub Actions](github-actions.md#path-routing)) runs the Chromium `desktop` and `mobile` projects plus the `firefox` and `webkit` key-spec projects in both Quick and Full modes (see [Validation](#validation)). The `mobile` project emulates a mobile viewport inside Chromium. Record manual verification results whenever a UI change relies on browser-specific rendering or behavior, because Playwright's bundled Firefox and WebKit engines do not match the minimum supported Safari 16.4 or Firefox 128 releases.

### Component sources

`web/components.json` holds the configuration for the shadcn-vue CLI. The files under `web/src/components/ui` are maintained as project-owned source code, and a CLI run is not an upgrade. When pulling upstream changes from `apps/v4/registry/new-york-v4/ui/<component>` or `apps/v4/registry/new-york-v4/lib/utils.ts`, review diffs manually for keyboard handling, ARIA attributes, Portal mounting, and prop types, rewrite any generated `lucide-vue-next` imports to `@lucide/vue`, and preserve the local adaptations below:

| Area | Local behavior to preserve |
| --- | --- |
| Button, Badge, Label, Sheet, Dialog | Explicit `as`/`asChild` defaults for `exactOptionalPropertyTypes`; Badge defaults to `span`, Label to `label` with `for` falling through |
| Input and Textarea | `defineModel` replaces `useVModel`; no `defaultValue` prop |
| NativeSelect | `defineModel`, attributes fall through to `select`; plain `option` children |
| Sheet and Dialog | No `tw-animate-css` dependency; instant open/close, translated `closeLabel`, 32px close targets; Sheet close carries `data-slot` |
| Dialog | Content scrolls within `max-h-[calc(100dvh-2rem)]`; no footer-generated close button |
| DropdownMenu | No animation utilities; Trigger defaults `as` to `button`; explicit `as`/`dir`/`modelValue` defaults, Content limited to `align`/`side`/`sideOffset` and items without `textValue` for `exactOptionalPropertyTypes`; no Sub or Radio parts |

The baseline primitives were vendor-copied on 2026-10-04 from `unovue/shadcn-vue` commit `b251d9fd92aa496495e127137a7734704fb34a29` (CLI 2.8.2) under the MIT license, preserved at `web/src/components/ui/LICENSE.shadcn-vue`. Runtime dependencies are pinned in `web/package.json` (`reka-ui`, `@vueuse/core`, `class-variance-authority`, `clsx`, and `tailwind-merge`), paired with build-time `tailwindcss` and `@tailwindcss/vite`. UI primitives remain purely presentational and never call APIs, read Workspace state, or evaluate permissions.

### Shell and page components

The application shell adapts the shadcn-vue `dashboard-01` inset layout without pulling in the upstream `sidebar` primitive family: the navigation rail uses the `sidebar` theme tokens, the active view renders inside an inset rounded panel, and the top header displays the section title resolved from `components/layout/navigation.ts`.

`App.vue` manages readiness polling, Workspace selection, and post-login return navigation. Layout and shared components render data passed down by their parent view, while views render a plain `<main>` element inside the shell's width and padding container. Keep the keyboard skip link, `aria-current` nav markers, titled mobile navigation Sheet, post-navigation focus management, and `status`/`alert` live regions intact.

Node mutation dialogs wrap a single form inside `OperationDialog`, while the parent page or feature owns form fields, pending flags, and submit handlers. Maintain focus trapping and restoration, Escape and Cancel dismissal, and localized labels wired to help text through `aria-describedby`. Disable form submission while a request is in flight. If a request fails, keep ordinary field values intact while clearing any password inputs held by secret-owning forms. Render disabled-action explanations visibly in the UI as well as in `title` attributes, and keep in mind that client-side approval inputs never substitute for server-side authorization. Quota help text must explain UTC calendar-month resets, traffic direction, zero disabling the user, and unlimited quota, and expiration inputs must state that timestamps are evaluated in UTC.

<a id="nodes-list"></a>
<a id="node-detail-read-areas"></a>
<a id="node-detail-write-forms"></a>
### Read models and list state

- On the Nodes view, filtering, search, sorting, and optional column toggles operate over the complete Fleet snapshot, which swaps in only after all pages finish loading. If a background refresh fails, the existing table stays visible and is badged as stale. [`node-list.ts`](../../web/src/features/nodes/node-list.ts) owns the filter, sort, and URL query synchronization logic, preserving explicit unknown-observation states and supporting infinity or extended-year timestamps. Visually, the table follows the Tailscale Machines pattern with a search box, a single flat Filters menu (avoiding nested submenus that behave poorly on touch devices), removable filter chips, and a per-row action menu.
- Fleet rollout target selection tracks node IDs across filter changes: Select All toggles currently visible eligible rows, the confirmation step lists every selected node explicitly, selections hidden by active filters remain visible as a badge count, and switching Workspaces clears the selection. Node list rows do not fetch or display per-action write availability; advisory action permissions are loaded only on the node detail view.
- Navigating from the Nodes list to a node detail page preserves the list's query string so the Back link restores the operator's filters. Detail views distinguish missing Agent observations from unavailable or 404 node states, and presentation components never trigger their own data fetches.
- Overview cards identify their backing API source and distinguish ready, stale, unavailable, and loading states. Session totals reflect the last count reported by each node, and direct-path indicators include offline nodes. Active and `unknown` operation counts span the entire Workspace. The recent failed count covers only the latest 20 operations. Do not present these point-in-time counters as historical time series.

<a id="operations-and-rollouts"></a>
<a id="approvals-and-audit"></a>
<a id="overview"></a>
### Operations, approvals and audit

Background snapshot refreshes run only while the browser tab is visible and focused. When the document loses focus or becomes hidden, Fleet closes its EventSource stream and aborts in-flight snapshot reads. When the user returns, it immediately rebuilds state from REST and reconnects the stream. While the event stream is healthy, Fleet and Overview run a supplemental full refresh 60 seconds after the previous read finishes, dropping to a 15-second retry interval after a stream or read error. Readiness polling follows the same 60-second healthy and 15-second failure cadence. An active rollout detail view polls every 2 seconds. Any successful read clears the stale indicator.

All periodic and event-driven refreshes go through the shared `foreground-refresh.ts` scheduler, which collapses event notifications and timer ticks into a single timer per owner, prevents overlapping reads, and coalesces events that arrive during an in-flight read into one follow-up fetch. Navigating away from a Fleet view stops its snapshot timer and clears the active detail selection, and navigating back triggers an immediate refresh. Tracking for already-accepted node operations runs independently and continues across focus changes or route transitions.

For a standalone node operation, an `unknown` state renders as a warning while Fleet continues polling for convergence. For an Agent upgrade or rollout node item, `unknown` is treated as a terminal failure. Visual tone helpers in [`state-tone.ts`](../../web/src/features/operations/state-tone.ts) never control polling behavior (which lives in [`fleet.ts`](../../web/src/shared/fleet.ts)). Rollout summary counts separate succeeded, failed (including rolled-back), and `unknown` nodes, and leaving the rollout detail page stops its 2-second poller. Rollout detail reads, resume calls, and creation responses are fenced by the route's rollout ID, the active Workspace ID, and the Workspace generation counter. Changing the route or Workspace clears the displayed rollout and hides the resume button before fetching fresh data. Starting a resume drops any in-flight detail read. Neither resume nor rollout creation ever resends automatically if the HTTP response is lost. Closing the rollout creation dialog cancels automatic navigation to the new rollout without aborting the already-submitted POST request.

The Approvals queue paginates with opaque cursors and clears stale rows if a refresh fails, while deep links, ID lookups, and queue rows all resolve to the same detail route. Granting an approval authorizes a future mutation; the approval's own status does not report whether that downstream operation succeeded. Expired approval requests hide the decision form. Submitting a decision requires both a reason string and the review confirmation checkbox, and any lost or rejected decision (including a 409 conflict) clears the detail view and forces a fresh read without resending. On the Audit view, client-side search and filtering operate only over the latest 50 records loaded in memory instead of querying the server across the full audit log.

On narrow viewports, wrap wide data tables in horizontal scroll containers while keeping expanded row details constrained to the visible viewport width.

<a id="ui-regression-checklist"></a>
UI regression test requirements are covered in [Validation](#validation).

## Node detail workflows

NodeDetail wires up the configuration and certificate feature composables during component setup. User quota and expiry editing is encapsulated in `components/nodes/UserPolicyDialog.vue` (backed by `adapters/user-policy.ts`). Remaining desired-state user/group forms and controlled session or service actions stay directly in the page view.

### Boundaries

Paths in the table below are relative to `web/src`.

| Owner                                            | Responsibilities                                                                                                                   |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| `views/NodeDetailView.vue`                       | Route ID, Fleet selection, authorized-read readiness, Workspace listener, closing dialogs on navigation, and template composition  |
| `features/configuration/useNodeConfiguration.ts` | Configuration form, captured revision, Plan/Apply requests, Plan polling, receipt recovery, errors/loading and dialog cancellation |
| `features/certificates/useNodeCertificates.ts`   | Certificate form, CSR polling, issue/P12/download/revoke requests, receipt/grant recovery, errors/loading and dialog cancellation  |
| `components/nodes/UserPolicyDialog.vue`          | Quota/expiry form, policy read on mount, save, errors/loading; unmounting detaches late responses but does not undo a sent save    |
| `features/node-workflow.ts`                      | Existing shared context fence, cancellable wait, pending-mutation tickets, identifier receipts and expiring in-memory grants       |
| `shared/fleet.ts`                                | Shared operation tracking and telemetry; feature disposal does not stop Fleet tracking                                             |
| `api/workspace.ts`                               | Sole Workspace authority; features receive its context getter, not a second Workspace store                                        |

Each feature composable receives only a readonly node ref, a readonly flag indicating whether the initial authorized node read succeeded, a Workspace context getter, an operation-tracking callback, and the i18n translator function. Neither composable imports the router, the Fleet store, any view component, or the other feature composable. NodeDetail instantiates each composable once during setup, binds it to the route-matched authorized node, and derives overall view readiness from the detail request state, Fleet selection, and any selection error.

### Preserved behavior

- ConfigPlan workflows capture only a known, nonnegative JavaScript-safe configuration revision from the node detail response, never substituting node version or automatically rebasing or retrying a stale revision.
- Closing a workflow dialog immediately aborts its in-flight reads and polling loops and resets its transient form state. Disposing the composable's Vue effect scope runs the same cleanup, and route or Workspace changes continue to close open dialogs from the parent view.
- Component teardown never aborts an already-sent mutation request or resends it automatically. If an accepted operation or plan ID arrives after the dialog closes, the shared receipt and ticket helpers in `features/node-workflow.ts` record it so reopening the workflow waits for the in-flight request to settle and then reloads authorized server state.
- Configuration Plan polling and certificate `csr_pending` polling keep their distinct terminal-state predicates while sharing the same 30-attempt cap and 500 ms retry delay.
- Submitting Configuration Apply closes the dialog before handing the resulting operation ID to Fleet tracking. PKCS#12 export and certificate revocation hand off operation tracking while keeping the certificate dialog open. Disposing a feature composable detaches only its local reads and leaves accepted server operations tracked in Fleet.
- One-time certificate download grants stay in memory only, scoped to the active Workspace, node, and certificate ID and bounded by their expiration timestamp. A download triggered just before closing the dialog still runs to completion, and grant tokens are never written to persistent storage, so a full page reload, tab close, or second browser tab cannot recover the secret.
- Stored certificate operation receipts are restored only when the receipt's resource ID matches the currently selected certificate; switching to another certificate or viewing a node with no active certificate neither fetches nor displays the older receipt's operation.

<a id="verification-1"></a>
Unit and browser verification suites for these workflows are listed under [Validation](#validation).

### Advisory action availability

Authorized node detail responses include an `effective_actions` map. On the backend, the telemetry read path combines node trust state and approved capabilities from the Operations Store, and the Node HTTP handler invokes its parent-supplied callback to evaluate the same resource-scoped RBAC rules used by write endpoints. That callback reuses the caller's request context without introducing a second authorization store or permission engine inside the module. Any lookup error fails closed, caller-specific detail responses are never shared across sessions, and collection list endpoints skip these per-node action lookups.

In the browser, NodeDetail uses `effective_actions` to disable unavailable buttons and explain which capability, trust state, or RBAC permission is missing before the operator fills out a dialog. Viewing certificate metadata continues to use its dedicated read permission. `effective_actions` is advisory UI guidance only. Every write endpoint enforces its corresponding server-side authorization and domain validation checks on submission. Only the applicable sensitive or node-targeted operations require independent two-principal approval, node capabilities, expected revisions, sealing keys, or `SecretRef` constraints. Configuration dialogs display their template source and captured configuration revision, and generated Plan previews show the complete redacted candidate without fabricating current field values. Existing desired and observed user/group tables clearly distinguish unmanaged local accounts from missing Agent observations without altering convergence records or implicitly adopting unmanaged accounts.

## Validation

Run verification checks inside an authorized, isolated environment with all dependencies installed, selecting suites according to [Validate a change](testing.md). Unless a repository-root script is shown, file paths in the table below are relative to `web/`.

| Change | Relevant evidence |
| --- | --- |
| API adapters, session or Workspace | `test/api-client.test.ts`, generated-auth serialization checks; `node test/run-auth-browser.mjs` against the production build for login/Workspace/SSE races |
| Configuration/certificate workflows | `test/configuration-feature.test.ts`, `test/certificates-feature.test.ts`, `test/node-config-plan.test.ts`, `test/node-workflows.test.ts`; browser specs `e2e/config-plan.spec.ts`, `e2e/certificate-lifecycle.spec.ts`, `e2e/node-forms.spec.ts` on desktop/mobile |
| Operations/rollouts | `test/operation-state-tone.test.ts`, `test/rollout-views.test.ts`, `test/nodes-rollout-submit.test.ts`; browser specs `e2e/operations.spec.ts`, `e2e/agent-rollout.spec.ts` for unknown states, partial failure, polling disposal and single resume; a real one-node rollout runs in [manual integration](real-business-validation.md) |
| Approvals, audit, overview | Corresponding view/unit tests and browser specs `e2e/approval-queue.spec.ts`, `e2e/approvals-audit.spec.ts`, `e2e/overview.spec.ts`; verify permission, stale/empty states and Workspace switching |
| Shared UI, styles or shell | `bash scripts/web-check.sh basic` from repository root; full `npm run test:e2e`, desktop/mobile layout and keyboard checks below |
| ESLint configuration | `npm run lint`, `npx vitest run test/eslint-vue.test.ts` (Vue rules reach SFCs) and `npx --no-install prettier --check eslint.config.ts`; lint's prelint builds the generated client |

`scripts/web-check.sh basic` builds the generated OpenAPI client and runs formatting checks, ESLint, typecheck, Vitest unit tests, the production Vite build, and generated-auth verification. Running `scripts/web-check.sh full` adds the focused authentication browser runner (`test/run-auth-browser.mjs`), while the full Playwright E2E suite requires `npm run test:e2e` or `make e2e`. Neither this runner nor stubbed Playwright specs validate a live Controller, database recovery, or host installation. Specs that exercise the development simulator (`local-slice` and the first two `overview` tests) require a running backend stack; if the backend is not running, report those specs as not run. To run focused Playwright specs against an already-serving production bundle, export `PLAYWRIGHT_BASE_URL` beforehand.

Playwright projects are named after their browser target: `desktop` runs Chromium, `mobile` emulates an iPhone 13 viewport inside Chromium (and therefore does not provide Safari engine coverage), and `firefox` and `webkit` run the cross-engine subset covering login/Workspace selection, rollouts, node forms, password dialogs, shell/Overview layout, Nodes/detail, Operations, and approvals/audit. `npm run test:e2e` executes all four projects unless filtered with `--project`. `node test/run-auth-browser.mjs` accepts the same `--project` flags (for example `node test/run-auth-browser.mjs agent-rollout.spec.ts --project=desktop --project=webkit`) and exits with an error if a requested browser binary is missing; always report any project whose browser engine is not installed as not run.

Passing `--gateway` (which requires Docker) serves `dist/` behind the pinned production Caddy image and `deploy/production/Caddyfile` over HTTPS in place of Vite preview. Passing `--gateway=integrated` places the Integrated Edge container (`edge.Dockerfile` base image with the NGINX template and entrypoint from `deploy/production/integrated`) in front of the Integrated Caddyfile at `https://controller.localhost` on `127.0.0.1:443` so Caddy receives traffic only through Edge's SNI router and PROXY protocol. The cross-engine specs import `e2e/csp.ts`, which fails the test on any `securitypolicyviolation` event. `csp-control.spec.ts` also triggers a deliberate inline violation and asserts that the browser blocks it with `disposition=enforce` across every selected project, because the Content Security Policy is enforced without a report endpoint. Playwright's WebKit and Firefox builds do not substitute for testing on minimum supported Safari 16.4 or Firefox 128 releases.

When a pull request modifies shared UI primitives, layout, or styles, include the following evidence in the PR description:

- Before-and-after screenshots captured against identical fixtures at both 1440 px and 390 px widths for overview, nodes, node detail (including missing-node state), operations, rollout, approvals list and detail, audit, settings, and login, confirming that the document itself never scrolls horizontally.
- Verification of the keyboard skip link, desktop sidebar and mobile Sheet navigation, Nodes filter controls and detail links, and dialog open, cancel, and confirm flows returning focus to the triggering element.
- CSS and JS production bundle size comparisons against the base branch, along with the rationale for any newly added package dependencies.
- Manual Safari and Firefox test results whenever the change touches browser-sensitive CSS or DOM APIs, since Chromium mobile emulation does not cover those engines.
