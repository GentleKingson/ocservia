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
identical to v1, except that `CompleteConfigPlan` (129) and
`CompleteConfigApply` (130) are defined only under v2 and are rejected under v1.
Their encodings are owned by the Go `semanticpayload` package and Rust
`command-authorization` crate and exercised by `testdata/complete-config-v1.json`.

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

The Controller signs v2 for every command it issues. The Agent and privd
independently recompute either supported version, and the Agent does so before
journal acceptance. A stored command is replayed only under the same hash
version. Results retain the exact hash version, and backend stores accept v2
explicitly rather than using an unknown-version fallback.

The shared Go/Rust ConfigPlan vector is
`testdata/semantic-payload-hash-v2.json`; `AgentUpgrade` has
`testdata/semantic-payload-hash-v2-agent-upgrade.json`. Any future semantic
change requires a new version or a new payload kind; v1 and v2 are frozen.
