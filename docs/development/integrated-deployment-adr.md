# Integrated deployment contract

Status: proposed implementation contract, not production support or runtime
acceptance. Reviewed on 2026-09-24 against local `main` and remote `main` at
`e861b72130d4d409883f68d01805566f26cbafbc`, identical to the planning baseline.
P0 delivers this decision record only. P1-P6 implement and prove it; a gate
below is a required outcome, not a recorded pass. Existing
[stable contracts](../reference/stable-contracts.md) remain authoritative.

## Context and review

The target is one host, one public IP, two distinct DNS names, one
non-redundant Relay and an independent Signer process. Preserve Gateway,
control-plane and transportd. This is not a three-container deployment.
No HA, Caddy replacement, process merger, HSM/KMS implementation, new installer
or long-lived evidence service is included. Maintenance interruptions and
operator reconciliation of uncertain mutations remain explicit limitations.

| Verified baseline | Gap | Proposed owner |
| --- | --- | --- |
| [Controller Compose](../../deploy/production/compose.yaml) publishes Gateway TCP443 to TLS 8443; control-plane listens on plaintext 8080. [Caddy](../../deploy/production/Caddyfile) overwrites the API client-IP header using its peer address. | An extra TCP proxy would replace the observed client address. The catch-all site does not enforce the proposed Host allowlist. | P1 network |
| [Relay Compose](../../deploy/production/relay/compose.yaml) independently publishes TCP80, TCP443 and UDP7842. | Simply combining the two deployments conflicts on 443. No Integrated overlay exists. | P1/P3 |
| [HTTPSigner](../../control-plane/internal/certificates/http_signer.go) uses HTTPS, Bearer authentication and three fixed path shapes. | No bundled production Signer or dedicated CA trust mount is supplied. The [business fixture](../../scripts/release-business-signer.py) stores issuance in memory and only seals P12 passwords. It is not a production implementation. | P2 |
| [Enrollment proof](../../control-plane/internal/enrollment/proof.go) signs purpose, key ID and public-key digest; [enrollment service](../../control-plane/internal/enrollment/service.go) persists and checks them. | Descriptors are not public-key material. A node UUID alone cannot authorize a sealing-key import. | P2 |
| [Lifecycle](../../deploy/production/controller.sh) strictly accepts manifest v1 with six image roles. | Edge, Relay, Signer and backend-specific backup images are not all covered by that schema. | P3/P4 |
| [Release](../../.github/workflows/release.yml) gates production writes in `release-publishing`; [product builds](../../.github/workflows/release-products.yml) export archives before publishing. Dispatch is a dry run. | Registry candidate acceptance must precede final publication; it cannot depend on an already published release. | P4 |

These are design prerequisites, not reasons to weaken existing validators.
The single-host alternative keeps independent deployments unchanged but needs
additional public addresses/ports. TLS termination at Edge would change the
Gateway trust boundary. Both are rejected for this mode. Retain standalone
deployment as the compatibility default.

## Network and trust

Proposed topology; boxes are trust boundaries, not a container count:

```text
Internet: controller.example / relay.example -> same public IP
  TCP443 -> Edge (NGINX stream; no TLS private key)
              | controller SNI -> Gateway:8443 (Controller TLS ends here)
              |                     -> control-plane:8080 (private HTTP)
              | relay SNI ------> Relay:8443 (Relay TLS ends here)
  UDP7842 ---------------------> Relay:7842 (QUIC discovery)

Private networks:
  control-plane -> Signer:9443/sign (verified HTTPS + Bearer)
  control-plane <-> transportd (existing protected Unix sockets)
  transportd -> relay.example:443 -> Edge -> Relay:8443
  control-plane -> database (runtime credential)
  migrate      -> database (owner credential; one-shot)
  backup       -> database (backend-specific backup credential)
  control-plane / transportd -> optional otel-collector -> operator backend
```

Only Edge publishes TCP443; only Relay publishes UDP7842. Neither Gateway nor
Signer publishes a host port. Do not publish TCP80, Controller 8080, database,
Relay's private HTTP8080 or observability ports in Integrated mode. Provision
public certificates externally; HTTP-01 issuance is not implied. TLS SNI must
be visible; missing/unknown SNI and non-TLS input fail closed, never fall back
to the Controller. Gateway permits only the configured Controller Host/HTTP2
authority (with optional standard `:443`); wrong Host is rejected before API
or SPA serving. Relay's TLS/HTTP entry must similarly reject the wrong name.
Edge cannot enforce HTTP Host without terminating TLS.

Use separate Edge-to-Gateway and Edge-to-Relay networks, a Controller-to-Signer
internal network, and the existing application/database/observability
boundaries. Retain transportd's outbound Relay connectivity and required
OIDC/telemetry egress. A diagram does not prove Docker egress restrictions.
P1/P3 must inspect the rendered network attachments, including external DB
overlays, and deny unneeded cross-boundary access.

Gateway alone owns Controller TLS keys. Relay alone owns Relay TLS keys and
the relay access token; transportd and managed nodes also receive that token
as today. Signer alone owns CA signing keys, its HTTPS private key and durable
issuance/revocation state. Only Controller and Signer share the signing API
token. Mount the Signer CA certificate, not its private key, into Controller's
verified trust store; SAN must cover `signer`. This mount is new P2/P3 work:
the current client uses Go's default trust roots, not a custom CA option.
Do not use insecure TLS, redirects or the Relay CA mount as an implicit
Signer trust mechanism. Existing database, audit, session, command-signing
and transport key separation and file permissions remain unchanged.
Node sealing private keys remain root-owned on nodes, never in Signer.

### P1 network proof gates

The [NGINX preread module](https://nginx.org/en/docs/stream/ngx_stream_ssl_preread_module.html)
can route on ClientHello without terminating TLS, but must be present in the
locked image. [Stream proxy](https://nginx.org/en/docs/stream/ngx_stream_proxy_module.html)
documents fixed `proxy_protocol` settings, not a per-request variable there.
No untested generated configuration is frozen by this ADR.

| Risk | Candidate and smallest required proof |
| --- | --- |
| Real Controller client IP | Front SNI router forwards to private, Edge-local branch listeners with PROXY enabled. Branches accept PROXY only from that router: Gateway branch emits PROXY; Relay branch strips it before raw TLS forwarding. Verify NGINX stream real-IP handling preserves the original address across both hops, not the loopback peer. Verify locked Gateway's PROXY listener support/order and exact trusted Edge source; do not assume stock Caddy includes a module. Gateway must overwrite `X-Ocservia-Client-IP` from verified PROXY metadata. Controller continues trusting only Gateway's application `/32`, not the whole subnet. |
| Relay source IP | Baseline Relay PROXY parsing is not established. The strip branch preserves TLS compatibility but loses client IP at Relay. P1 must inventory IP-based Relay limits/logs and prove token authorization and reconnect still work. If original IP is required by a security control, block this candidate and review a native PROXY-capable Relay; do not silently claim IP preservation or add transparent-routing privileges. |
| Spoofing / name rejection | Test two external source IPs, forged forwarding headers, a direct PROXY preamble, missing/unknown SNI, and valid SNI with wrong Host. Gateway and branch listeners must not be externally reachable. Internal health probes must remain possible without opening a public bypass. |
| Container replacement | Use Compose service DNS with explicit runtime re-resolution supported by the locked NGINX version, or controlled regeneration/reload if that proof fails. Replace Gateway and Relay separately with changed addresses; new connections recover without stale upstreams. Preserve Gateway's existing fixed application address used by Controller trust. |
| Same-host Relay return path | Prefer private DNS/network alias for `relay.example` pointing to Edge on transportd's Relay network; keep URL, TLS name and certificate validation unchanged. Avoid relying on public-IP NAT hairpin. Test transportd and a real external node together, then recreate Edge/Relay and reconnect. Do not substitute an IP URL or disable verification. |

P1 records image digests, module inventory, rendered configuration, observed
addresses and negative cases on BuildServer. Failure of any required proof
blocks network acceptance, not a fallback that erases client identity.

## Signer contract

Proposed default URL: `https://signer:9443/sign`. Preserve the current client
wire format; do not reinterpret the endpoint as an origin or append a second
`/sign`. All calls are POST with `Authorization: Bearer <token>`. No redirects.
Wrong token returns 401; wrong method 405; unknown path 404. P2 implements
bounded requests, certificate policy and errors without logging credentials,
passwords or private material. The existing client collapses non-success
statuses to errors; richer server status codes do not imply client retry logic.

| Path | Request | Success | Proposed rejection rules |
| --- | --- | --- | --- |
| `/sign` | JSON strings `certificate_id` (UUID), `csr_der` (standard base64 DER); `Idempotency-Key` equals UUID | 200 JSON `certificate_chain_pem`, leaf first; entire response at most 512 KiB | 400 malformed/mismatched key; 422 invalid CSR signature or certificate policy; 409 UUID already bound to different CSR |
| `/sign/revoke` | JSON strings `certificate_id`, `serial_number` (decimal, matching stored issuance), `reason`; idempotency key `<UUID>:revoke` | 204 after durable revocation (client also accepts 200) | 404 unknown issuance; 409 serial or repeated-payload conflict; 400 malformed input |
| `/sign/seal` | Raw password bytes, `application/octet-stream`; `X-Ocservia-Node-ID` UUID; `X-Ocservia-Seal-Purpose` is `user_password` or `certificate_p12_password` | 200 JSON `sealed` (standard base64), `key_id`, `version: 1`, exact requested `purpose`; response below 20 KiB and ciphertext 32..16384 bytes | 400 invalid purpose/UUID; 403 missing, disabled or mismatched approved key binding; 413 oversize plaintext |

JSON methods use `application/json`; unsupported media types return 415.
Proposed JSON body cap is 64 KiB. Sealing plaintext must fit the selected
RSA-OAEP-SHA256 key (`modulus_bytes - 2*32 - 2`); reject, never truncate.
Use RSA-OAEP with SHA-256/MGF1-SHA256 and empty label, matching the existing
[node adapter](../../rust/crates/ocserv-adapter/src/lib.rs).
P2 restricts certificate extensions/usages and validity to the existing
Controller certificate workflow; never blindly copies CA privileges from CSR.

Persist UUID, exact CSR digest, certificate bytes/serial, policy/issuer
identity, revocation payload and terminal state before acknowledging success.
Serialize same-ID concurrent requests with storage uniqueness and a transaction.
Same ID/same CSR returns the exact stored certificate, including after revoke
(historical replay, not reissuance or unrevocation); different CSR returns 409.
Revoke duplicates with the same semantic payload succeed without another
effect; changed serial/reason conflicts. Sign/revoke races serialize by ID;
revoke before issuance commits may return 404 and must not create a tombstone
that pretends an unknown certificate was revoked.

No successful reply may precede durable commit. A crash before commit exposes
no certificate; commit plus lost response replays the same result on retry.
No serial reuse or second externally observable issuance is allowed. Storage
failure returns 503 and readiness fails; corruption fails closed on restart.
The seal endpoint has no idempotency key and is randomized/stateless for
plaintext: retries may return different ciphertext for the same approved key.
It neither creates another key binding nor executes a node mutation.

Revocation means a durable issuer revocation record, not automatic VPN session
termination or proof that ocserv consumed a CRL. P2 must produce a signed CRL
from that state through a protected operator export; P5/P6 must prove its
configured verifier distribution/enforcement before claiming certificate-use
revocation. No public CRL endpoint or new node command is assumed. Preserve
Controller `revocation_unknown` and other uncertain outcomes until reconciled;
Signer idempotency is not general automatic business recovery.

### Trusted public-key import

P2 adds a protected local import/export operation, not an unauthenticated HTTP
registration endpoint. It must implement this complete chain:

1. On the node, derive each purpose's public SPKI DER from the protected RSA
   private key, using the same encoding as [privd startup](../../rust/crates/privd/src/main.rs)
   (`openssl rsa -pubout -outform DER`). Export only public material, node
   UUID, endpoint identity, purpose, descriptor version and key ID.
2. Obtain the corresponding active, approved Controller enrollment binding
   through an authenticated administrative export implemented by P2. Verify
   workspace/node/endpoint identity and enrollment approval, not merely a
   self-asserted descriptor supplied alongside a key.
3. Compute SHA-256 of the DER and compare exactly with the persisted verified
   enrollment digest for each purpose/version/key ID. Require two distinct
   keys and descriptors. Missing enrollment, digest mismatch, cross-node or
   cross-purpose substitution is rejected before any import.
4. Atomically import the verified tuple and public key into Signer's protected
   durable store, retaining approval provenance and a non-secret audit record.
   Exact repeats are no-ops; conflicting replacements fail closed. Sealing
   resolves only this mapping, never an arbitrary key supplied by the caller.
5. First version has no in-place sealing-key rotation: the existing enrollment
   binding is immutable. Disable the old mapping before retiring/re-enrolling
   a node; import a newly approved binding through the same chain. Stop affected
   mutations during changes and preserve evidence for pending operations.
   No automatic watch/sync is promised. P6 must make disabling Signer bindings
   an explicit decommissioning step; P2 tests stale/disabled mappings.

P2 proves both purposes through real node decryption, concurrent duplicates,
conflicts, lost responses and restart. Import/export names and storage format
are implementation deliverables, not existing CLI/API claims.

## Configuration and delivery

| Input | Contract / status |
| --- | --- |
| `OCSERV_DEPLOYMENT_MODE` | Proposed, not parsed today: `standalone` default, `integrated` explicit opt-in. Never infer mode from installed containers. |
| `OCSERV_PUBLIC_HOST`, `OCSERV_PUBLIC_ORIGIN` | Existing Controller hostname/origin; Integrated requires HTTPS and the Controller DNS name. |
| `OCSERV_RELAY_PUBLIC_HOST` | Proposed second distinct DNS name; derive `OCSERV_RELAY_URL_A=https://<name>`. Reject nonempty B for this single-Relay profile. Existing standalone A/B behavior stays unchanged. |
| `OCSERV_CERTIFICATE_SIGNER_URL` | Existing; Integrated derives the internal URL above and rejects conflicting overrides. Standalone retains external HTTPS Signer. |
| Secret directories / CA trust | Existing `OCSERV_SECRET_DIR`, `OCSERV_RELAY_SECRET_DIR` remain operator-provisioned. Proposed `OCSERV_SIGNER_SECRET_DIR` and `OCSERV_SIGNER_STATE_DIR` are private canonical host paths; P2/P3 define and check ownership/mode before use. Never generate replacement production trust on retry. |
| Database | Existing `OCSERV_DATABASE_BACKEND` / `OCSERV_DATABASE_DEPLOYMENT`, owner/runtime/backup credentials; retain the [production support matrix](../operations/production-deployment.md#database-support). Integrated does not add bundled MySQL/MariaDB or PG18 support. |
| Login | Existing Local/OIDC selection and user-provisioned credentials; no default password. OIDC issuer/client/redirect remain operator inputs with matching public origin. |
| Images / ports | Derive all runtime images from the verified mode/platform manifest, internal ports from versioned templates. Proposed Edge/Relay/Signer image mappings are not new user-selectable mutable tags. |

Reuse [install.sh](../../deploy/production/install.sh),
[compose.sh](../../deploy/production/compose.sh) and `controller.sh`.
P3 extends the existing non-executing environment allowlist and pending/current/
previous state, never sources an arbitrary env file or copies an installer.
The complete inventory includes Edge, Gateway, control-plane, transportd,
Relay, Signer, migrate, transport-runtime-init, backup, optional bundled
PostgreSQL and optional collector. One-shot services share control/transport
images; every actually invoked image, including backup/restore, must be bound.

### Proposed manifest v2

Keep v1's exact schema, six roles, verification and standalone meaning intact.
New readers dispatch explicitly on version; old readers reject v2. Proposed
v2 retains `release_version`, `release_tag`, `source_commit`, `platform` and
`database_migration`, and adds `deployment_mode`, `database_backend`,
`database_deployment`, `observability_enabled`, `signer_state_version` and
`images`. These are proposals, not accepted fields in today's validator.

P3/P4 implement one closed schema: each combination has an exact image-role
set, not arbitrary extensions. Common roles are gateway/control/transport/backup;
postgres is required only for bundled PostgreSQL, otel only when enabled;
Integrated additionally requires edge/relay/signer and a positive Signer state
version, while standalone requires a null Signer state version. The backup
role selects the actual backend-specific backup image. Preserve strict scalar,
SemVer, platform, SHA and digest validation and reject unknown/missing roles.
Bind mode/backend selection in pending/current/previous state; env changes
must not reinterpret the same manifest. P4 signs separate configuration
variants using this finite schema; v1 assets and the amd64 alias keep their
existing meaning. No manifest contains Secret values.

### Candidate and release order

1. Authorized CI builds exact-source, native-platform candidate products once;
   scan them and push candidate images to Registry under immutable digest
   identities. This is an external write requiring its own approval gate, not
   a redefinition of today's dry-run dispatch.
2. Assemble and sign candidate manifests/checksums against those Registry
   identities. Provision the trusted Release public key/fingerprint through
   an independent protected channel; bundled public keys cannot establish trust.
   Candidate verification does not require a public Git tag or GitHub Release:
   P3/P4 must add an explicit candidate admission path using the same verifier,
   pinned source SHA and protected state in isolated acceptance environments.
   Production installers retain exact-release/tag admission.
3. Acceptance pulls only these specified Registry digests and verifies source,
   platform, signed manifest and per-platform image identity. Never rebuild
   from source for final production-path acceptance. Keep index and child
   manifest digests distinct from local Docker image config IDs.
4. After required acceptance and explicit release authorization, publish the
   tag/final signed assets using the same accepted images. Any changed image
   or payload invalidates its acceptance. Sign final checksum files as needed
   without rebuilding payloads; fail if promotion changes bound digests.

P4 separates candidate-push permission from final publication permission and
updates the existing release graph. Do not remove protected environments,
allow unsigned fallback, dispatch CI or change repository rules as part of P0.

## Compatibility and rollback

| Deployment / transition | Required support or refusal |
| --- | --- |
| Existing standalone + v1 | Preserve install/start/upgrade/rollback behavior and exact schema. New reader may consume v1; old reader is not expected to consume v2. |
| Standalone + v2 | Only after P3/P4 prove explicit backend/mode variants. External Relay and HTTPS Signer remain independent operator dependencies. |
| Fresh Integrated + v2 | Only after P1-P6 gates; both native `linux/amd64` and `linux/arm64` images must be accepted before claiming both. |
| Same-mode upgrade | Verified backup, database migration/compatibility checks, Signer state compatibility and protected lifecycle state precede activation. Unknown effects block conflicting work. |
| Same-mode rollback | Existing previous-release guards plus target-readable Signer state. Never roll back images automatically after partial activation or downgrade schema/state blindly. Preserve CA, issued serials and revocations. |
| Standalone <-> Integrated | No automatic in-place conversion or cross-mode rollback in the first version. Reject before changing ports/state; operator-planned maintenance migration is a separate authorized task. |
| Incompatible DB or Signer state | Forward recovery or an explicitly planned isolated restore. A pre-upgrade Signer backup alone may lose later issuance/revocations: reconcile them before resuming. No generic down migration. |
| Nodes / protocols | Keep existing ALPN, enrollment proof, purpose-separated keys and finite release compatibility matrix. No pre-1.0 in-place upgrade promise, process merging or guarantee of recovering every Unknown mutation. |

P3 records Signer state version with the release and refuses activation if its
reader/writer compatibility is unknown. P2 supplies that compatibility range
and crash-safe migration/backup procedure. Restoring Controller and Signer
independently is not proof of a consistent recovery point.

## Ownership and acceptance

| Owner | Unique checks / delivery | Depends on |
| --- | --- | --- |
| P1 network | Port/name/trust assertions, client-IP spoof rejection, address replacement and same-host/external Relay reconnect above | This ADR; locked Edge/Gateway/Relay candidates |
| P2 Signer | Three-path wire compatibility, certificate policy, durable concurrency/restart/revocation, trusted key import and both sealing purposes | Existing client/enrollment/privd contracts; no P1 dependency for isolated tests |
| P3 lifecycle | Mode rendering, secret mounts, closed v1/v2 admission, pending-state retry, same-mode rollback/refusal and candidate admission | P1/P2 artifacts and state contracts |
| P4 delivery | Complete image inventory, native builds/scans, approved candidate push, signed digest binding, pull-only acceptance and final publication gate | P3 manifest contract; existing release workflow/checks |
| P5 business | Real external node enrollment and independent requester/approver workflows, certificate issuance/P12, revocation enforcement, audit and uncertain outcomes on the same candidate | P1-P4; reuse [business coverage](release-business-coverage.md) and [Release Check](release-checks.md) |
| P6 operations | Support matrix, DNS/certificate/Secret preparation, key retirement, maintenance/restore drill and operator handoff | P3-P5; reuse existing backend backup/restore and upgrade coverage |

Do not duplicate all database combinations, resilience/long-duration suites
or evidence storage. Existing checks retain their owners and original results;
new checks address the concrete Integrated gaps above. Local experiments run
only on BuildServer; production-path acceptance uses approved Registry
candidates. A01 topology maps to Network; A02 interfaces and A03 key provenance
to Signer; A04 publication order to Delivery; A05 to Compatibility; A06 to this
ownership table; A07 to the explicit baseline/proposal separation throughout.

P0 static review resolves the design obligations, not the P1-P6 runtime gates.
Runtime network, production Signer, manifest v2 and Registry candidate checks
are NOT_RUN in P0. Reevaluate this ADR if PROXY support, Relay IP dependence,
key provenance, revocation enforcement or candidate admission cannot meet the
specified gates. Record the smallest revised decision before implementation;
do not silently lower the contract. No production state is changed by this
document, and its rollback is a documentation-only revert.
