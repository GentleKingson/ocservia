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

Set `OCSERV_OIDC_ISSUER` to the provider's exact issuer, including any trailing
slash, port and path. It is an identity identifier, not a URL to normalize.
Discovery metadata and ID Token `iss` must match it; the OIDC library constructs
the Discovery request URL without changing the identity identifier. Do not use
email or username to link accounts or disable issuer verification to fix a mismatch.

### Correcting a historical issuer configuration

An already working no-trailing-slash issuer needs no configuration or data
change: its `(issuer, subject)` key, identity ID, role bindings and sessions remain
unchanged. The former application code removed one final slash before Discovery.
If the provider actually advertised the slash, strict Discovery validation failed
before login or identity insertion. Such a deployment does not need an identity
migration merely because it encountered this bug. An ID Token issuer mismatch
also failed before insertion.

If Discovery and tokens instead both used the no-slash value, earlier logins
could have succeeded under that value, even when the configured value ended in
a slash. Confirm the provider's authoritative issuer before changing configuration;
if it is still no-slash, configure that exact value. Import/manual provisioning or
an actual provider issuer change can also leave historical rows. Never infer
their presence or ownership from the configuration alone.

Changing an existing row's issuer changes the unique `(issuer, subject)` key.
The slash and no-slash values are distinct accounts; the application neither
merges nor migrates them. If a separately approved migration is genuinely needed:

1. In a maintenance window, stop login/session writes for the affected deployment.
   Record a change ticket, operator, approver, exact old/new issuers and an explicit
   list of identity IDs and subjects. Obtain independent IdP evidence that each
   subject still identifies the same person; matching email/name is insufficient.
2. Take a restorable database backup and export the selected identity rows,
   role bindings (including workspace/resource scope), active sessions and other
   identity-ID references. Keep the original configuration and a before/after
   manifest in the restricted change record, not tokens or client secrets.
3. Read both issuer populations and check target-key conflicts, including disabled
   identities. For each approved subject, query `identities` for both exact issuer
   values. Review `role_bindings` by identity ID with the security owner. Any
   existing target `(issuer, subject)` is a stop condition, not permission to merge,
   delete the target, or transfer roles.
4. Rehearse on a restored isolated database. In an explicit transaction, lock
   `identities` against concurrent writes, repeat the conflict/ownership checks,
   and update only approved IDs whose old issuer and subject still match the
   manifest. Require exactly the approved row count. Preserve IDs, subjects,
   disabled state and roles; revoke affected active sessions and require fresh
   login. Roll back on any discrepancy. Record the committed before/after mapping
   through the approved operational audit process; do not rewrite historical audit
   events. Apply the exact new issuer configuration through normal change control.
5. Verify fresh Discovery/token validation, unchanged role ownership, and no
   duplicate identities before reopening login. For rollback, stop writes again,
   verify the manifest and absence of old-key conflicts, then transactionally
   restore only the mapped issuers and original configuration. Revoke sessions
   issued during the migration window; never reactivate revoked sessions. Stop for
   manual review if identities/roles have since changed, rather than overwriting
   newer data. Retain rollback evidence with the same change ticket.

These are controlled operator steps, not a login-time fallback. R5 does not run
them against production or assume that any deployment requires them.

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
   must be applied through `000034`, and PostgreSQL must be ready. Stop older
   Controllers before migration; schema 34 intentionally rejects older binaries.
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
3. Assign the administrator and approver to **different responsible people**.
   Two identity IDs alone do not establish personnel independence. Deliver two
   different passwords separately through the secret manager, in
   `OCSERV_SECRET_DIR/local-bootstrap-password` and
   `OCSERV_SECRET_DIR/local-bootstrap-approver-password`. Both files must be
   owned by the one-shot process UID, mode `0400` or `0600`, within a private
   `0700` directory. Final-path symlinks, hard links and group/world-readable files are
   rejected. Never put password contents in environment variables, command
   arguments, shell tracing or logs. Mount both read-only only for this command:

   ```bash
   # The repository's production control image runs as 65534:65532.
   sudo chown 65534:65532 \
     "${OCSERV_SECRET_DIR}/local-bootstrap-password" \
     "${OCSERV_SECRET_DIR}/local-bootstrap-approver-password"
   sudo chmod 0400 \
     "${OCSERV_SECRET_DIR}/local-bootstrap-password" \
     "${OCSERV_SECRET_DIR}/local-bootstrap-approver-password"
   deploy/production/compose.sh run --rm --no-deps \
     -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=initial-admin \
     -e OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID='<management-workspace-uuidv7>' \
     -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/local-bootstrap-password \
     -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME=initial-approver \
     -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE=/run/secrets/local-bootstrap-approver-password \
     -v "${OCSERV_SECRET_DIR}/local-bootstrap-password:/run/secrets/local-bootstrap-password:ro" \
     -v "${OCSERV_SECRET_DIR}/local-bootstrap-approver-password:/run/secrets/local-bootstrap-approver-password:ro" \
     control-plane --bootstrap-local-admin
   ```

   The password must satisfy the Local new-password policy below. The existing
   secret-file format removes one trailing LF and then CR; other spaces and the
   password's case are preserved. Use a password from your secret manager.
   The command uses
   the application's database role, grants existing workspace-scoped
   `PlatformAdmin` to the administrator and only existing `SecurityAdmin` to the
   approver in that same workspace. Both identities, credentials, role bindings,
   completion marker and audit events commit atomically. The command
   exits without starting listeners/workers or contacting the IdP.
4. After success, `--rm` removes the one-shot container and its secret mount.
   Delete both temporary host bootstrap files through your secret-management
   procedure and remove any temporary bootstrap settings or mounts. Verify
   both Local logins and their management workspace roles. Keep their durable
   credentials separately controlled. The approver cannot create/reset Local
   users or grant PlatformAdmin alone; it can independently approve the
   administrator's content-bound role grants and password resets.

Concurrent or subsequent bootstrap attempts are rejected. An existing Local
SecurityAdmin/PlatformAdmin also blocks initialization. Controller restart does
not read the bootstrap file or reset/synchronize passwords. Disabling the first
admin or resetting its password does not re-enable bootstrap. Preserve the
initialization marker in backups; do not delete it or roll back its migration
to recover an account.

### Upgrading a single-administrator installation

Migration 34 records `completion_pending=true` only for the original active
Local bootstrap PlatformAdmin, with a matching bootstrap audit event and no
other historical SecurityAdmin/PlatformAdmin binding in the fixed workspace.
Disabled bindings also close eligibility. All other existing deployments are
marked closed without changing identities, credentials or roles. No marker or
no verifiable original admin means no automatic recovery exception.

Inspect the marker using the protected administrative database connection:

```sql
SELECT identity_id, workspace_id, completion_pending, completed_at,
       approver_identity_id FROM local_auth_bootstrap;
```

For an eligible deployment, run the **same one-shot command and two mounts
above**, replacing `--bootstrap-local-admin` with `--complete-local-bootstrap`.
Set the original administrator username and fixed workspace. The administrator
Secret file must contain its **current** password, not a replacement; the other
file contains the new, separately delivered approver password. The command
verifies the original credential under Local attempt protection and rechecks
the frozen state while holding the initialization lock. It creates only a new
SecurityAdmin identity, never modifies an existing password, and consumes the
completion opportunity atomically with role and audit. Repeated/concurrent
completion fails closed. Later elevated grants close pending completion too.

An installation with existing independent authority needs **no** completion
command. Losing that authority later does not reopen initialization. Normal
Controller startup and login never create a workspace, an account or a grant.

### Approval and password operations

Use the authenticated API client over HTTPS with the exact trusted `Origin`.
`POST /api/v1/local-users` creates a Local identity with no roles. To elevate it,
the administrator submits `POST /api/v1/approval-requests` with
`action=role_binding.elevate`, `resource_type=role_binding`, the new identity as
`resource_id`, and `role_binding={identity_id,role,resource_type:"workspace"}`.
Use the fixed `X-Workspace-ID`, a reason and `ttl_seconds` (60..86400). The
separate approver reviews the returned content and submits
`POST /api/v1/approval-requests/{id}:approve` with `reason` and
`expected_request_hash`. The administrator then submits `POST /api/v1/role-bindings`
with the same identity/role/scope, `workspace_id`, `reason` and `approval_id`.
Requester self-approval remains forbidden, including after bootstrap.

For self-service, provision a private JSON request file through your secret
manager containing only `current_password` and `new_password`. With a private
cookie jar from Local login, execute:

```sh
curl --fail-with-body --silent --show-error \
  --cookie /run/secrets/local-session.cookies \
  --cookie-jar /run/secrets/local-session.cookies \
  -H "Origin: ${OCSERV_PUBLIC_ORIGIN}" -H 'Content-Type: application/json' \
  --data-binary @/run/secrets/change-password.json \
  "${OCSERV_PUBLIC_ORIGIN}/api/v1/auth/change-password"
```

Success is **204 and mandatory re-login**: every old session is revoked, not
just the current cookie. Do not submit an identity ID or workspace. Incorrect
current passwords share the login account's backoff; OIDC and Break-glass
sessions cannot use this endpoint. Policy rejection is 400; invalid/stale Local
credentials or a blocked attempt is 401. Remove the temporary request file.
Administrator `:reset-password` still requires the independent, one-use
`local_user.reset-password` approval and `X-Approval-ID`; reset does not enable
a disabled account.

### Management protection and recovery

Disable returns 409 rather than removing the last active Local workspace
PlatformAdmin or reducing Local workspace approval authority below two distinct
active identities. Only matching Local credentials, non-disabled identities and
workspace-wide bindings in the fixed management workspace count. Other
workspaces, narrower/historical bindings, OIDC and Break-glass do not substitute
for these durable Local recovery anchors. Checks and disable serialize in one
transaction; two administrators cannot bypass this by disabling each other.
The current application has no role-binding deletion or identity deletion API.
Direct database administration remains a trusted, externally controlled boundary.

Before disabling a protected account, create a replacement through the normal
Local API, obtain an independent elevated-role approval, verify its login under
the new responsible person's control, then retry disable. If one password is
lost, use the surviving administrator and an independent approver to perform
the approved reset. A separately governed, already enabled Break-glass session
can request/execute a reset with a surviving SecurityAdmin's approval; it cannot
self-approve and must follow the existing rotation/incident policy.

If no usable administrator/independent approver pair remains, stop writes,
preserve incident evidence and follow the authorized
[backup/PITR recovery procedure](postgres-pitr-restore.md) to a verified state
with independently controlled credentials. Keep all initialization markers and
audit history. Before reopening access, revoke restored sessions using the
protected administrative connection:

```sql
UPDATE auth_sessions SET revoked_at=now() WHERE revoked_at IS NULL;
```

Record this offline recovery in the incident/change record, verify both logins
and approval separation, and rotate any exposed credentials through the normal
flows. No generic account-unlock CLI or anonymous HTTP recovery endpoint is
introduced. **Never delete/edit the Bootstrap marker to reinitialize.**

## Local new-password policy

All Local password writes (trusted `CreateLocalCredential`, bootstrap, managed
user creation, self-service change and administrator reset) use
`ValidateNewPassword` through the shared hashing function.

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

## Authentication security logs (R6)

Local login and OIDC start/callback reuse the Controller's structured `slog`
output. These routes replace the duplicate `http request` entry with one
`auth.result` per outcome (subject to sampling below). `started` means an OIDC
redirect was prepared, not that a session exists. `succeeded` is emitted only
after successful session creation. No production logging configuration changes
or external logging service are required.

| Field | Contract |
| --- | --- |
| `event` | `auth.result` or `auth.summary` |
| `auth_method` | `local` or `oidc` |
| `outcome` | `succeeded`, `rejected`, `unavailable`, or `started` |
| `reason_code` | Fixed internal classification from the table below; never returned to clients |
| `request_id` | Result only; existing request correlation, at most 128 printable ASCII bytes; invalid incoming IDs are replaced |
| `source_ip` | Result only; `authSource` peer/trusted `X-Ocservia-Client-IP` policy, never arbitrary `X-Forwarded-For`; at most 64 bytes |
| `identity_id` | UUID only on successful Local/OIDC session creation |
| `account_ref` | Local only after complete input decoding; normalized username HMAC, 67 bytes; invalid usernames share `invalid` |
| `suppressed_count`, `window_seconds` | Summary only; number of omitted results for that method/outcome/reason in the preceding 60-second window |

| Paths | Reason codes |
| --- | --- |
| Local success / credential denial | `session_created`, `credentials_rejected` |
| Local account admission | `account_limited` (cooldown or occupied lease), `account_capacity` |
| Local infrastructure | `infrastructure_failure` (DB, hash verification, attempt completion, or session creation failure) |
| Local input boundary | `disabled`, `origin_rejected`, `invalid_request` |
| OIDC start | `disabled`, `start_failed`, `redirect_created` |
| OIDC callback | `disabled`, `state_rejected`, `callback_rejected`, `session_created` |
| Both methods' resource admission | `source_limited`, `global_limited`, `concurrency_limited`, `source_capacity` |

Unknown users, wrong passwords and disabled accounts retain the same credential
response; account cooldown still returns that same 401 without Retry-After.
OIDC callback errors remain generic. Raw upstream errors are never logged: they
can contain response bodies, tokens or URLs. No request bodies, passwords/hashes,
session values/Cookies, OIDC codes/tokens, client secrets, connection strings or
callback URLs enter these events. Request-derived output is length-bounded,
control/non-ASCII bytes are replaced, and the existing JSON handler encodes it.

`account_ref` is a pseudonym, **not anonymous data** or a bare username hash.
HMAC-SHA-256 derives a log-only key from the session key with domain
`ocservia/auth-log/account-key/v1\0`; account HMAC uses the separate domain
`ocservia/auth-log/local-account/v1\0` and the credential normalizer. References
correlate across Controllers sharing that key and change on key rotation. Treat
references and source addresses as restricted security data; knowing the key
allows username enumeration. Do not expose logs or keys to login clients.

Each Controller samples the first **10 results per fixed category per 60-second
window**, including successes, with a saturating suppressed counter. There are
22 fixed categories: at most 220 result entries plus 22 summaries per window,
constant memory, no per-account/IP log map, queue or timer. The next authentication
event after expiry flushes summaries; idle periods delay them, and process exit
loses pending counts. Summaries carry no individual request/account attribution.
Success/start uses Info; refusals, failures and summaries use Warn, not Debug.
This bounds application authentication output, not gateway or unrelated access
logs. Keep existing host/container rotation, retention and access protections;
this change does not alter them. Logging errors/panics are best effort and do not
change authentication or authorization; a blocked output sink can still delay
requests. Sampling does not bypass the independent authentication/KDF budgets.

These events are separate from `audit_events` and `security_alerts`. Wrong
passwords do not enter workspace audit transactions. Password changes, account
administration, approvals and Break-glass retain their existing transactional
audit/alert requirements. The exclusions and bounded logging follow the
[OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html).

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
