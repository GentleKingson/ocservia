# Certificate and secret lifecycle

Certificate requests generate their private key on the managed node. The
controller receives a signed CSR and public-key digest, then sends the CSR to a
configured external PKI signer only after an independent, content-bound
approval. The controller does not store a CA private key or a node private key.

Configure the external HTTPS service with
`OCSERV_CERTIFICATE_SIGNER_URL`, `OCSERV_CERTIFICATE_SIGNER_TOKEN`, and
`OCSERV_CERTIFICATE_SIGNER_TIMEOUT`. The service must make signing and
revocation idempotent by certificate ID and provide node-targeted secret
sealing. An unavailable signer leaves the certificate request recoverable and
returns a service-unavailable problem response.

P12 export uses a fresh random password and a separately random artifact token.
The password and token are returned only by the initial request and are never
stored in PostgreSQL. Privd decrypts the sealed password locally, creates an
encrypted UUID-addressed artifact in its fixed spool, and returns only its size
and SHA-256 digest. Downloads require ordinary node authorization plus the
separate `X-Artifact-Token`, are limited to 64 MiB, verified before delivery,
expire after ten minutes, and are consumed before response bytes are released.

Secret provider records contain only provider, opaque key path, version, and
lifecycle state. Secret values must remain in the external provider. Rotation
records the new external version and appends an authenticated audit event; it
does not copy the value into the control plane.

Certificate expiry enters `expiring` thirty days before `not_after` and emits a
high-severity alert. Revocation is sent idempotently to the external signer and
then removes only the UUID-derived node-local key. Before migration rollback,
stop certificate and artifact creation and reconcile all nonterminal
certificate commands. Rollback refuses active work and preserves terminal typed
command history.
