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
Passwords are not normalized and accept 1 to 1024 bytes; future provisioning
entry points must apply their password-strength and login rate-limit policy.

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

## Initial Local administrator and lifecycle (P4)

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
ocserv-control --bootstrap-local-admin
```

Provision the password file using the deployment secret manager, with restricted
read permissions. The existing secret-file reader requires an absolute regular
file, rejects symlinks and file replacement, limits reads, and removes a trailing
CR/LF. The Local password limit remains 1..1024 bytes; use a strong generated
password. No default, plaintext environment password, or password CLI option
exists. Remove the bootstrap environment and secret mount after success.

The one-shot exits without starting listeners/workers or contacting OIDC. One
transaction creates the `local` identity, Argon2id credential, existing
workspace-scoped `PlatformAdmin` binding, singleton initialization marker and
audit event. A missing workspace or any insert/audit failure rolls everything
back. Concurrent or later bootstrap attempts fail closed; an existing Local
SecurityAdmin/PlatformAdmin also prevents bootstrap. Disabling the bootstrap
identity or changing its password does not clear the marker. Normal startup does
not read the bootstrap password file or synchronize credentials. Preserve the
marker in backups; do not drop migration 000032 to reset credentials.

Lifecycle endpoints (under `/api/v1`, `Content-Type: application/json`):

| Endpoint | Body | Success |
| --- | --- | --- |
| `POST /local-users` | `{"username":"operator1","password":"..."}` | 201, `identity_id` |
| `POST /local-users/{identity_id}:disable` | `{}` | 204 |
| `POST /local-users/{identity_id}:reset-password` | `{"password":"..."}` | 204 |

Every mutation requires an authenticated session, exact trusted `Origin`, and
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
Establish separately owned approval authority using the existing provisioning
procedure before relying on password reset; bootstrap deliberately does not
create a second approver or bypass elevated role-grant approval.

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
same username/email. There is no linking, self-registration, recovery or MFA.

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
