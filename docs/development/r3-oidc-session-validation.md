# R3: Disabled OIDC identities and bounded login retries

## Scope and baseline

- Date: 2026-09-08.
- Starting HEAD: `3f1c0652f2ca08083718070744081c7ea6c778f0`; working tree was clean.
- Reference: `7e463a34bc8363021daeae12101d3cbd215e24ae`.
- Read repository `AGENTS.md`; no scoped `AGENTS.override.md` was found.
- Existing Local credential/attempt fencing changes were preserved.
- All execution was through `ssh BuildServer`, under `/tmp/ocservia-r3.T1SJXQ`.
- Used a dedicated PostgreSQL 17 container, migrated schema, and separate owner/runtime roles. No production IdP, database, deployment, commit, or PR was used.

## Reproduction and classification

The signed test IdP returned a valid token for the same issuer/subject after its identity was disabled. Before the fix, two legitimate logins created two sessions; the disabled login returned a new cookie and increased the count to three. The identity count remained one and `disabled_at` remained non-null. An existing session was rejected by `Authenticate` after disable.

This establishes unwanted session issuance, not an authorization bypass. The per-request disabled-identity predicate already prevented authentication. The final HTTP test additionally verifies that a freshly issued, initially usable cookie receives 401 from `/api/v1/workspaces` after disable.

Separately, actual Chromium navigation with route-mocked OIDC success and subsequent workspace 401 responses produced three automatic SSO starts with zero user clicks. The reproducer deliberately stopped at three; it did not run an unbounded loop. The original callback error response itself was terminal 401 JSON, not an automatic redirect. Revisiting `/login` previously started SSO again.

Backend IdP validation uses signed tokens and real HTTP token/JWKS endpoints; browser navigation tests use simulated HTTP responses. These are separate dynamic checks, not a claim of a single browser-to-IdP-to-database end-to-end run.

## Transaction and frontend changes

`CompleteLogin` retains token exchange and verification before calling `createSession`. Within its existing transaction, the OIDC upsert now updates a conflicting identity only when `identities.disabled_at IS NULL`. No returned row maps to `ErrUnauthenticated`, before any session insertion. The upsert's row lock lasts through session insertion and commit. The unique issuer/subject key prevents creation of a replacement identity, and no branch clears `disabled_at`.

Deterministic concurrent tests observe PostgreSQL lock waits. If disable commits first, login creates zero sessions. If login commits first, one session exists but is unusable once disable commits. `Authenticate` retains its per-request disabled check. Local lock-held credential revalidation, password-reset fencing, and session revocation are unchanged. PKCE, state, nonce, signature, issuer, audience, and lifetime validation code is unchanged.

The login page stores only the fixed `started` attempt value before SSO. OIDC-only first entry still redirects automatically. An unfinished attempt, or fixed `auth=failed` state, stops automatic SSO and exposes the SSO button for explicit retry. An immediate authentication 401 returns to the stopped login page without overwriting the original safe return path. Successful shell authentication consumes the return path and removes the attempt marker; a later expired session can start a fresh login.

The callback continues returning the existing generic 401 problem response and clearing the login-state cookie, without setting an authentication cookie. No HTTP/OpenAPI response contract changed. Retry is available upon returning to `/login`; the callback remains a JSON error response. No provider error, code, or token is included in new frontend state. Query-supplied external return destinations are not used. The frontend marker has no authorization role, and no IdP-wide logout was added.

## Verification

The following commands passed in BuildServer containers against the final candidate:

```sh
go build -o /work/ocserv-control ./cmd/ocserv-control
go test -p 1 -race -count=1 -v ./internal/auth -run 'Test(OIDC|Local)'
go test -p 1 -race -count=1 -v ./internal/api -run 'Test(AuthHTTP|LocalLoginHTTP|LocalUserLifecycle)'
go test -count=1 -v ./internal/auth -run 'Test(BeginLogin|NewRejects|Optional)'
npm run typecheck
npm run build
npx vitest run test/login.test.ts
npx eslint src/shared/login.ts src/api/client.ts src/views/LoginView.vue src/shared/i18n.ts e2e/login.spec.ts
npx eslint e2e/auth-workspace.spec.ts
PLAYWRIGHT_BASE_URL=http://127.0.0.1:4183 npx playwright test e2e/login.spec.ts e2e/auth-workspace.spec.ts
```

- Go coverage includes new/existing OIDC identity login, disabled identity rejection with unchanged session/identity counts, both concurrent commit orders, previously issued session rejection, callback cookies/protocol, and Local lifecycle/credential/attempt regressions.
- Vitest: 13 passed, including internal return paths and open-redirect rejection.
- Playwright: 24 passed across desktop and mobile Chromium, including initial automatic SSO, terminal callback failure, immediate 401, explicit retry, marker cleanup, subsequent session expiry, Local-only and mixed modes, and return-path preservation.
- Targeted formatting and `git diff --check` passed. No full repository test suite was run.
- A read-only candidate review identified an in-memory attempt flag that would outlive successful login; it was removed and the later-session-expiry regression was dynamically verified.

BuildServer evidence: `baseline-go.log`, `baseline-web.log`, `baseline-loop.log`, `final-go.log`, `final-web.log`, and the `go.sh`, `web.sh`, `baseline-loop.sh` runners in the directory above. Initial environment issues (archive metadata and missing browser libraries) were resolved before the reported reproductions and final checks.

## References

- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html): session issuance and acceptance are separate controls.
- [OWASP Authorization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html): validate authorization on every request; frontend state is not an access-control boundary.
