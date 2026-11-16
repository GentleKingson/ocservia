# Production authentication

Production deployments support three authentication modes: Local only, OIDC
only, and Local + OIDC (OIDC is optional when Local authentication is enabled).
Follow the standard [Controller installation](production-deployment.md#install-the-controller)
and [production secret permissions](production-deployment.md#production-secrets)
for all three modes. The snippets below replace only the authentication section
of `install.env`, leaving the database, TLS, relay, signer, backup, and release
settings as configured for your manual Standalone or Integrated deployment on
any supported database. Use a manual installation whenever you need OIDC. Quick
mode (`controller-bootstrap.sh --quick`) always configures Local-only
authentication, ignores `./install.env`, and creates one Local
administrator automatically.

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
showing a password form, and Local bootstrap is unused. OIDC login
authenticates identity without granting administrative RBAC roles on its own, so
provision a workspace-scoped PlatformAdmin binding through the standard identity and role provisioning workflow. One Admin can perform all management operations within that workspace.

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
variables override `install.env`, even when exported as empty strings.
Subsequent lifecycle commands and direct `compose.sh` invocations do not read
`install.env`, so export the same effective configuration (along with the image
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
`username` and `password`. OIDC login starts with `GET /api/v1/auth/login`
instead. Every Local login request must send the exact
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
lines. A bare HTTP status code cannot distinguish every failure cause, because
public error responses intentionally omit whether an account exists. When
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
validation.

If the identity provider becomes unreachable, existing sessions continue
working, new SSO logins fail closed, and Local login remains available when
enabled. The Controller caches successful issuer Discovery for 15 minutes
and refetches signing keys when a token fails against the cached keys, so changed
provider endpoints take effect within that window; failed Discovery is not
cached. Browser sessions use `Secure`, `HttpOnly`, `SameSite` cookies
without storing OIDC tokens in browser storage.

Every subject the IdP authenticates receives an identity row and a session on
first login, even without any role binding. Such a principal sees no workspaces
and is denied by every RBAC-checked route until a role is granted. The API has
no endpoint to delete or disable OIDC identities, and rows are kept for audit
history. Restrict sign-in at the IdP (application assignment or group policy) to
the intended operators rather than relying on the Controller to filter them.
Treat the identity table as growing with that assigned population.

### Revoking an OIDC identity or role

The Controller validates every request against its own database, not the IdP,
so an IdP-side block alone leaves existing sessions valid until they expire and
leaves role bindings in place. To revoke a compromised or departed OIDC
operator, use the externally governed database administration path:

1. Block the subject at the IdP first, so no new SSO login can succeed; a
   browser logout is not enough.
2. Identify the target by identity UUID and exact `(issuer, subject)`, never by
   email, and confirm it is not a Break-glass or Local recovery identity.
3. In one transaction, lock that identity row, set `disabled_at` and
   `updated_at`, and set `revoked_at` on every unrevoked `auth_sessions` row of
   that identity.
4. Instead of step 3, to remove only one workspace's access, delete every
   `role_bindings` row for that identity and `workspace_id` in one transaction
   (an identity can hold several roles and resource-scoped bindings there, and
   any remaining row still authorizes), then confirm after commit that none
   remain. To remove only one role, delete just that exact row. Either way,
   confirm the change does not remove the Local recovery authority described in
   [Management protection and recovery](#management-protection-and-recovery).
5. Record the action in the incident record. Keep the identity row and its
   audit history; do not delete or rewrite audit records.

Session validation requires an unrevoked session and an identity without
`disabled_at`, and an OIDC callback for a disabled identity does not re-enable
it, so the old sessions stop at the next request and new logins fail. Requests
already authorized before the commit are not undone, and revoking an identity
does not cancel commands already signed for nodes. Shortening
`OCSERV_SESSION_TTL` narrows exposure but replaces none of these steps.

### Correcting a historical issuer configuration

Always configure the provider's authoritative issuer string, including its
trailing slash if present. Before modifying an existing deployment, query the
database for stored `(issuer, subject)` rows: failed Discovery or token
validation never creates an identity row, so a misconfigured `install.env`
alone does not mean historical rows exist, and working identities need no
migration. The application never merges accounts across slash and no-slash
issuer strings, so any change to stored identities must follow
[OIDC issuer recovery](incident-recovery.md#oidc-issuer-correction) with
independent subject-ownership evidence and explicit migration approval.

## Bootstrap the first Local admin

Ocservia ships with no default administrator password. On a manual
installation, operators initialize the first Local accounts through a separate
one-shot command outside the normal install and restart path. A
Quick installation (`quick-install.sh`) runs the equivalent initialization
automatically. It provisions the management workspace using the owner one-shot
below, generates one random password, and runs `--bootstrap-local-admin` to
create `initial-admin` (`PlatformAdmin`). Its credential is stored in the
root-only file `/root/ocservia-initial-credentials/admin.txt`. Change the
password after first login. Do not run the manual procedure below on a Quick
host, because Quick reruns accept an already-initialized database.

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
   the Controller is ready. Run the remaining commands from the retained
   release runtime with the same effective exported production settings and
   verified image digests; in Integrated mode, run `compose.sh` as root with
   the Integrated environment exported.
2. Obtain the UUIDv7 of the management workspace. `--bootstrap-local-admin`
   requires an existing workspace, so an authorized database operator must
   provision one first when starting from an empty database. Run the PostgreSQL
   owner one-shot:

   ```bash
   deploy/production/compose.sh run --rm --no-deps migrate --provision-management-workspace
   ```

   When no workspaces exist, this command creates the `Administration`
   workspace (slug `administration`) with a fresh UUIDv7 and prints
   `workspace_id` in the `management workspace ready` log line. Rerunning it
   returns the same workspace, but running it when other workspaces already
   exist without `administration` fails closed. You can also insert the row
   manually by following [database preparation](#database-preparation) below.
   Record the resulting UUID, as bootstrap permanently binds Local identity
   management to that workspace regardless of later API headers.
3. Provision one administrator password from your secret manager into
   `OCSERV_SECRET_DIR/local-bootstrap-password`. It must be a regular file
   without symlinks or hard links, owned by the one-shot process UID with mode
   `0400` or `0600` inside a private `0700` directory. Never pass the password
   through environment values, CLI arguments, shell tracing, or logs.

   ```bash
   sudo chown 65534:65532 "${OCSERV_SECRET_DIR}/local-bootstrap-password"
   sudo chmod 0400 "${OCSERV_SECRET_DIR}/local-bootstrap-password"
   deploy/production/compose.sh run --rm --no-deps \
     -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=initial-admin \
     -e OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID='<management-workspace-uuidv7>' \
     -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/local-bootstrap-password \
     -v "${OCSERV_SECRET_DIR}/local-bootstrap-password:/run/secrets/local-bootstrap-password:ro" \
     control-plane --bootstrap-local-admin
   ```

   Bootstrap creates one workspace-scoped `PlatformAdmin`, its credential,
   the initialization marker and audit event in one transaction. It does not
   create an approver account. The password must meet the policy below.
4. After success, remove the temporary host password file according to your
   secret-management procedure and verify administrator login.

The bootstrap guarantees (repeat attempts, restarts, the initialization marker
and backups) are in
[Local initialization and lifecycle](../reference/stable-contracts.md#local-initialization-and-lifecycle).

### Database preparation

When preparing PostgreSQL manually, connect with the administrative owner
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

Do not run this current-schema statement against an unmigrated or
pre-checkpoint database. After creating or confirming the management workspace,
return to step 3 of the bootstrap procedure above.

### Upgrading an existing installation

Keep existing identities, role bindings, initialization markers and audit
history. A surviving workspace `PlatformAdmin` can manage the installation
alone. Existing SecurityAdmin accounts do not need to approve operations;
retire unused accounts through the authenticated Local user lifecycle API.
Never delete bootstrap markers or replay initialization to reset a password.

### Role and password operations

Use HTTPS with an authenticated administrator session and the exact configured
`Origin`. `POST /api/v1/local-users` creates an identity without roles. Grant
roles directly through `POST /api/v1/role-bindings` with the target identity,
role, scope, `workspace_id` and `reason`. Administrator password resets use
`POST /api/v1/local-users/{identity_id}:reset-password`. Neither operation
requires an approval request, second identity, or `X-Approval-ID`.

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
call this endpoint.

### Management protection and recovery

The Controller refuses with `409 Conflict` to disable an account if doing so
would break the Local recovery authority. Which accounts count, and how the
check is serialized, are in
[Local initialization and lifecycle](../reference/stable-contracts.md#local-initialization-and-lifecycle).

To retire the last active Local PlatformAdmin, first create a replacement,
grant its workspace role, and verify its login. The Controller prevents
removing the last active Local PlatformAdmin. Another working Admin can reset
a lost password directly; an explicitly enabled Break-glass session follows
the documented incident and credential-rotation procedure.

If no working administrator remains, preserve incident evidence and follow
[Local authority recovery](incident-recovery.md#lost-local-administration-authority).
Keep initialization markers and audit history intact, and revoke restored
sessions before reopening access.

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
  sessions unchanged. Plaintext passwords and hashes are
  never logged.

Existing stored hashes continue to verify against their original password
bytes, even if a historical password is shorter than 15 code points or appears
on a newer blocklist. The login endpoint does not run new-password policy
checks and returns uniform credential-rejection errors. Any subsequent
password change or reset must satisfy the current policy. Hashing continues to
use Argon2id with random salts, fixed PHC parameters and resource limits, and
dummy verification for unknown users. Length and blocklist checks follow
[NIST SP 800-63B-4, section 3.1.1.2](https://pages.nist.gov/800-63-4/sp800-63b.html#passwordver),
while Unicode normalization remains intentionally omitted so existing hashes
continue to validate.

## Authentication request budgets and proxy trust

OIDC login initiation and OIDC callback each enforce separate fixed one-minute
budgets of 30 requests per source IP, 120 total requests across all sources,
and 8 concurrent in-flight requests per API process. The two budgets are
independent, so a flood of login starts cannot starve callbacks. Requests
exceeding these limits receive `429` with a `Retry-After` header without
locking any account. Each source table tracks up to 4096 addresses and keeps
live one-minute windows in place until they expire so an attacker cannot gain
fresh quota by forcing table evictions. Running multiple API replicas scales
these per-process limits linearly; maintain edge rate limiting for distributed
floods.

Local login enforces its own per-process budget of 5 requests per source IP per
minute, 120 total requests per minute, and 4 concurrent in-flight requests.
These rate limits apply before and independently of password verification. The
cross-Controller account failure backoff described below provides a second,
account-keyed layer.

In production Compose deployments, the shipped Caddy gateway overwrites
`X-Ocservia-Client-IP` with the client IP it observes: the direct TCP peer in
Standalone mode, or the client address extracted from Edge's PROXY header in
Integrated mode (accepted exclusively from Edge's dedicated address; see the
[Integrated deployment](../architecture.md#integrated-deployment)).
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
[deployment networking](production-deployment.md#relay-and-networking).

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
step skip KDF execution altogether. IP and global rate-limit refusals continue
to return `429` with `Retry-After`. Only one 30-second verification lease is
permitted per username at a time, so a legitimate login submitted concurrently
with another attempt for the same username will also receive `401`.

When `local_auth_attempts` reaches its 16,384-row capacity, requests
for untracked usernames fail closed with a generic `503` while already-tracked
usernames retain their backoff state (live rows are never evicted to admit new
keys); database or infrastructure failures likewise return `503`. Abandoned
verification leases expire after at most 30 seconds. Before replacing Local login instances, stop the affected
instances, preserve the database backoff state, and verify authentication
before returning to service (binary rollback does not clear database backoff
rows). See
[identity, authorization and audit](../reference/stable-contracts.md#identity-approval-and-audit)
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
prepared prior to session creation. `succeeded` is emitted only after a session
is established. These events require no extra logging configuration or
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
Protect both the logs and the session key as restricted security material and
never expose them to clients, because anyone holding the session key can
enumerate usernames against `account_ref`.

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
do not write workspace audit rows. Password changes, account
administration and Break-glass sessions continue to record their
transactional audit events and security alerts. These redaction and rate-bounding
rules follow the
[OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html).

## Local identity and sessions

The identity model, session revocation and Local user lifecycle rules are in
[Local authentication core](../reference/stable-contracts.md#local-authentication-core)
and [Local initialization and lifecycle](../reference/stable-contracts.md#local-initialization-and-lifecycle).

### Local user lifecycle endpoints

Lifecycle endpoints live under `/api/v1` and use `Content-Type: application/json`:

| Endpoint | Body | Success |
| --- | --- | --- |
| `POST /local-users` | `{"username":"operator1","password":"..."}` | 201, `identity_id` |
| `POST /local-users/{identity_id}:disable` | `{}` | 204 |
| `POST /local-users/{identity_id}:reset-password` | `{"password":"..."}` | 204 |
| `POST /auth/change-password` | `{"current_password":"...","new_password":"..."}` | 204, all old sessions revoked; re-login required |

The permission, `Origin`, session and error rules for these endpoints are in
[Local initialization and lifecycle](../reference/stable-contracts.md#local-initialization-and-lifecycle).

Administrator password resets require the fixed management workspace,
a valid administrator session and trusted `Origin`. Credential replacement,
session revocation and audit commit together; no second authorization identity
or approval header is required.

## Break-glass and deployment checks

Break-glass stays disabled unless `OCSERV_BREAK_GLASS_ENABLED=true` and
`OCSERV_BREAK_GLASS_TOKEN_SHA256` holds the SHA-256 digest of a high-entropy
offline secret. Generate the secret from at least 32 random bytes (for example
`openssl rand -hex 32`), never derive it from a password or passphrase, keep it
offline apart from Local administrator credentials, and record who holds
custody. Store the Break-glass credential and its incident runbook separately
from Local administrator passwords. The session rules, alerting and the
unsalted digest caveat are in
[Break-glass](../reference/stable-contracts.md#break-glass), and use after an
incident is in
[Break-glass credential after use](incident-recovery.md#break-glass-credential-after-use).

Before activating a deployment, export the effective configuration and validate
the rendered Compose specification and secret file metadata:

```bash
deploy/production/compose.sh config --quiet
```

`compose.sh config --quiet` checks only Compose syntax and host file
permissions without inspecting secret contents or contacting the IdP, so full
authentication validation runs at Controller startup. After installation,
verify the login page options over HTTPS along with login, logout, RBAC
enforcement, and session revocation, without printing secret contents during
troubleshooting. When validating documentation or code changes in an isolated
test environment, follow [Choose the validation scope](../development/release-checks.md#choose-the-validation-scope) and run
the focused configuration regression script
`scripts/test-production-auth-config.sh`.

## Rollout and rollback

Run the installed release's full backend migrations before enabling OIDC.
Provision and verify one workspace-scoped PlatformAdmin before turning off
development authentication. Test direct administrative operations, denied
non-admin requests, audit checkpoint verification, IdP outage behavior and
Break-glass token rotation in the target environment.

To roll back the Controller binary, follow
[Deployment rollback](incident-recovery.md#deployment-rollback).
