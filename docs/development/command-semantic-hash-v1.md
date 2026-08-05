# Command semantic hash v1

This document is the single source of truth for the **Canonical Semantic Payload
Hash v1**: the versioned identity that binds a command's side-effecting
semantics across the Go Controller, the Rust Agent, and every test vector. It is
read by both production implementations and by the shared golden fixture.

## Why a dedicated semantic hash

Protobuf binary serialization is **not** canonical across languages, runtimes,
or schema versions. Field ordering and unknown-field retention are not stable.
A language's "deterministic" marshal only guarantees stability within one binary
build; it does not guarantee equality across Go and Rust, and it breaks the
moment the schema evolves. Hashing re-serialized Protobuf bytes therefore cannot
serve as a durable command identity.

The semantic payload hash solves this by hashing a **hand-specified canonical
byte layout** instead of Protobuf wire bytes. Two commands with the same
side-effecting semantics always produce the same hash, regardless of wire
encoding. Two commands with different side-effecting semantics always produce
different hashes.

The hash never depends on `proto.Marshal`, `encode_to_vec`, deterministic
marshal, original field order, or whether a runtime happens to retain an
unknown field.

## Security invariants

1. **Semantic identity is separated from wire encoding.** Equal semantics must
   yield equal hashes even if Protobuf wire field order differs. Different
   semantics must yield different hashes.

2. **Unknown fields are not silently ignored.** Any unknown field in a command
   envelope or payload that causes side effects is rejected before a journal
   record is created and before any effect runs. Unknown-field rejection is
   enforced by strict wire-schema validation, not by this hash.

3. **Both sides compute and verify independently.** The Controller-computed
   expected hash, the Agent-recomputed hash, and the hash carried on the Agent
   result must all agree. Any mismatch fails closed.

4. **The hash algorithm is versioned.** The version is persisted alongside the
   digest. When the algorithm or semantics change, a new version (for example v2)
   is introduced; the definition of an existing version is never edited.

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

The literal ASCII string `ocservia.command.semantic-hash.v1` followed by one
`0x00` terminator. As hex:

```
6f637365727669612e636f6d6d616e642e73656d616e7469632d686173682e763100
```

The trailing NUL is part of the hashed bytes.

### `payload_kind`

`payload_kind` reuses the existing Protobuf oneof field number of the command
payload. The current schema uses:

| Payload | Field number |
|---|---:|
| `SessionDisconnect` | 100 |
| `ServiceReload` | 105 |
| `SyntheticNoop` | 107 |
| `SyntheticEcho` | 108 |
| `SessionTerminate` | 112 |
| `IpBanRemove` | 113 |

These field numbers are reserved forever. When a payload type is removed, its
field number stays reserved so old wire data is never reinterpreted as a new
field. Adding a new side-effecting payload adds a new oneof member and its field
number becomes a new `payload_kind`.

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

The length is the UTF-8 **byte** count, not the Unicode scalar count.

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

### IpBanRemove

```
payload_kind = uint32_be(113)
canonical_payload = uint32_be(len(ip_utf8)) || ip_utf8
```

The IP string must already be in canonical form before hashing.

**No Unicode normalization is performed.** A precomposed character and its
decomposed equivalent are different byte sequences and therefore produce
different hashes. For example, `é` (`U+00E9`, UTF-8 `c3 a9`) and `e` plus
combining acute accent (`U+0301`, UTF-8 `65 cc 81`) hash to different digests.
This keeps the hash a pure function of bytes and avoids cross-runtime
normalization differences.

## Excluded fields

The following fields do not change side-effecting semantics, so they are
explicitly excluded from the hash:

```
protocol_version
message_id
command_id
idempotency_key
sequence
issued_at
expires_at
traceparent
actor_id
reason
delivery_mode
semantic_hash_version
semantic_payload_sha256
```

- `command_id` and `idempotency_key` identity is enforced by the journal's
  independent uniqueness constraints.
- `delivery_mode` only selects execute, reconcile, or safe-retry behavior.
- Timestamps, tracing, and actor metadata are delivery and audit data.

Changing any excluded field while leaving the included fields identical must not
change the hash.

## Version freezing

Once released, **no field may be added to or removed from the v1 canonical
rule**. Any future change that affects the side effect of `SyntheticEcho` (or any
other payload) must be expressed either as a **new semantic hash version** (v2)
or as a **new payload oneof type**, never by editing the v1 hash input.

## Shared golden fixture

The reference vectors live in a single shared file that both Rust and Go read:

```
testdata/semantic-payload-hash-v1.json
```

Each vector records the input fields, the exact `canonical_preimage_hex` bytes
that are hashed, and the resulting `expected_sha256`. Reviewers can verify any
vector directly: decode `canonical_preimage_hex`, compute `SHA-256` of those
bytes, and compare with `expected_sha256`.

Rust and Go **must read the same fixture**. Neither language is permitted to
maintain its own hardcoded digest table for this algorithm.
