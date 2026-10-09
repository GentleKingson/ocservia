# Integrated deployment contract

Integrated deployment shares one host and public IP across two distinct DNS
names, one non-redundant Relay and an independent Signer. Gateway,
control-plane and transportd remain separate processes. Standalone is the
default; there is no automatic cross-mode conversion or rollback.

The [deployment procedure](../../deploy/production/integrated/README.md)
owns configuration, network rendering and maintenance commands.
[Stable contracts](../reference/stable-contracts.md) define support;
this contract does not establish runtime acceptance.

## Network and trust

Only Edge publishes TCP443 and Relay publishes UDP7842. Edge routes visible
TLS SNI without TLS keys. Missing/unknown SNI and non-TLS input fail closed.
Gateway terminates Controller TLS and rejects an incorrect Host/HTTP2 authority
before API or SPA routing; the configured name optionally accepts standard
`:443`. Relay requires its exact SNI and token authentication, not an
additional HTTP Host admission check.

Gateway receives verified PROXY metadata from Edge's dedicated trusted address
and overwrites `X-Ocservia-Client-IP`. Controller trusts only Gateway's
application `/32`. The Relay branch strips PROXY before TLS forwarding;
Relay sees Edge's address, not the original client IP. Do not claim original-IP
abuse controls there.

Separate Edge/Gateway, Edge/Relay and Controller/Signer networks retain the
application, database and observability boundaries. Inspect the rendered
attachments and permitted egress, including external database overlays.
Container replacement requires DNS re-resolution and client reconnect.
Same-host transport retains the Relay HTTPS name and certificate validation;
do not substitute IP URLs or disable verification to solve routing failures.

Gateway owns Controller TLS keys; Relay owns Relay TLS keys. Relay, transportd
and managed nodes share the Relay token. Signer alone owns its issuing key,
HTTPS private key and durable issuance/revocation ledger. Only Controller and
Signer share the signing API token. Controller mounts Signer's public HTTPS CA
bundle, with SAN `signer`; node sealing private keys remain root-only on nodes.
Database, audit, session, command-signing and transport keys remain separate.
No Gateway, Signer, database, Controller HTTP or observability host port is added.
Certificates are externally provisioned (`OCSERV_TLS_MODE=manual`, the default), or with `OCSERV_TLS_MODE=acme` obtained
by Gateway and Relay themselves through TLS-ALPN-01 over Edge's SNI routing
([ACME certificates](../../deploy/production/integrated/README.md#acme-certificates));
HTTP-01 is never used. ACME exists only in Integrated; `compose.sh` rejects it
for Standalone, and the TLS mode cannot change after first installation.

## Signer contract

[Production Signer](certificates-and-signer.md#production-signer) owns the wire limits, certificate
policy, durable idempotency, approved public-key import and recovery procedure.
Controller calls `https://signer:9443/sign` over verified HTTPS with Bearer
authentication and no redirects. The endpoint is a path, not an origin to which
a second `/sign` may be appended.

Issuance/revocation commits before success. Replays preserve exact issuance and
revocation state; sealing is randomized and does not mutate node state. Import
requires independently authenticated Controller enrollment evidence and matching
node public-key descriptors, never a self-asserted key registration.
Revocation records do not prove CRL distribution or terminate existing VPN
sessions. CRL refresh and Signer binding disable remain explicit operator work.

## Configuration and delivery

Reuse `install.sh`, `controller.sh` and `compose.sh`. Protected
`install.env` is allowlisted data, never sourced shell. Integrated requires the
explicit root lifecycle, two distinct DNS names and Compose >= 2.24.4. A manual
installation supplies every Controller, Relay (manual TLS) and Signer file
itself; the lifecycle never generates CAs or Signer material. Quick is a preset
over the same installer for a new host (Integrated, bundled PostgreSQL, ACME,
Local authentication): `quick-materials.sh` generates the protected material,
including the Signer's offline root CA, issuing intermediate and HTTPS identity,
and the Local administrators are created at the end. Quick does not alter this
contract. Mode/backend/deployment (and ACME)
are bound in `deployment-profile.json` across pending/current/previous activation.
Conflicting changes fail before stopping services. Retries never replace trust
material or initialize a missing existing Signer ledger.

Manifest v1 retains its schema, six image roles and standalone meaning. V2
adds `edge`, `relay`, `signer`, `mysql_backup` and
`signer_state_version: 1` in one file per architecture. All ten image
references are explicit version tags or SHA-256 references. Unknown fields,
roles/state versions, missing roles, invalid SemVer/platform/source revision
or noncanonical JSON fail validation. Old readers reject v2; Integrated
requires v2. No manifest contains secret values. Selected services alone are
pulled, including the backend's manifest-bound backup image.

Keep the [database support matrix](../operations/production-deployment.md#database-support)
and Local/OIDC provisioning unchanged. Integrated does not add bundled
MySQL support. Images and internal ports come from
the selected manifest and versioned templates, not mutable operator overrides.
[Release policy](release-checks.md) owns qualification and publication order.

## Compatibility and recovery

Same-mode activation validates the target, performs owner-only database
initialization and retains protected lifecycle and Signer checkpoints.
Software-version order, schema ranges and descriptor equality are not
compatibility admission gates or safety guarantees.

Rollback requires target-readable state with the same Signer identity and
monotonic revision. Never automatically reverse schema/state after partial
activation, reset identity or lower a checkpoint to make a target start.
A pre-upgrade backup can omit later issuance/revocations; reconcile those,
imports, disables and Controller state before resuming. Independent restores
are not proof of a consistent recovery point. Use a forward fix or a planned
isolated restore when state is incompatible.

Keep enrollment proof, purpose-separated keys, ALPNs and real command capability
checks. Unknown effects block conflicting work and require reconciliation.
No HA, process merger, HSM/KMS implementation, automatic CRL distribution,
cross-version safety or guaranteed recovery of every Unknown mutation is promised.

## Verification

Network checks must cover client-IP propagation/spoof rejection, SNI/Host
rejection, private-port isolation, changed-address replacement and authenticated
same-host/external Relay reconnect. Signer checks cover durable concurrent
replay, lost responses, restart, key provenance, both sealing purposes and
explicit CRL enforcement.

Lifecycle checks cover both manifest versions, modes/database overlays,
pending-state retry, same-mode rollback and refusal before side effects.
[Business validation](real-business-validation.md) covers real enrollment,
independent approval, native effects and browser workflows; native product
jobs cover both architectures. Existing database and backup/restore checks
retain their owners. Missing proof blocks the corresponding claim; historical
results remain in Git history, not current acceptance.
