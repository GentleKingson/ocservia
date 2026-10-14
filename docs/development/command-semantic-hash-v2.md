# Command semantic hash v2

The Controller signs semantic payload hash v2 as the command identity for all
newly issued commands. Version 2 leaves the v1 specification untouched and
introduces a distinct domain separator so the envelope revision can carry the
Controller authorization epoch while `ConfigPlan` binds its own expected
desired-state revision.

The canonical preimage is constructed as:

```
ASCII("ocservia.command.semantic-hash.v2") || 0x00
|| node_id
|| uint64_be(authorization_revision)
|| uint32_be(payload_kind)
|| canonical_payload_v2
```

Here `node_id` is the 16-byte raw UUID, `authorization_revision` is the nonzero
epoch from the verified `SessionGrantV1`, and all integer fields use unsigned
big-endian encoding. Aside from `ConfigPlan`, payload kind numbers and payload
encodings match v1, with the addition of `CompleteConfigPlan` (129) and
`CompleteConfigApply` (130), which exist only in v2 and are rejected under v1.
Their canonical layouts live in the Go `semanticpayload` package and the Rust
`command-authorization` crate, verified against
`testdata/complete-config-v1.json`.

For `ConfigPlan`, the v2 payload layout appends the expected configuration
revision:

```
payload_kind = uint32_be(103)
canonical_payload_v2 =
    candidate_hash
    || uint64_be(config_expected_revision)
```

In this layout, `candidate_hash` is the 32-byte candidate digest and
`config_expected_revision` is the authoritative applied configuration revision
against which the plan was built. `ConfigApply` continues to bind its 32-byte
candidate hash, 32-byte expected current hash, and unsigned 64-bit desired
effect revision.

While the Controller issues v2 exclusively, both the Agent and privd recompute
whichever supported version a command carries, and the Agent validates the
digest before recording acceptance in its journal. Stored commands replay only
under their original hash version, command results preserve that exact version,
and backend stores validate v2 explicitly without an unknown-version fallback.

Cross-language Go and Rust test vectors for `ConfigPlan` live in
`testdata/semantic-payload-hash-v2.json`, and `AgentUpgrade` vectors live in
`testdata/semantic-payload-hash-v2-agent-upgrade.json`. Both v1 and v2 are
frozen; any future semantic change requires a new hash version or a new
`payload_kind`.
