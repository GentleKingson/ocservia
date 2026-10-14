# Integrated deployment contract

In an Integrated deployment, a single host and public IP serve two distinct DNS names alongside one non-redundant Relay and an independent Signer, while Gateway, the control-plane process, and transportd continue to run as separate processes. Standalone mode remains the default installation topology, and the lifecycle tools do not perform automatic cross-mode conversion or rollback.

Day-to-day configuration, network template rendering, and maintenance commands live in the [deployment procedure](../../deploy/production/integrated/README.md). Supported topologies and guarantees are defined in [Stable contracts](../reference/stable-contracts.md); this document specifies the architectural contract and does not establish runtime acceptance on its own.

## Network and trust

On the host network, only Edge publishes TCP443 and only Relay publishes UDP7842. Edge inspects the plaintext TLS ClientHello SNI and routes connections without holding TLS private keys, failing closed on missing or unknown SNI and non-TLS traffic. On the Controller branch, Gateway terminates Controller TLS and validates the HTTP Host or HTTP2 authority header before forwarding requests to the API or SPA (accepting an optional explicit `:443` port on the configured hostname). On the Relay branch, admission relies on exact SNI matching and Relay token authentication without a second HTTP Host admission check.

Edge attaches verified PROXY metadata from its dedicated trusted address when forwarding to Gateway, which uses that metadata to overwrite `X-Ocservia-Client-IP`, and the Controller trusts that header exclusively from Gateway's `/32` application address. Before forwarding TLS traffic to Relay, Edge strips the PROXY header, so Relay observes Edge's internal IP instead of the original client IP; do not rely on Relay for original-IP abuse controls.

Dedicated internal networks (Edge/Gateway, Edge/Relay, and Controller/Signer) isolate application, database, and observability boundaries. Operators should inspect the rendered network attachments and permitted egress whenever external database overlays are added. Replacing a container requires peers to re-resolve DNS and reconnect. Even when Controller and Relay run on the same physical machine, same-host transport connects through the configured Relay HTTPS hostname with full certificate verification; never substitute raw IP URLs or disable verification to work around routing failures.

Key material stays partitioned by service role:

- Gateway holds the Controller TLS keys, while Relay holds the Relay TLS keys and shares the Relay token with transportd and managed nodes.
- Signer is the sole owner of its certificate-issuing key, its HTTPS private key, and its durable issuance and revocation ledger.
- Controller and Signer share only the signing API token, and Controller mounts Signer's public HTTPS CA bundle (with SAN `signer`), while node sealing private keys remain root-only on the managed nodes.
- Database, audit, session, command-signing, and transport keys remain separate, and no host ports are added for Gateway, Signer, the database, Controller HTTP, or observability services.

Certificates come either from external files (`OCSERV_TLS_MODE=manual`, the default) or, when `OCSERV_TLS_MODE=acme` is selected, from automated TLS-ALPN-01 challenges handled directly by Gateway and Relay over Edge's SNI routing ([ACME certificates](../../deploy/production/integrated/README.md#acme-certificates)). HTTP-01 is never used. ACME provisioning exists only in Integrated mode (`compose.sh` rejects it for Standalone), and the TLS mode cannot change after initial installation.

## Signer contract

Wire payload limits, certificate policy, durable idempotency, approved public-key imports, and recovery steps are specified under [Production Signer](certificates-and-signer.md#production-signer). The Controller calls `https://signer:9443/sign` over verified HTTPS with Bearer authentication and redirects disabled. That endpoint value is already the full path, so callers must not treat it as an origin and append a second `/sign`.

Every issuance or revocation request commits to the Signer ledger before returning success. Idempotent replays return the exact recorded issuance and revocation state, whereas envelope sealing uses fresh randomness on each call without mutating node state. Importing an existing node key requires independently authenticated Controller enrollment evidence alongside matching node public-key descriptors; self-asserted key registrations from a node are rejected. Recording a revocation in the ledger does not automatically distribute a new CRL or terminate active VPN sessions, so refreshing CRLs and disabling Signer bindings remain explicit operator tasks.

## Configuration and delivery

Integrated deployments reuse `install.sh`, `controller.sh`, and `compose.sh`. Protected `install.env` is parsed as allowlisted data and is never sourced by a shell. Running in Integrated mode requires the explicit root lifecycle layout, two distinct DNS names, and Compose >= 2.24.4. In a manual installation, the operator supplies all Controller, Relay (when using manual TLS), and Signer files ahead of time, as the lifecycle never generates CAs or Signer material during normal install or upgrade runs. For new hosts, the Quick preset layers on top of the same installer (selecting Integrated, bundled PostgreSQL, ACME, and Local authentication): `quick-materials.sh` generates the protected key material, including the Signer's offline root CA, issuing intermediate, and HTTPS identity, and creates the initial Local administrators at the end of setup without altering this runtime contract.

The active mode, backend, deployment topology, and ACME setting are bound in `deployment-profile.json` across pending, current, and previous activations. Any conflicting profile change aborts preflight before existing services are stopped, and retrying an interrupted lifecycle command never replaces existing trust material or initializes a missing existing Signer ledger.

Manifest v1 keeps its existing schema, six image roles, and standalone meaning. Manifest v2 extends the schema in a single per-architecture file by adding `edge`, `relay`, `signer`, `mysql_backup`, and `signer_state_version: 1`. All ten image references must carry explicit version tags or SHA-256 digests. Manifest validation rejects unknown fields, unrecognized roles or state versions, missing required roles, invalid SemVer, platform, or source revisions, and noncanonical JSON formatting. Older lifecycle readers reject v2 manifests, whereas Integrated deployments require v2. Manifests never contain secret values, and Compose pulls only the images needed by the selected services, including the backend's manifest-bound backup image.

Database engine support follows the [database support matrix](../operations/production-deployment.md#database-support), and Local or OIDC authentication provisioning works the same as in Standalone mode. Integrated mode does not add bundled MySQL support. Container images and internal service ports come strictly from the selected manifest and versioned templates, with no mutable operator overrides. Qualification gates and artifact publication order follow the [Release policy](release-checks.md).

## Compatibility and recovery

Same-mode activation validates the target manifest, runs owner-only database initialization, and preserves protected lifecycle and Signer checkpoints. Software-version ordering, legacy schema ranges, and snapshot descriptor equality are not used as admission gates or cross-version safety guarantees.

Rolling back to the previous slot requires state that the target binaries can read under the same Signer identity and a monotonic ledger revision. Lifecycle scripts never automatically reverse schema or state after a partial activation, reset node or Signer identity, or lower a recorded checkpoint to make an older binary start. Because a backup taken before an upgrade may omit certificates issued or revoked afterward, operators must reconcile those records along with key imports, disabled bindings, and Controller state before resuming traffic; restoring the Controller database and Signer ledger independently does not by itself prove a consistent recovery point. When on-disk state is incompatible with the previous binary, apply a forward fix or execute a planned isolated restore.

Enrollment possession proofs, purpose-separated keys, ALPNs, and real command capability checks remain mandatory across upgrades and rollbacks. Unresolved Unknown effects block conflicting work on the affected resource and require reconciliation. No high availability, process merger, HSM/KMS integration, automatic CRL distribution, cross-version rollback safety, or guaranteed automatic recovery of every Unknown mutation is promised.

## Verification

Network verification must cover client-IP propagation and spoof rejection, invalid SNI and Host rejection, private-port isolation, changed-address container replacement, and authenticated Relay reconnection from both same-host and external clients. Signer verification must cover durable concurrent replay, lost responses, daemon restarts, key provenance checks, both sealing purposes, and explicit CRL enforcement.

Lifecycle verification covers both manifest versions, mode and database overlays, pending-state retry, same-mode rollback, and preflight refusal before side effects occur. End-to-end [Business validation](real-business-validation.md) exercises real enrollment, independent approval, native privileged effects, and browser workflows, while native product jobs cover both supported architectures. Existing database and backup/restore suites remain governed by their respective owners. Missing proof blocks the corresponding claim, and historical results in Git history do not count as current acceptance.
