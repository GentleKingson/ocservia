# Controller command authorization v1

Under command protocol `1.1`, every command accepted by an Agent must carry an
Ed25519 authorization signed by the Controller. When forwarding a command,
`transportd` checks the bounded Protobuf wire schema, the negotiated session
mode, capability, revision, and expiry, and uses its pinned Controller public
keys to verify the owner-fence carriers before relaying the command bytes.
Because `transportd` never holds a Controller signing key, it cannot mint or
modify an executable command, leaving command authorization itself to the Agent
and privd.

The Controller signs a command only after finalizing RBAC checks, approval
consumption, operation identity, required capability, revision, delivery mode,
and the semantic payload hash. Although the Controller signs semantic hash v2
for all newly issued commands, the Agent, privd, and the Controller result
handler also accept v1 for historical compatibility. Both the Agent and privd
pin Controller public keys independently: the Agent verifies the proof before
recording the command in its local journal, and privd verifies the original
proof again before reserving or executing any root effect, with no unsigned
mutation fallback at either boundary. A valid proof authorizes a single
command's execution; it does not by itself indicate that the command has been
dispatched, executed, or confirmed.

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

Signatures are computed over the canonical `CommandAuthorizationV1` byte layout
independent of Protobuf wire serialization, with every field encoded once in
the order below:

| Field | Encoding |
|---|---|
| domain | ASCII `ocservia/controller-command/v1`, followed by `0x00` |
| authorization version | unsigned 32-bit integer, big-endian; v1 is `1` |
| `key_id` | unsigned 32-bit big-endian UTF-8 byte length, then exact UTF-8 bytes |
| command protocol version | length-prefixed UTF-8; v1 requires `1.1` |
| `command_id` | exactly 16 UUID bytes in network order |
| `idempotency_key` | exactly 16 UUID bytes in network order |
| `node_id` | exactly 16 UUID bytes in network order |
| `operation_id` | exactly 16 UUID bytes in network order |
| actor identity | length-prefixed UTF-8 |
| action | length-prefixed UTF-8 |
| required capability | length-prefixed UTF-8 |
| approval ID | `0x00` if absent; otherwise `0x01` then exactly 16 UUID bytes |
| approval request hash | `0x00` if absent; otherwise `0x01` then exactly 32 bytes |
| Controller authorization revision | unsigned 64-bit integer, big-endian |
| semantic hash version | unsigned 32-bit integer, big-endian |
| semantic payload hash | exactly 32 bytes |
| payload kind | unsigned 32-bit big-endian Protobuf oneof field number |
| delivery mode | unsigned 32-bit big-endian enum value |
| `issued_at.seconds` | signed 64-bit big-endian two's-complement integer |
| `issued_at.nanos` | unsigned 32-bit big-endian integer, less than 1,000,000,000 |
| `expires_at.seconds` | signed 64-bit big-endian two's-complement integer |
| `expires_at.nanos` | unsigned 32-bit big-endian integer, less than 1,000,000,000 |

String lengths measure raw UTF-8 byte counts that fit within `uint32`, and
strings undergo no Unicode normalization. Optional fields always include their
one-byte presence marker, though an approval request hash is invalid unless an
approval ID is also present. The expiration timestamp must be strictly later
than the issuance timestamp, and `CommandAuthorizationProof.signature` is
excluded from the signing preimage.

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
strict Ed25519 signature verification; any mismatch aborts processing before
journal persistence or privilege escalation.

Within a mutation-capable `1.1` session, the Agent also requires the
Controller-signed `ConnectionFenceV2` and `FenceBindingV2` carriers attached at
dispatch time (which remain outside `CommandAuthorizationV1` and the semantic
hash). If a command arrives with an owner epoch below the Agent's durable
floor, the Agent rejects it immediately with `stale_owner_epoch` without
executing it. Privd does not inspect these connection-fence carriers.

Across the local Unix socket, the Agent forwards the original signed
`CommandEnvelope` intact without supplying separate Agent-chosen mutation
arguments. Privd pins its own node ID and Controller keyring, repeats strict
validation of the signature, expiry, node ID, UUIDs, action, delivery mode, and
semantic hash, and derives the fixed adapter invocation and effect identity
from the signed typed payload. Its authenticated durable effect ledger binds
the node, command, operation, idempotency key, action, revisions, semantic
hash, effect kind, and target resource. Replaying an exact match of a succeeded
command returns the stored typed result without repeating the root effect,
whereas a prepared record in an unknown state stays fail-closed until an
explicitly Controller-signed retry-after-absence flow resolves it.

Shared Go and Rust test vectors live in `testdata/command-authorization-v1.json`,
which pairs exact canonical preimage bytes with deterministic Go-generated
signatures verified by the Rust test suite.

## Signed session authority

During a protocol `1.1` handshake, the Agent advertises its supported
capabilities and the Controller replies with the sorted intersection against
the node's approved capability set, accompanied by a signed `SessionGrantV1`.
While `transportd` relays this grant and caches its negotiated metadata, it has
no signing key of its own, and the Agent refuses to enter a mutation-capable
session until it verifies `SessionGrantV1` against the same pinned Controller
keyring used for command verification.

Like command proofs, the session grant is signed over a fixed canonical byte
layout independent of Protobuf serialization, with fields ordered as follows:

| Field | Encoding |
|---|---|
| domain | ASCII `ocservia/controller-session-grant/v1`, followed by `0x00` |
| grant version | unsigned 32-bit integer, big-endian; v1 is `1` |
| `key_id` | length-prefixed UTF-8 |
| protocol major, protocol minor | two unsigned 32-bit integers, big-endian |
| `node_id` | exactly 16 UUID bytes in network order |
| Agent EndpointID | exactly 32 public-key bytes |
| authorization revision | unsigned 64-bit integer, big-endian and nonzero |
| capability count | unsigned 32-bit integer, big-endian |
| capabilities | strictly sorted, unique, individually length-prefixed UTF-8 strings |
| `issued_at.seconds` | signed 64-bit big-endian integer |
| `issued_at.nanos` | unsigned 32-bit big-endian integer |
| `expires_at.seconds` | signed 64-bit big-endian integer |
| `expires_at.nanos` | unsigned 32-bit big-endian integer |

Upon receiving the grant, the Agent validates the signature, `key_id`, node ID,
EndpointID, protocol version, sorted capability list, issuance window, and
expiration time. Every subsequent command on that session must carry the same
nonzero authorization revision and request a capability from the verified set.
Before opening an Agent command stream, `transportd` independently rejects any
command whose revision mismatches the session, whose session has expired, or
whose required capability was not negotiated, and advancing the authoritative
trust revision closes any connection still holding an older grant.

Protocol `1.0` handshakes remain read-only: they receive no session grant, can
negotiate only approved capabilities ending in `.read`, and cannot establish a
mutation-capable Agent command context. The shared test vector for session
grants is `testdata/session-grant-v1.json`.

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
parent directory ancestry is unsafe. Privd additionally checks its locally
configured node UUID and rejects valid proofs addressed to any other node.

To rotate the signing key, distribute the new public key to every Agent and
privd instance first, then switch the Controller to the new private key. Keep
the previous public key pinned until all commands signed under it have expired
and any pending reconciliation has finished, after which the old public key can
be removed. Rolling back during rotation simply switches the Controller back to
the previous private key while both public keys remain pinned on the nodes.

Because protocol `1.1` enforces a strict mutation compatibility boundary,
upgrade and provision Agents before enabling `1.1` command issuance on the
Controller. During a mixed-version rollout, unsupported or unsigned mutations
are rejected outright without a downgrade fallback, while health checks and
local read-only probes continue on their separate paths.
