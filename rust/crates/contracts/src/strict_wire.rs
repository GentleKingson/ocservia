//! Strict raw-wire validation for command envelopes.
//!
//! Prost intentionally ignores unknown fields while decoding. Command envelopes
//! are side-effecting inputs, so the transport and Agent ingress paths validate
//! the raw wire schema before decoding and executing them.

#![forbid(unsafe_code)]

use std::collections::{BTreeMap, HashMap};
use std::error::Error;
use std::fmt;
use std::sync::LazyLock;

use prost::Message;
use prost::encoding::{DecodeContext, WireType, decode_key, decode_length_delimiter, skip_field};
use prost_types::field_descriptor_proto::{Label, Type};
use prost_types::{DescriptorProto, FileDescriptorSet};

use crate::generated::ocserv::platform::agent::v1::{self as agent, CommandEnvelope};

/// A malformed or schema-incompatible command wire frame.
#[derive(Debug)]
pub enum StrictWireError {
    /// The bytes are not a valid Protobuf message.
    Decode(prost::DecodeError),
    /// A field tag is not part of the frozen message schema.
    UnknownField {
        /// The message containing the unknown field.
        message: &'static str,
        /// The unknown Protobuf field number.
        tag: u32,
    },
    /// A known field used a wire type different from its schema declaration.
    UnexpectedWireType {
        /// The message containing the field.
        message: &'static str,
        /// The field number.
        tag: u32,
        /// The schema-declared wire type.
        expected: WireType,
        /// The wire type present on the input.
        actual: WireType,
    },
    /// A nested message length exceeded the remaining frame.
    NestedMessageTruncated {
        /// The nested message type.
        message: &'static str,
    },
}

impl fmt::Display for StrictWireError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Decode(error) => write!(formatter, "invalid command protobuf: {error}"),
            Self::UnknownField { message, tag } => {
                write!(formatter, "unknown field {tag} in {message}")
            }
            Self::UnexpectedWireType {
                message,
                tag,
                expected,
                actual,
            } => write!(
                formatter,
                "field {tag} in {message} has wire type {actual:?}, expected {expected:?}"
            ),
            Self::NestedMessageTruncated { message } => {
                write!(formatter, "truncated nested {message} message")
            }
        }
    }
}

impl Error for StrictWireError {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        match self {
            Self::Decode(error) => Some(error),
            _ => None,
        }
    }
}

/// Decodes a command envelope only after rejecting unknown raw-wire fields.
///
/// # Errors
///
/// Returns an error for malformed Protobuf, unknown fields at any command
/// message depth, or a known field with the wrong wire type.
pub fn decode_strict_command_envelope(bytes: &[u8]) -> Result<CommandEnvelope, StrictWireError> {
    validate_strict_command_envelope(bytes)?;
    CommandEnvelope::decode(bytes).map_err(StrictWireError::Decode)
}

/// Validates every raw-wire field in a command envelope and its nested messages.
///
/// # Errors
///
/// Returns an error for malformed Protobuf, unknown fields at any command
/// message depth, or a known field with the wrong wire type.
pub fn validate_strict_command_envelope(bytes: &[u8]) -> Result<(), StrictWireError> {
    let schema = &*COMMAND_SCHEMA;
    validate_message(bytes, schema, schema.root)
}

#[derive(Clone, Copy)]
enum FieldKind {
    Scalar(WireType),
    Nested(usize),
}

struct MessageSchema {
    name: &'static str,
    fields: Vec<(u32, FieldKind)>,
}

impl MessageSchema {
    fn field_kind(&self, tag: u32) -> Option<FieldKind> {
        self.fields
            .binary_search_by_key(&tag, |&(field_tag, _)| field_tag)
            .ok()
            .map(|idx| self.fields[idx].1)
    }
}

struct SchemaTable {
    root: usize,
    messages: Vec<MessageSchema>,
}

static COMMAND_SCHEMA: LazyLock<SchemaTable> = LazyLock::new(build_command_schema);

const COMMAND_ENVELOPE_FQN: &str = ".ocserv.platform.agent.v1.CommandEnvelope";
const TIMESTAMP_FQN: &str = ".google.protobuf.Timestamp";

fn collect_descriptors(
    prefix: &str,
    descriptors: &[DescriptorProto],
    out: &mut BTreeMap<String, DescriptorProto>,
) {
    for desc in descriptors {
        let fqn = format!("{prefix}.{}", desc.name());
        collect_descriptors(&fqn, &desc.nested_type, out);
        out.insert(fqn, desc.clone());
    }
}

fn scalar_wire_type(field_type: Type) -> WireType {
    match field_type {
        Type::Double | Type::Fixed64 | Type::Sfixed64 => WireType::SixtyFourBit,
        Type::Float | Type::Fixed32 | Type::Sfixed32 => WireType::ThirtyTwoBit,
        Type::String | Type::Bytes => WireType::LengthDelimited,
        Type::Int64
        | Type::Uint64
        | Type::Int32
        | Type::Bool
        | Type::Uint32
        | Type::Enum
        | Type::Sint32
        | Type::Sint64 => WireType::Varint,
        Type::Message | Type::Group => unreachable!("handled before scalar_wire_type"),
    }
}

fn build_command_schema() -> SchemaTable {
    let fds = FileDescriptorSet::decode(agent::FILE_DESCRIPTOR_SET)
        .expect("generated FILE_DESCRIPTOR_SET must decode");
    let mut by_fqn = BTreeMap::new();
    for file in &fds.file {
        let package = file.package.as_deref().unwrap_or_default();
        let prefix = if package.is_empty() {
            String::new()
        } else {
            format!(".{package}")
        };
        collect_descriptors(&prefix, &file.message_type, &mut by_fqn);
    }

    let mut indices: HashMap<String, usize> = HashMap::new();
    let mut messages: Vec<MessageSchema> = Vec::new();
    let mut queue: Vec<String> = Vec::new();

    let alloc_message = |fqn: &str,
                         indices: &mut HashMap<String, usize>,
                         messages: &mut Vec<MessageSchema>,
                         queue: &mut Vec<String>|
     -> usize {
        if let Some(&idx) = indices.get(fqn) {
            return idx;
        }
        let idx = messages.len();
        let short_name = fqn.rsplit('.').next().unwrap_or(fqn);
        let leaked: &'static str = Box::leak(short_name.to_owned().into_boxed_str());
        indices.insert(fqn.to_owned(), idx);
        messages.push(MessageSchema {
            name: leaked,
            fields: Vec::new(),
        });
        queue.push(fqn.to_owned());
        idx
    };

    let root = alloc_message(
        COMMAND_ENVELOPE_FQN,
        &mut indices,
        &mut messages,
        &mut queue,
    );

    let mut cursor = 0;
    while cursor < queue.len() {
        let fqn = queue[cursor].clone();
        cursor += 1;
        let idx = indices[&fqn];

        let fields = if fqn == TIMESTAMP_FQN {
            vec![
                (1, FieldKind::Scalar(WireType::Varint)),
                (2, FieldKind::Scalar(WireType::Varint)),
            ]
        } else {
            let desc = by_fqn
                .get(&fqn)
                .unwrap_or_else(|| panic!("missing protobuf descriptor for {fqn}"));
            let mut list = Vec::with_capacity(desc.field.len());
            for field in &desc.field {
                let tag = u32::try_from(field.number()).expect("positive protobuf field number");
                let kind = match field.r#type() {
                    Type::Message => {
                        let child_idx = alloc_message(
                            field.type_name(),
                            &mut indices,
                            &mut messages,
                            &mut queue,
                        );
                        FieldKind::Nested(child_idx)
                    }
                    Type::Group => panic!("unsupported protobuf group field {fqn}.{tag}"),
                    scalar => {
                        let wire = scalar_wire_type(scalar);
                        // Proto3 repeated numeric/bool/enum fields are packed on the wire.
                        let effective = if field.label() == Label::Repeated
                            && wire != WireType::LengthDelimited
                        {
                            WireType::LengthDelimited
                        } else {
                            wire
                        };
                        FieldKind::Scalar(effective)
                    }
                };
                list.push((tag, kind));
            }
            list.sort_unstable_by_key(|&(tag, _)| tag);
            list
        };
        messages[idx].fields = fields;
    }

    SchemaTable { root, messages }
}

fn validate_message(
    mut bytes: &[u8],
    schema: &SchemaTable,
    message_idx: usize,
) -> Result<(), StrictWireError> {
    let msg = &schema.messages[message_idx];
    while !bytes.is_empty() {
        let (tag, wire_type) = decode_key(&mut bytes).map_err(StrictWireError::Decode)?;
        let Some(field) = msg.field_kind(tag) else {
            return Err(StrictWireError::UnknownField {
                message: msg.name,
                tag,
            });
        };
        let expected = match field {
            FieldKind::Scalar(expected) => expected,
            FieldKind::Nested(_) => WireType::LengthDelimited,
        };
        if wire_type != expected {
            return Err(StrictWireError::UnexpectedWireType {
                message: msg.name,
                tag,
                expected,
                actual: wire_type,
            });
        }
        match field {
            FieldKind::Scalar(_) => {
                skip_field(wire_type, tag, &mut bytes, DecodeContext::default())
                    .map_err(StrictWireError::Decode)?;
            }
            FieldKind::Nested(nested_idx) => {
                let nested_bytes = take_nested(&mut bytes, schema.messages[nested_idx].name)?;
                validate_message(nested_bytes, schema, nested_idx)?;
            }
        }
    }
    Ok(())
}

fn take_nested<'a>(
    bytes: &mut &'a [u8],
    message: &'static str,
) -> Result<&'a [u8], StrictWireError> {
    let length = decode_length_delimiter(&mut *bytes).map_err(StrictWireError::Decode)?;
    if length > bytes.len() {
        return Err(StrictWireError::NestedMessageTruncated { message });
    }
    let (nested, rest) = bytes.split_at(length);
    *bytes = rest;
    Ok(nested)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::generated::ocserv::platform::agent::v1::{
        self as agent, CommandAuthorizationProof, CommandEnvelope, command_envelope,
    };
    use prost::Message;
    use prost_types::Timestamp;
    use std::collections::BTreeMap;

    fn append_varint(bytes: &mut Vec<u8>, mut value: usize) {
        loop {
            let mut byte = u8::try_from(value & 0x7f).expect("masked varint byte fits");
            value >>= 7;
            if value != 0 {
                byte |= 0x80;
            }
            bytes.push(byte);
            if value == 0 {
                break;
            }
        }
    }

    fn nested_field(tag: usize, payload: &[u8]) -> Vec<u8> {
        let mut bytes = Vec::new();
        append_varint(&mut bytes, (tag << 3) | 2);
        append_varint(&mut bytes, payload.len());
        bytes.extend(payload);
        bytes
    }

    #[test]
    fn accepts_go_encoded_command_fixtures() {
        use command_envelope::Payload;
        let fixtures = go_wire_commands();
        assert_eq!(fixtures.len(), 26);
        for (name, hex) in fixtures
            .iter()
            .filter(|(name, _)| !name.starts_with("full_"))
        {
            let bytes = decode_hex(hex);
            let command = decode_strict_command_envelope(&bytes).expect("Go command accepted");
            assert_eq!(command.protocol_version, "1.1");
            let secret = match (name.as_str(), command.payload.expect("payload")) {
                ("user_create", Payload::UserCreate(value)) => {
                    assert_eq!(value.username, "alice");
                    assert_eq!(value.desired_revision, 42);
                    value.sealed_password_v1
                }
                ("password_rotate", Payload::UserPasswordRotate(value)) => {
                    assert_eq!(value.username, "alice");
                    assert_eq!(value.desired_revision, 42);
                    value.sealed_password_v1
                }
                ("certificate_p12", Payload::CertificateP12(value)) => {
                    assert_eq!(value.certificate_id, b"certificate");
                    assert_eq!(value.artifact_id, b"artifact");
                    assert_eq!(value.certificate_version, 42);
                    assert_eq!(
                        value.artifact_expires_at,
                        Some(Timestamp {
                            seconds: 1_700_000_060,
                            nanos: 123
                        })
                    );
                    value.sealed_password_v1
                }
                ("certificate_revoke", Payload::CertificateRevoke(value)) => {
                    assert_eq!(value.certificate_id, b"certificate");
                    assert_eq!(value.certificate_version, 42);
                    assert_eq!(value.reason, "rotation");
                    continue;
                }
                ("config_plan_zero" | "config_plan_revision", Payload::ConfigPlan(value)) => {
                    assert_eq!(value.candidate, b"config");
                    assert_eq!(value.candidate_hash, b"hash");
                    assert_eq!(
                        value.expected_revision,
                        if name == "config_plan_zero" { 0 } else { 42 }
                    );
                    continue;
                }
                _ => panic!("unexpected fixture {name}"),
            }
            .expect("sealed password");
            assert_eq!(secret.version, 1);
            assert_eq!(
                secret.purpose,
                if name == "certificate_p12" { 2 } else { 1 }
            );
            assert_eq!(secret.key_id, "test-key");
            assert_eq!(secret.ciphertext, b"sealed-test-data");
        }
    }

    fn go_wire_document() -> serde_json::Value {
        serde_json::from_str(include_str!(
            "../../../../testdata/command-strict-wire.json"
        ))
        .unwrap()
    }

    fn go_wire_commands() -> BTreeMap<String, String> {
        serde_json::from_value(go_wire_document()["commands"].clone()).unwrap()
    }

    fn decode_hex(hex: &str) -> Vec<u8> {
        let (pairs, rem) = hex.as_bytes().as_chunks::<2>();
        assert_eq!(rem.len(), 0);
        pairs
            .iter()
            .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
            .collect()
    }

    fn fixture_secret(purpose: i32) -> agent::SealedSecretV1 {
        agent::SealedSecretV1 {
            version: 1,
            purpose,
            key_id: "test-key".into(),
            ciphertext: b"sealed-test-data".to_vec(),
        }
    }

    // Independent Rust expectations, not values decoded back out of the fixture.
    #[allow(clippy::too_many_lines)]
    fn full_payloads() -> BTreeMap<&'static str, command_envelope::Payload> {
        use command_envelope::Payload;
        let complete = agent::CompleteConfigCandidate {
            node_id: b"node".to_vec(),
            expected_revision: 42,
            directives: vec![
                agent::CompleteConfigDirective {
                    name: "device".into(),
                    value: Some(agent::complete_config_directive::Value::Literal(
                        "vpns".into(),
                    )),
                },
                agent::CompleteConfigDirective {
                    name: "server-cert".into(),
                    value: Some(agent::complete_config_directive::Value::Tls(
                        agent::NodeLocalTlsReference {
                            secret_ref_id: b"ref".to_vec(),
                            version: "v1".into(),
                            certificate_sha256: b"certificate-hash".to_vec(),
                            spki_sha256: b"spki-hash".to_vec(),
                            ca_sha256: b"ca-hash".to_vec(),
                        },
                    )),
                },
            ],
        };
        BTreeMap::from([
            (
                "complete_config_plan",
                Payload::CompleteConfigPlan(agent::CompleteConfigPlan {
                    candidate: Some(complete.clone()),
                    candidate_hash: b"logical".to_vec(),
                }),
            ),
            (
                "complete_config_apply",
                Payload::CompleteConfigApply(agent::CompleteConfigApply {
                    candidate: Some(complete),
                    candidate_hash: b"logical".to_vec(),
                    expected_current_hash: b"current".to_vec(),
                    materialized_hash: b"materialized".to_vec(),
                    desired_revision: 43,
                    plan_id: b"plan".to_vec(),
                    plan_expires_at: Some(Timestamp {
                        seconds: 1_700_000_060,
                        nanos: 123,
                    }),
                }),
            ),
            (
                "session_disconnect",
                Payload::SessionDisconnect(agent::SessionDisconnect {
                    session_id: "session".into(),
                    boot_id: "boot".into(),
                }),
            ),
            (
                "session_terminate",
                Payload::SessionTerminate(agent::SessionTerminate {
                    session_id: "session".into(),
                    boot_id: "boot".into(),
                }),
            ),
            (
                "ip_ban_remove",
                Payload::IpBanRemove(agent::IpBanRemove {
                    ip: "192.0.2.1".into(),
                }),
            ),
            (
                "user_create",
                Payload::UserCreate(agent::UserCreate {
                    username: "alice".into(),
                    desired_revision: 42,
                    sealed_password_v1: Some(fixture_secret(1)),
                }),
            ),
            (
                "user_disable",
                Payload::UserDisable(agent::UserDisable {
                    username: "alice".into(),
                    desired_revision: 42,
                }),
            ),
            (
                "user_enable",
                Payload::UserEnable(agent::UserEnable {
                    username: "alice".into(),
                    desired_revision: 42,
                }),
            ),
            (
                "password_rotate",
                Payload::UserPasswordRotate(agent::UserPasswordRotate {
                    username: "alice".into(),
                    desired_revision: 42,
                    sealed_password_v1: Some(fixture_secret(1)),
                }),
            ),
            (
                "group_apply",
                Payload::GroupApply(agent::GroupApply {
                    group_name: "admins".into(),
                    members: vec!["alice".into(), "bob".into()],
                    desired_revision: 42,
                }),
            ),
            (
                "config_plan",
                Payload::ConfigPlan(agent::ConfigPlan {
                    candidate: b"config".to_vec(),
                    candidate_hash: b"hash".to_vec(),
                    expected_revision: 42,
                }),
            ),
            (
                "config_apply",
                Payload::ConfigApply(agent::ConfigApply {
                    candidate_hash: b"hash".to_vec(),
                    candidate: b"config".to_vec(),
                    expected_current_hash: b"current".to_vec(),
                    desired_revision: 42,
                }),
            ),
            (
                "certificate_csr",
                Payload::CertificateCsr(agent::CertificateCsr {
                    certificate_id: b"certificate".to_vec(),
                    common_name: "vpn.example.test".into(),
                    dns_names: vec!["vpn.example.test".into(), "alt.example.test".into()],
                    key_bits: 3072,
                }),
            ),
            (
                "certificate_p12",
                Payload::CertificateP12(agent::CertificateP12 {
                    certificate_id: b"certificate".to_vec(),
                    certificate_chain_pem: b"chain".to_vec(),
                    artifact_id: b"artifact".to_vec(),
                    sealed_password_v1: Some(fixture_secret(2)),
                    certificate_version: 42,
                    artifact_expires_at: Some(Timestamp {
                        seconds: 1_700_000_060,
                        nanos: 123,
                    }),
                }),
            ),
            (
                "certificate_revoke",
                Payload::CertificateRevoke(agent::CertificateRevoke {
                    certificate_id: b"certificate".to_vec(),
                    reason: "rotation".into(),
                    certificate_version: 42,
                }),
            ),
            (
                "agent_upgrade",
                Payload::AgentUpgrade(agent::AgentUpgrade {
                    target_version: "1.2.3".into(),
                    package_sha256: b"package-hash".to_vec(),
                    architecture: "arm64".into(),
                }),
            ),
            (
                "service_reload",
                Payload::ServiceReload(agent::ServiceReload {}),
            ),
            (
                "simulation_probe",
                Payload::SimulationProbe(agent::SimulationProbe {
                    heartbeat_count: 3,
                    delay_millis: 17,
                    duplicate_event: true,
                    return_error: true,
                    disconnect_after: true,
                }),
            ),
            (
                "synthetic_noop",
                Payload::SyntheticNoop(agent::SyntheticNoop {}),
            ),
            (
                "synthetic_echo",
                Payload::SyntheticEcho(agent::SyntheticEcho {
                    message: "hello".into(),
                }),
            ),
        ])
    }

    fn full_envelope(payload: command_envelope::Payload) -> CommandEnvelope {
        let issued_at = Some(Timestamp {
            seconds: 1_700_000_000,
            nanos: 123,
        });
        let expires_at = Some(Timestamp {
            seconds: 1_700_000_060,
            nanos: 456,
        });
        CommandEnvelope {
            protocol_version: "1.1".into(),
            message_id: b"message".to_vec(),
            command_id: b"command".to_vec(),
            idempotency_key: b"idempotency".to_vec(),
            node_id: b"node".to_vec(),
            sequence: 17,
            issued_at,
            expires_at,
            expected_revision: 42,
            traceparent: "trace".into(),
            actor_id: "actor".into(),
            reason: "reason".into(),
            delivery_mode: 2,
            semantic_payload_hash_version: 2,
            semantic_payload_sha256: b"semantic-hash".to_vec(),
            operation_id: b"operation".to_vec(),
            action: "synthetic.echo".into(),
            required_capability: "synthetic.v1".into(),
            authorization: Some(CommandAuthorizationProof {
                version: 1,
                key_id: "auth-key".into(),
                signature: b"auth-signature".to_vec(),
            }),
            payload: Some(payload),
        }
    }

    #[test]
    fn all_go_payloads_decode_to_independent_non_default_values() {
        let fixtures = go_wire_commands();
        let expected = full_payloads();
        assert_eq!(
            fixtures
                .keys()
                .filter(|name| name.starts_with("full_"))
                .count(),
            expected.len()
        );
        for (name, payload) in expected {
            let hex = &fixtures[&format!("full_{name}")];
            let bytes = decode_hex(hex);
            let actual = decode_strict_command_envelope(&bytes).unwrap();
            let expected = if name == "synthetic_echo" {
                full_envelope(payload)
            } else {
                CommandEnvelope {
                    protocol_version: "1.1".into(),
                    payload: Some(payload),
                    ..CommandEnvelope::default()
                }
            };
            assert_eq!(actual, expected, "Go fixture {name}");
        }
    }

    fn descriptors() -> BTreeMap<String, prost_types::DescriptorProto> {
        let mut descriptors: BTreeMap<_, _> =
            prost_types::FileDescriptorSet::decode(agent::FILE_DESCRIPTOR_SET)
                .unwrap()
                .file
                .into_iter()
                .flat_map(|file| {
                    let package = file.package.unwrap();
                    file.message_type
                        .into_iter()
                        .map(move |message| (format!(".{package}.{}", message.name()), message))
                })
                .collect();
        let wire = decode_hex(
            go_wire_document()["timestamp_descriptor_hex"]
                .as_str()
                .unwrap(),
        );
        descriptors.insert(
            TIMESTAMP_FQN.to_owned(),
            prost_types::DescriptorProto::decode(wire.as_slice()).unwrap(),
        );
        descriptors
    }

    fn schema_paths() -> Vec<(&'static str, prost_types::DescriptorProto, Vec<usize>)> {
        let descriptors = descriptors();
        let schema = &*COMMAND_SCHEMA;
        let mut pending = vec![(schema.root, COMMAND_ENVELOPE_FQN.to_owned(), Vec::new())];
        let mut paths = Vec::new();
        while let Some((idx, fqn, path)) = pending.pop() {
            let descriptor = descriptors[&fqn].clone();
            let msg = &schema.messages[idx];
            for field in &descriptor.field {
                let tag = u32::try_from(field.number()).unwrap();
                if let Some(FieldKind::Nested(child_idx)) = msg.field_kind(tag) {
                    let mut child_path = path.clone();
                    child_path.push(usize::try_from(tag).unwrap());
                    pending.push((child_idx, field.type_name().to_owned(), child_path));
                }
            }
            paths.push((msg.name, descriptor, path));
        }
        paths
    }

    fn wrap_path(path: &[usize], mut bytes: Vec<u8>) -> Vec<u8> {
        for &tag in path.iter().rev() {
            bytes = nested_field(tag, &bytes);
        }
        bytes
    }

    #[test]
    fn strict_policy_covers_generated_command_descriptor() {
        let paths = schema_paths();
        let messages: std::collections::BTreeSet<_> =
            paths.iter().map(|(name, _, _)| *name).collect();
        assert_eq!(messages.len(), COMMAND_SCHEMA.messages.len());
    }

    #[test]
    fn rejects_unknown_tags_at_every_command_message_path() {
        for (name, descriptor, path) in schema_paths() {
            let known: std::collections::BTreeSet<_> = descriptor
                .field
                .iter()
                .map(prost_types::FieldDescriptorProto::number)
                .collect();
            for tag in (1..=135)
                .chain([2000, 536_870_911])
                .filter(|tag| !known.contains(tag))
            {
                let mut bytes = Vec::new();
                append_varint(&mut bytes, usize::try_from(tag).unwrap() << 3);
                bytes.push(1);
                assert!(
                    matches!(decode_strict_command_envelope(&wrap_path(&path, bytes)),
                    Err(StrictWireError::UnknownField { message, tag: actual }) if message == name && actual == u32::try_from(tag).unwrap()),
                    "{name} path {path:?} tag {tag}"
                );
            }
        }
    }

    #[test]
    fn rejects_every_wrong_wire_type_at_every_command_field_path() {
        for (name, descriptor, path) in schema_paths() {
            for field in descriptor.field {
                let expected = match field.r#type() {
                    Type::String | Type::Bytes | Type::Message => WireType::LengthDelimited,
                    _ => WireType::Varint,
                };
                for wire in [
                    WireType::Varint,
                    WireType::SixtyFourBit,
                    WireType::LengthDelimited,
                    WireType::StartGroup,
                    WireType::EndGroup,
                    WireType::ThirtyTwoBit,
                ] {
                    if wire == expected {
                        continue;
                    }
                    let tag = usize::try_from(field.number()).unwrap();
                    let mut bytes = Vec::new();
                    append_varint(&mut bytes, (tag << 3) | wire as usize);
                    bytes.extend([0; 8]);
                    assert!(
                        matches!(decode_strict_command_envelope(&wrap_path(&path, bytes)),
                        Err(StrictWireError::UnexpectedWireType { message, tag: actual, expected: want, actual: got })
                            if message == name && actual == u32::try_from(tag).unwrap() && want == expected && got == wire),
                        "{name} path {path:?} tag {tag} wire {wire:?}"
                    );
                }
            }
        }
    }

    #[test]
    fn rejects_truncated_messages_at_every_nested_path() {
        for (name, _, path) in schema_paths() {
            let Some((&tag, parents)) = path.split_last() else {
                continue;
            };
            let mut bytes = Vec::new();
            append_varint(&mut bytes, (tag << 3) | 2);
            bytes.push(1); // Declared nested length exceeds the remaining empty body.
            assert!(
                matches!(decode_strict_command_envelope(&wrap_path(parents, bytes)),
                Err(StrictWireError::NestedMessageTruncated { message }) if message == name)
            );
        }
    }
}
