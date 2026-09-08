# Production authentication

Production supports **Local only**, **OIDC only**, and **Local + OIDC**. OIDC is
not mandatory when Local authentication is enabled. Use the normal
[Controller installation](../getting-started/production.md) and
[production secret permissions](production-deployment.md#secrets-and-release-trust)
for all three modes; these examples replace only the authentication section of
`install.env`, not the database, TLS, relay, signer, backup or release settings.

## Choose a mode

### Local only

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=true
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
```

Leave `OCSERV_OIDC_ISSUER`, `OCSERV_OIDC_CLIENT_ID` and
`OCSERV_OIDC_REDIRECT_URL` unset or empty, including in the invoking shell.
No OIDC client secret file is needed or mounted. The login page shows username
and password. Create the first Local admin using the one-shot procedure below.

### OIDC only

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=false
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
OCSERV_OIDC_ISSUER=https://id.example.com
OCSERV_OIDC_CLIENT_ID=ocservia
OCSERV_OIDC_REDIRECT_URL=https://controller.example.com/api/v1/auth/callback
```

Provision `oidc-client-secret` in `OCSERV_SECRET_DIR`. The login page automatically
redirects to SSO, without a Local password form. Local bootstrap is not used.
OIDC authentication does not itself grant administrative RBAC roles; retain the
existing identity/role provisioning and independent approval procedure.

### Local + OIDC

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=true
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
OCSERV_OIDC_ISSUER=https://id.example.com
OCSERV_OIDC_CLIENT_ID=ocservia
OCSERV_OIDC_REDIRECT_URL=https://controller.example.com/api/v1/auth/callback
```

Provision `oidc-client-secret` and bootstrap the first Local admin. The same
login page offers both the username/password form and **Sign in with SSO**.

## Configuration and secrets

Use `OCSERV_PUBLIC_HOST=controller.example.com` with the examples above. The
public origin must be HTTPS and must match the browser-facing gateway. The
template defaults origin to `https://OCSERV_PUBLIC_HOST` and session TTL to `8h`;
the supported TTL range is `1m` through `24h`. Local auth defaults to `false`,
preserving existing OIDC installations. Neither authentication method enabled
is rejected. Partial OIDC configuration is rejected even when Local is enabled.

The production templates set these **container** settings, not plaintext values
in `install.env`:

| Modes | Container setting | Protected host source |
| --- | --- | --- |
| All three | `OCSERV_SESSION_KEY_FILE=/run/secrets/session_key` | `OCSERV_SECRET_DIR/session-key` |
| OIDC only / dual-auth | `OCSERV_OIDC_CLIENT_SECRET_FILE=/run/secrets/oidc_client_secret` | `OCSERV_SECRET_DIR/oidc-client-secret` |

`session-key` contains an independently provisioned 32-byte key encoded as 64
lowercase hexadecimal characters. Both files follow the existing launcher-owned
`0444` file / private `0700` parent-directory contract. Never put production
passwords or secret values in Compose YAML, `install.env`, shell arguments or Git.
The `*_FILE` paths above are fixed by Compose; do not add them to `install.env`.

The installer and versioned Controller bootstrap accept and forward the mode,
origin, TTL and OIDC redirect settings, including across `--root-lifecycle`.
Explicit exported values override `install.env`, even if empty. Later lifecycle
and `compose.sh` commands require the same effective exported configuration;
they do not load `install.env` themselves. Keep all six digest-pinned image
settings from the verified release manifest for direct `compose.sh` commands.

`compose.sh` automatically adds `compose.oidc.yaml` when any OIDC setting is
nonempty, checks the OIDC secret permissions only in that case, and passes the
same authentication settings to migrations and the Controller. Use this launcher
rather than starting the base YAML alone. PostgreSQL credential rotation also
retains the OIDC overlay when recreating the Controller. The overlay is covered
by the existing deployment-descriptor rollback guard; do not bypass that guard
to roll back across this deployment change.

## OIDC provider setup

Retain Authorization Code flow with **PKCE S256**. Register the exact callback
`https://controller.example.com/api/v1/auth/callback` at the provider, configure
the HTTPS issuer and client ID, and deliver the client secret through its file.
If redirect is omitted, the production overlay defaults it to that callback on
`OCSERV_PUBLIC_HOST`.

The origin (scheme, host and effective port) of `OCSERV_OIDC_REDIRECT_URL` must
equal `OCSERV_PUBLIC_ORIGIN`. A mismatch fails production startup. In dual-auth,
Local does not make an invalid OIDC configuration acceptable. During an IdP
outage, existing sessions remain usable, new SSO logins fail closed, and Local
login remains available when enabled. The browser receives Secure, HttpOnly,
SameSite cookies, not OIDC tokens in browser storage.

## Bootstrap the first Local admin

There is **no default administrator password**. Bootstrap is a separate,
operator-invoked **one-shot**, not part of normal install or restart.

1. Complete the pinned Controller installation with Local enabled. Migrations
   must be applied, including `000031` and `000032`, and PostgreSQL must be ready.
   Use the installed release checkout and its effective exported production
   settings and verified image digests for the commands below.
2. Select an existing management workspace UUIDv7. Bootstrap does not create a
   workspace. On a completely empty database, an authorized database operator
   must first provision one through the protected administrative connection.
   For example, replacing the placeholder with a newly allocated UUIDv7:

   ```sql
   INSERT INTO workspaces (id, name, slug, created_at, updated_at)
   VALUES ('<management-workspace-uuidv7>', 'Administration', 'administration', now(), now());
   ```

   Record that UUID: the Local identity management workspace is fixed at
   bootstrap, not selected later by an API header.
3. Through your protected secret channel, provision a strong password in
   `OCSERV_SECRET_DIR/local-bootstrap-password`, as a launcher-owned regular
   file with mode `0444` inside the private `0700` secret directory. Reject
   symlinks; never put its contents in an environment variable or command line.
   The file is mounted read-only only for this command:

   ```bash
   deploy/production/compose.sh run --rm --no-deps \
     -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=initial-admin \
     -e OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID='<management-workspace-uuidv7>' \
     -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/local-bootstrap-password \
     -v "${OCSERV_SECRET_DIR}/local-bootstrap-password:/run/secrets/local-bootstrap-password:ro" \
     control-plane --bootstrap-local-admin
   ```

   The password must satisfy the Local new-password policy below. The existing
   secret-file format removes one trailing LF and then CR; other spaces and the
   password's case are preserved. Use a password from your secret manager.
   The command uses
   the application's database role, grants existing workspace-scoped
   `PlatformAdmin`, records the initialization and audit event atomically, and
   exits without starting listeners/workers or contacting the IdP.
4. After success, `--rm` removes the one-shot container and its secret mount.
   Delete the host bootstrap password file through your secret-management
   procedure and remove any temporary bootstrap settings or mounts. Verify
   Local login and the management workspace role. Establish separately owned
   approval authority using existing RBAC procedures before password resets
   are needed; bootstrap does not create a second approver.

Concurrent or subsequent bootstrap attempts are rejected. An existing Local
SecurityAdmin/PlatformAdmin also blocks initialization. Controller restart does
not read the bootstrap file or reset/synchronize passwords. Disabling the first
admin or resetting its password does not re-enable bootstrap. Preserve the
initialization marker in backups; do not delete it or roll back its migration
to recover an account.

## Local new-password policy

All Local password writes (trusted `CreateLocalCredential`, bootstrap, managed
user creation and administrator reset) use `ValidateNewPassword` through the
shared hashing function. Future self-service changes must use the same path.

- At least 15 Unicode code points, at most 1024 UTF-8 bytes; invalid UTF-8 is rejected.
- Long passwords, spaces and valid Unicode are supported. No uppercase, digit
  or symbol composition rules, and no periodic forced changes are added.
- Complete candidates are checked offline against an embedded common/breached
  list and service-related supplement. No substring dictionary rejection or
  online password checks are used. See the
  [list provenance, license and maintenance procedure](../../control-plane/internal/auth/password_blocklist/README.md).
- Password bytes are not trimmed, truncated, case-changed or Unicode-normalized
  by the policy or hashing functions. Only blocklist lookup is case-insensitive.
- Policy failures return a safe explanation (HTTP 400 for management endpoints),
  before database writes: no identity, credential, role, audit mutation, session
  revocation or reset-approval consumption occurs. Passwords/hashes are not logged.

Existing hashes still verify the original password bytes, including historical
short or now-blocklisted passwords. Login does not apply this new-setting policy
and continues returning uniform credential errors. A later reset must meet the
new policy. Argon2id, random salts, PHC parameters/limits and dummy verification
remain unchanged. The length and blocklist rules follow
[NIST SP 800-63B-4, section 3.1.1.2](https://pages.nist.gov/800-63-4/sp800-63b.html#passwordver).
Unicode normalization is deliberately deferred to preserve historical hashes.

## Local account failure backoff (R2)

Local login retains the existing per-IP request limit, per-process global
request budget, Argon2id concurrency ceiling and trusted-proxy source validation.
These are resource budgets, not password failure counters. OIDC and Break-glass
keep their own entry points and budgets, including the independent emergency
budget for a valid Break-glass credential.

Account admission uses PostgreSQL `local_auth_attempts`, keyed by the exact
Local username normalization (trim, lowercase, existing ASCII/input rules).
Known and unknown usernames follow the same policy:

| Parameter | Policy |
| --- | --- |
| Observation window | Fixed 15 minutes, starting with the first admitted attempt |
| Failure threshold | 5 observed incorrect passwords in that window |
| Cooldown after failure N | `min(300, 2^(N-5))` seconds for N >= 5 |
| Maximum cooldown | 5 minutes; no permanent automatic lockout |
| Concurrent account verification | One live 30-second lease across all Controllers |
| Successful login | Clear state atomically with session creation |
| Capacity | At most 16,384 live username rows per database |
| Database admission/completion timeout | 2 seconds each |

Cooldown and occupied-lease refusals do not change the deadline or count. They
return the same 401 problem as invalid credentials, without `Retry-After` or
account-existence details. Ordinary unknown-user verification still executes
dummy Argon2id; account-refused requests do not execute KDF. Existing IP/global
resource refusals continue to use 429 and `Retry-After`. A valid concurrent login
can therefore receive 401 while another request holds that account's lease.

Database and corrupt-hash errors are not password failures. A correct password
for a disabled identity cannot create a session and is not counted as incorrect.
Once an incorrect password is observed, completion uses a separate bounded
context so client cancellation cannot erase it. Cancellation before verification
releases the reservation without adding a failure. Process exit, ambiguous commit
or unavailable completion leaves at most the lease duration before admission can
recover; abandoned reservations are not silently classified as bad passwords.
Expired lease holders cannot clear replacement state or create a session.

Expiry is driven by the PostgreSQL clock. Admission deletes expired rows through
the expiry index before allocating capacity. Rows expire when the observation
window, cooldown and active lease no longer require them. Without login traffic,
expired rows may remain physically present until the next admission, but the
table remains bounded. Random usernames cannot evict live state: at capacity,
new keys receive generic 503 while tracked keys retain their protection.
Cleanup/admission/completion errors also fail closed with generic 503, never
unlimited verification. PostgreSQL autovacuum remains responsible for dead tuples.

Password reset and disable delete the account state in the existing credential,
revocation and audit transaction. Rollback preserves it. Credential revalidation
and lease-token matching prevent an older verified request from clearing failures
or leases created after that transaction commits.

Migration 000033 creates the table and expiry index. The normal migration runner
grants only SELECT/INSERT/UPDATE/DELETE on this table to the runtime role; Owner
or TRUNCATE permissions are not needed. The minimum compatible Controller schema
is 33 because older binaries do not enforce this policy. Drain/stop older Local
login instances before migration and replacement; do not mix old and new login
handlers. Reverting this migration removes backoff state and requires stopping
schema-33 Controllers and the normal coordinated schema/metadata rollback.

This is bounded account protection, not a solution to targeted denial of login:
an attacker can still cause temporary account cooldown, and a saturated table
temporarily denies new keys. It intentionally retains IP protection alongside
account protection, as discussed in the OWASP
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#login-throttling)
and [Credential Stuffing Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Credential_Stuffing_Prevention_Cheat_Sheet.html).

## Local identity and sessions

Local passwords are stored as **Argon2id** hashes. Local identities use
`issuer=local`; Local and OIDC identities never automatically merge, even when
usernames or email addresses match. These are platform login accounts, not VPN
accounts on managed nodes. There is no self-registration or automatic linking.

Local user administration uses existing RBAC (`local_user.manage`) in the fixed
management workspace, not a new role system. Creating a user does not grant roles.
Password reset needs the existing independent approval flow. See
[identity administration](../development/identity-authorization-audit.md#initial-local-administrator-and-lifecycle-p4)
for endpoints and permission details.

Disabling a user or resetting its password revokes **all sessions for that
identity** in the same transaction. Disabled users cannot authenticate; password
reset does not re-enable them. A concurrent login cannot issue a usable stale
session after the change commits. Already-authorized in-flight requests are not
retroactively cancelled.

## Break-glass and deployment checks

Local authentication is **not a replacement for Break-glass**. Break-glass remains
an independent emergency offline-credential access mechanism, with mandatory
credential **rotation**, critical **alert**, **audit** event and a **15-minute
short session**. Keep its offline credential and response procedure separate
from Local administrator passwords. See the existing
[Break-glass contract](../development/identity-authorization-audit.md#break-glass).

Before activation, with the effective configuration exported, run:

```bash
deploy/production/compose.sh config --quiet
```

This validates Compose and secret metadata, not secret contents or IdP reachability.
Controller startup performs full authentication configuration validation. Verify
the intended login options over HTTPS, successful login and logout, RBAC and
session revocation after installation. Do not print secret contents for diagnosis.
Repository regression checks are `scripts/test-production-auth-config.sh`,
`scripts/docs-check.sh`, and the targeted Go config tests; run local validation
on `BuildServer` as required by `AGENTS.md`.
