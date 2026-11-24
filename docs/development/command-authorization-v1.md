# Controller command authorization v1

Under command protocols `1.1` and `1.2`, every command accepted by an Agent must carry an
Ed25519 authorization signed by the Controller. When forwarding a command,
`transportd` checks the bounded Protobuf wire schema, the negotiated session
mode, capability, revision, and expiry, and uses its pinned Controller public
keys to verify `SessionGrantV1` (including monotonic `session_epoch`) before
relaying the command bytes. `transportd` never holds a Controller signing key,
so it cannot mint or modify an executable command. Command authorization itself
stays with the Agent and privd.

The Controller signs a command only after finalizing single-Admin workspace
authorization, operation identity, required capability, revision, delivery mode,
and the semantic payload hash. The Controller issues and signs semantic hash v2
for all commands; semantic hash v1 has been removed and is rejected across the
Controller, Agent, and privd. Both the Agent and privd pin Controller public keys
independently: the Agent verifies the proof before recording the command in its
local journal, and privd verifies the original proof again before reserving or
executing any root effect, with no unsigned mutation fallback at either boundary.
A valid proof authorizes a single command's execution; it does not by itself
indicate that the command has been dispatched, executed, or confirmed.

## Key identity

The Controller signs commands with an Ed25519 PKCS#8 private key, while each
Agent pins the corresponding SubjectPublicKeyInfo public key identified by a
stable key ID:

```
ed25519-sha256: || lowercase_hex(SHA-256(raw_32_byte_ed25519_public_key))
```

Each Agent accepts up to eight distinct pinned public keys so a newly
provisioned key can overlap its predecessor during key rotation. Any proof
referencing an unknown `key_id` fails closed.

## Canonical signing input

Signatures are computed over an RFC 8949 Core Deterministic CBOR map preceded by
a null-terminated ASCII domain separator, independent of Protobuf wire
serialization:

```
ASCII("ocservia/controller-command/cbor.v1") || 0x00 || deterministic_cbor(CommandClaimsV1)
```

`CommandClaimsV1` is a deterministic CBOR text-keyed map containing the exact
fields below (map keys sorted by RFC 8949 Core Deterministic CBOR rules):

| CBOR key | Type / constraints |
|---|---|
| `version` | unsigned integer (`uint32`); v1 is `1` |
| `key_id` | UTF-8 text string (`ed25519-sha256:...`) |
| `protocol_version` | UTF-8 text string; current `1.2`, historical `1.1` |
| `command_id` | 16-byte UUID byte string in network order |
| `idempotency_key` | 16-byte UUID byte string in network order |
| `node_id` | 16-byte UUID byte string in network order |
| `operation_id` | 16-byte UUID byte string in network order |
| `actor_identity` | UTF-8 text string |
| `action` | UTF-8 text string |
| `required_capability` | UTF-8 text string |
| `expected_revision` | unsigned integer (`uint64`); Controller authorization revision |
| `semantic_hash_version` | unsigned integer (`uint32`); v2 (`2`) |
| `semantic_payload_sha256` | 32-byte SHA-256 byte string |
| `payload_kind` | unsigned integer (`uint32`); Protobuf oneof field number |
| `delivery_mode` | unsigned integer (`uint32`); enum value |
| `issued_at_seconds` | signed integer (`int64`) |
| `issued_at_nanos` | unsigned integer (`uint32`), less than 1,000,000,000 |
| `expires_at_seconds` | signed integer (`int64`) |
| `expires_at_nanos` | unsigned integer (`uint32`), less than 1,000,000,000 |

Strings undergo no Unicode normalization. The expiration timestamp must be
strictly later than the issuance timestamp, and
`CommandAuthorizationProof.signature` is excluded from the signing preimage.

Binding the delivery mode into the signature stops an untrusted relay from
converting an observation-only reconciliation request into an execution or
retry. The payload kind, action, and required capability must match the typed
command defined in the frozen protocol mapping, and both the Agent and privd
recompute the semantic hash independently.

## Verification

Before writing to its command journal or calling privd, the Agent checks the
protocol version, bounded typed payload, target node ID, validity window,
active-session authorization revision, required capability, UUIDs, semantic
hash version, and recomputed semantic hash. It then rebuilds the canonical
authorization preimage, looks up the pinned public key for `key_id`, and runs
strict Ed25519 signature verification. Any mismatch aborts processing before
journal persistence or privilege escalation.

For mutation-capable sessions, the Agent verifies the Controller-signed
`SessionGrantV1` and binds the command to its active session. Privd independently
verifies the grant and session epoch against its pinned node ID and Agent
`EndpointID`. It persists the accepted epoch before evaluating the requested
effect. Artifact access also requires a separate, Controller-signed
`ArtifactGrantV1` scoped to the artifact, operation and requester.

Across the local Unix socket, the Agent forwards the original signed
`CommandEnvelope` intact without supplying separate Agent-chosen mutation
arguments. Privd pins its own node ID and Controller keyring, repeats strict
validation of the signature, expiry, node ID, UUIDs, action, delivery mode, and
semantic hash, and derives the fixed adapter invocation and effect identity
from the signed typed payload. Its authenticated durable effect ledger binds
the node, command, operation, idempotency key, action, revisions, semantic
hash, effect kind, and target resource. Replaying an exact match of a succeeded
command returns the stored typed result without repeating the root effect. A
prepared record in an unknown state stays fail-closed until an explicitly
Controller-signed retry-after-absence flow resolves it.

Shared Go and Rust test vectors live in `testdata/command-authorization-v1.json`,
which pairs exact canonical preimage bytes with deterministic Go-generated
signatures verified by the Rust test suite.

## Signed session authority

During a protocol `1.1` handshake, the Agent advertises its supported
capabilities and the Controller replies with the sorted intersection against
the node's approved capability set, accompanied by a signed `SessionGrantV1`.
`transportd` relays this grant and caches its negotiated metadata, but it has no
signing key of its own. The Agent refuses to enter a mutation-capable session
until it verifies `SessionGrantV1` against the same pinned Controller keyring
used for command verification.

Like command proofs, the session grant is signed over an RFC 8949 Core
Deterministic CBOR map preceded by a null-terminated domain separator,
independent of Protobuf serialization:

```
ASCII("ocservia/controller-session-grant/cbor.v1") || 0x00 || deterministic_cbor(SessionGrantV1)
```

| CBOR key | Type / constraints |
|---|---|
| `version` | unsigned integer (`uint32`); v1 is `1` |
| `key_id` | UTF-8 text string (`ed25519-sha256:...`) |
| `protocol_major` | unsigned integer (`uint32`) |
| `protocol_minor` | unsigned integer (`uint32`) |
| `node_id` | 16-byte UUID byte string in network order |
| `endpoint_id` | 32-byte Agent public-key byte string |
| `authorization_revision` | unsigned integer (`uint64`), nonzero |
| `session_epoch` | unsigned integer (`uint64`), monotonic session ownership epoch |
| `negotiated_capabilities` | array of strictly sorted, unique UTF-8 text strings |
| `issued_at_seconds` | signed integer (`int64`) |
| `issued_at_nanos` | unsigned integer (`uint32`) |
| `expires_at_seconds` | signed integer (`int64`) |
| `expires_at_nanos` | unsigned integer (`uint32`) |

Upon receiving the grant, the Agent validates the signature, `key_id`, node ID,
EndpointID, protocol version, sorted capability list, issuance window, and
expiration time. Every subsequent command on that session must carry the same
nonzero authorization revision and request a capability from the verified set.
Before opening an Agent command stream, `transportd` independently rejects any
command whose revision mismatches the session, whose session has expired, or
whose required capability was not negotiated. Advancing the authoritative trust
revision closes any connection still holding an older grant.

Protocol `1.0` and grantless handshakes are retired and rejected by the Controller,
`transportd`, and `Agent`. Every session requires a Controller-signed `SessionGrantV1`
under protocol `1.1`. The shared test vector for session grants is
`testdata/session-grant-v1.json`.

## Provisioning and rotation

Generate the Controller key pair outside the repository checkout under a
restrictive umask, keeping the private key exclusively on the Controller:

```bash
umask 077
openssl genpkey -algorithm ED25519 -out controller-command-signing-key.pem
openssl pkey -in controller-command-signing-key.pem -pubout \
  -out controller-command-verification-key.pem
chmod 0400 controller-command-signing-key.pem
```

Install the public verification key on each managed node as `root:ocserv-agent`
with mode `0640` inside a root-owned directory hierarchy that is neither group-
nor world-writable. Point the Controller at its private key via
`OCSERV_COMMAND_SIGNING_KEY_FILE`, and pass one or more
`--controller-command-key-file` flags to both the Agent and privd. In
production, startup aborts if either verifier is missing a configured key or if
the key file's type, ownership, permissions, hardlink count, symlink status, or
parent directory ancestry is unsafe. Privd also checks its locally
configured node UUID and rejects valid proofs addressed to any other node.

To rotate the signing key, distribute the new public key to every Agent and
privd instance first, then switch the Controller to the new private key. Keep
the previous public key pinned until all commands signed under it have expired
and any pending reconciliation has finished, after which the old public key can
be removed. Rolling back during rotation switches the Controller back to
the previous private key while both public keys remain pinned on the nodes.

Protocol `1.1` enforces a strict mutation compatibility boundary, so upgrade
and provision Agents before enabling `1.1` command issuance on the Controller. During a mixed-version rollout, unsupported or unsigned mutations
are rejected outright without a downgrade fallback, while health checks and
local read-only probes continue on their separate paths.
