# Production authentication

Production deployments support three authentication modes: Local only, OIDC
only, and Local + OIDC (OIDC is optional when Local authentication is enabled).
Follow the standard [Controller installation](../getting-started/production.md)
and [production secret permissions](production-deployment.md#production-secrets)
for all three modes. The snippets below replace only the authentication section
of `install.env`, leaving the database, TLS, relay, signer, backup, and release
settings as configured for your manual Standalone or Integrated deployment on
any supported database. Because Quick mode (`controller-bootstrap.sh --quick`)
always configures Local-only authentication, ignores `./install.env`, and
creates the first two Local administrators automatically, use a manual
installation whenever you need OIDC.

## Choose a mode

### Local only

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=true
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
```

Leave `OCSERV_OIDC_ISSUER`, `OCSERV_OIDC_CLIENT_ID`, and
`OCSERV_OIDC_REDIRECT_URL` unset or empty in both `install.env` and the
invoking shell. No OIDC client secret file is required or mounted, and the
login page renders only the username and password form. In a manual
installation, create the initial Local administrator using the one-shot
bootstrap procedure below.

### OIDC only

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=false
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
OCSERV_OIDC_ISSUER=https://id.example.com
OCSERV_OIDC_CLIENT_ID=ocservia
OCSERV_OIDC_REDIRECT_URL=https://controller.example.com/api/v1/auth/callback
```

Provision `oidc-client-secret` in `OCSERV_SECRET_DIR`. With Local
authentication disabled, the login page redirects straight to SSO without
showing a password form, and Local bootstrap is unused. Because OIDC login
authenticates identity without granting administrative RBAC roles on its own,
continue to use the standard identity and role provisioning workflow with
independent approval.

### Local + OIDC

```dotenv
OCSERV_LOCAL_AUTH_ENABLED=true
OCSERV_PUBLIC_ORIGIN=https://controller.example.com
OCSERV_SESSION_TTL=8h
OCSERV_OIDC_ISSUER=https://id.example.com
OCSERV_OIDC_CLIENT_ID=ocservia
OCSERV_OIDC_REDIRECT_URL=https://controller.example.com/api/v1/auth/callback
```

Provision `oidc-client-secret` and, on a manual installation, bootstrap the
first Local administrator. The login page then presents both the username and
password form and the Sign in with SSO button.

## Configuration and secrets

Pair the examples above with `OCSERV_PUBLIC_HOST=controller.example.com`. The
configured `OCSERV_PUBLIC_ORIGIN` must use HTTPS and match the browser-facing
gateway; the template defaults it to `https://OCSERV_PUBLIC_HOST` and defaults
`OCSERV_SESSION_TTL` to `8h` (accepted range `1m` through `24h`).
`OCSERV_LOCAL_AUTH_ENABLED` defaults to `false` so existing OIDC installations
keep their behavior unless changed. Validation rejects configurations where
both authentication methods are disabled, and rejects incomplete OIDC settings
even when Local authentication is enabled.

The production Compose templates mount secrets from the host into fixed
container paths instead of reading plaintext values from `install.env`:

| Modes | Container setting | Protected host source |
| --- | --- | --- |
| All three | `OCSERV_SESSION_KEY_FILE=/run/secrets/session_key` | `OCSERV_SECRET_DIR/session-key` |
| OIDC only / dual-auth | `OCSERV_OIDC_CLIENT_SECRET_FILE=/run/secrets/oidc_client_secret` | `OCSERV_SECRET_DIR/oidc-client-secret` |

`session-key` holds an independently generated 32-byte key encoded as 64
lowercase hexadecimal characters. Both secret files follow the launcher-owned
`0444` file mode inside a private `0700` parent directory. Never place
production passwords or secret values in Compose YAML, `install.env`, shell
arguments, or Git, and do not add the fixed `*_FILE` variables above to
`install.env`. The Controller checks only that each `*_FILE` path is an
absolute regular file, so Docker and Compose secret mounts keep working; host
file protection comes from `compose.sh`, which rejects other modes, owners and
parent directories. A custom launcher must keep the same private parent
directory and must not mount these files from a shared location.

Both the production installer and the versioned Controller bootstrap accept and
forward the authentication mode, origin, session TTL, and OIDC redirect
variables, including across `--root-lifecycle`. Explicitly exported shell
variables override `install.env`, even when exported as empty strings. Because
subsequent lifecycle commands and direct `compose.sh` invocations do not read
`install.env`, export the same effective configuration (along with the image
settings from the validated deployment configuration) whenever running them.

When any of `OCSERV_OIDC_ISSUER`, `OCSERV_OIDC_CLIENT_ID`, or
`OCSERV_OIDC_REDIRECT_URL` is nonempty, `compose.sh` automatically includes
`compose.oidc.yaml`, checks the permissions on `oidc-client-secret`, and passes
the authentication settings to both the migration job and the Controller.
Always invoke `compose.sh` so these overlays and checks take effect. PostgreSQL
credential rotation likewise preserves the OIDC overlay when recreating the
Controller. During a rollback, the lifecycle resolves the target release's
deployment files and verifies that its authentication settings and secret
mounts are satisfied, without requiring the target descriptor to match the
prior release.

## Local login from a terminal and 401 diagnosis

Both the Web login form and terminal clients authenticate against
`POST /api/v1/auth/login` with `Content-Type: application/json` and JSON fields
`username` and `password` (whereas OIDC login starts with
`GET /api/v1/auth/login`). Every Local login request must send the exact
configured HTTPS `OCSERV_PUBLIC_ORIGIN` in the `Origin` header; missing or
mismatched origins are rejected, and the JSON body does not accept an OIDC
token or workspace field.

Run the diagnostic snippet below from an administrator workstation or the
Controller host using an existing Local account. It prompts for the password
without terminal echo so the credential never enters shell history, process
arguments, or diagnostic output, and it deletes the temporary request payload,
response headers, and cookie jar on exit. Avoid `curl -v`, request tracing, or
`set -x` because response headers carry session cookies, and never copy an
administrator cookie jar onto a managed node.

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

An HTTP `204` response from `/api/v1/auth/login` establishes the session, and
the follow-up `GET /api/v1/workspaces` confirms that the client stored the
session cookie. Pass one of the returned workspace IDs in `X-Workspace-ID` when
calling workspace-scoped APIs, and continue sending `Origin` on subsequent
browser-session mutations. Authenticating a session does not grant permissions
in every workspace. If an account or source IP cooldown is active, wait for it
to expire before retrying.

| Failure stage | Check and correlate |
| --- | --- |
| Login POST 400/415 | Exact URL/method, JSON field names/types, Content-Type; internal `invalid_request` |
| Login POST 403 | Exact Origin and request context; internal `origin_rejected` |
| Login POST 401 | Generic credentials rejection or account protection; distinguish internal `credentials_rejected` / `account_limited` using the login Request-ID |
| Login POST 429/503 | Admission limits, capacity or infrastructure; inspect the existing internal reason codes |
| Login 204, protected API 401 | That API's Request-ID, cookie retention, HTTPS host/path, session expiry/revocation; this is a separate request from login |
| Protected API 403 | Principal role, selected workspace and resource scope; authentication success alone is insufficient |

Use the `X-Request-ID` response header to look up the corresponding redacted
`auth.result.reason_code` entry in
[Authentication security logs](#authentication-security-logs-r6), and check
`auth.summary` counters as well in case sampling suppressed individual log
lines. Because public error responses intentionally omit whether an account
exists, a bare HTTP status code cannot distinguish every failure cause. When
filing an incident report, record the failure stage, HTTP status, request ID,
and non-secret request attributes, and never include passwords, cookie values,
request bodies, or raw response headers.

## OIDC provider setup

Configure the OIDC provider for the Authorization Code flow with PKCE S256,
register the exact callback URL
`https://controller.example.com/api/v1/auth/callback`, set the HTTPS issuer and
client ID, and supply the client secret via its secret file. When
`OCSERV_OIDC_REDIRECT_URL` is omitted, the production overlay defaults to that
callback path on `OCSERV_PUBLIC_HOST`.

Set `OCSERV_OIDC_ISSUER` to the provider's exact issuer string, preserving any
trailing slash, port, and path. The Controller treats this value as a literal
identity identifier without URL normalization: both the Discovery metadata and
the ID Token `iss` claim must match it byte-for-byte, and the OIDC client builds
the Discovery URL without altering the configured identifier. Never link
accounts by email or username, and do not disable issuer verification to work
around a mismatch.

The origin (scheme, host, and effective port) of `OCSERV_OIDC_REDIRECT_URL`
must match `OCSERV_PUBLIC_ORIGIN`, or production startup fails. In dual-auth
mode (`Local + OIDC`), enabling Local authentication does not relax OIDC
validation. If the identity provider becomes unreachable, existing sessions
continue working, new SSO logins fail closed, and Local login remains available
when enabled. The Controller caches successful issuer Discovery for 15 minutes
and refetches signing keys when a token fails against the cached keys, so changed
provider endpoints take effect within that window; failed Discovery is not
cached. Browser sessions use `Secure`, `HttpOnly`, `SameSite` cookies
without storing OIDC tokens in browser storage.

Every subject the IdP authenticates receives an identity row and a session on
first login, even without any role binding; such a principal sees no
workspaces and is denied by every RBAC-checked route until a role is granted. The API has no endpoint
to delete or disable OIDC identities, and rows are kept for audit history, so
restrict sign-in at the IdP (application assignment or group policy) to the
intended operators rather than relying on the Controller to filter them. Treat
the identity table as growing with that assigned population.

### Correcting a historical issuer configuration

Always configure the provider's authoritative issuer string, including its
trailing slash if present. Before modifying an existing deployment, query the
database for stored `(issuer, subject)` rows: failed Discovery or token
validation never creates an identity row, so a misconfigured `install.env`
alone does not mean historical rows exist, and working identities need no
migration. Because the application never merges accounts across slash and
no-slash issuer strings, any change to stored identities must follow
[OIDC issuer recovery](incident-recovery.md#oidc-issuer-correction) with
independent subject-ownership evidence and explicit migration approval.

## Bootstrap the first Local admin

Ocservia ships with no default administrator password. On a manual
installation, operators initialize the first Local accounts through a separate
one-shot command outside the normal install and restart path. A
Quick installation (`quick-install.sh`) runs the equivalent initialization
automatically: it provisions the management workspace using the owner one-shot
below, generates two random passwords, and runs `--bootstrap-local-admin` to
create `initial-admin` (`PlatformAdmin`) and `initial-approver`
(`SecurityAdmin`), storing their credentials in root-only files under
`/root/ocservia-initial-credentials`. Assign those two accounts to different
responsible individuals and change both passwords after first login (the system
does not force a change on first use). Because Quick reruns accept an
already-initialized database, do not run the manual procedure below on a Quick
host.

1. Complete the pinned Controller installation with Local authentication
   enabled and a
   [supported database deployment](production-deployment.md#database-support).
   Stop any incompatible older Controllers, then run the installed release's
   one-shot owner migration (`--migrate-only`). That command initializes an
   empty database from `schema.sql` or applies bounded forward upgrades from
   `upgrade.sql` against `schema_revisions` (accepting only current-epoch
   receipts or the fixed `v1.2.0` checkpoint at epoch 1 / revision 0; older
   databases must upgrade to `v1.2.0` first). Confirm that the migration
   process logged `database migrations complete` and exited cleanly, and that
   the Controller is ready. Run the remaining commands from the installed
   release checkout with the same effective exported production settings and
   verified image digests; in Integrated mode, run `compose.sh` as root with
   the Integrated environment exported.
2. Obtain the UUIDv7 of the management workspace. Because `--bootstrap-local-admin`
   requires an existing workspace, an authorized database operator must
   provision one first when starting from an empty database. The owner one-shot
   handles both PostgreSQL and MySQL:

   ```bash
   deploy/production/compose.sh run --rm --no-deps migrate --provision-management-workspace
   ```

   When no workspaces exist, this command creates the `Administration`
   workspace (slug `administration`) with a fresh UUIDv7 and prints
   `workspace_id` in the `management workspace ready` log line; rerunning it
   returns the same workspace, whereas running it when other workspaces already
   exist without `administration` fails closed. You can also insert the row
   manually by following [database preparation](#database-preparation) below
   (note that the PostgreSQL SQL is not portable to MySQL). Record the
   resulting UUID, as bootstrap permanently binds Local identity management to
   that workspace regardless of later API headers.
3. Prepare separate requester and approver principals with distinct identities,
   credentials, authenticated sessions, and RBAC roles; never share a
   credential between them or substitute a second identity ID during
   authentication. The system enforces this technical separation of
   principals, while deployments subject to a two-person custody
   rule must also assign the credentials to two different people under their
   enterprise security procedures (automated two-principal tests verify only
   the technical separation). Provision two distinct passwords from your secret
   manager into `OCSERV_SECRET_DIR/local-bootstrap-password` and
   `OCSERV_SECRET_DIR/local-bootstrap-approver-password`. Each file must be a
   regular file (no final-path symlinks or hard links) owned by the one-shot
   process UID with mode `0400` or `0600` inside a private `0700` directory,
   and must never be passed via environment variables, CLI flags, shell
   tracing, or logs. Mount both files read-only for the bootstrap invocation:

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

   Both passwords must meet the Local new-password policy below; the secret
   reader strips at most one trailing LF (and preceding CR) while preserving
   all other whitespace and letter case. Connecting under the application's
   database role, the command grants workspace-scoped `PlatformAdmin` to the
   administrator and workspace-scoped `SecurityAdmin` to the approver in the
   management workspace, committing both identities, credentials, role
   bindings, the completion marker, and audit events in a single transaction
   before exiting without starting network listeners, background workers, or
   IdP connections.
4. Once the command succeeds (`--rm` removes the container and its mounts),
   delete both temporary host password files according to your
   secret-management procedure and verify that both accounts can log in with
   their expected management workspace roles. Keep their ongoing credentials
   under separate custody: the approver cannot create or reset Local users or
   grant `PlatformAdmin` on its own, and instead provides independent approval
   for the administrator's content-bound role grants and password resets.

Any concurrent or subsequent bootstrap attempt is rejected, as is bootstrapping
when a Local `SecurityAdmin` or `PlatformAdmin` already exists. Restarting the
Controller never reads bootstrap password files or resets existing passwords,
and disabling the initial administrator or resetting its password does not
re-enable bootstrap. Always retain the initialization marker in backups, and
never delete the marker or roll back its migration to recover an account.

### Database preparation

When preparing the database manually, connect with the administrative owner
role over a protected connection (using verified TLS for external databases and
credentials retrieved from your secret manager, never passed on the command
line). Reserve the owner connection for schema migration and workspace
provisioning; the running Controller and `--bootstrap-local-admin` use the
application runtime role. If the intended management workspace already exists,
reuse it. On an empty database, generate a fresh UUIDv7 outside these SQL
snippets and substitute it for `<management-workspace-uuidv7>` (do not call a
database UUID function that produces another UUID version).

#### PostgreSQL

Inspect the verified schema journal:

```sql
SELECT epoch, revision, encode(checksum, 'hex') AS checksum, state, step
FROM schema_revisions ORDER BY epoch, revision;
```

Before applying forward SQL, owner initialization validates the supported epoch
and revision, exact artifact receipts, and live schema structure, refusing
unknown or modified history (never edit the journal manually). The
previous-checkpoint window is pinned to `v1.2.0` at epoch 1 / revision 0, and
Controller readiness checks live read queries and permissions directly without
consulting legacy migration filename counters.

PostgreSQL stores workspace IDs as `uuid` and timestamps as `timestamptz`. On
an empty database, create the management workspace:

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

Every required row must be in the `verified` state and match the installed SQL
artifacts. A fresh epoch-2 initialization records revision 2, while a
checkpoint upgrade verifies revision 0 before applying the journaled cleanup at
revision 1 and the maintenance-clock update at revision 2. Any row left in
`running` requires exact artifact repair and must
not be edited manually. Owner migration verifies both the live schema and
runtime privileges; runtime startup checks telemetry structures and
permissions; and readiness checks core read queries and stream health. The
`epoch` and `revision` columns identify backend SQL artifacts and do not map to
PostgreSQL legacy migration numbers or a Controller compatibility range.

In the current migrated MySQL schema, workspace IDs use `VARBINARY(16)` with an
exact 16-byte length constraint in unswapped RFC UUID byte order, and workspace
timestamps are signed `BIGINT` microseconds since `2000-01-01 00:00:00 UTC` (do
not pass Unix seconds or SQL date/time strings). Pass the same UUIDv7 string to
`OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID` in step 3:

```sql
SET @workspace_id = UNHEX(REPLACE('<management-workspace-uuidv7>', '-', ''));
SET @created_at = TIMESTAMPDIFF(MICROSECOND, '2000-01-01 00:00:00', UTC_TIMESTAMP(6));
INSERT INTO workspaces (id, name, slug, created_at, updated_at)
VALUES (@workspace_id, 'Administration', 'administration', @created_at, @created_at);
```

Never insert swapped UUID bytes or call `now()` directly into `BIGINT`
timestamp columns, and do not run this current-schema statement against an
unmigrated or pre-checkpoint database. After completing either database branch,
return to step 3 of the bootstrap procedure above.

### Upgrading a single-administrator installation

Prior to the `v1.2.0` checkpoint, PostgreSQL migration `000034` set
`completion_pending=true` only when the original Local bootstrap
`PlatformAdmin` remained active, had a matching bootstrap audit event, and
shared the fixed management workspace with no other historical `SecurityAdmin`
or `PlatformAdmin` binding. Any disabled binding or additional elevated binding
marked the deployment closed without altering identities, credentials, or
roles. Current releases upgrade only from the `v1.2.0` checkpoint or
current-epoch `schema_revisions` and do not replay removed pre-checkpoint
migrations, so a deployment without that marker or without a verifiable
original administrator has no automatic completion path. MySQL deployments
never ran that pre-checkpoint PostgreSQL migration because their initial schema
already enforced the two-principal contract. Always inspect the stored
eligibility marker in `local_auth_bootstrap` (and not the backend revision
number) to determine whether bootstrap completion is permitted:

```sql
SELECT identity_id, workspace_id, completion_pending, completed_at,
       approver_identity_id FROM local_auth_bootstrap;
```

When `completion_pending` is true, run the same one-shot container command and
two read-only secret mounts from step 3 above, substituting
`--complete-local-bootstrap` for `--bootstrap-local-admin`. Supply the original
administrator username and fixed workspace ID, place the administrator's
current password in `local-bootstrap-password` (this command verifies the
existing password and does not replace it), and place the new, separately
delivered approver password in `local-bootstrap-approver-password`. Holding the
initialization lock, the command verifies the administrator's current
credential under Local attempt protection, rechecks the frozen bootstrap state,
creates the new `SecurityAdmin` identity, and records the role binding,
completion timestamp, and audit event in a single transaction. Concurrent or
repeated completion attempts fail closed, and any subsequent elevated role
grant also closes pending completion.

Deployments that already have independent approval authority do not need this
command, and losing that authority later never reopens initialization. Normal
Controller startup and login paths never create workspaces, accounts, or role
grants.

### Approval and password operations

Perform Local user administration over HTTPS using an authenticated API client
that sends the exact configured `Origin`. Calling `POST /api/v1/local-users`
creates a Local identity without roles. To grant it an elevated role, the
administrator first submits `POST /api/v1/approval-requests` with
`action=role_binding.elevate`, `resource_type=role_binding`, the new identity
ID as `resource_id`, `role_binding={identity_id,role,resource_type:"workspace"}`,
the fixed management workspace in `X-Workspace-ID`, a `reason`, and
`ttl_seconds` (between 60 and 86400). The independent approver reviews the
request payload and calls `POST /api/v1/approval-requests/{id}:approve` with a
`reason` and `expected_request_hash`. Once approved, the administrator calls
`POST /api/v1/role-bindings` with the matching identity, role, scope,
`workspace_id`, `reason`, and `approval_id`. Requesters cannot approve their
own requests at any stage.

For self-service password changes, write a private JSON file containing only
`current_password` and `new_password` (omitting any identity ID or workspace
field) and submit it using a cookie jar from an active Local session:

```sh
curl --fail-with-body --silent --show-error \
  --cookie /run/secrets/local-session.cookies \
  --cookie-jar /run/secrets/local-session.cookies \
  -H "Origin: ${OCSERV_PUBLIC_ORIGIN}" -H 'Content-Type: application/json' \
  --data-binary @/run/secrets/change-password.json \
  "${OCSERV_PUBLIC_ORIGIN}/api/v1/auth/change-password"
```

Delete the temporary JSON file immediately after the request completes. An HTTP
`204` response confirms the password change and revokes every active session
for that identity (including the current cookie), requiring the user to log in
again. Failed current-password checks share the account's login failure
backoff, returning `401` on invalid or stale credentials or an active cooldown
and `400` on password-policy violations; OIDC and Break-glass sessions cannot
call this endpoint. An administrator-initiated `:reset-password` call requires
an independent, single-use `local_user.reset-password` approval passed via
`X-Approval-ID` and leaves disabled accounts disabled.

### Management protection and recovery

The Controller refuses to disable an account (`409 Conflict`) if doing so would
remove the last active Local `PlatformAdmin` or leave fewer than two distinct
active Local identities holding workspace-wide approval authority in the fixed
management workspace. Only active Local identities with valid Local credentials
and workspace-wide bindings in that management workspace satisfy this invariant;
bindings in other workspaces, narrower or historical bindings, OIDC accounts,
and Break-glass credentials do not count toward Local recovery authority.
Because the invariant check and the disable update execute in a single
serialized transaction, two administrators cannot concurrently disable one
another. The API exposes no endpoint to delete role bindings or identities, and
direct database administration remains an externally governed boundary.

To retire a protected Local account, first create its replacement via the Local
API, complete an independent elevated-role approval, and verify that the new
responsible operator can log in before disabling the old account. If one
administrator loses a password, the surviving administrator and an independent
approver can execute an approved password reset. An already-enabled, separately
governed Break-glass session can also request and execute a password reset
once approved by a surviving `SecurityAdmin`, subject to the standard
Break-glass rotation and incident rules (it cannot self-approve).

If no working administrator and independent approver pair remains, halt write
operations, preserve incident evidence, and follow
[Local authority recovery](incident-recovery.md#lost-local-administration-authority).
Retain all initialization markers and audit records, never delete or alter the
bootstrap marker to force reinitialization, and revoke any restored sessions
before reopening access.

## Local new-password policy

Bootstrap, managed user creation, self-service password changes, and
administrator password resets all enforce the same new-password policy:

- Passwords must contain at least 15 Unicode code points and at most 1024 UTF-8
  bytes; invalid UTF-8 sequences are rejected.
- Long passphrases, spaces, and arbitrary valid Unicode characters are accepted
  without uppercase, digit, or symbol composition rules or periodic expiration.
- Each candidate password is checked offline against an embedded list of common
  and breached passwords plus a service-specific supplement, without substring
  dictionary rules or external network queries; see the
  [list provenance, license and maintenance procedure](../../control-plane/internal/auth/password_blocklist/README.md).
- Neither policy validation nor hashing trims, truncates, alters case, or
  Unicode-normalizes the password bytes (only the blocklist comparison is
  case-insensitive).
- When a candidate fails policy checks, the endpoint returns a safe error
  explanation (`400 Bad Request` on management endpoints) before touching the
  database, leaving identities, credentials, roles, audit logs, active
  sessions, and reset approvals unchanged. Plaintext passwords and hashes are
  never logged.

Existing stored hashes continue to verify against their original password
bytes, even if a historical password is shorter than 15 code points or appears
on a newer blocklist. The login endpoint does not run new-password policy
checks and returns uniform credential-rejection errors, whereas any subsequent
password change or reset must satisfy the current policy. Hashing continues to
use Argon2id with random salts, fixed PHC parameters and resource limits, and
dummy verification for unknown users. Length and blocklist checks follow
[NIST SP 800-63B-4, section 3.1.1.2](https://pages.nist.gov/800-63-4/sp800-63b.html#passwordver),
while Unicode normalization remains intentionally omitted so existing hashes
continue to validate.

## Authentication request budgets and proxy trust

OIDC login initiation and OIDC callback each enforce separate fixed one-minute
budgets of 30 requests per source IP, 120 total requests across all sources,
and 8 concurrent in-flight requests per API process. Because the two budgets
are independent, a flood of login starts cannot starve callbacks. Requests
exceeding these limits receive `429` with a `Retry-After` header without
locking any account. Each source table tracks up to 4096 addresses and keeps
live one-minute windows in place until they expire so an attacker cannot gain
fresh quota by forcing table evictions. Running multiple API replicas scales
these per-process limits linearly; maintain edge rate limiting for distributed
floods.

Local login enforces its own per-process budget of 5 requests per source IP per
minute, 120 total requests per minute, and 4 concurrent in-flight requests.
These rate limits apply before and independently of password verification,
while the cross-Controller account failure backoff described below provides a
second, account-keyed layer.

In production Compose deployments, the shipped Caddy gateway overwrites
`X-Ocservia-Client-IP` with the client IP it observes: the direct TCP peer in
Standalone mode, or the client address extracted from Edge's PROXY header in
Integrated mode (accepted exclusively from Edge's dedicated address; see the
[Integrated contract](../development/integrated-deployment-contract.md#network-and-trust)).
The Controller API trusts `X-Ocservia-Client-IP` only when the connection comes
from a configured trusted proxy peer and always ignores client-supplied
`X-Forwarded-For` headers. Never mark the entire shared application network as
trusted or publish the Controller container port directly.

Outside production Compose, the API trusts no proxy by default, so all requests
arriving through an unconfigured gateway share that gateway peer's per-source
budget. Similarly, if you place an external reverse proxy or load balancer in
front of Caddy, all clients behind that proxy share its source IP budget at
Caddy; enforce per-client rate limits at the outer proxy and do not configure
the Controller to trust arbitrary forwarded headers.

Break-glass authentication uses a dedicated budget of 4 concurrent in-flight
requests and allows at most 5 invalid token attempts per source IP per minute.
Presenting a valid, enabled emergency credential bypasses the failed-request
rate and source-table limits while still enforcing `Origin` validation,
rotation checks, audit logging, and session creation rules, so OIDC outages or
exhausted OIDC/Local rate limits cannot block emergency access. Keep the
offline Break-glass credential accessible and alert on authentication `429`
responses via HTTP telemetry. Application rate limiting complements external
DDoS mitigation and does not protect a compromised emergency credential.

For gateway addresses and subnet overrides, see
[deployment networking](production-deployment.md#authentication-request-budgets).

<a id="local-account-failure-backoff-r2"></a>
## Local account failure backoff

Account failure backoff is coordinated across Controllers in the database and
operates independently of the
[per-process request budgets](#authentication-request-budgets-and-proxy-trust).

Admission tracks attempts in the backend's `local_auth_attempts` table, keyed
by the normalized Local username (trimmed, lowercased, and validated under the
existing ASCII and input rules). Known and unknown usernames follow identical
rules. Self-service password changes apply the same policy to a separate
`password-change:<identity UUID>` row, so login and password-change failures
never block each other:

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

Refusals caused by an active cooldown or an occupied verification lease leave
the failure counter and window deadline unchanged and return the same generic
`401` response as an invalid password (omitting `Retry-After` and any
indication of whether the account exists). While normal unknown-user attempts
run a dummy Argon2id verification, requests refused at the account-admission
step skip KDF execution altogether; IP and global rate-limit refusals continue
to return `429` with `Retry-After`. Because only one 30-second verification
lease is permitted per username at a time, a legitimate login submitted
concurrently with another attempt for the same username will also receive `401`.

Database errors or corrupt password hashes do not increment the incorrect-password
counter, and a valid password for a disabled identity still cannot establish a
session. When `local_auth_attempts` reaches its 16,384-row capacity, requests
for untracked usernames fail closed with a generic `503` while already-tracked
usernames retain their backoff state (live rows are never evicted to admit new
keys); database or infrastructure failures likewise return `503`. Abandoned
verification leases expire after at most 30 seconds, and administrative
password resets or account disables clear the account's backoff row within the
same transaction. Before replacing Local login instances, stop the affected
instances, preserve the database backoff state, and verify authentication
before returning to service (binary rollback does not clear database backoff
rows). See
[attempt completion and expiry](../development/identity-authorization-audit.md#attempt-completion-and-expiry)
for request cancellation, lease fencing, and database-clock details.

This mechanism bounds password-guessing throughput per account while keeping
per-IP throttling active; a targeted attacker can still trigger temporary
five-minute cooldowns on a known username or saturate the 16,384-row table to
block untracked usernames temporarily. This dual-layer design follows the OWASP
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#login-throttling)
and
[Credential Stuffing Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Credential_Stuffing_Prevention_Cheat_Sheet.html).

<a id="authentication-security-logs-r6"></a>
## Authentication security logs

Local login and OIDC start and callback routes write to the Controller's
standard structured `slog` stream, replacing the generic `http request` log
line on those paths with one `auth.result` record per outcome (subject to the
sampling rules below). An outcome of `started` means an OIDC redirect was
prepared prior to session creation, whereas `succeeded` is emitted only after a
session is established. These events require no extra logging configuration or
external log collector.

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

Unknown usernames, wrong passwords, and disabled accounts all return the same
external credential error, and account cooldown returns that same `401` without
`Retry-After`; OIDC callback failures likewise return generic errors to the
caller. The logger never records raw upstream IdP errors (which can contain
response bodies, tokens, or URLs), request bodies, passwords, password hashes,
session cookies, OIDC authorization codes or tokens, client secrets, database
connection strings, or callback URLs. All request-derived fields are bounded in
length, have control and non-ASCII bytes replaced, and pass through the
standard JSON log encoder.

The `account_ref` field is a keyed pseudonym (not anonymous data and not a
plain username hash). Using HMAC-SHA-256, the Controller derives a dedicated
logging key from the session key under the domain
`ocservia/auth-log/account-key/v1\0`, and then computes the account HMAC over
the normalized username under the domain
`ocservia/auth-log/local-account/v1\0`. Values correlate across Controllers
that share the same session key and rotate whenever the session key rotates.
Because anyone holding the session key can enumerate usernames against
`account_ref`, protect both the logs and the session key as restricted security
material and never expose them to clients.

To bound log volume in constant memory without per-account or per-IP maps,
queues, or background timers, each Controller logs the first 10 `auth.result`
entries per fixed category in each 60-second window (including successes) and
counts any additional occurrences in a saturating suppressed counter. Across
the 23 fixed categories, a Controller emits at most 230 `auth.result` records
and 23 `auth.summary` records per window. Summary records carry no individual
request or account identifiers and are flushed by the first authentication
event after a window expires (so idle periods delay summary emission and
process exit drops unflushed counters). Successful logins and OIDC starts log
at `Info` level, while refusals, failures, and summaries log at `Warn` level.
This sampling applies only to application authentication events, leaving
gateway access logs and host or container log-rotation and retention policies
unchanged. Logging errors or panics are handled best-effort without altering
authentication or authorization decisions, though a blocked stdout sink can
still slow request handling, and log sampling never bypasses the authentication
rate or KDF budgets.

Structured `auth.result` and `auth.summary` logs are separate from the
database `audit_events` and `security_alerts` tables: failed password attempts
do not write workspace audit rows, whereas password changes, account
administration, approvals, and Break-glass sessions continue to record their
transactional audit events and security alerts. These redaction and rate-bounding
rules follow the
[OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html).

## Local identity and sessions

Local passwords are stored as Argon2id hashes under identities with
`issuer=local`. Local and OIDC identities never merge automatically, even when
their usernames or email addresses match, and neither self-registration nor
automatic account linking is supported. These identities govern platform
console and API access and are completely separate from VPN user accounts on
managed nodes.

Administrators manage Local users through the existing `local_user.manage` RBAC
permission within the fixed management workspace. Creating a Local user grants
no roles, and resetting a user's password requires the standard independent
approval workflow; see
[identity administration](../development/identity-authorization-audit.md#local-initialization-and-lifecycle-r4)
for endpoint and permission specifications.

Disabling a Local user or resetting its password revokes all active sessions
for that identity in the same database transaction. A disabled user cannot
authenticate, and resetting a password does not re-enable a disabled account.
Once the transaction commits, any concurrent login attempt in flight cannot
issue a usable stale session, although already-authorized requests that are
currently executing are not retroactively aborted.

## Break-glass and deployment checks

Local authentication complements Break-glass and does not replace it.
Break-glass remains a separate offline emergency access path with mandatory
credential rotation, a critical security alert, a transactional audit event,
and a 15-minute session lifetime. Store the Break-glass credential and its
incident runbook separately from Local administrator passwords; see the
[Break-glass contract](../development/identity-authorization-audit.md#break-glass).

Before activating a deployment, export the effective configuration and validate
the rendered Compose specification and secret file metadata:

```bash
deploy/production/compose.sh config --quiet
```

Because `compose.sh config --quiet` checks only Compose syntax and host file
permissions without inspecting secret contents or contacting the IdP, full
authentication validation runs at Controller startup. After installation,
verify the login page options over HTTPS along with login, logout, RBAC
enforcement, and session revocation, without printing secret contents during
troubleshooting. When validating documentation or code changes in an isolated
test environment, follow [Validate a change](../development/testing.md) and run
the focused configuration regression script
`scripts/test-production-auth-config.sh`.
