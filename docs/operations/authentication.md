# Production authentication

Production supports **Local only**, **OIDC only**, and **Local + OIDC**. OIDC is
not mandatory when Local authentication is enabled. Use the normal
[Controller installation](../getting-started/production.md) and
[production secret permissions](production-deployment.md#production-secrets)
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
they do not load `install.env` themselves. Keep the selected image
settings from the validated deployment configuration for direct `compose.sh` commands.

`compose.sh` automatically adds `compose.oidc.yaml` when any OIDC setting is
nonempty, checks the OIDC secret permissions only in that case, and passes the
same authentication settings to migrations and the Controller. Use this launcher
rather than starting the base YAML alone. PostgreSQL credential rotation also
retains the OIDC overlay when recreating the Controller. Rollback resolves the
target's deployment files and must satisfy its actual
authentication settings and secret mounts; descriptor equality is not a gate.

## Local login from a terminal and 401 diagnosis

The Web login form and terminal client use the same Local endpoint:
`POST /api/v1/auth/login`, JSON fields `username` and `password`, and
`Content-Type: application/json`. Supply the exact configured HTTPS
`OCSERV_PUBLIC_ORIGIN` as `Origin`; a missing or different origin is rejected.
OIDC login instead starts with `GET /api/v1/auth/login`. Local login does not
accept an OIDC token or workspace field in its JSON body.

Run this on the administrator's workstation or Controller host, with an existing
Local account. The password is prompted without echo and never appears in shell
history, arguments or diagnostic output. The private request, response headers
and cookie jar are removed on exit. Do not use `curl -v`, tracing, or `set -x`;
headers can contain session cookies. Do not transfer the administrator cookie jar
to a managed Node.

```sh
(
  set -eu
  umask 077
  : "${OCSERV_PUBLIC_ORIGIN:?set the exact configured HTTPS public origin}"
  case "${OCSERV_PUBLIC_ORIGIN}" in https://*) ;; *) exit 2 ;; esac
  auth_tmp="$(mktemp -d)"
  trap 'rm -rf -- "${auth_tmp}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  python3 - "${auth_tmp}/login.json" <<'PYTHON'
import getpass
import json
import sys
with open("/dev/tty", encoding="utf-8") as terminal:
    sys.stdin = terminal
    username = input("Local username: ")
    password = getpass.getpass("Local password: ")
with open(sys.argv[1], "w", encoding="utf-8") as output:
    json.dump({"username": username, "password": password}, output)
del password
PYTHON
  login_status="$(curl --silent --show-error \
    --output "${auth_tmp}/login-result.json" \
    --dump-header "${auth_tmp}/login-headers" \
    --cookie-jar "${auth_tmp}/session.cookies" \
    --write-out '%{http_code}' \
    -H "Origin: ${OCSERV_PUBLIC_ORIGIN}" -H 'Content-Type: application/json' \
    --data-binary "@${auth_tmp}/login.json" \
    "${OCSERV_PUBLIC_ORIGIN}/api/v1/auth/login")"
  rm -f -- "${auth_tmp}/login.json"
  printf 'login POST: HTTP %s\n' "${login_status}"
  awk 'tolower($1) == "x-request-id:" {print "login Request-ID:", $2}' \
    "${auth_tmp}/login-headers"
  [ "${login_status}" = 204 ] || exit 1
  workspace_status="$(curl --silent --show-error \
    --cookie "${auth_tmp}/session.cookies" \
    --output "${auth_tmp}/workspaces.json" \
    --dump-header "${auth_tmp}/workspace-headers" \
    --write-out '%{http_code}' \
    "${OCSERV_PUBLIC_ORIGIN}/api/v1/workspaces")"
  printf 'authenticated workspaces GET: HTTP %s\n' "${workspace_status}"
  awk 'tolower($1) == "x-request-id:" {print "workspaces Request-ID:", $2}' \
    "${auth_tmp}/workspace-headers"
  [ "${workspace_status}" = 200 ] || exit 1
  cat "${auth_tmp}/workspaces.json"
)
```

A `204` login response establishes the session; the following authenticated GET
checks that the client retained it. Use an authorized returned workspace ID in
`X-Workspace-ID` for workspace-scoped APIs. Keep `Origin` on later browser-session
mutations; login success does not grant a role or permission in every workspace.
Do not keep retrying bad requests while an account/source cooldown is active.

| Failure stage | Check and correlate |
| --- | --- |
| Login POST 400/415 | Exact URL/method, JSON field names/types, Content-Type; internal `invalid_request` |
| Login POST 403 | Exact Origin and request context; internal `origin_rejected` |
| Login POST 401 | Generic credentials rejection or account protection; distinguish internal `credentials_rejected` / `account_limited` using the login Request-ID |
| Login POST 429/503 | Admission limits, capacity or infrastructure; inspect the existing internal reason codes |
| Login 204, protected API 401 | That API's Request-ID, cookie retention, HTTPS host/path, session expiry/revocation; this is a separate request from login |
| Protected API 403 | Principal role, selected workspace and resource scope; authentication success alone is insufficient |

The server returns `X-Request-ID`; use it to correlate the fixed, redacted
`auth.result.reason_code` fields in [Authentication security logs](#authentication-security-logs-r6).
Sampling can suppress individual events; inspect `auth.summary` counts too.
Public errors deliberately do not disclose whether an account exists. Never send
passwords, cookie values, request bodies or raw response headers in an incident
report. Record the stage, status, request ID and relevant non-secret request
shape; a status code alone does not identify the cause.

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

The origin (scheme, host and effective port) of `OCSERV_OIDC_REDIRECT_URL` must
equal `OCSERV_PUBLIC_ORIGIN`. A mismatch fails production startup. In dual-auth,
Local does not make an invalid OIDC configuration acceptable. During an IdP
outage, existing sessions remain usable, new SSO logins fail closed, and Local
login remains available when enabled. The browser receives Secure, HttpOnly,
SameSite cookies, not OIDC tokens in browser storage.

### Correcting a historical issuer configuration

Use the provider's authoritative issuer exactly, including its trailing slash.
Before changing an existing installation, inspect stored `(issuer, subject)`
values: failed Discovery/token validation created no identity, and configuration
alone does not prove historical rows exist. Working identities need no migration.
The application never merges slash/no-slash accounts. If stored identities must
change, follow [OIDC issuer recovery](incident-recovery.md#oidc-issuer-correction)
with independent subject-ownership evidence and explicit migration approval.

## Bootstrap the first Local admin

There is **no default administrator password**. Bootstrap is a separate,
operator-invoked **one-shot**, not part of normal install or restart.

1. Complete the pinned Controller installation with Local enabled and a
   [supported database deployment](production-deployment.md#database-support).
   Apply the installed release's full migration history, not just the historical
   Local-auth migrations. Stop incompatible older Controllers before migration.
   Confirm the guarded installation's migration process exited successfully
   (`database migrations complete`) and the Controller is ready.
   Use the installed release checkout and its effective exported production
   settings and verified image digests for the commands below.
2. Select an existing management workspace UUIDv7. Bootstrap does not create a
   workspace. On a completely empty database, an authorized database operator
   must first provision one through the protected administrative connection.
   Follow the matching [database preparation](#database-preparation) below;
   the PostgreSQL SQL is not portable to MySQL.
   Record that UUID: the Local identity management workspace is fixed at
   bootstrap, not selected later by an API header.
3. Establish **independently controlled requester and approver principals**:
   distinct identities, credentials and authenticated sessions, with separate
   RBAC authorization. Never share a credential or substitute a second identity
   ID for authentication. Baseline 1.0 requires this principal separation, not
   organizational custody by two people. Deployments requiring a two-person
   rule must additionally assign the credentials to different responsible
   people and validate custody under their production-hardening or enterprise
   security profile; automated two-principal tests do not prove that property.
   Deliver two different passwords separately through the secret manager, in
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

### Database preparation

Use an authorized, protected administrative connection to the installed
database, with verified TLS for an external server and credentials from the
secret manager, never command-line passwords. The owner connection is for
provisioning/migration, not for the running Controller or bootstrap command.
Do not create another workspace when the intended management workspace exists.
For an empty database, allocate a fresh UUIDv7 outside these SQL examples and
replace `<management-workspace-uuidv7>` consistently; do not use a server UUID
function that generates a different UUID version.

#### PostgreSQL

Inspect the verified schema journal:

```sql
SELECT epoch, revision, encode(checksum, 'hex') AS checksum, state, step
FROM schema_revisions ORDER BY epoch, revision;
```

Owner initialization validates the supported epoch/revision, exact artifact
receipts and actual schema before applying forward SQL. Unknown or altered
history is refused; do not rewrite the journal. The previous-checkpoint window
is fixed to v1.2.0, epoch 1 / revision 0. Readiness checks current reads and
permissions; it does not use a legacy migration filename counter.

PostgreSQL stores the workspace ID as native `uuid` and the times as
`timestamptz`. On an empty database, provision the workspace:

```sql
INSERT INTO workspaces (id, name, slug, created_at, updated_at)
VALUES ('<management-workspace-uuidv7>', 'Administration', 'administration', now(), now());
```

#### MySQL

Inspect the sole schema journal after owner migration succeeds:

```sql
SELECT epoch, revision, checksum, state, step
FROM schema_revisions ORDER BY epoch, revision;
```

Every required row must be `verified` and match the installed SQL artifacts.
Fresh epoch-2 initialization records revision 1; a checkpoint upgrade verifies
revision 0 before journaled cleanup revision 1. A `running` row requires exact
artifact repair rather than manual editing. Owner migration verifies actual
schema and runtime privileges. Runtime startup checks current telemetry
structures and permissions; readiness checks core reads and stream health.
Epoch/revision values are backend artifact identities, not PostgreSQL legacy
migration numbers or a Controller compatibility range.

At the current migrated schema, workspace IDs are `VARBINARY(16)` with an exact
16-byte length constraint, using unswapped RFC UUID byte order.
Workspace times are signed `BIGINT` microseconds since
`2000-01-01 00:00:00 UTC`, not Unix seconds or SQL date/time strings.
Use the same UUIDv7 text later in `OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID`:

```sql
SET @workspace_id = UNHEX(REPLACE('<management-workspace-uuidv7>', '-', ''));
SET @created_at = TIMESTAMPDIFF(MICROSECOND, '2000-01-01 00:00:00', UTC_TIMESTAMP(6));
INSERT INTO workspaces (id, name, slug, created_at, updated_at)
VALUES (@workspace_id, 'Administration', 'administration', @created_at, @created_at);
```

Do not use swapped UUID bytes, `now()` directly in BIGINT columns, or this
current-schema example on an incompletely migrated historical database.
After either branch, return to step 3 of the shared bootstrap procedure.

### Upgrading a single-administrator installation

The historical PostgreSQL migration `000034` records `completion_pending=true`
only for the original active
Local bootstrap PlatformAdmin, with a matching bootstrap audit event and no
other historical SecurityAdmin/PlatformAdmin binding in the fixed workspace.
Disabled bindings also close eligibility. All other existing deployments are
marked closed without changing identities, credentials or roles. No marker or
no verifiable original admin means no automatic recovery exception.
MySQL does not run that PostgreSQL migration; its initialized schema
includes the two-principal contract. Use the stored eligibility marker, not a
backend revision number, to decide whether completion is allowed.

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
preserve incident evidence and follow [Local authority recovery](incident-recovery.md#lost-local-administration-authority).
Preserve initialization markers and audit history; never delete or edit the
bootstrap marker to reinitialize. Recovery includes revoking restored sessions
before access reopens.

## Local new-password policy

Bootstrap, managed user creation, self-service password changes and administrator
resets share the same new-password policy:

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

## Authentication request budgets and proxy trust

OIDC login and callback each allow 30 requests per source per minute, 120 total
requests per minute, and 8 in-flight requests per API process. Their budgets
are independent, so starting logins cannot consume callback capacity. Excess
requests receive `429` with `Retry-After`; no account is persistently locked.
Each source table holds at most 4096 addresses and retains live windows rather
than evicting them to give an attacker fresh capacity. These are fixed one-minute
windows, not rolling quotas. Multiple API replicas multiply the limits; keep
edge protection for distributed floods.

Local login separately allows 5 requests per source per minute, 120 total
requests per minute and 4 concurrent requests per API process. These limits
apply even to correct passwords; account backoff below is a separate boundary.

The shipped Caddy overwrites `X-Ocservia-Client-IP` with its direct peer address.
The API accepts this single IP only from configured trusted peers; it ignores
client-supplied `X-Forwarded-For`. Do not trust the entire shared application
network or publish the Controller's port.

Outside production Compose, the API still defaults to trusting no proxy.
Requests then share the directly connected peer's budget, including all users
behind an unconfigured gateway. If another proxy is placed in
front of Caddy, its clients share that proxy's budget; enforce client-level
limits there rather than trusting arbitrary forwarded addresses.

Break-glass has its own 4-request in-flight budget and permits 5 invalid token
attempts per source per minute. A matching enabled emergency credential bypasses
the failed-request rate/table limits, but never bypasses origin validation,
rotation enforcement, auditing, or session creation checks. OIDC outages or
exhausted OIDC budgets therefore do not consume emergency capacity. Keep the
offline credential available and alert on authentication `429` responses through
the existing HTTP telemetry. Rate limiting does not replace an external DDoS
control or make a stolen emergency credential safe.

For gateway addresses and subnet overrides, see
[deployment networking](production-deployment.md#authentication-request-budgets).

<a id="local-account-failure-backoff-r2"></a>
## Local account failure backoff

Account backoff is shared across Controllers and is independent of the
[per-process request budgets](#authentication-request-budgets-and-proxy-trust).

Account admission uses the selected backend's `local_auth_attempts`, keyed by the exact
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

Database or corrupt-hash failures are not incorrect passwords. A correct password
for a disabled identity still cannot create a session. At username capacity,
new keys receive generic 503 while tracked keys retain protection; live state is
never evicted to admit new names. Infrastructure errors also fail closed with 503.
Abandoned verification leases expire after at most 30 seconds. Reset and disable
clear account state transactionally. Stop affected Local login instances before
replacement, preserve backoff state and verify authentication before resuming service. Binary rollback does not undo it.
See [attempt completion and expiry](../development/identity-authorization-audit.md#attempt-completion-and-expiry)
for cancellation, lease fencing and database-clock behavior.

This is bounded account protection, not a solution to targeted denial of login:
an attacker can still cause temporary account cooldown, and a saturated table
temporarily denies new keys. It intentionally retains IP protection alongside
account protection, as discussed in the OWASP
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#login-throttling)
and [Credential Stuffing Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Credential_Stuffing_Prevention_Cheat_Sheet.html).

<a id="authentication-security-logs-r6"></a>
## Authentication security logs

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
| OIDC callback | `disabled`, `state_rejected`, `callback_rejected`, `session_created`; post-validation session/DB failure: `infrastructure_failure` (`unavailable`) |
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
23 fixed categories: at most 230 result entries plus 23 summaries per window,
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
[identity administration](../development/identity-authorization-audit.md#local-initialization-and-lifecycle-r4)
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
For documentation and implementation changes, use the appropriate checks in
[Validate a change](../development/testing.md) in an authorized isolated environment.
The focused deployment configuration check is `scripts/test-production-auth-config.sh`.
