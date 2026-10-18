# Certificates, secrets and Signer

## Certificate and secret lifecycle

Managed nodes generate certificate private keys locally and never expose them
to the control plane. The Controller receives a signed CSR and public-key
digest, and forwards the CSR to a configured external PKI signer only after an
independent, content-bound approval is recorded. Because the Controller stores
neither a CA private key nor any node private key, and because a CSR
self-signature alone does not prove privileged execution on the node, a request
enters `csr_ready` only after the Controller verifies a root privd receipt that
binds the certificate ID, CSR and public-key digests, requested-subject digest,
node, command, operation, idempotency key, and root effect record. Immediately
before calling the external signer, the Controller locks the certificate row
and rechecks the exact CSR digest, receipt-bound request and version, node,
approval hash, and non-revoked key state. P12 export and certificate-key
revocation enforce the same terminal-result attestation rule.

Managed-node certificates and secret sealing are distinct from the Controller's
own HTTPS listener certificate (configured via manual files or ACME in
Integrated mode), which the Signer neither issues nor serves.

The external HTTPS signer client is configured through
`OCSERV_CERTIFICATE_SIGNER_URL`, `OCSERV_CERTIFICATE_SIGNER_TOKEN`, and
`OCSERV_CERTIFICATE_SIGNER_TIMEOUT`. The upstream service must treat signing
and revocation as idempotent by certificate ID and support node-targeted secret
sealing. When the signer is unreachable, certificate issuance transitions to
`signer_unavailable` (which can be resumed by retrying under the same approval)
and revocation transitions to `revocation_unknown` (which a retry may submit
again); both paths return a service-unavailable problem response without
marking the operation successful. Setting `OCSERV_CERTIFICATE_SIGNER_CA_FILE`
to a public PEM CA bundle configures a private trust chain scoped exclusively
to this client, leaving the process-wide trust store and other external HTTPS
connections untouched. See [Production Signer](#production-signer) for the
bundled implementation, public-key transfer workflow, and offline recovery
procedures.

Requests to `/sign/seal` pass `X-Ocservia-Node-ID` and an exact
`X-Ocservia-Seal-Purpose` header (`user_password` or
`certificate_p12_password`). The response must echo `version: 1`, the requested
purpose, the enrolled purpose-specific `key_id`, and the base64 ciphertext; the
Controller rejects any response with a missing, substituted, or unregistered
binding.

When exporting a P12 bundle, the Controller generates a fresh random password
and an independent random artifact token, returning both only in the initial
HTTP response and never persisting them in the Controller database. The
password is sealed to the node's dedicated P12-password public key, which
cannot be decrypted by the separate user-password key. On the node, privd
decrypts the typed secret locally, writes an encrypted UUID-addressed artifact
into its fixed root-owned spool, and records the certificate and version,
operation, digest, byte size, expiry, and state in its authenticated effect
store. Because the root mapping enters `prepared` before any staging file is
created and advances to `available` only after the final artifact is published,
certificate revocation can clean up both interrupted staging files and
completed artifacts.

Downloading an exported P12 bundle requires standard node authorization, the
separate `X-Artifact-Token` header, and a short-lived Controller-signed
`ArtifactGrantV1` bound to the node, artifact, certificate and version,
operation, requester, purpose, maximum size, and unique grant ID. Both the
Agent and privd verify the grant independently. Only one grant may lease an
artifact at a time, and an interrupted lease remains locked until its bounded
expiry elapses. The root ledger also tracks the exact chunk offset so a second
stream cannot replay the same grant from offset zero. Streaming the bytes does
not itself consume the artifact: once the Control Plane receives the full
stream and verifies its size and digest, it sends a separate finalize request
carrying the same signed grant to the Agent and privd, which durably marks the
grant consumed and deletes the local P12 file. Consumed grants cannot be used
to fetch the artifact again, though an exact finalize retry may acknowledge the
already-consumed root record without reopening the file or repeating deletion
so a dropped network response never strands the Controller lease. Revoking a
certificate invalidates all outstanding grants and deletes any mapped P12 and
staging files, leaving time-based cleanup solely as crash recovery for expired
or orphaned files.

In the control plane, secret provider records store only the provider name,
opaque key path, version, and lifecycle state while the secret values themselves
stay in the external provider. Rotating a secret records the new external
version and appends an authenticated audit event without copying the secret
value into the Controller.

Thirty days before `not_after`, a certificate enters `expiring` and triggers a
high-severity alert. Revocation calls the external signer idempotently first
and, once that succeeds, releases the node operation to remove only the
UUID-derived node-local key. Before running database recovery, stop certificate
and artifact creation, reconcile all nonterminal certificate commands, and
preserve terminal command history along with root effect evidence. Because the
repository does not use database down migrations, recover through a forward fix
or an explicitly planned
[isolated restore](../operations/incident-recovery.md#database-recovery).

## Production Signer

The production Signer is a standalone Go module and process operating within
the [Integrated custody boundary](integrated-deployment-contract.md#signer-contract)
for node certificates and secret sealing; it does not sign Controller commands,
and the Python business signer in the repository exists only as a test fixture.

Key custody and permissions are partitioned across four roles:

- The offline root CA private key is never mounted into the Signer container;
  Quick install exports it in encrypted form for operator custody.
- Signer holds only the online issuing intermediate key, its own HTTPS private
  key, and the bbolt ledger containing issued certificates, node sealing-key
  bindings, and the audit log. It never receives Controller database
  credentials.
- The Controller holds neither a CA private key nor any node key, authenticating
  to Signer through `certificates.HTTPSigner` using a shared Bearer token and
  the public HTTPS CA bundle.
- Each managed node keeps its certificate private key and its two sealing
  private keys (`user_password` and `certificate_p12_password`) restricted to
  root, exporting only public key material so Signer can seal secrets to those
  imported public keys.

On the write path, the Controller validates the approval and privd receipt
before sending an authenticated HTTPS `POST /sign` or `/sign/revoke` request,
and Signer commits the change to its bbolt ledger before replying. Recovery
across the two services is coupled: restoring the Signer ledger without the
matching issuer CA, or without reconciling Controller certificate state and all
subsequent issuances, revocations, key imports, and key disables, leaves the
deployment inconsistent (see [State and recovery](#state-and-recovery)). A
committed revocation updates the ledger and subsequent CRL exports, while
active VPN sessions remain connected until enforced by the verifier.

### Runtime contract

Build the container image with `docker build -f deploy/production/signer.Dockerfile .`.
The container executes `/ocserv-signer serve` as UID:GID `65532:65532` with a
read-only root filesystem, all Linux capabilities dropped,
`no-new-privileges`, and network attachment restricted to the internal
Controller-to-Signer network without host port bindings.

Default file paths and listener settings:

| Input | Default |
| --- | --- |
| HTTPS listener | `:9443` |
| Online issuing intermediate chain, intermediate first and root last | `/run/secrets/issuer-chain.pem` |
| Online intermediate private key, unencrypted PEM | `/run/secrets/issuer-key.pem` |
| HTTPS certificate/key | `/run/secrets/tls-cert.pem`, `/run/secrets/tls-key.pem` |
| Independent API Bearer token, 32..256 non-whitespace bytes | `/run/secrets/api-token` |
| Exclusive writable state volume | `/var/lib/ocservia-signer/ledger.db` |
| Health client's public trust bundle | `/run/secrets/tls-ca.pem` |

All configured paths must be absolute canonical paths residing under real
directories owned by root or the runtime process, with no group- or
world-writable ancestor. The state directory requires mode `0700` and the
ledger file requires mode `0600` owned by the runtime process. Private key and
token files must be regular, single-link, owner-only files readable by the
runtime UID (mode `0400` or `0600` recommended) and mounted read-only.
Provision the root CA offline and never mount its private key. At startup, the
service verifies that the intermediate-and-root chain matches the intermediate
private key, carries CA certificate-signing and CRL-signing key usages, and has
at least 24h of remaining validity; it never generates CA, node, or TLS keys of
its own.

First-time installation runs `init` with the same CA and state flags as
`serve` to create a fresh ledger when none exists. Do not use `init` to repair
lost state: normal `serve` startup refuses to run if the ledger is absent,
empty, incompatible, corrupt, owned by another user, or bound to a different
issuer, and never initializes a replacement automatically.

On the Controller, set `OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign`
alongside the token-file setting and point `OCSERV_CERTIFICATE_SIGNER_CA_FILE`
to the mounted public HTTPS CA bundle. The Signer HTTPS leaf certificate must
include `signer` in its SAN list. Because this custom trust pool applies only
to the Signer HTTP client, neither `SSL_CERT_FILE` overrides nor TLS
verification bypasses are needed, and omitting the custom CA setting leaves
standard external HTTPS trust in place. The TLS CA bundle authenticates the
HTTPS connection only and confers no authority to sign client certificates.

`GET /healthz` returns `204` while the ledger is readable, the issuer
certificate has sufficient remaining lifetime, and no storage error has latched
readiness off, without performing any signing operation. The CLI check
`health --url https://signer:9443/healthz` validates HTTPS with `--tls-ca`,
refuses HTTP redirects, and can target loopback only when the leaf SAN permits
it. If a storage fault latches readiness off, restart the service after
repairing the underlying volume; restarting preserves ledger state.

The server exposes four POST endpoints: `/sign`, `/sign/revoke`, `/sign/seal`,
and `/sign/public-key`. JSON request bodies are capped at 64 KiB, while raw
sealing payloads are bounded to 512 bytes at ingress and further constrained by
the RSA-OAEP payload limit (190 bytes for RSA-2048). Concurrency is limited to
32 in-flight business requests, with additional requests rejected as `503`.
Header, read, write, and idle timeouts are fixed at 5, 10, 15, and 30 seconds,
and error responses return fixed status messages without echoing request
content.

### Quick install materials

For an initial integrated installation using bundled PostgreSQL and root-managed
lifecycle scripts, `deploy/production/quick-install.sh` invokes
`deploy/production/quick-materials.sh`. Running as root prior to service
activation with `OCSERV_SECRET_DIR`, `OCSERV_SIGNER_SECRET_DIR`, and
`OCSERV_SIGNER_STATE_DIR`, the script creates any missing Controller and Signer
secrets with the exact ownership and file modes verified by `compose.sh`,
leaving Gateway and Relay TLS identities to their own provisioning paths.

- Random secrets are generated as 32-byte hex strings, with database DSNs and
  `postgres.pgpass` derived from the generated role passwords. Controller
  command signing and `controller-iroh.key` use distinct Ed25519 keys.
- A one-shot TLS CA issues the Signer HTTPS leaf certificate (SAN `signer`,
  P-256, 5-year validity) and immediately discards its own private key,
  retaining only `tls-ca.pem`.
- The offline root CA (P-256, 20-year validity, `pathlen:1`) is generated with
  its private key encrypted under the operator passphrase supplied via
  `--root-ca-passphrase-file` or an interactive prompt, so the plaintext root
  key never touches disk. It signs the online issuing intermediate (P-256,
  5-year validity, `CA:TRUE, pathlen:0`, certificate and CRL signing usages,
  SKI), and writes `issuer-chain.pem` with the intermediate certificate
  followed by the root certificate.
- Before installing the online issuer, the script exports `root-ca.crt`,
  `root-ca.key.enc`, and `root-ca.sha256` to `/root/ocservia-root-ca-export`
  (or `--root-ca-export-dir`). In interactive mode, the operator copies those
  files off the host and confirms the final 8 characters of the fingerprint
  before the script deletes `root-ca.key.enc` locally. Under
  `--non-interactive`, the script records `root_ca_custody: delegated` and
  leaves off-host removal of `root-ca.key.enc` to the operator.

Re-running the script reuses existing files and completes any interrupted
setup, including an exported root CA awaiting confirmation. Once setup
succeeds, `quick-install.json` in the Controller secret directory records the
custody mode, root fingerprint, Controller endpoint ID, and file SHA-256
digests without storing secret values. After `quick-install.json` or a Signer
ledger exists, the script refuses to regenerate material and fails closed if
any required file is missing; Signer `init` remains the authority that
validates the CA chain during startup. You can exercise this workflow in
isolation by running `scripts/test-controller-quick-materials.sh` as root or
with passwordless sudo.

### Policy and durable effects

The `rsa-client-v1-24h` signing policy accepts CSRs using RSA-2048, RSA-3072,
or RSA-4096 keys with public exponent 65537, signed with SHA-256, SHA-384, or
SHA-512 RSA or RSA-PSS, containing one CN and up to 32 DNS SANs. Both CN and
DNS SAN values must be bounded printable ASCII strings free of whitespace and
path separators; any non-DNS SAN or other X.509 extension is rejected. Issued
certificates carry only digital-signature, key-encipherment, and `clientAuth`
usages without CA privileges, are valid for 24h within the issuer's validity
window, and are verified against the CA chain before being persisted. Subject
authorization, independent approval, and privd receipt validation remain
Controller responsibilities, as a CSR self-signature never establishes approval.

Each certificate's unsigned 128-bit UUID is used directly as its positive X.509
serial number, preventing a single certificate ID from ever receiving multiple
serials. Before returning a response, a single-writer bbolt transaction binds
the certificate UUID, exact CSR bytes and digest, issued certificate chain,
serial number, and audit record. If the process crashes before that transaction
commits, no certificate is exposed externally; if a response is lost after
commit, replaying the identical CSR returns the stored certificate chain, while
submitting a different CSR for the same ID conflicts even after revocation.
Revocation commits the timestamp and exact reason string, rejecting any later
attempt that alters the serial or reason, and replaying an earlier sign request
never clears a revocation or reissues the certificate.

Running `crl` reads durable revocations from the ledger and writes a signed PEM
CRL to stdout with a one-hour `nextUpdate` window and the current audit
revision as the CRL number. Operators run `crl` through the protected
maintenance workflow and publish the output atomically to the target verifier.
Because the ledger stores free-form reason text, the X.509 CRL reason code
extension is omitted. Receiving a `204` from `/sign/revoke` confirms only that
the ledger recorded the revocation; it does not clean up node files, terminate
active VPN sessions, or distribute the updated CRL, and the system exposes no
public CRL endpoint or OCSP responder.

### Trusted public-key transfer

Pause affected node mutations before modifying sealing-key mappings. Signer
provides neither automatic synchronization nor in-place key rotation, and
exposes no HTTP endpoint for key registration; only a protected operator can
import or disable node bindings:

1. On the managed node, run `scripts/export-node-sealing-keys.py` as root with
   `--node`, `--endpoint`, `--user-key`, `--user-key-id`, `--p12-key`, and
   `--p12-key-id`. The script derives the exact OpenSSL RSA public SPKI DER
   checked by privd at startup without exporting private keys. Set `umask 077`
   before redirecting its output.
2. On the Controller host, run `/usr/local/bin/ocserv-sealing-export` with
   `--database-url-file`, `--backend`, `--database-ca-file` for an external
   database or `--bundled-postgres` for bundled PostgreSQL, and the
   independently verified `--workspace`, `--node`, `--endpoint`, and
   `--approval` flags. This OS-protected administrative CLI authenticates
   directly to the database without exposing a public API. The database
   credential file must be a single-link regular file (mode `0400` or `0600`,
   1 to 4096 bytes, owned by root or the process) at an absolute canonical path
   without symlinks or group- or world-writable ancestors, and is read from the
   verified file descriptor without a second path lookup. Inside a
   repeatable-read transaction using the domain stores, the command locks the
   node row, verifies that the node is active or offline with an active
   endpoint, reconstructs the approval binding, and confirms its independently
   approved and consumed state before exporting the persisted key descriptors,
   rejecting pending or revoked nodes and mismatched identities.
3. Copy both the Controller export and the node export over an authenticated
   operator channel into owner-only regular files on the Signer host. Never
   accept an approval JSON file originating from the node itself: because JSON
   carries no self-authenticating signature, security depends on file
   provenance and the trusted operator transfer channel.
4. Stop Signer and run `import --approved /path/controller.json
   --public /path/node.json --workspace UUID --node UUID --endpoint HEX` with
   the configured CA and state flags. The Controller export must be under 15
   minutes old. `import` verifies that both files agree on identities,
   purposes, versions, key IDs, and DER hashes across two distinct RSA keys,
   then atomically commits the provenance record and audit event before you
   restart Signer. Repeating an identical import is a no-op, whereas attempting
   to replace an existing binding fails.
5. Before retiring or re-enrolling a node, stop affected mutations, reconcile
   pending work, stop Signer, run `disable --node UUID`, and restart Signer.
   Disabled bindings remain permanently recorded in the ledger and cannot be
   reactivated or overwritten, so a newly enrolled node identity requires a
   fresh approved import. Because revoking a node in the Controller does not
   automatically disable Signer's offline key mapping, always complete both
   Controller retirement and this Signer disable step following the
   [Integrated maintenance procedure](../../deploy/production/integrated/README.md#operator-lifecycle-and-maintenance).

During normal operation, Signer's HTTP sealing handler uses only mappings
created by `import`, encrypting passwords with RSA-OAEP-SHA256 and MGF1-SHA256
(empty label, standard base64 output) without persisting or logging plaintext
passwords. Responses echo `version: 1`, the exact purpose, and the key ID;
missing or disabled bindings return `403`, and the two sealing purposes may
never share a key or descriptor.

### State and recovery

The ledger uses state format version 1 backed by bbolt (version pinned in
[`signer/go.mod`](../../signer/go.mod)) with an exclusive process lock,
transaction-consistent snapshots, and default synchronous commits enabled. The
underlying local filesystem must honor `fsync`; network filesystems, multi-node
replication, and high-availability clustering are unsupported.

Inside the database, four buckets organize state: `meta` (schema version,
issuer SHA-256, policy name, and record checksums), `issued` (certificate
records keyed by UUID), `bindings` (node key mappings), and `audit` (monotonic
revision log). On startup, Signer runs bbolt's consistency check and validates
all record checksums, certificate signatures, CSR bindings, and imported key
descriptors. These checksums detect accidental storage corruption; they do not
defend against deliberate rewriting by an authorized owner of the state volume.
Never edit or downgrade the ledger file by hand.

Because every administrative CLI subcommand acquires the same exclusive bbolt
lock as the server, stop `serve` before running `import`, `disable`, `inspect`,
`backup`, `restore`, or `crl`. Running `inspect` prints the state version,
issuer fingerprint, policy, and latest audit revision; record this audit
high-water mark outside the state volume alongside your backup manifest and CA
fingerprint. Running `backup --output /protected/new-snapshot.db` opens a
consistent read transaction, creates the destination file exclusively, and
fsyncs both the file and its parent directory without ever overwriting an
existing snapshot. Because a partial file left by an interrupted backup is unusable,
always validate a new snapshot by running `inspect --state` against it.

When recovering, `restore --state /protected/snapshot.db --output /protected/new-ledger.db
--minimum-revision N` verifies the snapshot and refuses to write if its audit
revision falls below the independently reconciled high-water mark `N` or if the
output path already exists. Place the restored ledger in a mode `0700`
directory with runtime ownership and permissions, paired with the original
issuer CA (whose private key custody and backup are managed separately). Put
the restored ledger into service only after reconciling Controller state and
accounting for all certificate issuances, revocations, imports, and disables
that occurred after the snapshot. Never take `--minimum-revision` solely from
the old snapshot itself. If both the ledger and independent audit evidence are
lost, no local CLI can prove state freshness; halt automated recovery and
reconcile manually instead of passing a zero floor, wiping state, swapping CAs,
or rolling back container images automatically.

### Focused verification

Run the verification suite inside an authorized, isolated environment equipped
with the pinned Go and Rust toolchains, a repository checkout under trusted
directory ancestry, and a private `TMPDIR`. Test runs generate disposable
fixture CAs and node keys for test cases only.

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

The Rust interop test exercises actual privd adapter and OpenSSL decryption,
P12 bundle creation, wrong-purpose rejection, and idempotent replay against
ciphertext produced by the Go HTTP handler from node-script key exports. Its
scope is limited to adapter interoperability; full privd RPC and Controller
end-to-end flows, registry image checks, and live ocserv CRL enforcement are
covered by the workflow below. Unit and subprocess fault-injection tests cover
crash boundaries during preparation, signing, pre-commit, and post-commit lost
replies, as well as concurrent duplicates, ledger corruption, recovery floors,
TLS trust and SAN isolation, authentication, request size limits, and disabled
bindings.

### Disposable Actions acceptance

To run end-to-end acceptance on a test branch, dispatch `release-upgrade.yml`
with `purpose=integration`, `production_signer=true`, and a plain test version
such as `0.0.0`. The workflow natively builds all eight first-party images on
amd64 and pushes them to a loopback registry using standard platform
configuration, while release publication jobs separately scan amd64 and arm64
images prior to smoke testing and publishing. The Integrated lifecycle harness
runs the production Go Signer without granting GHCR publishing permissions to
test jobs. While manual Release Check runs on `main` always execute Integrated
Business Smoke with four finite single-instance recoveries, the extended checks
described below run under manual `purpose=integration` dispatches; see the
[CI and publication flow](../../deploy/production/integrated/README.md#ci-and-publication).

Those extended native-daemon checks exercise the production Signer with an
online intermediate CA, an approved Controller key export, and a real node
public-key export so both sealing purposes drive actual privd mutations. The
exported P12 bundle then authenticates against a second native `ocserv`
instance on the disposable runner. After the Controller revokes the
certificate, the test harness exports and verifies a fresh CRL, atomically
places it into that `ocserv` instance, and sends `SIGHUP` to reload it. Passing
acceptance requires `ocserv` to reject the revoked certificate, report an
incremented CRL number, and still authenticate an unrevoked control
certificate, demonstrating the explicit operator CRL refresh workflow (as
distinct from scheduled CRL distribution, online OCSP, or active session
termination). Private keys, credentials, and session cookies stay confined to
the disposable runner.

Sanitized `business-*` Actions artifacts retain `crl-acceptance.json` alongside
API, root-receipt, browser, and business verification logs. Any missing
artifact file or failed assertion blocks acceptance and merge, and opening a
pull request while the workflow is still running does not count as passing
verification.


### Browser password sealing

The authenticated `POST /sign/public-key` endpoint accepts requests containing
only `node_id` and `purpose=user_password`. It reads the active durable binding
created by `import` and returns the workspace, node, endpoint, version, key ID,
SHA-256 fingerprint, and standard base64-encoded RSA SPKI DER under the same
Bearer authentication, concurrency limits, and JSON size bounds as other Signer
endpoints. Missing or disabled bindings fail even across restarts, no private
key material is ever exposed, and the 15-minute freshness window enforced
during `import` does not act as a runtime key expiration.

On the Controller, `GET /api/v1/nodes/{node_id}/user-password-sealing-key` requires
the node-scoped `user.manage` permission and queries Signer through the API
role's configured HTTPS Signer client and dedicated CA pool. Before returning
the key with `Cache-Control: no-store`, the enrollment service verifies the
Signer response against the current node, workspace, and endpoint state, checks
that `ocserv.users.write` is approved, confirms that the purpose, version, key
ID, and fingerprint match the enrolled record, and validates the SPKI digest,
RSA modulus size, and public exponent. State or descriptor mismatches return
`409`, while an unreachable Signer returns `503`. Rotating a sealing key
requires completing the trusted provisioning flow; this read endpoint never
imports or updates bindings.

In the Web UI, the Create user and Rotate password dialogs accept a plaintext
password in a secure browser context and use WebCrypto to encrypt its UTF-8
bytes with RSA-OAEP-SHA256 (MGF1-SHA256, empty label) into a version-1
`user_password` envelope so only ciphertext is sent to the mutation API. The
browser clears the form input before starting asynchronous requests and zeros
temporary byte buffers upon completion, failure, or stale cancellation, while
closing the dialog, switching nodes or workspaces, or unmounting the component
aborts any in-flight key lookup. Plaintext passwords never enter receipts,
URLs, logs, analytics, or browser storage, and a failed submission requires the
operator to type the password again. All existing mutation authorization,
desired-revision, signed-command, and privd checks still apply on the backend.
