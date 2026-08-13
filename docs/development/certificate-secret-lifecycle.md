# Certificate and secret lifecycle

Certificate requests generate their private key on the managed node. The
controller receives a signed CSR and public-key digest, then sends the CSR to a
configured external PKI signer only after an independent, content-bound
approval. The controller does not store a CA private key or a node private key.
CSR self-signature is not privilege evidence. A CSR enters `csr_ready` only
after Controller verifies a root privd receipt binding certificate ID, CSR and
public-key digests, requested-subject digest, node, command, operation,
idempotency key, and root effect record. Immediately before calling the signer,
Controller locks the certificate row and rechecks exact CSR digest,
receipt-bound request/version, node, approval hash, and non-revoked key. P12 and
certificate-key revocation use the same terminal-result attestation rule.

Configure the external HTTPS service with
`OCSERV_CERTIFICATE_SIGNER_URL`, `OCSERV_CERTIFICATE_SIGNER_TOKEN`, and
`OCSERV_CERTIFICATE_SIGNER_TIMEOUT`. The service must make signing and
revocation idempotent by certificate ID and provide node-targeted secret
sealing. An unavailable signer leaves the certificate request recoverable and
returns a service-unavailable problem response.

The `/seal` request includes `X-Ocservia-Node-ID` and the exact
`X-Ocservia-Seal-Purpose` (`user_password` or
`certificate_p12_password`). Its response must echo `version: 1`, the exact
purpose, the enrolled purpose-specific `key_id`, and the base64 ciphertext.
The Controller rejects a missing, substituted, or unregistered binding.

P12 export uses a fresh random password and a separately random artifact token.
The password is sealed with the node's dedicated P12-password public key and
cannot be opened by the independent user-password key. The password and token
are returned only by the initial request and are never stored in PostgreSQL.
Privd decrypts the typed secret locally, creates an encrypted UUID-addressed
artifact in its fixed root-owned spool, and records its certificate/version,
operation, digest, size, expiry, and state in the authenticated effect store.
The root mapping enters `prepared` before any staging file is created and moves
to `available` only after the final artifact is published, so revocation can
remove crash-left staging as well as completed artifacts.

Downloads require ordinary node authorization, the separate
`X-Artifact-Token`, and a short-lived Controller-signed `ArtifactGrantV1` bound
to the node, artifact, certificate/version, operation, requester, purpose,
maximum size, and unique grant ID. Agent and privd verify the grant
independently. Only one grant may lease an artifact at a time; an interrupted
lease becomes available only after its bounded expiry. The root ledger also
advances the exact chunk offset, so a second stream cannot reuse the same grant
from offset zero. Reading does not consume the artifact. After the Control Plane has received the complete stream and
verified its size and digest, it relays a separate finalize request carrying
the same signed grant to Agent and privd. Successful finalization is durably
consumed and the local P12 is deleted. Consumed grants cannot replay a fetch.
An exact finalize retry may acknowledge the already-consumed root record so a
lost response cannot strand the Controller lease; it never reopens the bytes
or repeats the deletion.
Certificate revocation invalidates outstanding grants and removes
all mapped P12 and staging files. Time-based cleanup is only crash recovery for
expired or orphaned files.

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
