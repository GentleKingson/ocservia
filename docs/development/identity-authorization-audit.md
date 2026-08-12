# Identity, authorization, approval, and audit operations

Production API access uses an OIDC Authorization Code flow with PKCE S256.
Configure an HTTPS issuer, exact HTTPS callback URL, client credentials, a
32-byte session encryption key, a 32-byte audit checkpoint key, and an
independent 32-byte audit event authentication key with a stable key ID. The browser
receives only Secure, HttpOnly, SameSite session cookies; OIDC tokens are not
stored in browser storage. Existing sessions remain usable during a temporary
identity-provider outage, while new logins fail closed.

The required environment variables are:

```text
OCSERV_OIDC_ISSUER
OCSERV_OIDC_CLIENT_ID
OCSERV_OIDC_CLIENT_SECRET
OCSERV_OIDC_REDIRECT_URL
OCSERV_SESSION_KEY
OCSERV_SESSION_TTL
OCSERV_AUDIT_CHECKPOINT_KEY
OCSERV_AUDIT_EVENT_KEY_ID
OCSERV_AUDIT_EVENT_KEY_FILE
```

The symmetric key values contain 64 lowercase hexadecimal characters. The
event key file must be a process-owned, single-link regular file with mode
`0400` or `0600` below root- or process-owned non-writable ancestry. Production startup
fails when OIDC, the checkpoint key, or the event key is absent. Development bearer
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

## Break-glass

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
