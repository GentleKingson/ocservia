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

   The password accepts 1..1024 bytes (a trailing CR/LF is removed); use a strong
   password from your secret manager, not the minimum length. The command uses
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
