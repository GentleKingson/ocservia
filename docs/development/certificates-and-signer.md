# Certificates, secrets and Signer

## Certificate and secret lifecycle

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

For a private Signer trust chain, set `OCSERV_CERTIFICATE_SIGNER_CA_FILE` to
the public PEM CA bundle. It affects only this client, never the process-wide
trust store. Without it, external HTTPS trust is unchanged. The bundled
implementation, controlled public-key transfer and offline recovery procedures
are described in [Production Signer](#production-signer).

The `/seal` request includes `X-Ocservia-Node-ID` and the exact
`X-Ocservia-Seal-Purpose` (`user_password` or
`certificate_p12_password`). Its response must echo `version: 1`, the exact
purpose, the enrolled purpose-specific `key_id`, and the base64 ciphertext.
The Controller rejects a missing, substituted, or unregistered binding.

P12 export uses a fresh random password and a separately random artifact token.
The password is sealed with the node's dedicated P12-password public key and
cannot be opened by the independent user-password key. The password and token
are returned only by the initial request and are never stored in the Controller database.
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
then removes only the UUID-derived node-local key. Before database recovery,
stop certificate and artifact creation and reconcile all nonterminal
certificate commands. Preserve terminal command history and root effect evidence.
The current tree provides no database down migrations; use a forward fix or an
explicitly planned [isolated restore](../operations/incident-recovery.md#database-recovery).

## Production Signer

Signer is a separate Go module/process under the
[Integrated custody boundary](integrated-deployment-contract.md#signer-contract),
not a Controller command-signing provider. The Python business signer is a test fixture.

### Runtime contract

Build with `docker build -f deploy/production/signer.Dockerfile .`.
The image runs `/ocserv-signer serve` as UID:GID `65532:65532`; run with a
read-only root filesystem, all capabilities dropped, no-new-privileges and
only the Controller-to-Signer internal network. No host port is required.

Default files and listener:

| Input | Default |
| --- | --- |
| HTTPS listener | `:9443` |
| Online issuing intermediate chain, intermediate first and root last | `/run/secrets/issuer-chain.pem` |
| Online intermediate private key, unencrypted PEM | `/run/secrets/issuer-key.pem` |
| HTTPS certificate/key | `/run/secrets/tls-cert.pem`, `/run/secrets/tls-key.pem` |
| Independent API Bearer token, 32..256 non-whitespace bytes | `/run/secrets/api-token` |
| Exclusive writable state volume | `/var/lib/ocservia-signer/ledger.db` |
| Health client's public trust bundle | `/run/secrets/tls-ca.pem` |

Use absolute canonical paths, real directories owned by root or the process
and no group/world-writable ancestor. State directory mode is 0700, ledger
0600 owned by the process. Private files must be regular, single-linked,
owner-only and readable by the runtime UID (0400 or 0600 recommended).
Mount secrets read-only. Provision the root CA offline; never mount its key.
The service requires a valid intermediate/root chain, matching intermediate
key, CA certificate-signing and CRL-signing usages and at least 24h remaining
validity. It does not generate any CA, node key or TLS identity.

`init` is an explicit first-install operation with the same CA/state flags as
`serve`. It exclusively creates a previously absent ledger. Never use it to
repair missing state. Normal startup refuses absent, empty, incompatible,
corrupt, differently owned or wrong-issuer state; it never creates a replacement.

Set Controller `OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign`, its
existing token-file setting and `OCSERV_CERTIFICATE_SIGNER_CA_FILE` to a mounted
public HTTPS CA bundle. The HTTPS leaf SAN must include `signer`. The custom
trust pool belongs only to this client; no `SSL_CERT_FILE` or TLS verification
bypass is needed. With no custom CA setting, external HTTPS defaults remain.
The CA bundle is for HTTPS trust, not permission to sign business certificates.

`GET /healthz` returns 204 only while the ledger is readable, issuer lifetime
sufficient and no storage failure has latched readiness off. It does not sign
certificates. `health --url https://signer:9443/healthz` verifies HTTPS using
`--tls-ca`; it rejects redirects. Health can use loopback only if SAN permits.
Restart after repairing a storage fault; restarting does not erase state.

The POST paths are exactly `/sign`, `/sign/revoke`, `/sign/seal`, and
`/sign/public-key`.
JSON requests are capped at 64 KiB, raw sealing requests at 512 bytes and then
the actual RSA-OAEP capacity (190 bytes for RSA-2048). At most 32 requests enter
business handling; excess returns 503. Header/read/write/idle timeouts are
5/10/15/30 seconds. Failures return fixed status text, not request content.

### Quick install materials

`deploy/production/quick-materials.sh` prepares a first integrated, bundled
PostgreSQL, root-lifecycle installation; `deploy/production/quick-install.sh`
calls it. It runs as root before activation with
`OCSERV_SECRET_DIR`, `OCSERV_SIGNER_SECRET_DIR` and `OCSERV_SIGNER_STATE_DIR`,
and creates every missing Controller and Signer file with the ownership and
modes `compose.sh` validates. It does not create Gateway or Relay TLS identities.

- Random secrets are 32-byte hex values; the database DSNs and `postgres.pgpass`
  are derived from the generated role passwords. The command-signing key and
  the `controller-iroh.key` identity are separate Ed25519 keys.
- The Signer HTTPS leaf (SAN `signer`, P-256, 5 years) comes from a one-shot TLS
  CA whose private key is discarded; only `tls-ca.pem` is kept.
- The offline root CA (P-256, 20 years, `pathlen:1`) is generated with its key
  encrypted by the operator passphrase from `--root-ca-passphrase-file` or a
  prompt; the plaintext key is never written. It signs the online issuing
  intermediate (P-256, 5 years, `CA:TRUE, pathlen:0`, certificate and CRL
  signing, SKI). `issuer-chain.pem` is the intermediate followed by the root.
- `root-ca.crt`, `root-ca.key.enc` and `root-ca.sha256` are exported to
  `/root/ocservia-root-ca-export` or `--root-ca-export-dir` before the issuer is
  installed. Interactively, the operator copies them off the host and confirms
  the last 8 fingerprint characters; the encrypted key is then deleted from the
  host. `--non-interactive` records `root_ca_custody: delegated` and leaves
  moving `root-ca.key.enc` off the host to the operator.

Existing files are reused and a rerun finishes an interrupted run, including a
prepared but unconfirmed root CA. On success, `quick-install.json` in the
Controller secret directory records the custody, root fingerprint, the
Controller endpoint ID and file SHA-256 digests, never secret values. After
that record exists, or once a Signer ledger exists, the script never generates
material again; a missing file fails closed. Signer `init` in the lifecycle
remains the authority that validates the CA. Focused check:
`scripts/test-controller-quick-materials.sh` (root or passwordless sudo).

### Policy and durable effects

Policy `rsa-client-v1-24h` accepts signed RSA-2048/3072/4096 CSRs with exponent
65537, SHA-256/384/512 RSA or RSA-PSS signatures, one CN, and at most 32 DNS SANs.
CN/DNS strings are bounded printable ASCII without whitespace or path
separators. Non-DNS SANs and extensions other than DNS SAN are rejected.
Issued certificates have only digital-signature/key-encipherment and clientAuth
usages, never CA privileges, and 24h validity within issuer constraints.
The resulting chain is verified against CA constraints before persistence.
Controller still owns subject authorization, independent approval and privd
receipt validation; a CSR self-signature is not approval evidence.

The unsigned 128-bit certificate UUID is its positive X.509 serial. One ID
therefore cannot acquire another serial. A bbolt write transaction binds the
UUID, exact CSR and digest, result chain, serial and audit record before reply.
Concurrent operations serialize through its single writer. A crash before
commit has no externally exposed certificate. After commit, a lost response
replays the exact stored chain. A different CSR conflicts even after revoke.
Revocation stores its timestamp and exact reason; changed serial/reason
conflicts. Historical sign replay never clears revocation or reissues.

`crl` writes a signed PEM CRL to stdout from durable revocations, with a one-hour
next-update and audit revision as CRL number. Run it through the protected
operator workflow and publish atomically to the intended verifier separately.
Reason text remains in the ledger; CRL reason code is unspecified. A 204 revoke
is not proof of node cleanup, VPN session termination or CRL distribution.
There is no public CRL endpoint or OCSP service; refresh and verifier enforcement
are explicit operator responsibilities, not established by unit checks.

### Trusted public-key transfer

Stop affected mutations while changing mappings. There is no automatic sync or
in-place rotation. Only a protected operator may import or disable bindings;
there is no HTTP key registration endpoint.

1. On the node, run `scripts/export-node-sealing-keys.py` as root with `--node`,
   `--endpoint`, `--user-key`, `--user-key-id`, `--p12-key`, `--p12-key-id`.
   Derivation is exactly OpenSSL RSA public SPKI DER, matching privd startup.
   Private keys never leave the node. Use `umask 077` before redirecting output.
2. On Controller, run `/usr/local/bin/ocserv-sealing-export` with
   `--database-url-file`, `--backend`, optional `--database-ca-file`, and the
   independently verified `--workspace`, `--node`, `--endpoint`, `--approval`.
   This is an OS-protected administrative CLI using authenticated database
   credentials, not a new public API. The credential must be an absolute canonical
   path with root- or process-owned ancestry that is not group/world writable;
   symlinks are rejected. The regular file must have one link, mode 0400/0600,
   root/process ownership and 1..4096 bytes. Reads use the verified descriptor,
   not a second path lookup. It uses existing domain stores and a
   repeatable-read transaction, locks the node, requires active/offline node
   plus active endpoint, reconstructs the approval binding and verifies its
   independently approved, consumed state before exporting persisted key
   descriptors. Pending/revoked nodes and mismatched identities are rejected.
3. Transfer the Controller output and node output over an authenticated
   operator channel into owner-only regular files on Signer. Do not accept
   an approval JSON supplied by the node. JSON is not a self-authenticating
   signature: file provenance and the trusted operator channel are mandatory.
4. Stop Signer and run `import --approved /path/controller.json
   --public /path/node.json --workspace UUID --node UUID --endpoint HEX`, with
   the configured CA/state flags. Controller export must be less than 15 minutes
   old. Import compares both independent identities, purposes, versions, IDs,
   DER hashes and two distinct RSA keys, then atomically stores provenance and
   an audit event. Restart Signer. Exact repeats are no-ops; replacements fail.
5. Before retiring/re-enrolling a node, stop affected mutations, reconcile
   pending work, stop Signer and run `disable --node UUID`. Restart Signer.
   Disabled bindings persist and cannot be reactivated or replaced. A newly
   enrolled node identity needs a new approved import. Controller revocation
   does not automatically disable Signer's offline mapping. Complete both
   Controller retirement and this explicit Signer step using the
   [Integrated maintenance procedure](../../deploy/production/integrated/README.md#operator-lifecycle-and-maintenance).

Signer never receives Controller database credentials. Its HTTP sealing path
selects only the imported mapping and uses RSA-OAEP-SHA256/MGF1-SHA256, empty
label and standard base64. Passwords are not persisted or logged. Responses
echo version 1, the exact purpose and its key ID. Missing/disabled mappings
return 403. The two purposes cannot share a key or descriptor.

### State and recovery

State version is 1, fixed to [bbolt v1.4.3](https://github.com/etcd-io/bbolt/releases/tag/v1.4.3),
with an exclusive process lock and transaction-consistent snapshots.
Default synchronous commits remain enabled. Local storage must honor fsync;
network filesystems, multiple replicas and HA are unsupported.

Buckets are `meta` (version, issuer SHA-256, policy, record checksums), `issued`
(UUID records), `bindings` (node records), and `audit` (monotonic revision).
Startup runs the database consistency check, verifies record checksums,
certificate signatures/CSR bindings and imported key descriptors. Checksums
detect accidental corruption, not malicious rewriting by an authorized
state-volume owner. Do not edit or downgrade state manually.

All administrative commands acquire the same exclusive lock, so stop the
server before import, disable, inspect, backup, restore or CRL export.
`inspect` emits version, issuer, policy and last audit revision. Retain this
high-water mark outside the volume with the backup manifest and CA fingerprint.
`backup --output /protected/new-snapshot.db` uses a consistent read transaction,
exclusive destination creation, file fsync and directory fsync. It never
overwrites an existing backup. A failed backup is not usable merely because
its destination exists; validate with `inspect` using `--state` on the snapshot.

`restore --state /protected/snapshot.db --output /protected/new-ledger.db
--minimum-revision N` validates the snapshot and refuses revisions below the
independently reconciled high-water mark or an existing destination. The
destination directory must be 0700 and final ownership/mode must match runtime.
Use the same CA; key custody/backup is separate. Activate the verified new file
only after reconciling Controller state, all later issuance/revocation, imports
and disables. Never derive the required floor solely from the old snapshot.
No local tool can prove freshness after both ledger and independent evidence
are lost. That situation requires manual reconciliation, not a floor of zero,
clearing state, changing CA, or an automatic image rollback.

### Focused verification

Run in an authorized isolated environment with the required Go/Rust toolchains,
a checkout with trusted ancestry and a private `TMPDIR`. Fixture CAs and node keys are generated for tests, never production.

```sh
cd signer
GOWORK=off go test -race -count=1 ./...
GOWORK=off go test -c -o /absolute/task/signer-interop.test
cd ../control-plane
GOWORK=off go test -race -count=1 -skip Integration ./internal/certificates ./internal/platform/config ./internal/platform/app ./internal/enrollment ./cmd/ocserv-sealing-export
# With an isolated, migrated PostgreSQL database and restricted runtime account:
GOWORK=off go test -race -count=1 ./internal/enrollment -run '^TestEnrollmentBackendIntegration$' -v
cd ../rust
OCSERV_SIGNER_INTEROP_HELPER=/absolute/task/signer-interop.test \
SIGNER_NODE_EXPORT_SCRIPT=/absolute/repo/scripts/export-node-sealing-keys.py \
cargo test --locked -p ocservia-ocserv-adapter certificate_p12_is_encrypted_bounded_and_replayable -- --nocapture
```

The Rust check exercises actual privd adapter/OpenSSL decryption, P12 creation,
wrong-purpose rejection and replay using ciphertext returned by Go's HTTP
handler, with node-script exports. It is not a full privd RPC/Controller business
E2E, Registry-image acceptance, or proof of deployed CRL enforcement.
Subprocess crashes cover preparation, signing, pre-commit and post-commit/lost
reply. Tests also cover duplicate concurrency, corruption, recovery floors,
TLS trust/SAN/default isolation, authentication, body limits and disabled keys.

### Disposable Actions acceptance

Dispatch `release-upgrade.yml` with `purpose=integration`,
`production_signer=true` and a plain test version such as `0.0.0` on the branch
being tested. It natively builds eight first-party images on amd64 and uses a
loopback test registry with ordinary platform configuration. Release product
jobs separately scan their exact images on both amd64 and arm64 before smoke
and publication.
The existing Integrated lifecycle runs the actual Signer implementation;
no job has GHCR publishing authority. Manual Release Check on main always runs
Integrated Business Smoke with four finite single-instance recoveries. The
extended checks below remain manual `purpose=integration` coverage. See the
[CI and publication flow](../../deploy/production/integrated/README.md#ci-and-publication).

The extended native daemon checks use the production Signer with an online
intermediate, approved Controller export and actual node public-key export.
Both sealing purposes reach real privd effects. The exported P12 authenticates
against a second native ocserv on the disposable runner. After Controller
revocation, the operator exports and verifies a new CRL, atomically distributes
it to that ocserv and reloads it with SIGHUP. Acceptance requires revoked
certificate rejection, an advancing CRL number and successful authentication
of an unrevoked control certificate. This proves the explicit operator refresh
path, not scheduled distribution, online OCSP or termination of existing VPN
sessions. Raw keys, credentials and login cookies remain private to the runner.

`registry-pulls.jsonl`, `crl-acceptance.json` and the existing API, root-receipt,
browser and business evidence are retained as sanitized Actions artifacts.
Missing files or failed assertions block acceptance and merge. A PR opened
while validation is running is not evidence of acceptance.


### Browser password sealing

The authenticated `POST /sign/public-key` accepts only `node_id` and
`purpose=user_password`. It reads the enabled durable binding established by
`import-binding`, and returns its workspace, node, endpoint, version, key ID,
SHA-256 fingerprint and RSA SPKI DER (standard base64). It uses the existing
Bearer authentication, admission limits and JSON bounds. Disabled or missing
bindings are rejected, including after restart; no private keys are returned.
The fifteen-minute import freshness check is not a key expiry time.

Controller `GET /api/v1/nodes/{node_id}/user-password-sealing-key` requires the
existing node-scoped `user.manage` permission. The API role reuses its configured
HTTPS Signer and dedicated CA client. Enrollment checks the response against
current node/workspace/endpoint state, approved `ocserv.users.write` capability
and the enrolled purpose/version/key ID/fingerprint, then verifies SPKI digest,
RSA size and exponent. Mismatches return 409; an unavailable source returns 503.
Responses use `Cache-Control: no-store`. A rotated descriptor requires matching
trusted provisioning; this read never imports or replaces a binding.

The Create user and Rotate password dialogs accept ordinary passwords. In a
secure browser context, WebCrypto seals UTF-8 bytes with RSA-OAEP/SHA-256
(MGF1-SHA-256, empty label) into the existing version-1 `user_password` envelope.
Only ciphertext reaches the existing mutation API. The page clears input
before asynchronous work and clears byte buffers on success, failure or stale
completion; closing, changing node/workspace, and unmounting cancel the read.
Passwords are never placed in receipts, URLs, logs, analytics or browser storage.
Retry after failure requires re-entry. Existing mutation authorization, desired
revision, signed command and privd checks remain in force.
