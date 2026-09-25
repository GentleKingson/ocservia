# Production Signer

P2 implements the [P0 wire and custody contract](integrated-deployment-adr.md#signer-contract).
It is a separate Go module/process, not a Controller command-signing provider.
The Python release-business signer remains a disposable test fixture.

## Runtime contract

Build with `docker build -f deploy/production/signer.Dockerfile .`.
The image runs `/ocserv-signer serve` as UID:GID `65532:65532`; run with a
read-only root filesystem, all capabilities dropped, no-new-privileges and
only the Controller-to-Signer internal network. No host port is required.
P3 owns Compose integration; P4 owns manifest/Registry publication.

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

The three POST paths are exactly `/sign`, `/sign/revoke`, `/sign/seal`.
JSON requests are capped at 64 KiB, raw sealing requests at 512 bytes and then
the actual RSA-OAEP capacity (190 bytes for RSA-2048). At most 32 requests enter
business handling; excess returns 503. Header/read/write/idle timeouts are
5/10/15/30 seconds. Failures return fixed status text, not request content.

## Policy and durable effects

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
P5/P6 must prove verifier installation/refresh/enforcement. There is no public
CRL endpoint or OCSP service in P2.

## Trusted public-key transfer

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
   does not automatically disable Signer's offline mapping; P6 must include
   this explicit step in decommissioning.

Signer never receives Controller database credentials. Its HTTP sealing path
selects only the imported mapping and uses RSA-OAEP-SHA256/MGF1-SHA256, empty
label and standard base64. Passwords are not persisted or logged. Responses
echo version 1, the exact purpose and its key ID. Missing/disabled mappings
return 403. The two purposes cannot share a key or descriptor.

## State and recovery

State version is **1**, fixed to [bbolt v1.4.3](https://github.com/etcd-io/bbolt/releases/tag/v1.4.3).
bbolt was chosen over suggested SQLite for a single pure-Go transactional
ledger with an exclusive process lock and transaction-consistent snapshots.
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

## Focused verification

Run only on BuildServer in an isolated copy with trusted ancestry and a private
`TMPDIR`. Fixture CAs and node keys are generated for tests, never production.

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

P3 receives the paths, UID/modes, health and state/recovery contracts above.
P4 receives `deploy/production/signer.Dockerfile` and Controller's export CLI.
P5 receives the focused tests; production-route acceptance must use the
designated Registry images and separately prove real daemon/workflow execution.

## Disposable Actions acceptance

With operator authorization for candidate publication, dispatch
`release-upgrade.yml` with `purpose=integration`, `production_signer=true` and
the candidate version on its exact branch. This opt-in path publishes six
candidate images under unique SHA/run/attempt GHCR tags, removes their local
tags, pulls their immutable digests and installs the Controller through the
signed candidate manifest. It does not publish a Release or change stable tags.
The ordinary smoke/integration caller has only `contents: read`; a separate
opt-in integration caller alone grants `packages: write`. Both call the same
reusable business workflow, which inherits rather than widens caller permissions.

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
Missing files or failed assertions block acceptance; the workflow must finish
successfully before creating an acceptance PR.
