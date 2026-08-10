# Command semantic hash v2

Semantic payload hash v2 is the current command identity issued by the
Controller. It preserves v1 unchanged and uses a new domain because the command
envelope revision is now the Controller authorization epoch, while ConfigPlan
also needs to bind an independent desired-state revision.

The canonical preimage is:

```
ASCII("ocservia.command.semantic-hash.v2") || 0x00
|| node_id
|| uint64_be(authorization_revision)
|| uint32_be(payload_kind)
|| canonical_payload_v2
```

`node_id` is exactly 16 UUID bytes. `authorization_revision` is the nonzero
revision from the verified `SessionGrantV1`. Integers use unsigned big-endian
encoding. Payload kinds and every payload encoding other than ConfigPlan are
identical to v1.

ConfigPlan v2 is:

```
payload_kind = uint32_be(103)
canonical_payload_v2 =
    candidate_hash
    || uint64_be(config_expected_revision)
```

`candidate_hash` is exactly 32 bytes. `config_expected_revision` is the
authoritative applied configuration revision against which the plan was
created. ConfigApply continues to bind its 32-byte candidate hash, 32-byte
expected current hash, and unsigned 64-bit desired effect revision.

The Controller emits v2 only after the session-authority protocol is available.
The Agent independently recomputes v2 before journal acceptance. Results retain
the exact hash version, and PostgreSQL accepts v2 explicitly rather than using
an unknown-version fallback.

The shared Go/Rust ConfigPlan vector is
`testdata/semantic-payload-hash-v2.json`. Any future semantic change requires a
new version or a new payload kind; v1 and v2 are frozen.
