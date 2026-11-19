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
    COMMAND_PROTOCOL_VERSION, ConnectionFenceClaimsV2, ControllerCommandKeyring,
    FENCE_SIGNATURE_VERSION_ED25519_V1, FenceBindingClaimsV2, canonical_connection_fence_v2,
    canonical_fence_binding_v2, canonical_v1, claims_from_envelope_v1, semantic_payload_hash_v2,
    verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandAuthorizationProof, CommandAuthorizationVersion, CommandDeliveryMode, CommandEnvelope,
    ConnectionFenceV2, FenceBindingV2, FenceOperationKind, FenceSignatureVersion,
    SemanticPayloadHashVersion, command_envelope,
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

#[derive(Clone, Debug)]
pub struct FenceSpec {
    pub fence_id: [u8; 16],
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capabilities: Vec<String>,
    pub lease_until: (i64, u32),
    pub issued_at: (i64, u32),
    pub expires_at: (i64, u32),
}

impl FenceSpec {
    pub fn new(node_id: [u8; 16], endpoint_id: [u8; 32], owner_epoch: u64) -> Self {
        Self {
            fence_id: [1; 16],
            node_id,
            endpoint_id,
            owner_instance_id: [2; 16],
            owner_incarnation: 1,
            owner_epoch,
            connection_id: [3; 16],
            authorization_revision: 1,
            capabilities: vec!["synthetic.noop".to_owned()],
            lease_until: (1_700_000_200, 0),
            issued_at: (1_700_000_000, 0),
            expires_at: (1_700_000_300, 0),
        }
    }

    pub fn authority(mut self, revision: u64) -> Self {
        self.authorization_revision = revision;
        self
    }

    pub fn capabilities<I, S>(mut self, caps: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.capabilities = caps.into_iter().map(Into::into).collect();
        self
    }

    pub fn lease(mut self, seconds: i64, nanos: u32) -> Self {
        self.lease_until = (seconds, nanos);
        self
    }

    pub fn issued(mut self, seconds: i64, nanos: u32) -> Self {
        self.issued_at = (seconds, nanos);
        self
    }

    pub fn expires(mut self, seconds: i64, nanos: u32) -> Self {
        self.expires_at = (seconds, nanos);
        self
    }

    pub fn ids(
        mut self,
        fence_id: [u8; 16],
        owner_instance_id: [u8; 16],
        owner_incarnation: u64,
        connection_id: [u8; 16],
    ) -> Self {
        self.fence_id = fence_id;
        self.owner_instance_id = owner_instance_id;
        self.owner_incarnation = owner_incarnation;
        self.connection_id = connection_id;
        self
    }

    pub fn sign(self, key: &SigningKey) -> ConnectionFenceV2 {
        let claims = ConnectionFenceClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&key.verifying_key()),
            fence_id: self.fence_id,
            node_id: self.node_id,
            endpoint_id: self.endpoint_id,
            owner_instance_id: self.owner_instance_id,
            owner_incarnation: self.owner_incarnation,
            owner_epoch: self.owner_epoch,
            connection_id: self.connection_id,
            authorization_revision: self.authorization_revision,
            capabilities: self.capabilities,
            lease_until_seconds: self.lease_until.0,
            lease_until_nanos: self.lease_until.1,
            issued_at_seconds: self.issued_at.0,
            issued_at_nanos: self.issued_at.1,
            expires_at_seconds: self.expires_at.0,
            expires_at_nanos: self.expires_at.1,
        };
        let canonical = canonical_connection_fence_v2(&claims).expect("canonical fence");
        let signature = key.sign(&canonical).to_bytes().to_vec();
        ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: claims.key_id,
            fence_id: claims.fence_id.to_vec(),
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            owner_instance_id: claims.owner_instance_id.to_vec(),
            owner_incarnation: claims.owner_incarnation,
            owner_epoch: claims.owner_epoch,
            connection_id: claims.connection_id.to_vec(),
            authorization_revision: claims.authorization_revision,
            capabilities: claims.capabilities,
            lease_until: Some(ts(
                claims.lease_until_seconds,
                i32::try_from(claims.lease_until_nanos).expect("lease nanos"),
            )),
            issued_at: Some(ts(
                claims.issued_at_seconds,
                i32::try_from(claims.issued_at_nanos).expect("issued nanos"),
            )),
            expires_at: Some(ts(
                claims.expires_at_seconds,
                i32::try_from(claims.expires_at_nanos).expect("expires nanos"),
            )),
            signature,
        }
    }
}

pub fn sign_fence_binding(
    key: &SigningKey,
    fence: &ConnectionFenceV2,
    operation_kind: FenceOperationKind,
    operation_id: [u8; 16],
    capability: &str,
    issued_at: (i64, u32),
    expires_at: (i64, u32),
) -> FenceBindingV2 {
    let claims = FenceBindingClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&key.verifying_key()),
        operation_kind: operation_kind as u32,
        operation_id,
        fence_id: fence.fence_id.as_slice().try_into().expect("fence ID"),
        node_id: fence.node_id.as_slice().try_into().expect("node ID"),
        endpoint_id: fence
            .endpoint_id
            .as_slice()
            .try_into()
            .expect("endpoint ID"),
        owner_instance_id: fence
            .owner_instance_id
            .as_slice()
            .try_into()
            .expect("owner instance ID"),
        owner_incarnation: fence.owner_incarnation,
        owner_epoch: fence.owner_epoch,
        connection_id: fence
            .connection_id
            .as_slice()
            .try_into()
            .expect("connection ID"),
        authorization_revision: fence.authorization_revision,
        capability: capability.to_owned(),
        issued_at_seconds: issued_at.0,
        issued_at_nanos: issued_at.1,
        expires_at_seconds: expires_at.0,
        expires_at_nanos: expires_at.1,
    };
    let canonical = canonical_fence_binding_v2(&claims).expect("canonical binding");
    let signature = key.sign(&canonical).to_bytes().to_vec();
    FenceBindingV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: claims.key_id,
        operation_kind: operation_kind.into(),
        operation_id: claims.operation_id.to_vec(),
        fence_id: claims.fence_id.to_vec(),
        node_id: claims.node_id.to_vec(),
        endpoint_id: claims.endpoint_id.to_vec(),
        owner_instance_id: claims.owner_instance_id.to_vec(),
        owner_incarnation: claims.owner_incarnation,
        owner_epoch: claims.owner_epoch,
        connection_id: claims.connection_id.to_vec(),
        authorization_revision: claims.authorization_revision,
        capability: claims.capability,
        issued_at: Some(ts(
            claims.issued_at_seconds,
            i32::try_from(claims.issued_at_nanos).expect("issued nanos"),
        )),
        expires_at: Some(ts(
            claims.expires_at_seconds,
            i32::try_from(claims.expires_at_nanos).expect("expires nanos"),
        )),
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
            version: CommandAuthorizationVersion::V1.into(),
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
        version: CommandAuthorizationVersion::V1.into(),
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

pub fn attach_command_fence(
    key: &SigningKey,
    command: &mut CommandEnvelope,
    fence_id: [u8; 16],
    endpoint_id: [u8; 32],
    lease_until: (i64, u32),
) {
    let issued_at = command.issued_at.expect("command issuance").seconds;
    let expires_at = command.expires_at.expect("command expiry").seconds;
    let node_id: [u8; 16] = command.node_id.as_slice().try_into().expect("node ID");
    let fence = FenceSpec::new(node_id, endpoint_id, 1)
        .ids(fence_id, [2; 16], 1, [3; 16])
        .authority(command.expected_revision)
        .capabilities([command.required_capability.clone()])
        .lease(lease_until.0, lease_until.1)
        .issued(issued_at - 1, 0)
        .expires(expires_at.max(lease_until.0 + 1), 0)
        .sign(key);
    let op_id: [u8; 16] = command
        .command_id
        .as_slice()
        .try_into()
        .expect("command ID");
    let binding = sign_fence_binding(
        key,
        &fence,
        FenceOperationKind::Command,
        op_id,
        &command.required_capability,
        (issued_at, 0),
        (fence.expires_at.expect("fence expiry").seconds, 0),
    );
    command.fence_binding = Some(binding);
    command.connection_fence = Some(fence);
}
