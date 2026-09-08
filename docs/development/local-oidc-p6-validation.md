# Local + OIDC P6 validation

Decision: PASS (scoped authentication gate)

Validated on 2026-09-08 via `ssh BuildServer`, using disposable PostgreSQL 17,
Go 1.26.6 and Node 24.18.1 containers. Base commit:
`abf7714bc647218a3f0dc798e6707679b46de4b3`.
The working-tree additions are regression tests only; production code is unchanged.
The three changed Go test files were SHA-256 matched between the workspace and
BuildServer after formatting and execution.

## Production configuration

The compiled controller was started with `OCSERV_ENVIRONMENT=production`, the
runtime database role, secure temporary key files and `--role=api`. Successful
starts were checked through `/api/v1/auth/methods`, then shut down normally.
OIDC discovery is lazy; these startup checks do not contact a real external IdP.
Separate TLS test-IdP integration exercises actual login and token verification.

| Configuration | Observed startup |
| --- | --- |
| Local only | PASS; local=true, oidc=false |
| OIDC only | PASS; local=false, oidc=true |
| Local + OIDC | PASS; local=true, oidc=true |
| Neither | Rejected |
| Partial OIDC | Rejected |
| Missing SessionKey | Rejected |
| Invalid PUBLIC_ORIGIN | Rejected |
| OIDC callback origin mismatch | Rejected |

`TestProductionAuthenticationConfiguration` additionally passed all 32 Local /
OIDC-field combinations and invalid key, TTL, issuer, callback and origin cases.

## Authentication and authorization

- Local: correct credentials succeed; unknown user, wrong password, disabled
  identity, malformed JSON and oversized fields fail. Normalized duplicate
  usernames cannot replace credentials; management returns 409.
- Unknown users execute `verifyPassword(dummyPasswordHash, password)`. The dummy
  and newly stored hashes use Argon2id m=19456 KiB, t=2, p=1. Wrong-password and
  unknown-user responses match. This is code-path/cost verification, not a claim
  of strict constant-time behavior or a statistical timing benchmark.
- Local cookies and database sessions expire; logout revokes sessions. Password
  reset and disable revoke every unrevoked session for that identity. Reset also
  invalidates the old password. A login rechecks the verified credential under
  lock, rejecting intervening password changes or disablement.
- OIDC Authorization Code, PKCE S256, state, nonce, ID-token signature checking,
  HTTPS configuration, TLS rejection and Secure/HttpOnly host cookies pass.
  Invalid nonce and signature are rejected; issuer outages fail new login closed
  without invalidating otherwise valid, bounded existing sessions.
- Actual Local/OIDC login cookies, in all three provider modes, enter the same
  session middleware. Ungranted users receive 403 for reads/mutations. Explicit
  PlatformAdmin grants allow workspace reads. SecurityAdmin role elevation fails
  without approval, rejects self-approval, succeeds after independent approval,
  rejects replay and records the original identity/session in audit_events.
  The independent approver in this parity test is a database fixture calling the
  existing approval service; requester actions use the full HTTP middleware.
- Matching subject, email and display name do not merge Local and OIDC identities.
  Disabling the Local identity does not invalidate the OIDC identity's session.

## Security and database boundaries

- Exact-origin rejection, uppercase/default-port normalization, trusted-proxy
  client-IP handling, per-source/global/concurrent admission and Retry-After pass.
  Local login does not log credential bodies; lifecycle tests verify passwords
  are absent from application logs and audit records.
- Break-glass retains a 15-minute database session, critical alert, audit,
  mandatory credential rotation and exact-origin checks. Its service regression
  test now runs with Local enabled alongside OIDC.
- Fresh migrations through 32 pass. Empty-database 32/31 down and forward
  migration back to 32 pass. SQL checks confirm credential FK, username uniqueness,
  one credential per identity, delete cascade and transaction rollback.
- Existing lifecycle tests inject audit failures and verify rollback of bootstrap,
  creation and password reset, including session revocation/approval consumption.

## Executed checks

All runtime checks below ran on BuildServer, not the workstation:

```sh
go build -o /work/ocserv-control ./cmd/ocserv-control
go test -p 1 ./internal/platform/config ./internal/auth ./internal/rbac ./internal/approvals ./internal/audit -count=1 -v
go test -p 1 ./migrations -count=1 -v
go test -p 1 ./internal/api -run 'Test(Local|Auth|BreakGlass|Browser|ApprovalDetail)' -count=1 -v
bash scripts/i12-oidc-rbac-audit.sh
bash scripts/web-check.sh
```

All passed. Targeted database suites used a freshly migrated database and the
runtime role; migration metadata tests used the owner role. No database tests
were skipped in those targeted runs. I12 ran without a database and therefore
covers unit tests and static security checks, not its optional integration cases.
Web passed format/lint, typecheck, 11 files / 87 tests (including OpenAPI contract
and login tests), production build and generated-client authentication checks.

Additional legacy API integration attempts were not green: certificate-route,
synthetic-command and config-plan fixtures use parameterized multi-statement SQL
rejected by default pgx protocol. A simple-protocol diagnostic run passed the latter
two but exposed an outdated certificate receipt fixture and incompatible JSON/
array encoding in other tests. These untouched fixture issues are not P6 blockers;
no claim is made that every repository integration suite passes. The final P6 run
uses the normal protocol. No full G6/Rust/agent E2E, release, PR or deployment ran.

BuildServer evidence directory: `/root/ocservia-p6.kFb5yp` (`validation-standard.log`,
`startup.log`, `web.log`, `i12.log`, plus diagnostic logs and runner scripts).
The disposable database container was removed after validation.
