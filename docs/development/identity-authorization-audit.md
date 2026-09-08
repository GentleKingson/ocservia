# Identity, authorization, approval, and audit operations

Production API access supports Local only, OIDC only, and Local + OIDC.
When OIDC is enabled, it uses Authorization Code flow with PKCE S256.
Configure an HTTPS issuer, exact HTTPS callback URL and client credentials for
OIDC; its redirect origin must match `OCSERV_PUBLIC_ORIGIN`. All modes require a
32-byte session encryption key, a 32-byte audit checkpoint key, and an
independent 32-byte audit event authentication key with a stable key ID. The browser
receives only Secure, HttpOnly, SameSite session cookies; OIDC tokens are not
stored in browser storage. Existing sessions remain usable during a temporary
identity-provider outage, while new SSO logins fail closed. Local login remains
available when enabled.

See [production authentication](../operations/authentication.md) for deployment
examples, secret mounts, first-admin bootstrap and login behavior. Common
authentication and audit settings are:

```text
OCSERV_LOCAL_AUTH_ENABLED
OCSERV_PUBLIC_ORIGIN
OCSERV_SESSION_KEY_FILE
OCSERV_SESSION_TTL
OCSERV_AUDIT_CHECKPOINT_KEY_FILE
OCSERV_AUDIT_EVENT_KEY_ID
OCSERV_AUDIT_EVENT_KEY_FILE
```

The symmetric key values contain 64 lowercase hexadecimal characters. The
event key file must be a process-owned, single-link regular file with mode
`0400` or `0600` below root- or process-owned non-writable ancestry. Production startup
fails when both Local and OIDC are disabled, OIDC is incomplete, or a required
session/audit key is absent. Development bearer
authentication remains limited to a non-production deployment.

Authorization combines a subject, workspace, resource type, resource ID, and
action. The baseline roles are Viewer, Operator, UserManager, ConfigManager,
Auditor, SecurityAdmin, and PlatformAdmin. Collection requests carry
`X-Workspace-ID`; object routes independently resolve the object's workspace so
changing an ID cannot cross an authorization boundary.

Node activation, node revocation, and service reload require an approved
request. Create the request first, have a different authorized principal approve
it, then submit the mutation with `X-Approval-ID`. Approval records are scoped
to one requester, action, workspace, resource, and expiry; they are consumed in
the business transaction and cannot be replayed. PlatformAdmin does not bypass
this requirement.

Audit intents commit with business writes. Agent terminal results append a
separate event. Every new row authenticates its canonical event hash with a
domain-separated application HMAC; signed checkpoints use a separate key.
Audit rows and checkpoints are append-only, and the verification endpoint checks
the hash chain, every event MAC, and the latest checkpoint. A failed audit insert rolls back
the business transaction.

## Local authentication core (P1)

The internal `auth.Service` can be constructed without an OIDC provider.
Trusted Go callers may opt in with `auth.Config.LocalEnabled`, provision an
identity using `CreateLocalCredential`, and log in using `AuthenticateLocal`.
Provisioning grants no roles. Runtime configuration, HTTP login, Local-only
production startup and user management use this core through the shared session
and authorization stack.

Migration 000031 stores passwords only in `local_credentials`, keyed by
`identity_id`. Local identities use issuer `local` and the normalized username
as subject. Usernames are trimmed and lowercased, with a 128-byte input limit;
the accepted alphabet is ASCII letters/digits plus `.`, `_`, and `-`, starting
with a letter or digit. No email/name matching or OIDC account linking occurs.
New passwords are not normalized and require at least 15 Unicode code points,
at most 1024 UTF-8 bytes, and the embedded offline blocklist check. Every new
hash passes `ValidateNewPassword` through `hashPassword`; future provisioning
and self-service changes must reuse it. Historical 1..1024-byte credentials
still verify unchanged. See the [Local password policy](../operations/authentication.md#local-new-password-policy).

Hashes use `golang.org/x/crypto/argon2` Argon2id v19, independent 16-byte random
salts, 32-byte outputs, and self-contained PHC parameters. The write cost is
19 MiB, two passes, one lane, following the
[OWASP recommendation](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id).
Verification reads stored parameters with resource bounds (up to 64 MiB, five
passes, four lanes). Unknown users perform a dummy verification at the current
write cost. Local login rechecks the credential and disabled state under lock
before the shared session insert; session cookies, AEAD, authentication,
logout, RBAC, approval, audit, and break-glass semantics remain unchanged.

Rolling back 000031 deletes local password hashes, not identities or sessions.
Stop local provisioning/login and preserve credentials before applying down.

Local login also requires migration 000033 for shared account failure backoff.
`AuthenticateLocal` reserves a PostgreSQL single-flight lease before reading a
credential or executing KDF, then completes only an observed incorrect password
as a failure. Admission serializes capacity allocation in a short transaction;
verification runs after commit, without a held connection or database lock.
Success clears the matching lease in the credential-revalidation/session
transaction. Reset and disable invalidate the state in their existing transaction,
so a stale completion cannot overwrite the next credential epoch's failures.
See [Local account failure backoff](../operations/authentication.md#local-account-failure-backoff-r2)
for parameters, expiry, capacity, response behavior and upgrade constraints.

## Local initialization and lifecycle (R4)

Apply migrations first using the existing `--migrate-only` procedure. Select an
existing workspace to own platform Local identity administration. Its UUIDv7 is
required explicitly: the repository has workspace-scoped RBAC, not global role
bindings. Bootstrap records this choice permanently; API headers cannot select
another workspace to obtain access to shared login identities.

With the normal database, Local auth, session and audit configuration loaded:

```sh
export OCSERV_LOCAL_AUTH_ENABLED=true
export OCSERV_LOCAL_BOOTSTRAP_USERNAME=initial-admin
export OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID='<existing-workspace-uuidv7>'
export OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/local-bootstrap-password
export OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME=initial-approver
export OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE=/run/secrets/local-bootstrap-approver-password
ocserv-control --bootstrap-local-admin
```

Provision two different passwords separately using the deployment secret
manager. The bootstrap reader uses the existing bounded Secret value format
(one trailing LF, then CR removed) and additionally enforces bootstrap-process ownership,
private mode, regular file, no final symlink and no hard links on the opened
descriptor. Nonblocking open prevents FIFO substitution from hanging the CLI.
Both new credentials use the shared password policy. No default, plaintext
environment password or password CLI option exists. Different identities are
not proof of different people: separate responsible owners and separate
credential custody are operational requirements. Remove both temporary mounts
and bootstrap settings after success.

The one-shot exits without starting listeners/workers or contacting OIDC. One
transaction creates two `local` identities, Argon2id credentials, existing
workspace-scoped `PlatformAdmin` and `SecurityAdmin` bindings respectively,
singleton completion marker and both audit events. A missing workspace or any insert/audit failure rolls everything
back. Concurrent or later bootstrap attempts fail closed; an existing Local
SecurityAdmin/PlatformAdmin also prevents bootstrap. Disabling the bootstrap
identity or changing its password does not clear the marker. Normal startup does
not read the bootstrap password file or synchronize credentials. Preserve the
marker in backups; do not drop migrations or delete the marker to reset credentials.

Migration 34 freezes the single-admin upgrade exception: the original bootstrap
identity must still be an active Local workspace PlatformAdmin with its original
bootstrap audit and no other historical elevated binding in that workspace.
Only that state receives `completion_pending=true`; other existing states close
the exception. `--complete-local-bootstrap` requires the original username,
fixed workspace, current administrator Secret and a separate new approver Secret.
It verifies the current password with the existing account lease/backoff,
rechecks credential/state under the initialization lock, then creates only the
new SecurityAdmin and atomically records completion and audit. It cannot replace
an existing account or password. New elevated grants permanently close pending
completion. Normal startup never fills this state. Schema 34 deliberately fences
older Controllers; stop old writers before migrating. Runtime is still not Owner
and receives only lifecycle-column UPDATE privileges on the bootstrap marker,
not DELETE or DDL. See the [executable operator steps](../operations/authentication.md).

Lifecycle endpoints (under `/api/v1`, `Content-Type: application/json`):

| Endpoint | Body | Success |
| --- | --- | --- |
| `POST /local-users` | `{"username":"operator1","password":"..."}` | 201, `identity_id` |
| `POST /local-users/{identity_id}:disable` | `{}` | 204 |
| `POST /local-users/{identity_id}:reset-password` | `{"password":"..."}` | 204 |
| `POST /auth/change-password` | `{"current_password":"...","new_password":"..."}` | 204, all old sessions revoked; re-login required |

Every administrative mutation requires an authenticated session, exact trusted `Origin`, and
existing RBAC action `local_user.manage` in the fixed management workspace.
Only existing `PlatformAdmin` has this action via its wildcard; SecurityAdmin,
UserManager, and PlatformAdmin in other workspaces do not. The existing explicit
break-glass policy remains available; development pseudo-principals are rejected.
Creating an account grants no roles. Use `/role-bindings` for subsequent grants,
including the unchanged independent approval requirement for elevated grants.

Reset additionally requires `X-Approval-ID`. Request approval through
`POST /approval-requests` in the management workspace using
`action: local_user.reset-password`, `resource_type: local_user`, the target
`resource_id`, `reason`, and `ttl_seconds`. A different existing authorized
SecurityAdmin/PlatformAdmin approves the returned request hash through the normal
approval endpoint. Approval authorizes replacing that identity's password, not a
specific password value; no password or password hash enters approval content.
Consumption, reset, session revocation and audit commit together. Replay fails.
Bootstrap now establishes separately owned approval authority; normal high-role
grants and administrator resets still consume independent approvals.

Self-service change is separate from `local_user.manage`: it requires only a
valid non-Break-glass Local session for its own identity. Strict JSON rejects
target identity/workspace fields. Current-password failures share login backoff;
new passwords use the existing policy. The transaction locks the identity,
rechecks disabled state, session revocation/expiry and the verified credential
hash (credential epoch), fences the attempt lease, changes the password, revokes
all sessions and appends audit. OIDC/Break-glass cannot obtain Local credentials.

Disable serializes on the initialization/management advisory lock, then
rechecks the acting session and fixed-workspace authority. It refuses to remove
an effective Local elevated identity if that leaves no active workspace
PlatformAdmin or fewer than two distinct active Local workspace approvers.
Disabled identities, inconsistent/missing Local credentials, other workspaces
and narrower bindings do not count. This conservative Local recovery-anchor
policy does not treat OIDC availability or temporary Break-glass as substitutes.
Role creation shares the lock; repository inspection found no production
role/identity deletion or other identity-disable path. No generic IAM revocation
framework is added. Raw database administration is outside the HTTP boundary.

Disable and reset revoke **all** `auth_sessions` for the target identity in the
same transaction as the change and audit. `Authenticate` also checks
`identities.disabled_at` on every request. A login verified before the change
rechecks the locked credential and disabled state before creating a session, so
it cannot issue a usable stale session after commit. Already-authorized in-flight
requests are not retroactively cancelled. Reset does not re-enable a disabled
identity. Each successful mutation records actor, session, target identity,
action, request ID and (for reset) approval ID in the existing audit chain.

These resources are platform login identities, not `/nodes/{node_id}/users` VPN
accounts. Duplicate normalized usernames return 409. Invalid input returns 400;
OIDC/non-Local identity targets return 404 and are never modified, even with the
same username/email. There is no linking, self-registration or MFA. Recovery uses
surviving independently controlled accounts and approved resets, or the existing
authorized backup/PITR procedure; it never reopens Bootstrap.

R4 follows OWASP's [least privilege and deny-by-default authorization](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html)
and [current-password verification for password changes](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html).

### R4 verification (2026-09-08)

Starting SHA: `002a42c0ef1a5b14f352eec7c8427504fff953de`; clean worktree.
Reference `7e463a34bc8363021daeae12101d3cbd215e24ae` is an ancestor. No commit,
PR, deployment or real-deployment account creation/reset was performed.

All runtime checks ran through `ssh BuildServer`, in the private directory
`/tmp/ocservia-r4.OG6by4`, with a dedicated PostgreSQL 17 container and separate
fresh, pre-R4 upgrade, existing-authority and Go-test databases. Migrations used
the Owner connection; CLI, HTTP and integration mutations used `ocservia_app`.

- Before the fix, an actual one-shot Bootstrap followed by HTTP Local creation
  and elevation request succeeded, but both requester and unprivileged second
  user received 403 on approval; binding creation failed. No elevated identity
  was inserted as a shortcut to claim this path worked.
- Fresh and legacy single-admin CLI runs each had exactly one success under
  concurrent initialization. Real HTTP creation, independent approval and
  PlatformAdmin binding then succeeded. Self-approval remained 403. Repeats,
  including after loss of an administrator, were rejected.
- Injected approver-audit failure rolled back legacy completion: pending stayed
  true, approver remained null and no approver credential existed. Fresh
  bootstrap and self-change audit rollback are covered by Go integration tests.
- HTTP tests covered correct/incorrect current password, strict self-only body,
  old-session revocation, reset without approval (409), reset/change/login and
  disable/change/login races, mutual admin disable (one 204, one 401), and last
  effective administrator rejection (409). Existing source admission 429 was
  honored using Retry-After rather than weakening the limiter.
- An explicitly pre-existing OIDC SecurityAdmin migration fixture closed the
  exception without changing identity/credential/binding snapshots. This fixture
  was only an upgrade compatibility test, not fresh-initialization evidence.
- Runtime checks confirmed no Superuser/CreateDB/CreateRole/BypassRLS, no Owner
  membership or table ownership, no marker DELETE or workspace-column UPDATE;
  only the three completion columns gained UPDATE. Schema-33 compatibility was
  rejected against schema 34.

Passed on BuildServer: Controller build; config/app tests; targeted auth and API
integration tests with `-race` (`TestLocal*`, `TestPassword*`), self-service issuer
authorization test, RBAC/approvals tests; generated client build and web typecheck;
10 OpenAPI contract tests and generated-client authentication/serialization test.
The test staging initially needed macOS archive metadata removed and private,
root-owned source ancestry for the existing strict-key tests; both environment
issues were corrected without changing those security checks.

Not exercised: production TLS/IdP infrastructure, personnel custody enforcement,
or a destructive full backup/PITR restore. No complete frontend user-management
screen, generic unlock command or broad IAM framework is part of R4.

## Break-glass

Local auth is not a replacement for this independent emergency offline access
mechanism. Its rotation, alert, audit and short-session requirements are unchanged.

Break-glass is disabled unless `OCSERV_BREAK_GLASS_ENABLED=true` and
`OCSERV_BREAK_GLASS_TOKEN_SHA256` contains the SHA-256 digest of a high-entropy
offline credential. Its session lasts 15 minutes. Every use creates a critical
alert and workspace audit event, and that credential cannot be reused until the
offline credential is rotated and the configured digest changes.

## Rollout and rollback

Apply migration 000011 before enabling OIDC. Establish at least two separately
owned SecurityAdmin bindings and test the independent approval path before
removing development access. Verify an audit checkpoint, an IdP outage, and a
break-glass rotation in the target environment.

For rollback, stop new writes, preserve audit, identity, session, approval, and
alert tables, and deploy the previous binary only after active OIDC sessions are
revoked or their expiry is accepted. The down migration is suitable only when
these new records are intentionally discarded; normal production rollback is a
binary rollback followed by a forward fix.
