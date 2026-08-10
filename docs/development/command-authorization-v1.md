# Controller command authorization v1

Command protocol `1.1` requires every command accepted by an Agent to carry an
Ed25519 authorization issued by the Controller. `transportd` only validates the
bounded Protobuf wire schema and relays the original bytes. It does not receive
a Controller signing key and cannot mint or alter an executable command.

The Controller signs only after RBAC, approval consumption, operation identity,
required capability, revision, delivery mode, and the semantic payload hash are
final. The Agent pins one or more Controller public keys and verifies the proof
before writing its command journal or invoking `privd`. There is no unsigned
mutation compatibility path.

## Key identity

The signing key is an Ed25519 PKCS#8 private key. The Agent pins the matching
SubjectPublicKeyInfo public key. The stable key ID is:

```
ed25519-sha256: || lowercase_hex(SHA-256(raw_32_byte_ed25519_public_key))
```

An Agent accepts at most eight distinct pinned public keys so a replacement key
can overlap the old key during rotation. A proof with an unknown `key_id` fails
closed.

## Canonical signing input

The signature is calculated over `CommandAuthorizationV1`, never over Protobuf
serialization. Fields appear exactly once, in the following order:

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

All string lengths count UTF-8 bytes and must fit in `uint32`. Strings are not
normalized. Optional fields always emit their presence byte; an approval request
hash is invalid without an approval ID. Expiry must be strictly later than
issuance. The `CommandAuthorizationProof.signature` field is not part of the
input.

The signed delivery mode prevents an untrusted relay from turning an
observation-only reconciliation into an execution or retry. Payload kind,
action, and required capability must match the typed command according to the
frozen protocol mapping. The semantic hash remains independently recomputed by
the Agent.

## Verification

Before accepting a command, the Agent validates the protocol and bounded typed
payload, target node, time window, exact active-session authorization revision, required capability,
UUID identities, semantic hash version, and recomputed semantic hash. It then
reconstructs the canonical authorization input, selects the pinned key by
`key_id`, and performs strict Ed25519 verification. Any failure occurs before a
journal write and before a privileged request.

The shared Go/Rust vectors are in
`testdata/command-authorization-v1.json`. They include exact canonical bytes and
deterministic Go-generated signatures that the Rust verifier must accept.

## Signed session authority

Handshake protocol `1.1` treats the Agent advertisement as its supported
capability set. The Controller reads the approved set and returns their sorted
intersection. It also signs `SessionGrantV1`; transportd relays the grant and
retains its negotiated metadata but has no signing key. An Agent does not enter
a mutation-capable session until it verifies the grant with the same pinned
Controller keyring used for commands.

The session grant canonical input is never Protobuf serialization. Fields are
encoded in this exact order:

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

The Agent checks the signature, `key_id`, node, EndpointID, protocol, sorted
capability set, issuance window, and expiry. Each command must then carry the
same nonzero authorization revision and a capability in the verified set.
transportd independently rejects a mismatched revision, expired session, or
unnegotiated capability before opening the Agent command stream. A higher
authoritative trust revision closes a connection using an older grant.

Handshake `1.0` compatibility is read-only: it receives no session grant and
may negotiate only approved capabilities ending in `.read`. It cannot create a
mutation-capable Agent command context. The shared session grant vector is
`testdata/session-grant-v1.json`.

## Provisioning and rotation

Generate the Controller key pair outside the checkout with a restrictive
umask. Keep the private key only with the Controller:

```bash
umask 077
openssl genpkey -algorithm ED25519 -out controller-command-signing-key.pem
openssl pkey -in controller-command-signing-key.pem -pubout \
  -out controller-command-verification-key.pem
chmod 0400 controller-command-signing-key.pem
```

Install the public key on each node as `root:ocserv-agent` mode `0640` beneath
a root-controlled, non-writable ancestry. Configure the Controller with
`OCSERV_COMMAND_SIGNING_KEY_FILE` and the Agent with one or more
`--controller-command-key-file` arguments. Production startup fails if the
required key is absent or its type, owner, mode, link count, symlink status, or
ancestry is unsafe.

For rotation, first add the new public key to every Agent, then switch the
Controller to the new private key. Retain the old public key until every command
issued by the old key has expired and reconciliation is complete, then remove
it. Rollback reverses the signing-key switch while both public keys remain
pinned.

This protocol is a mutation compatibility boundary. Upgrade and provision
Agents before enabling Controller `1.1` command issuance. During a mixed-version
window, unsupported or unsigned mutations are rejected rather than downgraded;
health and local read-only probes remain separate from the command path.
