//! Strict raw-wire validation for command envelopes.
//!
//! Prost intentionally ignores unknown fields while decoding. Command envelopes
//! are side-effecting inputs, so the transport and Agent ingress paths validate
//! the raw wire schema before decoding and executing them.

#![forbid(unsafe_code)]

use std::error::Error;
use std::fmt;

use prost::Message;
use prost::bytes::{Buf, Bytes};
use prost::encoding::{DecodeContext, WireType, decode_key, decode_length_delimiter, skip_field};

use crate::generated::ocserv::platform::agent::v1::CommandEnvelope;

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
    validate_message(Bytes::copy_from_slice(bytes), MessageKind::CommandEnvelope)
}

#[derive(Clone, Copy)]
enum MessageKind {
    CommandEnvelope,
    CommandAuthorizationProof,
    ConnectionFenceV2,
    FenceBindingV2,
    Timestamp,
    SealedSecretV1,
    SessionDisconnect,
    SessionTerminate,
    IpBanRemove,
    UserCreate,
    UserDisable,
    UserEnable,
    UserPasswordRotate,
    GroupApply,
    ConfigPlan,
    ConfigApply,
    CertificateCsr,
    CertificateP12,
    CertificateRevoke,
    AgentUpgrade,
    ServiceReload,
    SimulationProbe,
    SyntheticNoop,
    SyntheticEcho,
}

impl MessageKind {
    const fn name(self) -> &'static str {
        match self {
            Self::CommandEnvelope => "CommandEnvelope",
            Self::CommandAuthorizationProof => "CommandAuthorizationProof",
            Self::ConnectionFenceV2 => "ConnectionFenceV2",
            Self::FenceBindingV2 => "FenceBindingV2",
            Self::Timestamp => "Timestamp",
            Self::SealedSecretV1 => "SealedSecretV1",
            Self::SessionDisconnect => "SessionDisconnect",
            Self::SessionTerminate => "SessionTerminate",
            Self::IpBanRemove => "IpBanRemove",
            Self::UserCreate => "UserCreate",
            Self::UserDisable => "UserDisable",
            Self::UserEnable => "UserEnable",
            Self::UserPasswordRotate => "UserPasswordRotate",
            Self::GroupApply => "GroupApply",
            Self::ConfigPlan => "ConfigPlan",
            Self::ConfigApply => "ConfigApply",
            Self::CertificateCsr => "CertificateCsr",
            Self::CertificateP12 => "CertificateP12",
            Self::CertificateRevoke => "CertificateRevoke",
            Self::AgentUpgrade => "AgentUpgrade",
            Self::ServiceReload => "ServiceReload",
            Self::SimulationProbe => "SimulationProbe",
            Self::SyntheticNoop => "SyntheticNoop",
            Self::SyntheticEcho => "SyntheticEcho",
        }
    }
}

#[derive(Clone, Copy)]
enum FieldKind {
    Scalar(WireType),
    Nested(MessageKind),
}

fn validate_message(mut bytes: Bytes, message: MessageKind) -> Result<(), StrictWireError> {
    while bytes.has_remaining() {
        let (tag, wire_type) = decode_key(&mut bytes).map_err(StrictWireError::Decode)?;
        let Some(field) = field_kind(message, tag) else {
            return Err(StrictWireError::UnknownField {
                message: message.name(),
                tag,
            });
        };
        let expected = match field {
            FieldKind::Scalar(expected) => expected,
            FieldKind::Nested(_) => WireType::LengthDelimited,
        };
        if wire_type != expected {
            return Err(StrictWireError::UnexpectedWireType {
                message: message.name(),
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
            FieldKind::Nested(nested) => {
                let nested_bytes = take_nested(&mut bytes, nested.name())?;
                validate_message(nested_bytes, nested)?;
            }
        }
    }
    Ok(())
}

fn take_nested(bytes: &mut Bytes, message: &'static str) -> Result<Bytes, StrictWireError> {
    let length = decode_length_delimiter(&mut *bytes).map_err(StrictWireError::Decode)?;
    if length > bytes.remaining() {
        return Err(StrictWireError::NestedMessageTruncated { message });
    }
    Ok(bytes.split_to(length))
}

#[allow(clippy::too_many_lines)]
fn field_kind(message: MessageKind, tag: u32) -> Option<FieldKind> {
    use FieldKind::{Nested, Scalar};
    use MessageKind::{
        AgentUpgrade, CertificateCsr, CertificateP12, CertificateRevoke, CommandAuthorizationProof,
        CommandEnvelope, ConfigApply, ConfigPlan, ConnectionFenceV2, FenceBindingV2, GroupApply,
        IpBanRemove, SealedSecretV1, ServiceReload, SessionDisconnect, SessionTerminate,
        SimulationProbe, SyntheticEcho, SyntheticNoop, Timestamp, UserCreate, UserDisable,
        UserEnable, UserPasswordRotate,
    };
    use WireType::{LengthDelimited, Varint};

    match message {
        CommandEnvelope => match tag {
            1..=5 | 10..=12 | 111 | 120..=124 => Some(Scalar(LengthDelimited)),
            6 | 9 | 109 | 110 => Some(Scalar(Varint)),
            7 | 8 => Some(Nested(Timestamp)),
            125 => Some(Nested(CommandAuthorizationProof)),
            126 => Some(Nested(ConnectionFenceV2)),
            127 => Some(Nested(FenceBindingV2)),
            100 => Some(Nested(SessionDisconnect)),
            101 => Some(Nested(UserCreate)),
            102 => Some(Nested(UserDisable)),
            103 => Some(Nested(ConfigPlan)),
            104 => Some(Nested(ConfigApply)),
            105 => Some(Nested(ServiceReload)),
            106 => Some(Nested(SimulationProbe)),
            107 => Some(Nested(SyntheticNoop)),
            108 => Some(Nested(SyntheticEcho)),
            112 => Some(Nested(SessionTerminate)),
            113 => Some(Nested(IpBanRemove)),
            114 => Some(Nested(UserPasswordRotate)),
            115 => Some(Nested(GroupApply)),
            116 => Some(Nested(UserEnable)),
            117 => Some(Nested(CertificateCsr)),
            118 => Some(Nested(CertificateP12)),
            119 => Some(Nested(CertificateRevoke)),
            128 => Some(Nested(AgentUpgrade)),
            _ => None,
        },
        Timestamp => match tag {
            1 | 2 => Some(Scalar(Varint)),
            _ => None,
        },
        SealedSecretV1 => match tag {
            1 | 2 => Some(Scalar(Varint)),
            3 | 4 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        CommandAuthorizationProof => match tag {
            1 => Some(Scalar(Varint)),
            2 | 3 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        ConnectionFenceV2 => match tag {
            1 | 7 | 8 | 10 => Some(Scalar(Varint)),
            2..=6 | 9 | 11 | 15 => Some(Scalar(LengthDelimited)),
            12..=14 => Some(Nested(Timestamp)),
            _ => None,
        },
        FenceBindingV2 => match tag {
            1 | 3 | 9 | 10 | 12 => Some(Scalar(Varint)),
            2 | 4 | 5 | 6 | 7 | 8 | 11 | 13 | 16 => Some(Scalar(LengthDelimited)),
            14 | 15 => Some(Nested(Timestamp)),
            _ => None,
        },
        SessionDisconnect | SessionTerminate => match tag {
            1 | 2 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        IpBanRemove | SyntheticEcho => match tag {
            1 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        UserCreate | UserPasswordRotate => match tag {
            1..=3 => Some(Scalar(LengthDelimited)),
            4 => Some(Scalar(Varint)),
            5 => Some(Nested(SealedSecretV1)),
            _ => None,
        },
        ConfigApply | CertificateCsr => match tag {
            1..=3 => Some(Scalar(LengthDelimited)),
            4 => Some(Scalar(Varint)),
            _ => None,
        },
        UserDisable | UserEnable => match tag {
            1 => Some(Scalar(LengthDelimited)),
            2 => Some(Scalar(Varint)),
            _ => None,
        },
        GroupApply => match tag {
            1 | 2 => Some(Scalar(LengthDelimited)),
            3 => Some(Scalar(Varint)),
            _ => None,
        },
        ConfigPlan => match tag {
            1 | 2 => Some(Scalar(LengthDelimited)),
            3 => Some(Scalar(Varint)),
            _ => None,
        },
        CertificateP12 => match tag {
            1..=5 => Some(Scalar(LengthDelimited)),
            6 => Some(Nested(SealedSecretV1)),
            7 => Some(Scalar(Varint)),
            8 => Some(Nested(Timestamp)),
            _ => None,
        },
        CertificateRevoke => match tag {
            1 | 2 => Some(Scalar(LengthDelimited)),
            3 => Some(Scalar(Varint)),
            _ => None,
        },
        AgentUpgrade => match tag {
            1..=3 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        ServiceReload | SyntheticNoop => None,
        SimulationProbe => match tag {
            1..=5 => Some(Scalar(Varint)),
            _ => None,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::generated::ocserv::platform::agent::v1::{
        self as agent, CommandAuthorizationProof, CommandEnvelope, ConnectionFenceV2,
        FenceBindingV2, FenceOperationKind, FenceSignatureVersion, SyntheticEcho, command_envelope,
    };
    use prost::Message;
    use prost_types::Timestamp;
    use std::collections::BTreeMap;

    fn envelope() -> CommandEnvelope {
        CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            issued_at: Some(Timestamp {
                seconds: 1_700_000_000,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: 1_700_000_060,
                nanos: 0,
            }),
            payload: Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: "hello".to_owned(),
            })),
            authorization: Some(CommandAuthorizationProof {
                version: 1,
                key_id: "test-key".to_owned(),
                signature: vec![0xa5; 64],
            }),
            ..CommandEnvelope::default()
        }
    }

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
        assert_eq!(fixtures.len(), 24);
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
        assert_eq!(hex.len() % 2, 0);
        hex.as_bytes()
            .chunks_exact(2)
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
        BTreeMap::from([
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
                    sealed_password: b"legacy".to_vec(),
                    secret_key_id: "legacy-key".into(),
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
                    sealed_password: b"legacy".to_vec(),
                    secret_key_id: "legacy-key".into(),
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
                    sealed_password: b"legacy".to_vec(),
                    secret_key_id: "legacy-key".into(),
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
            approval_id: b"approval".to_vec(),
            approval_request_sha256: b"approval-hash".to_vec(),
            authorization: Some(CommandAuthorizationProof {
                version: 1,
                key_id: "auth-key".into(),
                signature: b"auth-signature".to_vec(),
            }),
            connection_fence: Some(ConnectionFenceV2 {
                signature_version: 1,
                key_id: "fence-key".into(),
                fence_id: b"fence".to_vec(),
                node_id: b"node".to_vec(),
                endpoint_id: b"endpoint".to_vec(),
                owner_instance_id: b"owner".to_vec(),
                owner_incarnation: 11,
                owner_epoch: 12,
                connection_id: b"connection".to_vec(),
                authorization_revision: 13,
                capabilities: vec!["ocserv.fencing.v2".into(), "synthetic.v1".into()],
                lease_until: Some(Timestamp {
                    seconds: 1_700_000_030,
                    nanos: 789,
                }),
                issued_at,
                expires_at,
                signature: b"fence-signature".to_vec(),
            }),
            fence_binding: Some(FenceBindingV2 {
                signature_version: 1,
                key_id: "binding-key".into(),
                operation_kind: 1,
                operation_id: b"operation".to_vec(),
                fence_id: b"fence".to_vec(),
                node_id: b"node".to_vec(),
                endpoint_id: b"endpoint".to_vec(),
                owner_instance_id: b"owner".to_vec(),
                owner_incarnation: 11,
                owner_epoch: 12,
                connection_id: b"connection".to_vec(),
                authorization_revision: 13,
                capability: "synthetic.v1".into(),
                issued_at,
                expires_at,
                signature: b"binding-signature".to_vec(),
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

    fn descriptor_name(kind: MessageKind) -> String {
        let package = if matches!(kind, MessageKind::Timestamp) {
            "google.protobuf"
        } else {
            "ocserv.platform.agent.v1"
        };
        format!(".{package}.{}", kind.name())
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
        // prost's package descriptor excludes imported Timestamp; Go supplies
        // its real descriptor in the same shared fixture, checked on every run.
        let wire = decode_hex(
            go_wire_document()["timestamp_descriptor_hex"]
                .as_str()
                .unwrap(),
        );
        descriptors.insert(
            descriptor_name(MessageKind::Timestamp),
            prost_types::DescriptorProto::decode(wire.as_slice()).unwrap(),
        );
        descriptors
    }

    // This test reads the generated schema; it never generates or extends the
    // runtime allowlist. New fields need protocol/capability/strict-policy review.
    fn compare_fields(
        kind: MessageKind,
        descriptor: &prost_types::DescriptorProto,
    ) -> Result<Vec<(u32, MessageKind)>, String> {
        use prost_types::field_descriptor_proto::{Label, Type};
        // All currently reviewed command tags are in this range, including holes.
        const REVIEWED_MAX_TAG: u32 = 128;
        let fields: BTreeMap<_, _> = descriptor
            .field
            .iter()
            .map(|field| {
                (
                    u32::try_from(field.number()).expect("positive field number"),
                    field,
                )
            })
            .collect();
        if fields.keys().any(|tag| *tag > REVIEWED_MAX_TAG) {
            return Err(format!(
                "{}: new tag requires strict-policy review",
                kind.name()
            ));
        }
        let mut nested = Vec::new();
        for tag in 1..=REVIEWED_MAX_TAG {
            let matches = match (fields.get(&tag), field_kind(kind, tag)) {
                (None, None) => true,
                (Some(field), Some(FieldKind::Nested(child))) => {
                    nested.push((tag, child));
                    field.r#type() == Type::Message && field.type_name() == descriptor_name(child)
                }
                (Some(field), Some(FieldKind::Scalar(wire))) => {
                    let expected = match field.r#type() {
                        Type::String | Type::Bytes => Some(WireType::LengthDelimited),
                        Type::Int32
                        | Type::Int64
                        | Type::Uint32
                        | Type::Uint64
                        | Type::Bool
                        | Type::Enum => Some(WireType::Varint),
                        _ => None,
                    };
                    expected == Some(wire)
                        && !(field.label() == Label::Repeated && wire != WireType::LengthDelimited)
                        && !field
                            .options
                            .as_ref()
                            .is_some_and(prost_types::FieldOptions::packed)
                }
                _ => false,
            };
            if !matches {
                return Err(format!(
                    "{} tag {tag}: descriptor/strict policy drift; review protocol and capability compatibility",
                    kind.name()
                ));
            }
        }
        Ok(nested)
    }

    fn schema_paths() -> Vec<(MessageKind, prost_types::DescriptorProto, Vec<usize>)> {
        let descriptors = descriptors();
        let mut pending = vec![(MessageKind::CommandEnvelope, Vec::new())];
        let mut paths = Vec::new();
        while let Some((kind, path)) = pending.pop() {
            let descriptor = &descriptors[&descriptor_name(kind)];
            for (tag, child) in compare_fields(kind, descriptor).unwrap() {
                let mut child_path = path.clone();
                child_path.push(usize::try_from(tag).unwrap());
                pending.push((child, child_path));
            }
            paths.push((kind, descriptor.clone(), path));
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
    fn strict_policy_matches_generated_command_descriptor() {
        let paths = schema_paths();
        let messages: std::collections::BTreeSet<_> =
            paths.iter().map(|(kind, _, _)| kind.name()).collect();
        assert_eq!(messages.len(), 24);
    }

    #[test]
    fn descriptor_drift_requires_explicit_policy_review() {
        use prost_types::field_descriptor_proto::Type;
        let descriptors = descriptors();
        let original = &descriptors[&descriptor_name(MessageKind::ConfigPlan)];
        let mut added = original.clone();
        let mut field = added.field[0].clone();
        field.number = Some(4);
        field.name = Some("new_field".into());
        added.field.push(field);
        assert!(compare_fields(MessageKind::ConfigPlan, &added).is_err());
        let mut removed = original.clone();
        removed.field.pop();
        assert!(compare_fields(MessageKind::ConfigPlan, &removed).is_err());
        let mut changed = original.clone();
        changed.field[0].r#type = Some(Type::Uint64 as i32);
        assert!(compare_fields(MessageKind::ConfigPlan, &changed).is_err());
        let original = &descriptors[&descriptor_name(MessageKind::CommandEnvelope)];
        let mut changed = original.clone();
        let field = changed
            .field
            .iter_mut()
            .find(|field| field.number() == 103)
            .unwrap();
        field.type_name = Some(descriptor_name(MessageKind::ConfigApply));
        assert!(compare_fields(MessageKind::CommandEnvelope, &changed).is_err());
        let mut added_payload = original.clone();
        let mut field = added_payload
            .field
            .iter()
            .find(|field| field.number() == 128)
            .unwrap()
            .clone();
        field.number = Some(129);
        added_payload.field.push(field);
        assert!(compare_fields(MessageKind::CommandEnvelope, &added_payload).is_err());
    }

    #[test]
    fn rejects_unknown_tags_at_every_command_message_path() {
        for (kind, descriptor, path) in schema_paths() {
            let known: std::collections::BTreeSet<_> = descriptor
                .field
                .iter()
                .map(prost_types::FieldDescriptorProto::number)
                .collect();
            for tag in (1..=129)
                .chain([2000, 536_870_911])
                .filter(|tag| !known.contains(tag))
            {
                let mut bytes = Vec::new();
                append_varint(&mut bytes, usize::try_from(tag).unwrap() << 3);
                bytes.push(1);
                assert!(
                    matches!(decode_strict_command_envelope(&wrap_path(&path, bytes)),
                    Err(StrictWireError::UnknownField { message, tag: actual }) if message == kind.name() && actual == u32::try_from(tag).unwrap()),
                    "{} path {path:?} tag {tag}",
                    kind.name()
                );
            }
        }
    }

    #[test]
    fn rejects_every_wrong_wire_type_at_every_command_field_path() {
        use prost_types::field_descriptor_proto::Type;
        for (kind, descriptor, path) in schema_paths() {
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
                            if message == kind.name() && actual == u32::try_from(tag).unwrap() && want == expected && got == wire),
                        "{} path {path:?} tag {tag} wire {wire:?}",
                        kind.name()
                    );
                }
            }
        }
    }

    #[test]
    fn rejects_truncated_messages_at_every_nested_path() {
        for (kind, _, path) in schema_paths() {
            let Some((&tag, parents)) = path.split_last() else {
                continue;
            };
            let mut bytes = Vec::new();
            append_varint(&mut bytes, (tag << 3) | 2);
            bytes.push(1); // Declared nested length exceeds the remaining empty body.
            assert!(
                matches!(decode_strict_command_envelope(&wrap_path(parents, bytes)),
                Err(StrictWireError::NestedMessageTruncated { message }) if message == kind.name())
            );
        }
    }

    #[test]
    fn rejects_unknown_secret_and_p12_timestamp_fields() {
        for (payload_tag, field_tag, message) in [
            (101, 5, "SealedSecretV1"),
            (114, 5, "SealedSecretV1"),
            (118, 6, "SealedSecretV1"),
            (118, 8, "Timestamp"),
        ] {
            let bytes = nested_field(payload_tag, &nested_field(field_tag, &[0x98, 0x06, 1]));
            assert!(matches!(decode_strict_command_envelope(&bytes),
                Err(StrictWireError::UnknownField { message: actual, tag: 99 }) if actual == message));
        }
    }

    #[test]
    fn rejects_wrong_wire_types_for_new_fields() {
        for (payload_tag, field_tag, nested) in [
            (101, 5, true),
            (114, 5, true),
            (103, 3, false),
            (118, 6, true),
            (118, 7, false),
            (118, 8, true),
            (119, 3, false),
        ] {
            let payload = if nested {
                vec![(field_tag << 3), 1]
            } else {
                nested_field(usize::from(field_tag), &[])
            };
            assert!(matches!(
                decode_strict_command_envelope(&nested_field(payload_tag, &payload)),
                Err(StrictWireError::UnexpectedWireType { .. })
            ));
        }
        for secret_tag in 1..=4 {
            let secret = if secret_tag <= 2 {
                nested_field(secret_tag, &[])
            } else {
                vec![u8::try_from(secret_tag << 3).unwrap(), 1]
            };
            let bytes = nested_field(118, &nested_field(6, &secret));
            assert!(matches!(
                decode_strict_command_envelope(&bytes),
                Err(StrictWireError::UnexpectedWireType {
                    message: "SealedSecretV1",
                    ..
                })
            ));
        }
        let bytes = nested_field(118, &nested_field(8, &nested_field(1, &[])));
        assert!(matches!(
            decode_strict_command_envelope(&bytes),
            Err(StrictWireError::UnexpectedWireType {
                message: "Timestamp",
                ..
            })
        ));
    }

    #[test]
    fn rejects_tag_five_on_unchanged_command_schemas() {
        for (tag, message) in [(104, "ConfigApply"), (117, "CertificateCsr")] {
            assert!(
                matches!(decode_strict_command_envelope(&nested_field(tag, &nested_field(5, &[]))),
                Err(StrictWireError::UnknownField { message: actual, tag: 5 }) if actual == message)
            );
        }
    }

    #[test]
    fn accepts_known_nested_command_fields() {
        let bytes = envelope().encode_to_vec();
        assert!(decode_strict_command_envelope(&bytes).is_ok());
    }

    #[test]
    fn rejects_unknown_top_level_field() {
        let mut bytes = envelope().encode_to_vec();
        bytes.extend([0x80, 0x7d, 0x01]);
        assert!(matches!(
            validate_strict_command_envelope(&bytes),
            Err(StrictWireError::UnknownField {
                message: "CommandEnvelope",
                tag: 2000
            })
        ));
    }

    #[test]
    fn accepts_owner_fence_carriers_on_command_envelope() {
        let mut command = envelope();
        command.connection_fence = Some(ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1 as i32,
            key_id: "ed25519-sha256:test".to_owned(),
            fence_id: vec![1; 16],
            node_id: vec![2; 16],
            endpoint_id: vec![3; 32],
            owner_instance_id: vec![4; 16],
            owner_incarnation: 1,
            owner_epoch: 1,
            connection_id: vec![5; 16],
            authorization_revision: 1,
            capabilities: vec!["ocserv.fencing.v2".to_owned()],
            lease_until: Some(Timestamp {
                seconds: 1_700_000_030,
                nanos: 0,
            }),
            issued_at: Some(Timestamp {
                seconds: 1_700_000_000,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: 1_700_000_330,
                nanos: 0,
            }),
            signature: vec![0xa5; 64],
        });
        command.fence_binding = Some(FenceBindingV2 {
            signature_version: FenceSignatureVersion::Ed25519V1 as i32,
            key_id: "ed25519-sha256:test".to_owned(),
            operation_kind: FenceOperationKind::Command as i32,
            operation_id: vec![6; 16],
            fence_id: vec![1; 16],
            node_id: vec![2; 16],
            endpoint_id: vec![3; 32],
            owner_instance_id: vec![4; 16],
            owner_incarnation: 1,
            owner_epoch: 1,
            connection_id: vec![5; 16],
            authorization_revision: 1,
            capability: "ocserv.fencing.v2".to_owned(),
            issued_at: Some(Timestamp {
                seconds: 1_700_000_000,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: 1_700_000_300,
                nanos: 0,
            }),
            signature: vec![0xa5; 64],
        });
        assert!(decode_strict_command_envelope(&command.encode_to_vec()).is_ok());
    }

    #[test]
    fn rejects_unknown_nested_payload_field() {
        let payload = match envelope().payload.as_ref().expect("payload") {
            command_envelope::Payload::SyntheticEcho(payload) => payload.encode_to_vec(),
            _ => unreachable!("test payload is synthetic echo"),
        };
        let mut payload = payload;
        payload.extend([0x98, 0x06, 0x01]);

        let mut without_payload = envelope();
        without_payload.payload = None;
        let mut bytes = without_payload.encode_to_vec();
        bytes.extend([0xe2, 0x06]);
        append_varint(&mut bytes, payload.len());
        bytes.extend(payload);

        assert!(matches!(
            validate_strict_command_envelope(&bytes),
            Err(StrictWireError::UnknownField {
                message: "SyntheticEcho",
                tag: 99
            })
        ));
    }

    #[test]
    fn rejects_unknown_authorization_field() {
        let mut authorization = envelope()
            .authorization
            .as_ref()
            .expect("authorization")
            .encode_to_vec();
        authorization.extend([0x98, 0x06, 0x01]);

        let mut without_authorization = envelope();
        without_authorization.authorization = None;
        let mut bytes = without_authorization.encode_to_vec();
        bytes.extend([0xea, 0x07]);
        append_varint(&mut bytes, authorization.len());
        bytes.extend(authorization);

        assert!(matches!(
            validate_strict_command_envelope(&bytes),
            Err(StrictWireError::UnknownField {
                message: "CommandAuthorizationProof",
                tag: 99
            })
        ));
    }

    #[test]
    fn rejects_unknown_timestamp_field() {
        let mut timestamp = Timestamp {
            seconds: 1_700_000_000,
            nanos: 0,
        }
        .encode_to_vec();
        timestamp.extend([0x18, 0x01]);

        let mut without_timestamp = envelope();
        without_timestamp.issued_at = None;
        let mut bytes = without_timestamp.encode_to_vec();
        bytes.extend([0x3a]);
        append_varint(&mut bytes, timestamp.len());
        bytes.extend(timestamp);

        assert!(matches!(
            validate_strict_command_envelope(&bytes),
            Err(StrictWireError::UnknownField {
                message: "Timestamp",
                tag: 3
            })
        ));
    }
}
