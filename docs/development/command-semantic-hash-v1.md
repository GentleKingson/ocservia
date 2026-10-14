# Command semantic hash v1

Canonical Semantic Payload Hash v1 is the versioned digest that binds a
command's side-effecting semantics across the Go Controller, the Rust Agent,
and the shared golden test vectors.

## Why a dedicated semantic hash

Protobuf binary serialization is not canonical across languages, runtimes, or
schema revisions: field ordering and unknown-field retention can vary, and a
runtime's "deterministic" marshal option only stabilizes output within a single
binary build. Because re-serializing Protobuf messages can produce different
byte sequences between Go and Rust or after a schema extension, hashing wire
bytes cannot provide a durable command identity.

Instead, the semantic payload hash operates over a fixed, hand-specified
canonical byte layout. Commands with identical side-effecting semantics always
hash to the same digest regardless of wire encoding, while commands with
different side-effecting semantics always produce distinct digests. The digest
never depends on `proto.Marshal`, `encode_to_vec`, deterministic marshal modes,
wire field order, or whether a runtime retains unknown fields.

## Security invariants

1. Equal command semantics must produce identical hashes even when Protobuf
   wire field ordering differs, and distinct semantics must produce distinct
   hashes.
2. Any unknown field in a side-effecting command envelope or payload is
   rejected by strict wire-schema validation before the Agent creates a journal
   record or executes an effect.
3. The Controller's expected hash, the hash recomputed by the Agent and privd,
   and the hash reported on the Agent result must all match; any discrepancy
   fails closed.
4. The hash algorithm is explicitly versioned and stored alongside the digest.
   When hashing rules or command semantics evolve, a new version (such as v2)
   is introduced without modifying the definition of an existing version.

## Byte layout

```
SHA-256(
    domain_separator
    || node_id
    || expected_revision
    || payload_kind
    || canonical_payload
)
```

| Field | Encoding |
|---|---|
| `domain_separator` | ASCII `ocservia.command.semantic-hash.v1` followed by a single `0x00` byte |
| `node_id` | Exactly 16 bytes |
| `expected_revision` | Unsigned 64-bit integer, big-endian |
| `payload_kind` | Unsigned 32-bit integer, big-endian |
| `canonical_payload` | Payload-specific; see below |

### Domain separator

The domain separator is the literal ASCII string
`ocservia.command.semantic-hash.v1` terminated by a single `0x00` NUL byte
(which is included in the hashed preimage):

```
6f637365727669612e636f6d6d616e642e73656d616e7469632d686173682e763100
```

### `payload_kind`

`payload_kind` reuses the Protobuf `oneof` field number of the command payload.
The initial v1 payload set, covered by the shared v1 fixture, uses these field
numbers:

| Payload | Field number |
|---|---:|
| `SessionDisconnect` | 100 |
| `ConfigPlan` | 103 |
| `ServiceReload` | 105 |
| `SyntheticNoop` | 107 |
| `SyntheticEcho` | 108 |
| `SessionTerminate` | 112 |
| `IpBanRemove` | 113 |

Additional side-effecting payloads (`UserCreate` 101, `UserDisable` 102,
`ConfigApply` 104, `UserPasswordRotate` 114, `GroupApply` 115, `UserEnable`
116, `CertificateCsr` 117, `CertificateP12` 118, `CertificateRevoke` 119, and
`AgentUpgrade` 128) define their canonical encodings in the Go
`semanticpayload` package and the Rust `command-authorization` crate.
`CompleteConfigPlan` 129 and `CompleteConfigApply` 130 exist only under v2, and
`SimulationProbe` 106 is not hashable.

Once assigned, payload field numbers remain reserved permanently so retired
payload types can never be reinterpreted as new fields on the wire. Introducing
a new side-effecting command adds a new `oneof` member whose field number
becomes its `payload_kind`.

## Payload encoding

### SyntheticNoop

```
payload_kind   = uint32_be(107)
canonical_payload = empty
```

### SyntheticEcho

```
payload_kind   = uint32_be(108)
canonical_payload =
    uint32_be(len(message_utf8))
    || message_utf8
```

The length prefix measures the raw UTF-8 byte count, as distinct from the
Unicode scalar count.

### SessionDisconnect and SessionTerminate

```
payload_kind = uint32_be(100) or uint32_be(112)
canonical_payload =
    uint32_be(len(session_id_utf8)) || session_id_utf8
    || uint32_be(len(boot_id_utf8)) || boot_id_utf8
```

### ServiceReload

```
payload_kind = uint32_be(105)
canonical_payload = empty
```

### ConfigPlan

```
payload_kind = uint32_be(103)
canonical_payload = candidate_hash
```

`candidate_hash` is a 32-byte digest verified independently against the bounded
configuration candidate prior to validation. Because the candidate content is
immutable for a given digest, delivery metadata and the temporary staging
filename are omitted from the hash. Version 1 does not bind the `ConfigPlan`
expected desired-state revision; the Controller now signs all newly issued
commands with semantic hash v2, while keeping v1 frozen and accepted by the
Agent, privd, and Controller result path for historical command records.

### IpBanRemove

```
payload_kind = uint32_be(113)
canonical_payload = uint32_be(len(ip_utf8)) || ip_utf8
```

The IP address string must already be formatted in canonical representation
before hashing.

Strings never undergo Unicode normalization: a precomposed character and its
decomposed sequence have different UTF-8 bytes and therefore hash to different
digests (for example, `é` at `U+00E9` encodes as `c3 a9`, whereas `e` followed
by `U+0301` encodes as `65 cc 81`). Hashing raw bytes directly avoids
cross-runtime differences in Unicode normalization tables.

## Excluded fields

Fields that carry transport, tracing, or envelope metadata without altering the
command's target side effect are excluded from the semantic payload hash:

```
protocol_version
message_id
command_id
idempotency_key
operation_id
sequence
issued_at
expires_at
traceparent
actor_id
reason
action
required_capability
approval_id
approval_request_sha256
delivery_mode
semantic_hash_version
semantic_payload_sha256
authorization
connection_fence
fence_binding
```

- `CommandAuthorizationV1` separately signs the command and operation IDs,
  actor, action, required capability, approval binding, delivery mode,
  timestamps, authorization revision, payload kind, and semantic digest.
- The Agent journal enforces independent uniqueness on `command_id` and
  `idempotency_key`.
- Tracing headers and human-readable reason strings remain outside both the
  semantic payload hash and the authorization signature.

Varying any of these excluded fields while holding the included fields constant
leaves the semantic digest unchanged.

## Version freezing

The v1 canonical encoding rules are permanently frozen: no field may be added
to or removed from v1. Any change that alters the side-effecting inputs of an
existing payload such as `SyntheticEcho` must be introduced either under a new
semantic hash version (such as v2) or as a new payload `oneof` variant.

## Shared golden fixture

Both the Rust and Go test suites verify their implementations against a single
shared vector file instead of maintaining language-specific digest tables:

```
testdata/semantic-payload-hash-v1.json
```

Each entry specifies the structured input fields, the exact
`canonical_preimage_hex` byte sequence, and the resulting `expected_sha256`
digest, allowing any vector to be checked directly by hex-decoding
`canonical_preimage_hex` and taking its `SHA-256` digest.
