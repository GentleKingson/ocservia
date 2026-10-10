# Web boundaries and node workflows

## Startup, routing and development server

- [`main.ts`](../../web/src/main.ts) installs Pinia, the router and i18n, then
  mounts only after `router.isReady()`, so `/login` never renders the shell.
- [`shared/routes.ts`](../../web/src/shared/routes.ts) declares the console
  routes. The `/dev` simulator route is registered only on a development
  runtime; production navigation never registers it.
- [`App.vue`](../../web/src/App.vue) runs Workspace discovery, the readiness
  refresh and the one-time login return described below.
- `npm run dev` (the `web` service of the [local stack](../getting-started/local-development.md)
  on port `4173`) proxies `/api` to `VITE_API_TARGET` and, when
  `VITE_DEV_AUTH_TOKEN` is set, adds that bearer token to proxied requests.
  A direct `npm run dev` listens on `127.0.0.1` only; the container passes
  `--host 0.0.0.0` and Compose publishes the port on host loopback.
  [`vite.config.ts`](../../web/vite.config.ts) refuses `vite build` while the
  token is set. Production serves the built bundle from the Gateway, whose
  [Caddyfile](../../deploy/production/Caddyfile) proxies `/api/*`; it is not the
  Vite proxy.
- Use the Node and npm versions in [`toolchains.lock`](../../toolchains.lock)
  and `npm ci` in `web/`. Checks are listed in [Validation](#validation).

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

[`eslint.config.ts`](../../web/eslint.config.ts) restricts static imports and
re-exports in handwritten API modules and configuration/certificate features.
They cannot import views, routing or concrete Fleet/local-slice stores; API
modules also cannot import features. Generated sources are excluded. This is
not a transitive dependency or runtime ownership check and does not cover
constructed/dynamic imports or every Vue SFC.

Keep the `@/` mapping to `web/src` identical in TypeScript and Vite configuration.
Features receive context and tracking callbacks from the page; they may use Vue,
generated types, domain APIs and `features/node-workflow.ts`. API transport may
call the session coordinator. Remove unused forwarding exports when their last
consumer disappears; use the [API source directory](../../web/src/api) for the
current export inventory.

<a id="export-and-caller-inventory"></a>
### Request and Workspace invariants

Domain adapters preserve workspace headers, pagination, abort signals, revision
`If-Match`, approvals, command TTLs and endpoint-specific idempotency policy.
Do not add mutation retries or assume every endpoint uses identical headers.
Certificate downloads use their grant token and same-origin session, without a
Workspace header; event-stream URLs must never contain bearer tokens.

Workspace discovery is cached and coalesced; selection accepts authorized IDs
only and increments a generation on change. Async consumers must fence results
by both Workspace ID and generation, including a switch away and back to the
same ID. The independent authentication probe must not change that authority.
Login return paths are validated internal paths and consumed once. The
remembered Workspace ID and login return path are optional `sessionStorage`
preferences: storage failures mean no preference and never block login,
Workspace selection or change events. SSE stores
own their subscriptions, timers and cancellation; importing an API starts none.

<a id="verification"></a>
Validation entry points are collected in [Validation](#validation).

## UI components and styles

The console is built from [shadcn-vue](https://www.shadcn-vue.com) primitives
on [Reka UI](https://reka-ui.com) and Tailwind CSS v4. Every page uses
utilities and these components; there is no second stylesheet or class
system.

### Cascade

`web/src/main.css` is the only CSS entry and owns the layer order
`theme, base, ui-base, utilities`:

- `base` holds the few page-wide rules: font stack, page color and background
  from the tokens, `box-sizing`, `body` margin and minimum width, and links
  inheriting color. Add a rule here only when every page needs it; style
  anything else with utilities in the component that renders it.
- Tailwind's global Preflight is not imported. `ui-base` applies the reset the
  primitives need only to elements carrying the `data-slot` marker (form
  controls also inherit font and color), which also covers content a
  primitive teleports to `<body>`. Use `components/ui` primitives instead of
  bare form controls, which keep browser default styles.
- Utilities win over both by layer order, never by `!important`. Unlayered
  rules (Vue scoped styles) would beat every layer; no component has one.
- Tailwind scans `web/src` for utility candidates. Keep complete utility class
  names visible in source; constructed class fragments are not reliable scan input.

### Tokens

Theme tokens are plain custom properties on `:root` in `main.css`, mapped to
Tailwind colors and radii through `@theme inline`: `background`, `foreground`,
`card`, `popover`, `primary`, `secondary`, `muted`, `accent` (each with a
`-foreground` pair), `destructive`, `success`, `border`, `input`, `ring`,
`sidebar` (with `-foreground`, `-accent`, `-accent-foreground`, `-border`) and
`--radius` (`rounded-sm` to `rounded-xl`). Values follow the shadcn-vue
new-york-v4 neutral palette, with muted text, input and ring darkened for
contrast. Change a color by editing its token,
not the utilities that use it. Warning states use Tailwind's `amber` scale
directly. The `dark` variant only matches an explicit `.dark` class; dark mode
is not a product capability and must not follow the OS setting.

### Supported browsers

The Web console supports Chrome 111+, Safari 16.4+, Firefox 128+ and other
Chromium-based browsers built on Chromium 111+ (for example Edge 111+). This
matches Tailwind CSS v4's baseline (cascade layers, `@property`,
`color-mix()`), and `vite.config.ts` sets the same `build.target`. Change both
together; do not lower the range without a reviewed compatibility plan.

The support range is a policy, not a test matrix. The Playwright specs and the
authentication browser runner use Chromium only (desktop Chrome and an iPhone
13 emulation that is forced to Chromium). Basic CI runs no browser at all: its
`web` job runs `scripts/web-check.sh basic`. Playwright specs run through
`npm run test:e2e` or `make e2e`; `scripts/web-check.sh full` runs the separate
authentication browser runner. Safari and Firefox are supported targets with no automated
coverage; record manual results for UI changes that depend on them.

### Component sources

`web/components.json` records the shadcn-vue CLI settings. Primitives are
project-owned source in `web/src/components/ui`; a CLI run is not an upgrade.
Compare upstream changes by hand (keyboard behavior, ARIA, Portal and props)
and keep the local modifications below. When the CLI generates icon imports
from `lucide-vue-next`, rewrite them to the existing `@lucide/vue` package.

The upstream paths are `apps/v4/registry/new-york-v4/ui/<component>` and
`apps/v4/registry/new-york-v4/lib/utils.ts`. Preserve these local adaptations:

| Area | Local behavior to preserve |
| --- | --- |
| Button, Badge, Label, Sheet, Dialog | Explicit `as`/`asChild` defaults for `exactOptionalPropertyTypes`; Badge defaults to `span`, Label to `label` with `for` falling through |
| Input and Textarea | `defineModel` replaces `useVModel`; no `defaultValue` prop |
| NativeSelect | `defineModel`, attributes fall through to `select`; plain `option` children |
| Sheet and Dialog | No `tw-animate-css` dependency; instant open/close, translated `closeLabel`, 32px close targets; Sheet close carries `data-slot` |
| Dialog | Content scrolls within `max-h-[calc(100dvh-2rem)]`; no footer-generated close button |
| DropdownMenu | No animation utilities; Trigger defaults `as` to `button`; explicit `as`/`dir`/`modelValue` defaults, Content limited to `align`/`side`/`sideOffset` and items without `textValue` for `exactOptionalPropertyTypes`; no Sub or Radio parts |

All sources were taken on 2026-10-04 from `unovue/shadcn-vue` commit
`b251d9fd92aa496495e127137a7734704fb34a29` (CLI 2.8.2) and are MIT licensed;
the notice is kept in `web/src/components/ui/LICENSE.shadcn-vue`. Runtime
dependencies are pinned in `web/package.json`: `reka-ui`, `@vueuse/core`
(already required by `reka-ui`), `class-variance-authority`, `clsx` and
`tailwind-merge`; build-time
`tailwindcss` and `@tailwindcss/vite`. Primitives do not call APIs, read the
Workspace or decide permissions.

### Shell and page components

The shell follows the shadcn-vue `dashboard-01` inset layout without the
`sidebar` primitive set: the sidebar sits on the `sidebar` token, page content
is an inset rounded panel, and the site header shows the current section title
from `components/layout/navigation.ts`.

`App.vue` owns readiness, Workspace selection and login-return navigation.
Layout/common components present caller-owned data; primitives do not fetch or
decide permissions. The shell owns page width and padding; views render a bare
`<main>`. Keep the skip link, `aria-current` navigation, titled mobile Sheet,
focus movement after navigation and `status`/`alert` data states.

Node write forms use `OperationDialog` with one form. Pages own fields, pending
state and submit handlers. Preserve focus trapping/return, Escape/Cancel behavior,
translated labels and help through `aria-describedby`. Pending requests disable
submission; errors retain ordinary inputs while secret owners clear passwords.
Show disabled-action reasons visibly as well as in `title`; approval fields do
not replace server authorization. Quota help must explain UTC monthly periods,
direction, zero disabling the user and unlimited quota; expiry is UTC.

<a id="nodes-list"></a>
<a id="node-detail-read-areas"></a>
<a id="node-detail-write-forms"></a>
### Read models and list state

- Nodes filters, search, sorting and optional columns run on the complete Fleet
  snapshot. Replace it only after all pages load; label failed refreshes as stale.
  [`node-list.ts`](../../web/src/features/nodes/node-list.ts) owns filter/sort and
  URL query rules. Keep unknown observations explicit and timestamp handling
  valid for infinity/extended years. The list follows the Tailscale Machines
  layout: search, a single Filters menu, removable chips for active filter
  values, and a per-row action menu. Filter groups stay flat in one menu;
  nested submenus are unreliable on touch screens.
- Rollout selection uses node IDs. Select-all covers visible eligible rows,
  confirmation lists every selected target, hidden selections remain visible as
  a count, and Workspace changes clear selection. Node list rows have no action
  availability; advisory write permissions come from detail reads.
- Detail navigation preserves the originating list query when available. Missing
  observations and unavailable/not-found states remain distinguishable. Views
  own route/selection and refresh; presentation components add no data loading.
- Overview metrics name their sources and distinguish ready, stale, unavailable
  and loading. Observed sessions are last-reported counts; direct paths include
  offline nodes. Active/unknown operations are workspace-wide, but failed counts
  cover only the latest 20. Do not turn these snapshots into historical trends.

<a id="operations-and-rollouts"></a>
<a id="approvals-and-audit"></a>
<a id="overview"></a>
### Operations, approvals and audit

Automatic snapshot refresh runs only while the document is visible and focused.
Fleet closes its event stream and cancels snapshot reads on blur/hide, then
rebuilds and reconnects immediately on return. With a healthy event stream,
Fleet and overview supplement live updates every 60 seconds after the previous
refresh completes; stream/read failures use 15 seconds. Readiness checks use
60 seconds when healthy and 15 seconds after failure. Active rollout detail
retains its 2-second interval. Successful reads clear the existing stale state.
The shared `foreground-refresh.ts` scheduler merges event notifications and
periodic polls into one timer per owner, never overlaps scheduled reads, and
coalesces events during a read into one follow-up. Leaving a Fleet view stops
its snapshot reads and clears detail selection; returning refreshes immediately.
Accepted node-operation tracking remains independent and is not cancelled by
focus changes or leaving the view.

A plain operation's `unknown` is a warning and Fleet keeps polling it. Upgrade
and rollout-node `unknown` are terminal failures. Tone helpers never decide
polling. Rollout totals distinguish success, failure (including rolled back) and
unknown, and leaving rollout detail stops its polling. Rollout reads, resume
and create results are fenced by route rollout, Workspace ID and generation; a
route or Workspace change clears the shown rollout and its resume action before
reading again. A read in flight when resume starts is dropped, and a resume or
create without a confirmed response is never resent automatically. See
[`fleet.ts`](../../web/src/shared/fleet.ts) and
[`state-tone.ts`](../../web/src/features/operations/state-tone.ts).

Approval queues use cursor paging and clear stale rows after refresh failure.
Deep links, lookup and queue rows share the detail route. Approval authorizes
execution; its status is not the resulting operation's outcome. Expired requests
have no decision form. Decisions require a reason and reviewed checkbox; lost
or rejected decisions (including 409) clear detail and require refresh, never an
automatic resend. Audit search/filter covers only the latest 50 records loaded
in the browser; it is not a server-wide search.

Keep wide tables inside scrollable regions on narrow screens, with expanded
content constrained to the visible width.

<a id="ui-regression-checklist"></a>
UI regression checks are listed under [Validation](#validation).

## Node detail workflows

NodeDetail composes configuration and certificate workflows during setup.
User-policy mapping lives in `adapters/user-policy.ts`; other desired-state
and controlled-action handlers remain in the page.

### Boundaries

Paths below are relative to `web/src`.

| Owner                                            | Responsibilities                                                                                                                   |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| `views/NodeDetailView.vue`                       | Route ID, Fleet selection, authorized-read readiness, Workspace listener, closing dialogs on navigation, and template composition  |
| `features/configuration/useNodeConfiguration.ts` | Configuration form, captured revision, Plan/Apply requests, Plan polling, receipt recovery, errors/loading and dialog cancellation |
| `features/certificates/useNodeCertificates.ts`   | Certificate form, CSR polling, issue/P12/download/revoke requests, receipt/grant recovery, errors/loading and dialog cancellation  |
| `features/node-workflow.ts`                      | Existing shared context fence, cancellable wait, pending-mutation tickets, identifier receipts and expiring in-memory grants       |
| `shared/fleet.ts`                                | Shared operation tracking and telemetry; feature disposal does not stop Fleet tracking                                             |
| `api/workspace.ts`                               | Sole Workspace authority; features receive its context getter, not a second Workspace store                                        |

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

<a id="verification-1"></a>
Feature and browser validation are listed under [Validation](#validation).

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

## Validation

Use an authorized isolated environment with dependencies installed; select checks
according to [Validate a change](testing.md). Paths in the table are relative to
`web/` unless a repository script is shown.

| Change | Relevant evidence |
| --- | --- |
| API adapters, session or Workspace | `test/api-client.test.ts`, generated-auth serialization checks; `node test/run-auth-browser.mjs` against the production build for login/Workspace/SSE races |
| Configuration/certificate workflows | `test/configuration-feature.test.ts`, `test/certificates-feature.test.ts`, `test/node-config-plan.test.ts`, `test/node-workflows.test.ts`; browser specs `e2e/config-plan.spec.ts`, `e2e/certificate-lifecycle.spec.ts`, `e2e/node-forms.spec.ts` on desktop/mobile |
| Operations/rollouts | `test/operation-state-tone.test.ts`, `test/rollout-views.test.ts`, `test/nodes-rollout-submit.test.ts`; browser specs `e2e/operations.spec.ts`, `e2e/agent-rollout.spec.ts` for unknown states, partial failure, polling disposal and single resume |
| Approvals, audit, overview | Corresponding view/unit tests and browser specs `e2e/approval-queue.spec.ts`, `e2e/approvals-audit.spec.ts`, `e2e/overview.spec.ts`; verify permission, stale/empty states and Workspace switching |
| Shared UI, styles or shell | `bash scripts/web-check.sh basic` from repository root; full `npm run test:e2e`, desktop/mobile layout and keyboard checks below |
| ESLint configuration | `npm run lint` and `npx --no-install prettier --check eslint.config.ts`; lint's prelint builds the generated client |

`scripts/web-check.sh basic` runs generated-client build, formatting, lint,
typecheck, unit tests, production build and generated-auth checks. Its `full`
mode adds the focused authentication browser runner (`test/run-auth-browser.mjs`)
but not the Playwright specs, which need `npm run test:e2e` or `make e2e`. This runner and stubbed
browser specs do not validate a live Controller, database recovery or installation.
Specs requiring the development simulator (`local-slice` and the first two
`overview` tests) need a running backend; report missing coverage as not run.
For a separately served production build, set `PLAYWRIGHT_BASE_URL` before
running focused Playwright specs.

For shared UI changes, record in the PR:

- Before/after screenshots with identical fixtures at 1440 px and 390 px for
  overview, nodes, node detail/missing node, operations, rollout, approvals/detail,
  audit, settings and login. The page itself must not scroll horizontally.
- Keyboard skip link, sidebar/mobile navigation, Nodes filters/detail link, and
  dialog open/cancel/confirm with focus returning to the trigger.
- CSS/JS bundle size compared with the base branch and reasons for new dependencies.
- Manual Safari/Firefox results when affected; Chromium device emulation does
  not supply those browsers' coverage.
