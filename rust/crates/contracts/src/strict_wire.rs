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
    Timestamp,
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
            Self::Timestamp => "Timestamp",
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

fn field_kind(message: MessageKind, tag: u32) -> Option<FieldKind> {
    use FieldKind::{Nested, Scalar};
    use MessageKind::{
        CertificateCsr, CertificateP12, CertificateRevoke, CommandAuthorizationProof,
        CommandEnvelope, ConfigApply, ConfigPlan, GroupApply, IpBanRemove, ServiceReload,
        SessionDisconnect, SessionTerminate, SimulationProbe, SyntheticEcho, SyntheticNoop,
        Timestamp, UserCreate, UserDisable, UserEnable, UserPasswordRotate,
    };
    use WireType::{LengthDelimited, Varint};

    match message {
        CommandEnvelope => match tag {
            1..=5 | 10..=12 | 111 | 120..=124 => Some(Scalar(LengthDelimited)),
            6 | 9 | 109 | 110 => Some(Scalar(Varint)),
            7 | 8 => Some(Nested(Timestamp)),
            125 => Some(Nested(CommandAuthorizationProof)),
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
            _ => None,
        },
        Timestamp => match tag {
            1 | 2 => Some(Scalar(Varint)),
            _ => None,
        },
        CommandAuthorizationProof => match tag {
            1 => Some(Scalar(Varint)),
            2 | 3 => Some(Scalar(LengthDelimited)),
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
        UserCreate | UserPasswordRotate | ConfigApply | CertificateCsr => match tag {
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
            _ => None,
        },
        CertificateP12 => match tag {
            1..=5 => Some(Scalar(LengthDelimited)),
            _ => None,
        },
        CertificateRevoke => match tag {
            1 | 2 => Some(Scalar(LengthDelimited)),
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
        CommandAuthorizationProof, CommandEnvelope, SyntheticEcho, command_envelope,
    };
    use prost::Message;
    use prost_types::Timestamp;

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

    #[test]
    fn accepts_known_nested_command_fields() {
        let bytes = envelope().encode_to_vec();
        assert!(decode_strict_command_envelope(&bytes).is_ok());
    }

    #[test]
    fn rejects_unknown_top_level_field() {
        let mut bytes = envelope().encode_to_vec();
        bytes.extend([0xf0, 0x07, 0x01]);
        assert!(matches!(
            validate_strict_command_envelope(&bytes),
            Err(StrictWireError::UnknownField {
                message: "CommandEnvelope",
                tag: 126
            })
        ));
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
