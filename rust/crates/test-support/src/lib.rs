#![allow(
    clippy::missing_panics_doc,
    clippy::must_use_candidate,
    clippy::return_self_not_must_use,
    clippy::too_many_arguments
)]

use std::fs;
use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_command_authorization::{
    COMMAND_PROTOCOL_VERSION, ControllerCommandKeyring, SessionGrantClaimsV1,
    canonical_session_grant_v1, canonical_v1, claims_from_envelope_v1, semantic_payload_hash_v2,
    verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandAuthorizationProof, CommandDeliveryMode, CommandEnvelope, SemanticPayloadHashVersion,
    SessionGrantV1, command_envelope,
};
use prost_types::Timestamp;
use uuid::Uuid;

pub fn unix_seconds() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_secs(),
    )
    .expect("test time")
}

pub fn deadline_ms(offset_ms: u64) -> u64 {
    u64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_millis(),
    )
    .expect("test time")
        + offset_ms
}

pub fn ts(seconds: i64, nanos: i32) -> Timestamp {
    Timestamp { seconds, nanos }
}

pub fn keyring(key: &SigningKey) -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([key.verifying_key()]).expect("test keyring")
}

pub fn host_architecture() -> &'static str {
    ocservia_contracts::agent_upgrade::runtime_architecture().expect("test host architecture")
}

pub fn foreign_architecture() -> &'static str {
    if host_architecture() == "amd64" {
        "arm64"
    } else {
        "amd64"
    }
}

pub fn write_executable(dir: &Path, name: &str, body: &str) -> PathBuf {
    let path = dir.join(name);
    fs::write(&path, body).expect("write executable fixture");
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755))
        .expect("chmod executable fixture");
    path
}

pub fn sign_session_grant(
    key: &SigningKey,
    node_id: [u8; 16],
    endpoint_id: [u8; 32],
    authorization_revision: u64,
    session_epoch: u64,
    capabilities: Vec<String>,
    issued_at: i64,
    expires_at: i64,
) -> SessionGrantV1 {
    let mut negotiated_capabilities = capabilities;
    negotiated_capabilities.sort();
    negotiated_capabilities.dedup();
    let claims = SessionGrantClaimsV1 {
        version: 1,
        key_id: verification_key_id(&key.verifying_key()),
        protocol_major: 1,
        protocol_minor: 1,
        node_id,
        endpoint_id,
        authorization_revision,
        session_epoch,
        negotiated_capabilities: negotiated_capabilities.clone(),
        issued_at_seconds: issued_at,
        issued_at_nanos: 0,
        expires_at_seconds: expires_at,
        expires_at_nanos: 0,
    };
    let canonical = canonical_session_grant_v1(&claims).expect("canonical session grant");
    let signature = key.sign(&canonical).to_bytes().to_vec();
    SessionGrantV1 {
        version: 1,
        key_id: claims.key_id,
        protocol_major: 1,
        protocol_minor: 1,
        node_id: node_id.to_vec(),
        endpoint_id: endpoint_id.to_vec(),
        authorization_revision,
        session_epoch,
        negotiated_capabilities,
        issued_at: Some(ts(issued_at, 0)),
        expires_at: Some(ts(expires_at, 0)),
        signature,
    }
}

pub fn sign_command_v2(
    key: &SigningKey,
    node_id: [u8; 16],
    issued_at: i64,
    expires_at: i64,
    expected_revision: u64,
    action: &str,
    capability: &str,
    payload: command_envelope::Payload,
) -> CommandEnvelope {
    let mut command = CommandEnvelope {
        protocol_version: COMMAND_PROTOCOL_VERSION.to_owned(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(ts(issued_at, 0)),
        expires_at: Some(ts(expires_at, 0)),
        expected_revision,
        actor_id: "controller:test".to_owned(),
        operation_id: Uuid::now_v7().as_bytes().to_vec(),
        action: action.to_owned(),
        required_capability: capability.to_owned(),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        semantic_payload_hash_version: SemanticPayloadHashVersion::V2.into(),
        payload: Some(payload),
        authorization: Some(CommandAuthorizationProof {
            version: 1,
            key_id: verification_key_id(&key.verifying_key()),
            signature: Vec::new(),
        }),
        ..CommandEnvelope::default()
    };
    sign_command_envelope_v2(key, &mut command);
    command
}

pub fn sign_command_envelope_v2(key: &SigningKey, command: &mut CommandEnvelope) {
    command.semantic_payload_hash_version = SemanticPayloadHashVersion::V2.into();
    command.semantic_payload_sha256 = semantic_payload_hash_v2(command)
        .expect("semantic hash v2")
        .to_vec();
    let key_id = verification_key_id(&key.verifying_key());
    command.authorization = Some(CommandAuthorizationProof {
        version: 1,
        key_id,
        signature: Vec::new(),
    });
    let claims = claims_from_envelope_v1(command).expect("authorization claims");
    let signature = key.sign(&canonical_v1(&claims).expect("canonical authorization"));
    command
        .authorization
        .as_mut()
        .expect("authorization")
        .signature = signature.to_bytes().to_vec();
}
