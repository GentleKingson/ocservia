# Node Detail Feature Ownership

NodeDetail composes two setup-scoped workflows rather than owning their state
and requests. The template, bindings, button rules and routing are unchanged.
User-policy mapping remains in `adapters/user-policy.ts`; unrelated desired-state
and controlled-action handlers remain in the page.

## Boundaries

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

The existing `node-workflow.ts` moved out of `views` because configuration,
certificates and the retained user-policy flow all use it. Its context factory
now receives the same Workspace getter explicitly. There is no compatibility
re-export or duplicate receipt, grant, mutation or Workspace singleton.

## Preserved Behavior

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
- API bodies, approvals, idempotency policies, operation counts and UI bindings
  are unchanged.
- A certificate receipt's operation is restored only when its resource ID matches
  the selected certificate. Falling back to another certificate, or finding no
  active certificate, does not fetch or display the old receipt's operation.

## Verification

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

This validates existing Plan/Apply and certificate/P12 UI interactions, not a
live Controller, database recovery or installation. No backend, API schema,
generated client, dependency or store-framework change is part of this split.
