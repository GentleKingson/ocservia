use crate::CONNECTION_FENCING_CAPABILITY;
use crate::commands::{
    map_external_effect, observe_external_effect, response_proof, synthetic_receipt_path,
    terminal_privd_error, terminal_privd_error_code, wait_for_synthetic_barrier,
};
use crate::session::{
    ActiveSessionAuthority, FenceDecision, SessionGrantAuthority,
    activate_session_connection_fence, fenced_artifact_authority,
    gate_signed_read_only_stale_diagnostic, negotiate_session_mode, negotiate_session_mode_at,
    negotiate_session_mode_at_instant, refresh_fence_epoch_floor, session_capabilities,
    supported_capabilities, verify_fenced_operation,
};
use crate::startup::{
    Config, build_relay_mode, build_sealing_keys, load_controller_command_keys, prepare_enrollment,
    read_enrollment_token, validate_enrollment_mode,
};
use iroh::EndpointId;
use iroh::endpoint::RelayMode;
use ocservia_agent::{ExternalEffectObservation, PrivdClient};
use ocservia_agent_protocol::{
    DesiredEffectState, ErrorKind, PrivdResponse, privd_request, privd_response,
};
use ocservia_command_authorization::{ControllerCommandKeyring, FenceBindingClaimsV2};
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, ConnectionFenceV2, FenceBindingV2, FenceOperationKind, HandshakeResult,
    SealedSecretPurpose, SealedSecretVersion, SealingKeyDescriptorV1, SessionHandshakeResponse,
    command_envelope,
};
use ocservia_contracts::session::{
    ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME, READ_ONLY_SESSION_CAPABILITIES,
};
use std::path::PathBuf;
use std::time::{Duration, SystemTime};
use uuid::Uuid;

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_agent_protocol::{
    DesiredEffectObservation, OcservVersion, PrivdRequest, read_frame, write_frame,
};
use ocservia_command_authorization::{
    ConnectionFenceClaimsV2, FENCE_SIGNATURE_VERSION_ED25519_V1, canonical_connection_fence_v2,
    canonical_fence_binding_v2, verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CompleteConfigApply, CompleteConfigPlan, EnrollRequest, FenceSignatureVersion, SealedSecretV1,
    SessionGrantV1, SessionGrantVersion, UserPasswordRotate,
};
use prost_types::Timestamp;
use std::os::unix::fs::PermissionsExt as _;

fn test_sealing_keys() -> Vec<SealingKeyDescriptorV1> {
    vec![
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "node-key-1".to_owned(),
            public_key_sha256: vec![0x11; 32],
        },
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::CertificateP12Password as i32,
            key_id: "node-p12-key-1".to_owned(),
            public_key_sha256: vec![0x22; 32],
        },
    ]
}

#[test]
fn agent_advertises_connection_owner_fencing_once() {
    assert_eq!(
        supported_capabilities()
            .iter()
            .filter(|capability| capability.as_str() == "ocserv.fencing.v2")
            .count(),
        1
    );
}

#[test]
fn signed_v11_read_only_rejects_both_artifact_frames_before_handler() {
    let authority = ActiveSessionAuthority::SignedReadOnlyV11(SessionGrantAuthority {
        negotiated_capabilities: ["ocserv.status.read".to_owned()].into_iter().collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    });
    for framed_length in [ARTIFACT_FETCH_FRAME | 32, ARTIFACT_CONSUME_FRAME | 32] {
        assert!(
            fenced_artifact_authority(&authority, framed_length).is_err(),
            "signed read-only session must reject artifact frame {framed_length:#x} before its handler"
        );
    }
}

fn test_user_password(ciphertext: Vec<u8>) -> SealedSecretV1 {
    SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::UserPassword as i32,
        key_id: "node-key-1".to_owned(),
        ciphertext,
    }
}

fn command_key_config(probe_only: bool) -> Config {
    Config {
        identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
        journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
        privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
        controller: None,
        node_id: None,
        probe_only,
        relay_mode: RelayMode::Disabled,
        command_verification_key_files: Vec::new(),
        prepare_enrollment: false,
        enrollment_token_file: None,
        enrollment_environment: None,
        sealing_keys: test_sealing_keys(),
        stats_file: None,
        relay_ca_file: None,
        synthetic_barrier_file: None,
    }
}

fn test_command_keyring() -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([SigningKey::from_bytes(&[7; 32]).verifying_key()])
        .expect("test command keyring")
}

#[tokio::test]
async fn synthetic_barrier_blocks_until_removed() {
    let command_id = Uuid::now_v7();
    let path = PathBuf::from(format!(
        "/tmp/ocservia-agent-barrier-{}",
        Uuid::now_v7().simple()
    ));
    tokio::fs::write(&path, b"")
        .await
        .expect("arm synthetic barrier");
    let waiting_path = path.clone();
    let waiter = tokio::spawn(async move {
        wait_for_synthetic_barrier(Some(&waiting_path), command_id.as_bytes()).await
    });
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert!(!waiter.is_finished());
    tokio::fs::remove_file(&path)
        .await
        .expect("release synthetic barrier");
    tokio::time::timeout(Duration::from_secs(1), waiter)
        .await
        .expect("synthetic barrier release timed out")
        .expect("synthetic barrier task")
        .expect("synthetic barrier wait");
}

#[tokio::test]
async fn exact_synthetic_barrier_signals_receipt_before_release() {
    let command_id = Uuid::now_v7();
    let path = PathBuf::from(format!(
        "/tmp/ocservia-agent-exact-barrier-{}",
        Uuid::now_v7().simple()
    ));
    let receipt = synthetic_receipt_path(&path);
    tokio::fs::write(&path, format!("{command_id}\n"))
        .await
        .expect("arm exact synthetic barrier");
    tokio::fs::write(&receipt, b"")
        .await
        .expect("pre-create synthetic receipt");
    let waiting_path = path.clone();
    let waiter = tokio::spawn(async move {
        wait_for_synthetic_barrier(Some(&waiting_path), command_id.as_bytes()).await
    });
    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            if tokio::fs::read_to_string(&receipt)
                .await
                .is_ok_and(|value| value == format!("{command_id}\n"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("exact synthetic receipt timed out");
    assert!(
        !waiter.is_finished(),
        "receipt must precede barrier release"
    );
    tokio::fs::remove_file(&path)
        .await
        .expect("release exact synthetic barrier");
    tokio::time::timeout(Duration::from_secs(1), waiter)
        .await
        .expect("exact synthetic barrier release timed out")
        .expect("exact synthetic barrier task")
        .expect("exact synthetic barrier wait");
    tokio::fs::remove_file(receipt)
        .await
        .expect("remove synthetic receipt");
}

fn signed_fence(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    owner_epoch: u64,
) -> ConnectionFenceV2 {
    signed_fence_for_authority(
        signing,
        node_id,
        endpoint_id,
        owner_epoch,
        1,
        vec!["synthetic.noop".to_owned()],
        1_700_000_200,
        0,
        1_700_000_300,
    )
}

#[allow(clippy::too_many_arguments)]
fn signed_fence_for_authority(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    owner_epoch: u64,
    authorization_revision: u64,
    capabilities: Vec<String>,
    lease_until_seconds: i64,
    lease_until_nanos: u32,
    expires_at_seconds: i64,
) -> ConnectionFenceV2 {
    signed_fence_for_term(
        signing,
        node_id,
        endpoint_id,
        owner_epoch,
        authorization_revision,
        capabilities,
        lease_until_seconds,
        lease_until_nanos,
        expires_at_seconds,
        0,
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    )
}

#[allow(clippy::too_many_arguments)]
fn signed_fence_for_term(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    owner_epoch: u64,
    authorization_revision: u64,
    capabilities: Vec<String>,
    lease_until_seconds: i64,
    lease_until_nanos: u32,
    expires_at_seconds: i64,
    expires_at_nanos: u32,
    fence_id: [u8; 16],
    owner_instance_id: [u8; 16],
    owner_incarnation: u64,
    connection_id: [u8; 16],
) -> ConnectionFenceV2 {
    let claims = ConnectionFenceClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&signing.verifying_key()),
        fence_id,
        node_id: *node_id.as_bytes(),
        endpoint_id: *endpoint_id.as_bytes(),
        owner_instance_id,
        owner_incarnation,
        owner_epoch,
        connection_id,
        authorization_revision,
        capabilities,
        lease_until_seconds,
        lease_until_nanos,
        issued_at_seconds: 1_700_000_000,
        issued_at_nanos: 0,
        expires_at_seconds,
        expires_at_nanos,
    };
    let canonical = canonical_connection_fence_v2(&claims).expect("canonical connection fence");
    let signature = signing.sign(&canonical).to_bytes();
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
        lease_until: Some(Timestamp {
            seconds: claims.lease_until_seconds,
            nanos: i32::try_from(claims.lease_until_nanos).expect("lease nanos"),
        }),
        issued_at: Some(Timestamp {
            seconds: claims.issued_at_seconds,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: i32::try_from(claims.expires_at_nanos).expect("proof expiry nanos"),
        }),
        signature: signature.to_vec(),
    }
}

#[allow(clippy::too_many_arguments)]
fn signed_fence_binding(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    owner_epoch: u64,
    authorization_revision: u64,
    operation_kind: FenceOperationKind,
    operation_id: [u8; 16],
    capability: &str,
    fence_id: [u8; 16],
    owner_instance_id: [u8; 16],
    owner_incarnation: u64,
    connection_id: [u8; 16],
) -> FenceBindingV2 {
    signed_fence_binding_until(
        signing,
        node_id,
        endpoint_id,
        owner_epoch,
        authorization_revision,
        operation_kind,
        operation_id,
        capability,
        fence_id,
        owner_instance_id,
        owner_incarnation,
        connection_id,
        1_700_000_300,
        0,
    )
}

#[allow(clippy::too_many_arguments)]
fn signed_fence_binding_until(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    owner_epoch: u64,
    authorization_revision: u64,
    operation_kind: FenceOperationKind,
    operation_id: [u8; 16],
    capability: &str,
    fence_id: [u8; 16],
    owner_instance_id: [u8; 16],
    owner_incarnation: u64,
    connection_id: [u8; 16],
    expires_at_seconds: i64,
    expires_at_nanos: u32,
) -> FenceBindingV2 {
    let claims = FenceBindingClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&signing.verifying_key()),
        operation_kind: operation_kind as u32,
        operation_id,
        fence_id,
        node_id: *node_id.as_bytes(),
        endpoint_id: *endpoint_id.as_bytes(),
        owner_instance_id,
        owner_incarnation,
        owner_epoch,
        connection_id,
        authorization_revision,
        capability: capability.to_owned(),
        issued_at_seconds: 1_700_000_000,
        issued_at_nanos: 0,
        expires_at_seconds,
        expires_at_nanos,
    };
    let canonical = canonical_fence_binding_v2(&claims).expect("canonical fence binding");
    let signature = signing.sign(&canonical).to_bytes();
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
        issued_at: Some(Timestamp {
            seconds: claims.issued_at_seconds,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: i32::try_from(claims.expires_at_nanos).expect("binding expiry nanos"),
        }),
        signature: signature.to_vec(),
    }
}

fn signed_handshake_response(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    authorization_revision: u64,
    capabilities: Vec<String>,
    connection_fence: Option<ConnectionFenceV2>,
) -> SessionHandshakeResponse {
    let mut grant = SessionGrantV1 {
        version: SessionGrantVersion::V1.into(),
        key_id: verification_key_id(&signing.verifying_key()),
        protocol_major: 1,
        protocol_minor: 1,
        node_id: node_id.as_bytes().to_vec(),
        endpoint_id: endpoint_id.as_bytes().to_vec(),
        authorization_revision,
        negotiated_capabilities: capabilities.clone(),
        issued_at: Some(Timestamp {
            seconds: 1_700_000_000,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: 1_700_000_300,
            nanos: 0,
        }),
        signature: Vec::new(),
    };
    let claims = ocservia_command_authorization::session_grant_claims_v1(&grant)
        .expect("session grant claims");
    let canonical = ocservia_command_authorization::canonical_session_grant_v1(&claims)
        .expect("canonical session grant");
    grant.signature = signing.sign(&canonical).to_bytes().to_vec();
    SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".to_owned(),
        negotiated_capabilities: capabilities,
        session_grant: Some(grant),
        connection_fence,
    }
}

#[test]
fn signed_read_only_stale_diagnostic_never_advances_the_floor() {
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring =
        ControllerCommandKeyring::new([signing.verifying_key()]).expect("test command keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id: EndpointId = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let command_id = [5_u8; 16];
    let now = 1_700_000_050_i64;
    let stale = signed_fence(&signing, &node_id, &endpoint_id, 7);
    assert!(matches!(
        gate_signed_read_only_stale_diagnostic(
            &keyring,
            8,
            &node_id,
            &endpoint_id,
            &stale,
            &command_id,
            now,
            0,
        )
        .expect("signed stale diagnostic classified"),
        FenceDecision::RejectedStaleOwnerEpoch
    ));

    for epoch in [8, 9] {
        let non_stale = signed_fence(&signing, &node_id, &endpoint_id, epoch);
        assert!(
            gate_signed_read_only_stale_diagnostic(
                &keyring,
                8,
                &node_id,
                &endpoint_id,
                &non_stale,
                &command_id,
                now,
                0,
            )
            .is_err(),
            "signed read-only command at epoch {epoch} must fail closed"
        );
    }
    let foreign_node = signed_fence(&signing, &Uuid::now_v7(), &endpoint_id, 7);
    assert!(
        gate_signed_read_only_stale_diagnostic(
            &keyring,
            8,
            &node_id,
            &endpoint_id,
            &foreign_node,
            &command_id,
            now,
            0,
        )
        .is_err()
    );
}

#[test]
#[allow(clippy::too_many_lines)]
fn fenced_operation_requires_the_active_term_and_never_advances_the_floor() {
    const NOW: i64 = 1_700_000_050;
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let capabilities = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];
    let grant = SessionGrantAuthority {
        negotiated_capabilities: capabilities.iter().cloned().collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    };
    let command_id = [5_u8; 16];
    let active = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        capabilities.clone(),
        NOW,
        100,
        1_700_000_300,
    );
    let active_verified = keyring
        .verify_connection_fence_v2(&active, node_id.as_bytes(), endpoint_id.as_bytes(), NOW)
        .expect("active fence");
    let binding = signed_fence_binding(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        FenceOperationKind::Command,
        command_id,
        "synthetic.noop",
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    verify_fenced_operation(
        &keyring,
        &grant,
        &active_verified,
        8,
        &node_id,
        &endpoint_id,
        Some(&active),
        Some(&binding),
        FenceOperationKind::Command,
        &command_id,
        "synthetic.noop",
        NOW,
        99,
    )
    .expect("active term accepted one nanosecond before lease deadline");

    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&active),
            Some(&binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            100,
        )
        .is_err(),
        "lease expires at its exact nanosecond deadline"
    );

    let proof_boundary = signed_fence_for_term(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        capabilities.clone(),
        NOW,
        100,
        NOW,
        100,
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    verify_fenced_operation(
        &keyring,
        &grant,
        &active_verified,
        8,
        &node_id,
        &endpoint_id,
        Some(&proof_boundary),
        Some(&binding),
        FenceOperationKind::Command,
        &command_id,
        "synthetic.noop",
        NOW,
        99,
    )
    .expect("fence proof remains live one nanosecond before expiry");
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&proof_boundary),
            Some(&binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            100,
        )
        .is_err(),
        "fence proof expires at its exact nanosecond deadline"
    );

    let live_refresh = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        capabilities.clone(),
        NOW + 1,
        0,
        1_700_000_300,
    );
    let binding_boundary = signed_fence_binding_until(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        FenceOperationKind::Command,
        command_id,
        "synthetic.noop",
        [1; 16],
        [2; 16],
        1,
        [3; 16],
        NOW,
        100,
    );
    verify_fenced_operation(
        &keyring,
        &grant,
        &active_verified,
        8,
        &node_id,
        &endpoint_id,
        Some(&live_refresh),
        Some(&binding_boundary),
        FenceOperationKind::Command,
        &command_id,
        "synthetic.noop",
        NOW,
        99,
    )
    .expect("fence binding remains live one nanosecond before expiry");
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&live_refresh),
            Some(&binding_boundary),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            100,
        )
        .is_err(),
        "fence binding expires at its exact nanosecond deadline"
    );

    for (fence, binding) in [(None, Some(&binding)), (Some(&active), None)] {
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                fence,
                binding,
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                99,
            )
            .is_err()
        );
    }

    let higher = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        9,
        3,
        capabilities.clone(),
        1_700_000_200,
        0,
        1_700_000_300,
    );
    let higher_binding = signed_fence_binding(
        &signing,
        &node_id,
        &endpoint_id,
        9,
        3,
        FenceOperationKind::Command,
        command_id,
        "synthetic.noop",
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&higher),
            Some(&higher_binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            0,
        )
        .is_err(),
        "a command cannot activate a successor epoch"
    );

    let different = signed_fence_for_term(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        capabilities,
        1_700_000_200,
        0,
        1_700_000_300,
        0,
        [9; 16],
        [2; 16],
        1,
        [4; 16],
    );
    let different_binding = signed_fence_binding(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        FenceOperationKind::Command,
        command_id,
        "synthetic.noop",
        [9; 16],
        [2; 16],
        1,
        [4; 16],
    );
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&different),
            Some(&different_binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            0,
        )
        .is_err(),
        "same epoch under a different immutable term fails closed"
    );

    let artifact_id = [6_u8; 16];
    let artifact_binding = signed_fence_binding(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        FenceOperationKind::Artifact,
        artifact_id,
        CONNECTION_FENCING_CAPABILITY,
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    verify_fenced_operation(
        &keyring,
        &grant,
        &active_verified,
        8,
        &node_id,
        &endpoint_id,
        Some(&active),
        Some(&artifact_binding),
        FenceOperationKind::Artifact,
        &artifact_id,
        CONNECTION_FENCING_CAPABILITY,
        NOW,
        99,
    )
    .expect("artifact binding is grounded in the active session term");
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&active),
            Some(&artifact_binding),
            FenceOperationKind::Artifact,
            &[7; 16],
            CONNECTION_FENCING_CAPABILITY,
            NOW,
            99,
        )
        .is_err(),
        "an artifact binding cannot be replayed for another operation"
    );
}

#[test]
fn overlapping_agent_floor_retires_an_old_active_session_before_dispatch() {
    const NOW: i64 = 1_700_000_050;
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let capabilities = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];
    let directory = std::env::temp_dir()
        .canonicalize()
        .expect("tmp")
        .join(format!("agent-floor-overlap-{}", Uuid::now_v7().simple()));
    std::fs::create_dir_all(&directory).expect("test directory");
    let journal_path = directory.join("journal.db");
    let active_agent = Journal::open(&journal_path).expect("active Agent journal");
    let successor_agent = Journal::open(&journal_path).expect("successor Agent journal");
    assert_eq!(
        active_agent
            .raise_owner_fence_epoch_floor(5)
            .expect("activate old term"),
        5
    );
    let mut cached_floor = 5_u64;
    assert_eq!(
        successor_agent
            .raise_owner_fence_epoch_floor(10)
            .expect("activate successor term"),
        10
    );
    refresh_fence_epoch_floor(&active_agent, &mut cached_floor)
        .expect("refresh durable epoch floor before dispatch");
    assert_eq!(cached_floor, 10);

    let old_fence = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        5,
        3,
        capabilities.clone(),
        1_700_000_200,
        0,
        1_700_000_300,
    );
    let old_verified = keyring
        .verify_connection_fence_v2(&old_fence, node_id.as_bytes(), endpoint_id.as_bytes(), NOW)
        .expect("old active fence");
    let command_id = [5_u8; 16];
    let binding = signed_fence_binding(
        &signing,
        &node_id,
        &endpoint_id,
        5,
        3,
        FenceOperationKind::Command,
        command_id,
        "synthetic.noop",
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    let grant = SessionGrantAuthority {
        negotiated_capabilities: capabilities.into_iter().collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    };
    assert!(
        verify_fenced_operation(
            &keyring,
            &grant,
            &old_verified,
            cached_floor,
            &node_id,
            &endpoint_id,
            Some(&old_fence),
            Some(&binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            0,
        )
        .is_err(),
        "the old active session must be retired before command execution"
    );
    assert_eq!(
        active_agent
            .owner_fence_epoch_floor()
            .expect("durable floor"),
        10
    );
    drop(successor_agent);
    drop(active_agent);
    std::fs::remove_dir_all(&directory).ok();
}

#[test]
fn fenced_handshake_persists_new_epoch_before_activation_and_rejects_replay() {
    const NOW: i64 = 1_700_000_050;
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let capabilities = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];
    let fence = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        8,
        3,
        capabilities.clone(),
        NOW,
        100,
        1_700_000_300,
    );
    let response = signed_handshake_response(
        &signing,
        &node_id,
        &endpoint_id,
        3,
        capabilities.clone(),
        Some(fence),
    );
    let mode = negotiate_session_mode_at(
        &response,
        &supported_capabilities(),
        node_id,
        endpoint_id,
        &keyring,
        NOW,
    )
    .expect("signed session grant");
    assert!(matches!(
        &mode,
        ActiveSessionAuthority::FencedV11 {
            connection_fence,
            ..
        } if connection_fence.owner_epoch == 8
    ));
    let directory = std::env::temp_dir()
        .canonicalize()
        .expect("tmp")
        .join(format!("agent-handshake-fence-{}", Uuid::now_v7().simple()));
    std::fs::create_dir_all(&directory).expect("test directory");
    let journal_path = directory.join("journal.db");
    let mut journal = Journal::open(&journal_path).expect("journal");
    journal
        .raise_owner_fence_epoch_floor(7)
        .expect("preseed floor");
    let mut floor = 7_u64;

    activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99)
        .expect("higher owner epoch activates");
    assert_eq!(floor, 8);
    assert_eq!(journal.owner_fence_epoch_floor().expect("durable floor"), 8);

    assert!(
        activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99,).is_err(),
        "a second connection cannot reactivate an already observed epoch"
    );

    let _ = capabilities;
    drop(journal);
    let reopened = Journal::open(&journal_path).expect("reopen journal");
    assert_eq!(
        reopened.owner_fence_epoch_floor().expect("reopened floor"),
        8
    );
    std::fs::remove_dir_all(&directory).ok();
}

#[test]
fn fenced_handshake_proof_uses_the_exact_nanosecond_boundary() {
    const NOW: i64 = 1_700_000_050;
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let capabilities = vec![CONNECTION_FENCING_CAPABILITY.to_owned()];
    let fence = signed_fence_for_term(
        &signing,
        &node_id,
        &endpoint_id,
        1,
        3,
        capabilities.clone(),
        NOW,
        100,
        NOW,
        100,
        [1; 16],
        [2; 16],
        1,
        [3; 16],
    );
    let response = signed_handshake_response(
        &signing,
        &node_id,
        &endpoint_id,
        3,
        capabilities,
        Some(fence),
    );
    let mode = negotiate_session_mode_at_instant(
        &response,
        &supported_capabilities(),
        node_id,
        endpoint_id,
        &keyring,
        NOW,
        99,
    )
    .expect("fence proof remains live one nanosecond before expiry");
    assert!(
        negotiate_session_mode_at_instant(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
            NOW,
            100,
        )
        .is_err(),
        "handshake verification rejects the exact proof expiry deadline"
    );

    let directory = std::env::temp_dir()
        .canonicalize()
        .expect("tmp")
        .join(format!("agent-proof-boundary-{}", Uuid::now_v7().simple()));
    std::fs::create_dir_all(&directory).expect("test directory");
    let journal_path = directory.join("journal.db");
    let mut journal = Journal::open(&journal_path).expect("journal");
    let mut floor = 0_u64;
    assert!(
        activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 100).is_err(),
        "activation rechecks the proof at its exact expiry deadline"
    );
    assert_eq!(floor, 0);
    assert_eq!(journal.owner_fence_epoch_floor().expect("durable floor"), 0);
    drop(journal);
    std::fs::remove_dir_all(&directory).ok();
}

#[test]
#[allow(clippy::too_many_lines)]
fn fenced_handshake_failures_do_not_advance_the_durable_epoch() {
    const NOW: i64 = 1_700_000_050;
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keyring = ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
    let capabilities = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];
    let valid_fence = signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        11,
        3,
        capabilities.clone(),
        1_700_000_200,
        0,
        1_700_000_300,
    );
    let valid_response = signed_handshake_response(
        &signing,
        &node_id,
        &endpoint_id,
        3,
        capabilities.clone(),
        Some(valid_fence.clone()),
    );

    let mut cases = Vec::new();
    let mut missing = valid_response.clone();
    missing.connection_fence = None;
    cases.push(("missing fence", missing));

    let non_fencing_capabilities = vec!["synthetic.noop".to_owned()];
    cases.push((
        "mutation capability without fencing",
        signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            non_fencing_capabilities.clone(),
            None,
        ),
    ));
    cases.push((
        "unexpected fence",
        signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            non_fencing_capabilities.clone(),
            Some(signed_fence_for_authority(
                &signing,
                &node_id,
                &endpoint_id,
                11,
                3,
                non_fencing_capabilities,
                1_700_000_200,
                0,
                1_700_000_300,
            )),
        ),
    ));

    for (name, epoch) in [("lower epoch", 9), ("equal epoch", 10)] {
        let mut response = valid_response.clone();
        response.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            epoch,
            3,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        ));
        cases.push((name, response));
    }

    let mut expired_proof = valid_response.clone();
    expired_proof.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        11,
        3,
        capabilities.clone(),
        NOW,
        0,
        NOW,
    ));
    cases.push(("expired proof", expired_proof));

    let mut expired_lease = valid_response.clone();
    expired_lease.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        11,
        3,
        capabilities.clone(),
        NOW,
        99,
        1_700_000_300,
    ));
    cases.push(("expired lease", expired_lease));

    let mut bad_signature = valid_response.clone();
    bad_signature
        .connection_fence
        .as_mut()
        .expect("fence")
        .signature[0] ^= 1;
    cases.push(("bad signature", bad_signature));

    let mut wrong_revision = valid_response.clone();
    wrong_revision.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        11,
        4,
        capabilities.clone(),
        1_700_000_200,
        0,
        1_700_000_300,
    ));
    cases.push(("authorization revision mismatch", wrong_revision));

    let mut wrong_capabilities = valid_response.clone();
    wrong_capabilities.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &node_id,
        &endpoint_id,
        11,
        3,
        vec![CONNECTION_FENCING_CAPABILITY.to_owned()],
        1_700_000_200,
        0,
        1_700_000_300,
    ));
    cases.push(("capability mismatch", wrong_capabilities));

    let other_node = Uuid::now_v7();
    let mut wrong_node = valid_response.clone();
    wrong_node.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &other_node,
        &endpoint_id,
        11,
        3,
        capabilities.clone(),
        1_700_000_200,
        0,
        1_700_000_300,
    ));
    cases.push(("node mismatch", wrong_node));

    let other_endpoint = iroh::SecretKey::from_bytes(&[8; 32]).public();
    let mut wrong_endpoint = valid_response;
    wrong_endpoint.connection_fence = Some(signed_fence_for_authority(
        &signing,
        &node_id,
        &other_endpoint,
        11,
        3,
        capabilities,
        1_700_000_200,
        0,
        1_700_000_300,
    ));
    cases.push(("endpoint mismatch", wrong_endpoint));

    let directory = std::env::temp_dir()
        .canonicalize()
        .expect("tmp")
        .join(format!(
            "agent-handshake-fence-negative-{}",
            Uuid::now_v7().simple()
        ));
    std::fs::create_dir_all(&directory).expect("test directory");
    let journal_path = directory.join("journal.db");
    let mut journal = Journal::open(&journal_path).expect("journal");
    journal
        .raise_owner_fence_epoch_floor(10)
        .expect("preseed floor");
    let mut floor = 10_u64;
    let read_only_response = signed_handshake_response(
        &signing,
        &node_id,
        &endpoint_id,
        3,
        vec!["ocserv.status.read".to_owned()],
        None,
    );
    let read_only_mode = negotiate_session_mode_at(
        &read_only_response,
        &supported_capabilities(),
        node_id,
        endpoint_id,
        &keyring,
        NOW,
    )
    .expect("signed read-only grant");
    assert!(matches!(
        &read_only_mode,
        ActiveSessionAuthority::SignedReadOnlyV11(_)
    ));
    activate_session_connection_fence(&read_only_mode, &mut journal, &mut floor, NOW, 100)
        .expect("authorized read-only compatibility session");
    assert_eq!(floor, 10);
    for (name, response) in cases {
        let negotiated = negotiate_session_mode_at(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
            NOW,
        );
        if let Ok(mode) = negotiated {
            assert!(
                activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 100,)
                    .is_err(),
                "{name} must fail closed"
            );
        }
        assert_eq!(floor, 10, "{name} changed the in-memory floor");
        assert_eq!(
            journal.owner_fence_epoch_floor().expect("durable floor"),
            10,
            "{name} changed the durable floor"
        );
    }
    drop(journal);
    let reopened = Journal::open(&journal_path).expect("reopen journal");
    assert_eq!(
        reopened.owner_fence_epoch_floor().expect("reopened floor"),
        10
    );
    std::fs::remove_dir_all(&directory).ok();
}

#[test]
fn agent_rejects_protocol_1_0_grantless_sessions() {
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::generate().public();
    for negotiated_capabilities in [
        READ_ONLY_SESSION_CAPABILITIES
            .iter()
            .map(|capability| (*capability).to_owned())
            .collect(),
        vec!["synthetic.noop".to_owned()],
    ] {
        let response = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: 0,
            max_message_size: 1024 * 1024,
            controller_version: "test".to_owned(),
            negotiated_capabilities,
            session_grant: None,
            connection_fence: None,
        };
        assert!(
            negotiate_session_mode(
                &response,
                &supported_capabilities(),
                node_id,
                endpoint_id,
                &test_command_keyring(),
            )
            .is_err()
        );
    }
}

#[test]
fn agent_rejects_grantless_protocol_v1_1() {
    let response = SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".to_owned(),
        negotiated_capabilities: vec!["ocserv.status.read".to_owned()],
        session_grant: None,
        connection_fence: None,
    };
    assert!(
        negotiate_session_mode(
            &response,
            &supported_capabilities(),
            Uuid::now_v7(),
            iroh::SecretKey::generate().public(),
            &test_command_keyring(),
        )
        .is_err()
    );
}

#[test]
fn agent_rejects_invalid_and_accepts_valid_protocol_v1_1_grant_signatures() {
    let node_id = Uuid::now_v7();
    let endpoint_id = iroh::SecretKey::generate().public();
    let signing_key = SigningKey::from_bytes(&[7; 32]);
    let capabilities = vec!["ocserv.status.read".to_owned()];
    let now = SystemTime::now();
    let mut response = SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".to_owned(),
        negotiated_capabilities: capabilities.clone(),
        session_grant: Some(SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: ocservia_command_authorization::verification_key_id(
                &signing_key.verifying_key(),
            ),
            protocol_major: 1,
            protocol_minor: 1,
            node_id: node_id.as_bytes().to_vec(),
            endpoint_id: endpoint_id.as_bytes().to_vec(),
            authorization_revision: 1,
            negotiated_capabilities: capabilities,
            issued_at: Some((now - Duration::from_secs(1)).into()),
            expires_at: Some((now + Duration::from_secs(30)).into()),
            signature: vec![0; 64],
        }),
        connection_fence: None,
    };
    let keyring =
        ControllerCommandKeyring::new([signing_key.verifying_key()]).expect("test command keyring");
    assert!(
        negotiate_session_mode(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
        )
        .is_err()
    );
    let grant = response.session_grant.as_mut().expect("session grant");
    let claims = ocservia_command_authorization::session_grant_claims_v1(grant)
        .expect("session grant claims");
    let canonical = ocservia_command_authorization::canonical_session_grant_v1(&claims)
        .expect("canonical session grant");
    grant.signature = signing_key.sign(&canonical).to_bytes().to_vec();
    assert!(matches!(
        negotiate_session_mode(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
        )
        .expect("valid signed protocol 1.1 session"),
        ActiveSessionAuthority::SignedReadOnlyV11(_)
    ));
}

#[tokio::test]
async fn session_capabilities_withhold_terminate_below_ocserv_1_4_2() {
    for (version, terminate) in [(Some("1.2.4"), false), (Some("1.4.2"), true), (None, false)] {
        let socket = PathBuf::from(format!("/tmp/ocsv-{}.sock", Uuid::now_v7().simple()));
        let listener = tokio::net::UnixListener::bind(&socket).expect("bind privd fixture");
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept version request");
            let request: PrivdRequest = read_frame(&mut stream).await.expect("version request");
            assert!(matches!(
                request.operation,
                Some(privd_request::Operation::OcservVersion(_))
            ));
            let result = version.map_or_else(
                || privd_response::Result::Error(ocservia_agent_protocol::PrivdError::default()),
                |version| {
                    privd_response::Result::OcservVersion(OcservVersion {
                        version: version.to_owned(),
                    })
                },
            );
            write_frame(
                &mut stream,
                &PrivdResponse {
                    request_id: request.request_id,
                    privileged_result_proof: None,
                    result: Some(result),
                },
            )
            .await
            .expect("version response");
        });
        let privd = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("privd client");
        let capabilities = session_capabilities(&privd).await;
        server.await.expect("privd fixture");
        std::fs::remove_file(&socket).expect("remove privd fixture");
        assert_eq!(
            capabilities
                .iter()
                .any(|capability| capability == "ocserv.session.terminate"),
            terminate,
            "{version:?}"
        );
        let mut expected = supported_capabilities();
        expected.retain(|capability| terminate || capability != "ocserv.session.terminate");
        assert_eq!(capabilities, expected);
    }
}

#[test]
fn network_agent_requires_a_controller_command_verification_key() {
    assert!(load_controller_command_keys(&command_key_config(false)).is_err());
    assert!(
        load_controller_command_keys(&command_key_config(true))
            .expect("read-only probe exception")
            .is_none()
    );
}

#[test]
fn network_agent_requires_two_distinct_password_sealing_keys() {
    let config = command_key_config(false);
    assert!(build_sealing_keys(&config, None, None, None, None).is_err());
    assert!(
        build_sealing_keys(
            &config,
            Some("shared".to_owned()),
            Some("11".repeat(32)),
            Some("shared".to_owned()),
            Some("22".repeat(32)),
        )
        .is_err()
    );
    assert!(
        build_sealing_keys(
            &config,
            Some("user".to_owned()),
            Some("11".repeat(32)),
            Some("p12".to_owned()),
            Some("11".repeat(32)),
        )
        .is_err()
    );
}

#[test]
fn enrollment_mode_uses_endpoint_identity_without_command_keys() {
    let mut config = command_key_config(false);
    config.enrollment_token_file = Some(PathBuf::from("/run/secrets/enrollment-token"));
    config.enrollment_environment = Some("production".to_owned());
    assert!(
        load_controller_command_keys(&config)
            .expect("enrollment has no mutation-capable command session")
            .is_none()
    );
}

#[test]
fn fresh_enrollment_preparation_reuses_the_endpoint_key_for_proof() {
    use ed25519_dalek::{Signature, Verifier as _, VerifyingKey};

    let directory = std::env::temp_dir().join(format!("ocservia-agent-prepare-{}", Uuid::now_v7()));
    let controller = iroh::SecretKey::generate().public();
    let mut config = command_key_config(false);
    config.identity_dir.clone_from(&directory);
    config.controller = Some(controller);
    config.prepare_enrollment = true;

    validate_enrollment_mode(&config).expect("standalone preparation mode");
    assert!(
        load_controller_command_keys(&config)
            .expect("preparation has no command session")
            .is_none()
    );
    let endpoint = prepare_enrollment(&config).expect("prepare fresh endpoint identity");
    assert_eq!(endpoint.len(), 64);
    assert!(
        endpoint
            .bytes()
            .all(|byte| byte.is_ascii_digit() || matches!(byte, b'a'..=b'f'))
    );

    let identity = ocservia_agent_identity::Identity::provision(&directory, controller)
        .expect("reload prepared identity");
    assert_eq!(endpoint, hex::encode(identity.endpoint_id().as_bytes()));
    let mut request = EnrollRequest {
        token: "a".repeat(43),
        endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
        agent_version: "test".to_owned(),
        os_release: "test".to_owned(),
        ocserv_version: "test".to_owned(),
        boot_id: "boot".to_owned(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: supported_capabilities(),
        environment: "production".to_owned(),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
        time: Some(SystemTime::now().into()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: config.sealing_keys.clone(),
    };
    identity
        .authorize_enrollment(&mut request)
        .expect("authorize enrollment with prepared key");
    let canonical = ocservia_agent_identity::enrollment_canonical_v1(&request)
        .expect("canonical enrollment claims");
    let proof = request.proof.expect("signed proof");
    let verification_key =
        VerifyingKey::from_bytes(identity.endpoint_id().as_bytes()).expect("endpoint key");
    let signature = Signature::from_slice(&proof.signature).expect("Ed25519 signature");
    verification_key
        .verify(&canonical, &signature)
        .expect("prepared endpoint proves possession");

    std::fs::remove_dir_all(directory).expect("remove enrollment preparation fixture");
}

#[test]
fn enrollment_token_file_requires_strict_metadata_and_format() {
    let directory =
        std::env::temp_dir().join(format!("ocservia-agent-enrollment-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("create test directory");
    let token = directory.join("enrollment.token");
    let valid_token = "0".repeat(43);
    std::fs::write(&token, format!("{valid_token}\n")).expect("write token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600))
        .expect("protect token");
    assert_eq!(
        read_enrollment_token(&token).expect("valid token"),
        valid_token
    );

    for value in [format!("obt2_{valid_token}"), valid_token.clone()] {
        std::fs::write(&token, format!("{value}\r\n")).expect("write supported token");
        assert_eq!(
            read_enrollment_token(&token).expect("supported token"),
            value
        );
    }
    for value in [
        format!("obt1_{valid_token}"),
        format!("oit1_{valid_token}"),
        format!("obt3_{valid_token}"),
        format!("obt2_{valid_token}extra"),
        "obt2_short".to_owned(),
    ] {
        std::fs::write(&token, value).expect("write malformed token");
        assert!(read_enrollment_token(&token).is_err());
    }
    std::fs::write(&token, format!("obt2_{valid_token}")).expect("restore valid token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644))
        .expect("make token insecure");
    assert!(read_enrollment_token(&token).is_err());
    std::fs::remove_dir_all(directory).expect("remove test directory");
}

#[test]
fn custom_relay_count_and_security_contract() {
    let directory = std::env::temp_dir().join(format!("relay-contract-{}", uuid::Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("test directory");
    let token = directory.join("token");
    std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("mode");

    assert!(build_relay_mode("custom", None, Some(&token)).is_err());
    let RelayMode::Custom(map) =
        build_relay_mode("custom", Some("https://relay-0.example.test"), Some(&token))
            .expect("valid single custom relay")
    else {
        panic!("custom mode required");
    };
    assert_eq!(map.len(), 1);

    for invalid_url in [
        "",
        "not a URL",
        "http://relay.example.test",
        "https://user:pass@relay.example.test",
        "https://relay.example.test?token=x",
        "https://relay.example.test#fragment",
        "https://bad host",
        "https://user\"@relay.example.test",
        "https://$(touch /tmp/relay-injection)",
    ] {
        assert!(
            build_relay_mode("custom", Some(invalid_url), Some(&token)).is_err(),
            "accepted {invalid_url:?}"
        );
    }
    let url = "https://relay.example.test";
    assert!(build_relay_mode("custom", Some(url), None).is_err());
    assert!(build_relay_mode("custom", Some(url), Some(&directory.join("missing"))).is_err());

    assert!(build_relay_mode("disabled", None, None).is_ok());
    assert!(build_relay_mode("disabled", Some(url), None).is_err());
    assert!(build_relay_mode("disabled", None, Some(&token)).is_err());
    assert!(build_relay_mode("default", None, None).is_err());

    for invalid_token in ["short", "0123456789abcdef 123456789abcdef0"] {
        std::fs::write(&token, invalid_token).expect("invalid token");
        assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
    }
    std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("valid token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644)).expect("mode");
    assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
    std::fs::remove_dir_all(directory).expect("cleanup");
}

#[test]
fn production_relay_selection_is_closed() {
    let directory = std::env::temp_dir().join(format!("ocservia-agent-relay-{}", Uuid::now_v7()));
    std::fs::create_dir(&directory).expect("create test directory");
    let token = directory.join("relay.token");
    std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("write token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600))
        .expect("protect token");

    let mode = build_relay_mode("custom", Some("https://relay-a.example.test"), Some(&token))
        .expect("valid custom relay mode");
    assert!(matches!(mode, RelayMode::Custom(_)));
    assert!(build_relay_mode("disabled", None, None).is_ok());
    assert!(build_relay_mode("default", None, None).is_err());

    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644))
        .expect("make token insecure");
    assert!(
        build_relay_mode("custom", Some("https://relay-a.example.test"), Some(&token)).is_err()
    );
    assert!(
        build_relay_mode(
            "custom",
            Some("https://user@relay-a.example.test"),
            Some(&token),
        )
        .is_err()
    );
    std::fs::remove_dir_all(directory).expect("remove test directory");
}

#[test]
fn authoritative_precondition_rejection_is_terminal() {
    assert!(terminal_privd_error(ErrorKind::InvalidRequest.into()));
    assert!(terminal_privd_error(ErrorKind::CapacityExceeded.into()));
    assert_eq!(
        terminal_privd_error_code(ErrorKind::CapacityExceeded.into()),
        Some("capacity_exceeded")
    );
    assert!(!terminal_privd_error(ErrorKind::OutputLimit.into()));
    assert!(!terminal_privd_error(ErrorKind::Unavailable.into()));
    assert!(!terminal_privd_error(ErrorKind::CommandFailed.into()));
}

#[test]
fn unexpected_privd_result_cannot_prove_a_password_effect() {
    let payload = command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
        username: "alice".to_owned(),
        sealed_password: Vec::new(),
        secret_key_id: String::new(),
        desired_revision: 7,
        sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
    });
    let result =
        privd_response::Result::Mutation(ocservia_agent_protocol::MutationResult { applied: true });

    assert_eq!(
        map_external_effect(&payload, result),
        ExternalEffectObservation::Unknown
    );
}

#[test]
fn privileged_success_without_root_receipt_is_never_terminal() {
    let response = PrivdResponse {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        privileged_result_proof: None,
        result: Some(privd_response::Result::Mutation(
            ocservia_agent_protocol::MutationResult { applied: true },
        )),
    };
    assert!(response_proof(&response).is_none());
}

#[tokio::test]
async fn password_reconcile_uses_non_secret_authoritative_effect_store() {
    let socket = PathBuf::from(format!("/tmp/ocsm-{}.sock", Uuid::now_v7().simple()));
    let listener = tokio::net::UnixListener::bind(&socket).expect("bind effect fixture");
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.expect("accept");
        let request: PrivdRequest = read_frame(&mut stream).await.expect("request");
        assert!(request.operation.is_none());
        assert_eq!(
            ocservia_agent_protocol::PrivilegedRequestMode::try_from(request.privileged_mode)
                .expect("privileged mode"),
            ocservia_agent_protocol::PrivilegedRequestMode::Reconcile
        );
        let Some(CommandEnvelope {
            payload: Some(command_envelope::Payload::UserPasswordRotate(observe)),
            ..
        }) = request.authorization_command
        else {
            panic!("desired effect observation required")
        };
        assert_eq!(observe.username, "alice");
        assert_eq!(observe.desired_revision, 7);
        write_frame(
            &mut stream,
            &PrivdResponse {
                request_id: request.request_id,
                privileged_result_proof: None,
                result: Some(privd_response::Result::DesiredEffectObservation(
                    DesiredEffectObservation {
                        state: DesiredEffectState::AppliedExact.into(),
                        observed_revision: observe.desired_revision,
                    },
                )),
            },
        )
        .await
        .expect("response");
    });
    let client = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
    let envelope = CommandEnvelope {
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        semantic_payload_sha256: vec![0x5a; 32],
        expires_at: Some(prost_types::Timestamp {
            seconds: i64::MAX,
            nanos: 0,
        }),
        payload: Some(command_envelope::Payload::UserPasswordRotate(
            UserPasswordRotate {
                username: "alice".to_owned(),
                sealed_password: Vec::new(),
                secret_key_id: String::new(),
                desired_revision: 7,
                sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
            },
        )),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        observe_external_effect(&client, &envelope).await,
        ExternalEffectObservation::AppliedExact
    );
    server.await.expect("server");
    std::fs::remove_file(socket).expect("remove socket");
}

#[tokio::test]
async fn interrupted_config_plan_is_safe_to_retry() {
    let socket = std::env::temp_dir().join(format!("missing-{}.sock", Uuid::now_v7()));
    let client = PrivdClient::new(socket, Duration::from_millis(20)).expect("client");
    let envelope = CommandEnvelope {
        payload: Some(command_envelope::Payload::CompleteConfigPlan(
            CompleteConfigPlan::default(),
        )),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        observe_external_effect(&client, &envelope).await,
        ExternalEffectObservation::Absent
    );
}

#[tokio::test]
async fn interrupted_config_apply_uses_durable_revision_and_effect_identity() {
    async fn observe(
        state: DesiredEffectState,
        observed_revision: u64,
    ) -> ExternalEffectObservation {
        let socket = std::env::temp_dir().join(format!("apply-{}.sock", Uuid::now_v7()));
        let listener = tokio::net::UnixListener::bind(&socket).expect("bind fingerprint fixture");
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            let request: PrivdRequest = read_frame(&mut stream).await.expect("request");
            assert!(request.operation.is_none());
            let Some(CommandEnvelope {
                payload: Some(command_envelope::Payload::CompleteConfigApply(observe)),
                ..
            }) = request.authorization_command
            else {
                panic!("config desired-effect observation required")
            };
            assert_eq!(observe.desired_revision, 2);
            write_frame(
                &mut stream,
                &PrivdResponse {
                    request_id: request.request_id,
                    privileged_result_proof: None,
                    result: Some(privd_response::Result::DesiredEffectObservation(
                        DesiredEffectObservation {
                            state: state.into(),
                            observed_revision,
                        },
                    )),
                },
            )
            .await
            .expect("response");
        });
        let client = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
        let envelope = CommandEnvelope {
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            semantic_payload_sha256: vec![0x6a; 32],
            expires_at: Some(prost_types::Timestamp {
                seconds: i64::MAX,
                nanos: 0,
            }),
            payload: Some(command_envelope::Payload::CompleteConfigApply(
                CompleteConfigApply {
                    candidate_hash: vec![0x5a; 32],
                    expected_current_hash: vec![0x4b; 32],
                    desired_revision: 2,
                    ..CompleteConfigApply::default()
                },
            )),
            ..CommandEnvelope::default()
        };
        let result = observe_external_effect(&client, &envelope).await;
        server.await.expect("server");
        std::fs::remove_file(socket).expect("remove socket");
        result
    }

    assert_eq!(
        observe(DesiredEffectState::AppliedExact, 2).await,
        ExternalEffectObservation::AppliedExact
    );
    assert_eq!(
        observe(DesiredEffectState::Absent, 2).await,
        ExternalEffectObservation::Absent
    );
    assert_eq!(
        observe(DesiredEffectState::SupersededByNewerRevision, 3).await,
        ExternalEffectObservation::SupersededByNewerRevision
    );
    assert_eq!(
        observe(DesiredEffectState::Unknown, 0).await,
        ExternalEffectObservation::Unknown
    );
}
