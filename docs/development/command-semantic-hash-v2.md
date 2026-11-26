# Command semantic hash v2

The Controller signs semantic payload hash v2 as the command identity for all
commands. Semantic hash v1 has been removed: `SemanticPayloadHashVersion`
defines only `UNSPECIFIED (0)` and `V2 (2)`, and both Go and Rust reject v1.

## Canonical digest construction

The semantic hash is SHA-256 over a null-terminated ASCII domain separator
followed by an RFC 8949 Core Deterministic CBOR map, independent of Protobuf
wire serialization:

```
SHA-256(
    ASCII("ocservia.command.semantic-hash.cbor.v2") || 0x00
    || deterministic_cbor(SemanticEnvelopeV2)
)
```

`SemanticEnvelopeV2` is a deterministic CBOR text-keyed map with four keys:

| CBOR key | Type / constraints |
|---|---|
| `node_id` | 16-byte raw UUID byte string |
| `expected_revision` | unsigned integer (`uint64`); nonzero authorization revision from `SessionGrantV1` |
| `payload_kind` | unsigned integer (`uint32`); Protobuf `CommandEnvelope` oneof field number |
| `payload` | deterministic CBOR map specific to `payload_kind` |

## Payload structures

All payload maps use RFC 8949 Core Deterministic CBOR encoding (empty byte
slices encode as zero-length byte strings and empty string lists encode as empty
arrays). Sealed secrets (`sealed_password`) encode as a nested map with keys
`version` (`uint32`), `purpose` (`uint32`), `key_id` (`string`), and
`ciphertext` (`bytes`).

| `payload_kind` | Command | `payload` CBOR keys |
|---|---|---|
| `100` | `SessionDisconnect` | `session_id`, `boot_id` |
| `101` | `UserCreate` | `username`, `desired_revision`, `sealed_password` |
| `102` | `UserDisable` | `username`, `desired_revision` |
| `105` | `ServiceReload` | `{}` (empty map) |
| `107` | `SyntheticNoop` | `{}` (empty map) |
| `108` | `SyntheticEcho` | `message` |
| `112` | `SessionTerminate` | `session_id`, `boot_id` |
| `113` | `IpBanRemove` | `ip` (canonical IP string) |
| `114` | `UserPasswordRotate` | `username`, `desired_revision`, `sealed_password` |
| `115` | `GroupApply` | `group_name`, `members` (`[]string`), `desired_revision` |
| `116` | `UserEnable` | `username`, `desired_revision` |
| `117` | `CertificateCsr` | `certificate_id` (16B), `common_name`, `dns_names` (`[]string`), `key_bits` |
| `118` | `CertificateP12` | `certificate_id` (16B), `artifact_id` (16B), `certificate_chain_pem` (`bytes`), `sealed_password`, `certificate_version`, `artifact_expires_at_seconds`, `artifact_expires_at_nanos` |
| `119` | `CertificateRevoke` | `certificate_id` (16B), `reason`, `certificate_version` |
| `128` | `AgentUpgrade` | `target_version`, `architecture`, `package_sha256` (32B) |
| `129` | `CompleteConfigPlan` | `candidate_hash` (32B) |
| `130` | `CompleteConfigApply` | `candidate_hash` (32B), `materialized_hash` (32B), `expected_current_hash` (32B), `desired_revision`, `plan_id` (16B), `plan_expires_at_seconds`, `plan_expires_at_nanos` |

For `CompleteConfigPlan` (129) and `CompleteConfigApply` (130), `candidate_hash`
must match the canonical `configprofile.Hash` of the candidate profile bound to
the same `node_id`.

## Verification and test vectors

The Controller issues v2 only. Both the Agent and privd recompute semantic hash
v2, and the Agent checks the digest before recording acceptance in its journal.
Stored commands replay only under v2, command results keep that version, and
backend stores reject unsupported hash versions with no fallback.

Shared Go and Rust test vectors live in `testdata/semantic-payload-hash-v2.json`,
`testdata/semantic-payload-hash-v2-agent-upgrade.json`, and
`testdata/complete-config-v1.json`. Semantic hash v2 is frozen; any semantic
change requires a new hash version or a new `payload_kind`.
