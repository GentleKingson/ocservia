use std::os::unix::fs::PermissionsExt as _;

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, ReadRequest, privd_request,
};
use ocservia_command_authorization::{
    ConnectionFenceClaimsV2, FENCE_SIGNATURE_VERSION_ED25519_V1, FenceBindingClaimsV2,
    canonical_artifact_grant_v1, canonical_connection_fence_v2, canonical_fence_binding_v2,
    canonical_v1, claims_from_envelope_v1, semantic_payload_hash_v2, verification_key_id,
};
use ocservia_contracts::config_profile;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, ArtifactGrantV1, ArtifactGrantVersion, CertificateP12, CommandAuthorizationProof,
    CommandAuthorizationVersion, CommandDeliveryMode, CompleteConfigApply, CompleteConfigCandidate,
    CompleteConfigDirective, ConnectionFenceV2, FenceBindingV2, FenceSignatureVersion,
    SealedSecretPurpose, SealedSecretV1, SealedSecretVersion, SemanticPayloadHashVersion,
    ServiceReload, UserCreate, command_envelope, complete_config_directive,
};
use ocservia_ocserv_adapter::{FixedResources, Limits};
use ocservia_upgrader::UpgradeTrigger;
use prost_types::Timestamp;

use super::*;

fn keyring(key: &SigningKey) -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([key.verifying_key()]).expect("test keyring")
}

fn unix_seconds() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_secs(),
    )
    .expect("test time")
}

fn deadline() -> u64 {
    u64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_millis(),
    )
    .expect("test time")
        + 5_000
}

fn signed_service_reload(
    key: &SigningKey,
    node_id: [u8; 16],
    issued_at: i64,
    expires_at: i64,
) -> CommandEnvelope {
    signed_command(
        key,
        node_id,
        issued_at,
        expires_at,
        "service.reload",
        "ocserv.service.reload",
        command_envelope::Payload::ServiceReload(ServiceReload {}),
    )
}

fn signed_agent_upgrade(
    key: &SigningKey,
    node_id: [u8; 16],
    issued_at: i64,
    expires_at: i64,
    target_version: &str,
    package_sha256: Vec<u8>,
    architecture: &str,
) -> CommandEnvelope {
    signed_command(
        key,
        node_id,
        issued_at,
        expires_at,
        "agent.upgrade",
        "ocserv.agent.upgrade.v2",
        command_envelope::Payload::AgentUpgrade(AgentUpgrade {
            target_version: target_version.to_owned(),
            package_sha256,
            architecture: architecture.to_owned(),
        }),
    )
}

fn host_architecture() -> &'static str {
    ocservia_contracts::agent_upgrade::runtime_architecture().expect("test host architecture")
}

fn foreign_architecture() -> &'static str {
    if host_architecture() == "amd64" {
        "arm64"
    } else {
        "amd64"
    }
}

fn signed_command(
    key: &SigningKey,
    node_id: [u8; 16],
    issued_at: i64,
    expires_at: i64,
    action: &str,
    capability: &str,
    payload: command_envelope::Payload,
) -> CommandEnvelope {
    let mut command = CommandEnvelope {
        protocol_version: ocservia_command_authorization::COMMAND_PROTOCOL_VERSION.to_owned(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(Timestamp {
            seconds: issued_at,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: expires_at,
            nanos: 0,
        }),
        expected_revision: 7,
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
    command.semantic_payload_sha256 = semantic_payload_hash_v2(&command)
        .expect("semantic hash")
        .to_vec();
    let claims = claims_from_envelope_v1(&command).expect("authorization claims");
    let signature = key.sign(&canonical_v1(&claims).expect("canonical authorization"));
    command
        .authorization
        .as_mut()
        .expect("authorization")
        .signature = signature.to_bytes().to_vec();
    let fence = signed_fence(key, &command, [1; 16], TEST_ENDPOINT_ID, (expires_at, 0));
    command.fence_binding = Some(signed_binding(
        key,
        &command,
        &fence,
        FenceOperationKind::Command,
        &command.command_id,
        &command.required_capability,
    ));
    command.connection_fence = Some(fence);
    command
}

/// Signs the Controller connection fence the Agent would forward with
/// `command`; fences sit outside the command authorization proof.
fn signed_fence(
    key: &SigningKey,
    command: &CommandEnvelope,
    fence_id: [u8; 16],
    endpoint_id: [u8; 32],
    lease_until: (i64, u32),
) -> ConnectionFenceV2 {
    let issued_at = command.issued_at.expect("command issuance").seconds;
    let expires_at = command.expires_at.expect("command expiry").seconds;
    let claims = ConnectionFenceClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&key.verifying_key()),
        fence_id,
        node_id: command.node_id.as_slice().try_into().expect("node ID"),
        endpoint_id,
        owner_instance_id: [2; 16],
        owner_incarnation: 1,
        owner_epoch: 1,
        connection_id: [3; 16],
        authorization_revision: command.expected_revision,
        capabilities: vec![command.required_capability.clone()],
        lease_until_seconds: lease_until.0,
        lease_until_nanos: lease_until.1,
        issued_at_seconds: issued_at - 1,
        issued_at_nanos: 0,
        expires_at_seconds: expires_at.max(lease_until.0 + 1),
        expires_at_nanos: 0,
    };
    let signature = key.sign(&canonical_connection_fence_v2(&claims).expect("canonical fence"));
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
            nanos: 0,
        }),
        signature: signature.to_bytes().to_vec(),
    }
}

fn signed_binding(
    key: &SigningKey,
    command: &CommandEnvelope,
    fence: &ConnectionFenceV2,
    operation_kind: FenceOperationKind,
    operation_id: &[u8],
    capability: &str,
) -> FenceBindingV2 {
    let claims = FenceBindingClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: verification_key_id(&key.verifying_key()),
        operation_kind: operation_kind as u32,
        operation_id: operation_id.try_into().expect("operation ID"),
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
            .expect("owner"),
        owner_incarnation: fence.owner_incarnation,
        owner_epoch: fence.owner_epoch,
        connection_id: fence
            .connection_id
            .as_slice()
            .try_into()
            .expect("connection"),
        authorization_revision: fence.authorization_revision,
        capability: capability.to_owned(),
        issued_at_seconds: command.issued_at.expect("command issuance").seconds,
        issued_at_nanos: 0,
        expires_at_seconds: fence.expires_at.expect("fence expiry").seconds,
        expires_at_nanos: 0,
    };
    let signature = key.sign(&canonical_fence_binding_v2(&claims).expect("canonical binding"));
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
            nanos: 0,
        }),
        signature: signature.to_bytes().to_vec(),
    }
}

fn command_request(command: CommandEnvelope) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline(),
        accepted_at: command.issued_at,
        authorization_command: Some(command),
        privileged_mode: PrivilegedRequestMode::Execute.into(),
        operation: None,
    }
}

fn reconcile_request(mut command: CommandEnvelope, key: &SigningKey) -> PrivdRequest {
    command.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    command.message_id = Uuid::now_v7().as_bytes().to_vec();
    let claims = claims_from_envelope_v1(&command).expect("reconciliation claims");
    command
        .authorization
        .as_mut()
        .expect("reconciliation authorization")
        .signature = key
        .sign(&canonical_v1(&claims).expect("reconciliation canonical bytes"))
        .to_bytes()
        .to_vec();
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline(),
        accepted_at: None,
        authorization_command: Some(command),
        privileged_mode: PrivilegedRequestMode::Reconcile.into(),
        operation: None,
    }
}

fn retry_request(mut command: CommandEnvelope, key: &SigningKey) -> PrivdRequest {
    command.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    command.message_id = Uuid::now_v7().as_bytes().to_vec();
    let claims = claims_from_envelope_v1(&command).expect("retry claims");
    command
        .authorization
        .as_mut()
        .expect("retry authorization")
        .signature = key
        .sign(&canonical_v1(&claims).expect("retry canonical bytes"))
        .to_bytes()
        .to_vec();
    command_request(command)
}

fn unsigned_request(operation: Option<privd_request::Operation>) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline(),
        accepted_at: None,
        authorization_command: None,
        privileged_mode: PrivilegedRequestMode::Unspecified.into(),
        operation,
    }
}

fn test_adapter() -> (Adapter, FixedResources, PathBuf, PathBuf) {
    let directory = PathBuf::from("/tmp").join(format!("ocp-{}", Uuid::now_v7().simple()));
    std::fs::create_dir(&directory).expect("create test directory");
    std::fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
        .expect("secure test directory");
    let counter = directory.join("reload-count");
    let systemctl = directory.join("systemctl");
    std::fs::write(
        &systemctl,
        format!("#!/bin/sh\nprintf x >> '{}'\n", counter.display()),
    )
    .expect("write fixed test command");
    std::fs::set_permissions(&systemctl, fs::Permissions::from_mode(0o700))
        .expect("make fixed test command executable");
    let config = directory.join("ocserv.conf");
    let boot = directory.join("boot_id");
    std::fs::write(&config, b"fixture\n").expect("write config fixture");
    std::fs::write(&boot, Uuid::now_v7().to_string()).expect("write boot fixture");
    let resources = FixedResources::new(
        systemctl,
        PathBuf::from("/bin/true"),
        PathBuf::from("/bin/true"),
        config,
        boot,
    )
    .expect("fixed resources")
    .with_effect_store(
        directory.join("effects.sqlite3"),
        directory.join("effects.key"),
    )
    .expect("effect resources");
    (
        Adapter::new(resources.clone(), Limits::default()),
        resources,
        counter,
        directory,
    )
}

fn assert_permission_denied(response: &PrivdResponse) {
    let Some(privd_response::Result::Error(failure)) = response.result.as_ref() else {
        panic!("privileged request must be rejected")
    };
    assert_eq!(
        ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified),
        ErrorKind::PermissionDenied
    );
}

#[tokio::test]
async fn rejects_expired_and_missing_operations() {
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let expired = PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: 1,
        accepted_at: None,
        authorization_command: None,
        privileged_mode: PrivilegedRequestMode::Unspecified.into(),
        operation: Some(privd_request::Operation::ServiceStatus(ReadRequest {})),
    };
    let response = dispatch(expired, &node_id, &keys, &adapter).await;
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(_))
    ));

    let missing = PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline(),
        accepted_at: None,
        authorization_command: None,
        privileged_mode: PrivilegedRequestMode::Unspecified.into(),
        operation: None,
    };
    let response = dispatch(missing, &node_id, &keys, &adapter).await;
    assert_permission_denied(&response);
}

#[tokio::test]
async fn rebound_authority_denies_old_node_key_and_quarantines_uncertain_effects() {
    let old_signing = SigningKey::from_bytes(&[8; 32]);
    let new_signing = SigningKey::from_bytes(&[9; 32]);
    let keys = keyring(&new_signing);
    let old_node = *Uuid::now_v7().as_bytes();
    let new_node = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let (adapter, resources, counter, directory) = test_adapter();
    for (signing, node) in [
        (&old_signing, old_node),
        (&old_signing, new_node),
        (&new_signing, old_node),
    ] {
        let command = signed_service_reload(signing, node, now, now + 60);
        assert_permission_denied(
            &dispatch(command_request(command), &new_node, &keys, &adapter).await,
        );
        assert!(!counter.exists());
    }
    let command = signed_service_reload(&new_signing, new_node, now, now + 60);
    let quarantined = Adapter::new(resources.with_mutations_blocked(true), Limits::default());
    let denied = dispatch(
        command_request(command.clone()),
        &new_node,
        &keys,
        &quarantined,
    )
    .await;
    assert!(matches!(
        denied.result,
        Some(privd_response::Result::Error(_))
    ));
    assert!(!counter.exists());
    let accepted = dispatch(command_request(command), &new_node, &keys, &adapter).await;
    assert!(
        matches!(accepted.result, Some(privd_response::Result::Mutation(ref result)) if result.applied)
    );
    assert_eq!(std::fs::read(counter).unwrap(), b"x");
    std::fs::remove_dir_all(directory).unwrap();
}

#[tokio::test]
async fn signed_effect_executes_once_and_replays_after_restart() {
    let signing = SigningKey::from_bytes(&[8; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let (adapter, resources, counter, directory) = test_adapter();

    let first = dispatch(command_request(command.clone()), &node_id, &keys, &adapter).await;
    assert!(matches!(
        first.result,
        Some(privd_response::Result::Mutation(ref result)) if result.applied
    ));
    let proof = first
        .privileged_result_proof
        .clone()
        .expect("successful root effect must be attested");
    ocservia_privd_attestation::verify_receipt(
        &proof,
        &SigningKey::from_bytes(&[41; 32]).verifying_key(),
    )
    .expect("valid root receipt");
    drop(adapter);
    let restarted = Adapter::new(resources, Limits::default());
    let replay = dispatch(
        reconcile_request(command.clone(), &signing),
        &node_id,
        &keys,
        &restarted,
    )
    .await;
    assert!(matches!(
        replay.result,
        Some(privd_response::Result::Mutation(ref result)) if result.applied
    ));
    assert_eq!(replay.privileged_result_proof, Some(proof));
    assert_eq!(replay.result, first.result);
    let duplicate = dispatch(command_request(command), &node_id, &keys, &restarted).await;
    assert_eq!(
        duplicate.privileged_result_proof,
        replay.privileged_result_proof
    );
    assert_eq!(duplicate.result, replay.result);
    assert_eq!(
        std::fs::read_to_string(counter).expect("effect counter"),
        "x"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

fn assert_requires_reconciliation(response: &PrivdResponse) {
    assert!(response.privileged_result_proof.is_none());
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(ref error))
            if ErrorKind::try_from(error.kind).ok() == Some(ErrorKind::Unavailable)
    ));
}

#[tokio::test]
async fn expired_commands_only_observe_and_legacy_retries_only_replay() {
    let signing = SigningKey::from_bytes(&[45; 32]);
    let keys = keyring(&signing);
    let node = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let expired = signed_service_reload(&signing, node, now - 120, now - 60);
    assert!(
        validate_request(
            &reconcile_request(expired.clone(), &signing),
            &node,
            &TEST_ENDPOINT_ID,
            &keys
        )
        .is_ok()
    );
    assert!(
        validate_request(
            &retry_request(expired.clone(), &signing),
            &node,
            &TEST_ENDPOINT_ID,
            &keys
        )
        .is_err()
    );
    assert!(validate_request(&command_request(expired), &node, &TEST_ENDPOINT_ID, &keys).is_err());

    let (adapter, _, counter, directory) = test_adapter();
    let control = signed_service_reload(&signing, node, now, now + 60);
    let response = dispatch(command_request(control), &node, &keys, &adapter).await;
    assert!(response.privileged_result_proof.is_some());
    let mut legacy = signed_command(
        &signing,
        node,
        now,
        now + 60,
        "user.create",
        "ocserv.users.write",
        command_envelope::Payload::UserCreate(UserCreate {
            username: "alice".to_owned(),
            desired_revision: 1,
            sealed_password_v1: Some(SealedSecretV1 {
                version: SealedSecretVersion::V1.into(),
                purpose: SealedSecretPurpose::UserPassword.into(),
                key_id: "user-key-v1".to_owned(),
                ciphertext: vec![0xa5; 256],
            }),
            ..UserCreate::default()
        }),
    );
    legacy.protocol_version =
        ocservia_command_authorization::LEGACY_COMMAND_PROTOCOL_VERSION.to_owned();
    let result = dispatch(retry_request(legacy, &signing), &node, &keys, &adapter).await;
    assert_requires_reconciliation(&result);
    assert_eq!(
        std::fs::read_to_string(counter).expect("control effect"),
        "x"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[test]
fn only_fixed_host_effects_lose_the_retry_grant() {
    let ip = command_envelope::Payload::IpBanRemove(
        ocservia_contracts::generated::ocserv::platform::agent::v1::IpBanRemove {
            ip: "192.0.2.1".to_owned(),
        },
    );
    for payload in [
        command_envelope::Payload::ServiceReload(ServiceReload {}),
        ip,
        command_envelope::Payload::SessionDisconnect(
            ocservia_contracts::generated::ocserv::platform::agent::v1::SessionDisconnect {
                session_id: "session".to_owned(),
                boot_id: "boot".to_owned(),
            },
        ),
        command_envelope::Payload::SessionTerminate(
            ocservia_contracts::generated::ocserv::platform::agent::v1::SessionTerminate {
                session_id: "session".to_owned(),
                boot_id: "boot".to_owned(),
            },
        ),
    ] {
        let command = CommandEnvelope {
            payload: Some(payload),
            ..CommandEnvelope::default()
        };
        assert!(fixed_host_effect(&command));
    }
    let command = CommandEnvelope {
        payload: Some(command_envelope::Payload::AgentUpgrade(
            AgentUpgrade::default(),
        )),
        ..CommandEnvelope::default()
    };
    assert!(!fixed_host_effect(&command));
}

#[tokio::test]
async fn retry_grant_never_reexecutes_fixed_host_effect_without_a_result() {
    for history in [
        "original execution still pending",
        "completed record pruned",
    ] {
        let signing = SigningKey::from_bytes(&[43; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_service_reload(&signing, node_id, now, now + 60);
        let (adapter, _resources, counter, directory) = test_adapter();
        if history == "original execution still pending" {
            let claims = claims_from_envelope_v1(&command).expect("claims");
            let binding = effect_binding(&command, &claims).expect("binding");
            assert_eq!(
                adapter
                    .prepare_authorized_effect(authorized_effect(&command, &claims, &binding))
                    .expect("original reservation"),
                AuthorizedEffectDecision::Execute
            );
        }
        for _ in 0..2 {
            let response = dispatch(
                retry_request(command.clone(), &signing),
                &node_id,
                &keys,
                &adapter,
            )
            .await;
            assert_requires_reconciliation(&response);
            assert!(!counter.exists(), "{history}: retry reached the executor");
        }
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }
}

#[tokio::test]
async fn retry_grant_replays_an_existing_signed_result_without_reexecution() {
    let signing = SigningKey::from_bytes(&[44; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let (adapter, _resources, counter, directory) = test_adapter();
    let first = dispatch(command_request(command.clone()), &node_id, &keys, &adapter).await;
    assert!(first.privileged_result_proof.is_some());
    let retried = dispatch(retry_request(command, &signing), &node_id, &keys, &adapter).await;
    assert_eq!(
        retried.privileged_result_proof,
        first.privileged_result_proof
    );
    assert_eq!(retried.result, first.result);
    assert_eq!(
        std::fs::read_to_string(counter).expect("effect counter"),
        "x"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn receipt_persistence_failure_cannot_return_privileged_success() {
    let signing = SigningKey::from_bytes(&[42; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let initializer = signed_service_reload(&signing, node_id, now, now + 60);
    let claims = claims_from_envelope_v1(&initializer).expect("initializer claims");
    let binding = effect_binding(&initializer, &claims).expect("initializer effect binding");
    let (adapter, _resources, counter, directory) = test_adapter();
    assert_eq!(
        adapter
            .prepare_authorized_effect(authorized_effect(&initializer, &claims, &binding))
            .expect("initialize root effect store"),
        AuthorizedEffectDecision::Execute
    );
    {
        let connection = rusqlite::Connection::open(directory.join("effects.sqlite3"))
            .expect("open root effect store for fault injection");
        connection
                .execute_batch(
                    "CREATE TRIGGER fail_attested_response BEFORE UPDATE OF response ON authorized_effects BEGIN SELECT RAISE(ABORT, 'injected receipt persistence failure'); END;",
                )
                .expect("install completion fault");
    }

    let response = dispatch(command_request(command), &node_id, &keys, &adapter).await;
    assert!(response.privileged_result_proof.is_none());
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(ref error))
            if ErrorKind::try_from(error.kind).ok() == Some(ErrorKind::Unavailable)
    ));
    assert_eq!(
        std::fs::read_to_string(counter).expect("effect counter"),
        "x",
        "the completed side effect must remain Unknown rather than be repeated"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn signed_claim_or_effect_tampering_is_rejected_before_root_effect() {
    let signing = SigningKey::from_bytes(&[9; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let (adapter, _, counter, directory) = test_adapter();
    let mut variants = Vec::new();

    let mut changed = command.clone();
    changed.node_id = Uuid::now_v7().as_bytes().to_vec();
    variants.push(changed);
    let mut changed = command.clone();
    changed.expected_revision += 1;
    variants.push(changed);
    let mut changed = command.clone();
    changed.command_id = Uuid::now_v7().as_bytes().to_vec();
    variants.push(changed);
    let mut changed = command.clone();
    changed.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
    variants.push(changed);
    let mut changed = command.clone();
    changed.required_capability = "ocserv.config.apply".to_owned();
    variants.push(changed);
    let mut changed = command.clone();
    changed.semantic_payload_sha256[0] ^= 1;
    variants.push(changed);
    let mut changed = command.clone();
    changed.operation_id = Uuid::now_v7().as_bytes().to_vec();
    variants.push(changed);
    let mut changed = command.clone();
    changed.actor_id = "forged-controller".to_owned();
    variants.push(changed);
    let mut changed = command.clone();
    changed.payload = Some(command_envelope::Payload::IpBanRemove(
        ocservia_contracts::generated::ocserv::platform::agent::v1::IpBanRemove {
            ip: "192.0.2.1".to_owned(),
        },
    ));
    variants.push(changed);
    let mut changed = command.clone();
    changed
        .authorization
        .as_mut()
        .expect("authorization")
        .signature[0] ^= 1;
    variants.push(changed);

    for changed in variants {
        let response = dispatch(command_request(changed), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);
    }
    let mut agent_selected_effect = command_request(command);
    agent_selected_effect.operation = Some(privd_request::Operation::ServiceStatus(ReadRequest {}));
    let response = dispatch(agent_selected_effect, &node_id, &keys, &adapter).await;
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(ref failure))
            if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                == ErrorKind::InvalidRequest
    ));
    assert!(!counter.exists());
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

fn upgrade_scheduler(directory: &Path) -> (UpgradeScheduler, PathBuf) {
    let operations_dir = directory.join("upgrade-operations");
    fs::create_dir(&operations_dir).expect("create upgrade operations root");
    fs::set_permissions(&operations_dir, fs::Permissions::from_mode(0o700))
        .expect("secure upgrade operations root");
    (
        UpgradeScheduler::new(operations_dir.clone(), UpgradeTrigger::Disabled),
        operations_dir,
    )
}

fn durable_operation_state(operations_dir: &Path, operation_id: &[u8]) -> String {
    let id = Uuid::from_slice(operation_id).expect("operation UUID");
    let state = fs::read_to_string(operations_dir.join(id.to_string()).join("state"))
        .expect("durable operation state");
    state.trim_end_matches('\n').to_owned()
}

#[tokio::test]
async fn agent_upgrade_prepare_attests_scheduled_intent_exactly_once() {
    let signing = SigningKey::from_bytes(&[29; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
    );
    let (adapter, resources, _, directory) = test_adapter();
    let (upgrades, operations_dir) = upgrade_scheduler(&directory);

    let first = dispatch_upgrade(
        command_request(command.clone()),
        &node_id,
        &keys,
        &adapter,
        &upgrades,
    )
    .await;
    let Some(privd_response::Result::AgentUpgradeScheduled(ref scheduled)) = first.result else {
        panic!("upgrade preparation must return the scheduled intent");
    };
    assert_eq!(scheduled.target_version, "1.2.3");
    assert_eq!(scheduled.package_sha256, vec![0x43; 32]);
    assert_eq!(scheduled.operation_id, command.operation_id);
    // The durable root-owned intent outlives both privd and the Agent.
    assert_eq!(
        durable_operation_state(&operations_dir, &command.operation_id),
        "accepted"
    );
    assert!(
        operations_dir
            .join(
                Uuid::from_slice(&command.operation_id)
                    .expect("operation UUID")
                    .to_string()
            )
            .join("intent")
            .exists()
    );
    let proof = first
        .privileged_result_proof
        .clone()
        .expect("scheduled intent must be root-attested");
    ocservia_privd_attestation::verify_receipt(
        &proof,
        &SigningKey::from_bytes(&[41; 32]).verifying_key(),
    )
    .expect("valid scheduled-intent receipt");
    let receipt = proof.receipt_v1.as_ref().expect("receipt");
    assert_eq!(
        PrivilegedCommandKind::try_from(receipt.command_kind),
        Ok(PrivilegedCommandKind::AgentUpgrade)
    );
    assert_eq!(
        PrivilegedResultKind::try_from(receipt.result_kind),
        Ok(PrivilegedResultKind::AgentUpgradeScheduled)
    );
    assert_eq!(
        CommandResultState::try_from(receipt.terminal_state),
        Ok(CommandResultState::Succeeded)
    );

    let duplicate = dispatch_upgrade(
        command_request(command.clone()),
        &node_id,
        &keys,
        &adapter,
        &upgrades,
    )
    .await;
    assert_eq!(duplicate.privileged_result_proof, Some(proof.clone()));
    assert_eq!(duplicate.result, first.result);
    assert_eq!(
        durable_operation_state(&operations_dir, &command.operation_id),
        "accepted"
    );
    drop(adapter);
    let restarted = Adapter::new(resources, Limits::default());
    let replay = dispatch_upgrade(
        reconcile_request(command, &signing),
        &node_id,
        &keys,
        &restarted,
        &upgrades,
    )
    .await;
    assert_eq!(replay.privileged_result_proof, Some(proof));
    assert_eq!(replay.result, first.result);
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn agent_upgrade_pending_journal_replays_while_runner_holds_execution_lock() {
    use std::fs::OpenOptions;
    use std::os::unix::fs::OpenOptionsExt as _;

    let signing = SigningKey::from_bytes(&[33; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x61; 32],
        host_architecture(),
    );
    let claims = claims_from_envelope_v1(&command).expect("upgrade claims");
    let binding = effect_binding(&command, &claims).expect("upgrade effect binding");
    let (adapter, _, _, directory) = test_adapter();
    assert_eq!(
        adapter
            .prepare_authorized_effect(authorized_effect(&command, &claims, &binding))
            .expect("prepare pending root effect"),
        AuthorizedEffectDecision::Execute
    );
    let (upgrades, operations_dir) = upgrade_scheduler(&directory);
    let Some(command_envelope::Payload::AgentUpgrade(payload)) = command.payload.as_ref() else {
        panic!("upgrade payload");
    };
    let intent = UpgradeIntent::new(
        claims.operation_id,
        claims.command_id,
        &payload.target_version,
        payload
            .package_sha256
            .clone()
            .try_into()
            .expect("package digest"),
        &payload.architecture,
        claims.semantic_payload_sha256,
    )
    .expect("upgrade intent");
    assert_eq!(
        upgrades
            .schedule_and_trigger(&intent)
            .expect("commit durable intent"),
        ScheduleOutcome::Scheduled
    );
    let durable_path = operations_dir.join(intent.operation_id.to_string());
    fs::write(durable_path.join("state"), b"running\n").expect("runner state");
    let execution_lock = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(operations_dir.join(".run.lock"))
        .expect("execution lock file");
    rustix::fs::flock(
        &execution_lock,
        rustix::fs::FlockOperation::NonBlockingLockExclusive,
    )
    .expect("hold runner execution lock");

    let replay = dispatch_upgrade(
        command_request(command.clone()),
        &node_id,
        &keys,
        &adapter,
        &upgrades,
    )
    .await;
    assert!(matches!(
        replay.result,
        Some(privd_response::Result::AgentUpgradeScheduled(_))
    ));
    let receipt = replay
        .privileged_result_proof
        .as_ref()
        .and_then(|proof| proof.receipt_v1.as_ref())
        .expect("successful replay receipt");
    assert_eq!(
        CommandResultState::try_from(receipt.terminal_state),
        Ok(CommandResultState::Succeeded)
    );
    assert_eq!(
        durable_operation_state(&operations_dir, &command.operation_id),
        "running"
    );
    drop(execution_lock);
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_duplicate_agent_upgrades_both_attest_the_schedule() {
    let signing = SigningKey::from_bytes(&[34; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let command = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x71; 32],
        host_architecture(),
    );
    let operation_id = command.operation_id.clone();
    let (adapter, _, _, directory) = test_adapter();
    let (upgrades, operations_dir) = upgrade_scheduler(&directory);

    // Two identical deliveries race through the Pending journal and the
    // durable schedule lock. The loser must observe the committed intent
    // as an exact replay - never a terminal privd_rejected receipt.
    let (first, second) = tokio::join!(
        dispatch_upgrade(
            command_request(command.clone()),
            &node_id,
            &keys,
            &adapter,
            &upgrades
        ),
        dispatch_upgrade(
            command_request(command),
            &node_id,
            &keys,
            &adapter,
            &upgrades
        )
    );
    for response in [&first, &second] {
        assert!(
            matches!(
                response.result,
                Some(privd_response::Result::AgentUpgradeScheduled(_))
            ),
            "concurrent duplicate must replay the scheduled intent, got {:?}",
            response.result
        );
        let receipt = response
            .privileged_result_proof
            .as_ref()
            .and_then(|proof| proof.receipt_v1.as_ref())
            .expect("successful scheduled-intent receipt");
        assert_eq!(
            CommandResultState::try_from(receipt.terminal_state),
            Ok(CommandResultState::Succeeded)
        );
    }
    let durable = fs::read_dir(&operations_dir)
        .expect("operations root")
        .filter_map(Result::ok)
        .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
        .count();
    assert_eq!(durable, 1);
    assert_eq!(
        durable_operation_state(&operations_dir, &operation_id),
        "accepted"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn agent_upgrade_prepare_refuses_a_second_active_operation() {
    let signing = SigningKey::from_bytes(&[32; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let first_command = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x51; 32],
        host_architecture(),
    );
    let second_command = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.4",
        vec![0x52; 32],
        host_architecture(),
    );
    let (adapter, _, _, directory) = test_adapter();
    let (upgrades, operations_dir) = upgrade_scheduler(&directory);

    let first = dispatch_upgrade(
        command_request(first_command),
        &node_id,
        &keys,
        &adapter,
        &upgrades,
    )
    .await;
    assert!(matches!(
        first.result,
        Some(privd_response::Result::AgentUpgradeScheduled(_))
    ));
    let second = dispatch_upgrade(
        command_request(second_command),
        &node_id,
        &keys,
        &adapter,
        &upgrades,
    )
    .await;
    assert!(matches!(
        second.result,
        Some(privd_response::Result::Error(ref failure))
            if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                == ErrorKind::InvalidRequest
    ));
    // The refused command still receives a terminal failure receipt.
    if let Some(proof) = second.privileged_result_proof.as_ref() {
        assert_eq!(
            CommandResultState::try_from(
                proof.receipt_v1.as_ref().expect("receipt").terminal_state
            ),
            Ok(CommandResultState::Failed)
        );
    }
    let durable: Vec<_> = std::fs::read_dir(&operations_dir)
        .expect("list durable operations")
        .filter_map(Result::ok)
        .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
        .collect();
    assert_eq!(durable.len(), 1, "only the first operation may be durable");
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn agent_upgrade_prepare_rejects_unverified_release_identities() {
    let signing = SigningKey::from_bytes(&[30; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());

    // A foreign-but-well-formed release identity passes the semantic hash
    // and Controller signature, so it must be refused by privd's own
    // host-architecture validation, not by an earlier generic failure.
    let foreign = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        foreign_architecture(),
    );
    let response = dispatch(command_request(foreign), &node_id, &keys, &adapter).await;
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(ref failure))
            if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                == ErrorKind::InvalidRequest
    ));

    // Unknown controller keys never reach the release identity checks.
    let unknown_key = SigningKey::from_bytes(&[31; 32]);
    let forged = signed_agent_upgrade(
        &unknown_key,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
    );
    let response = dispatch(command_request(forged), &node_id, &keys, &adapter).await;
    assert_permission_denied(&response);

    // Unsigned upgrade intents have no fixed local operation to ride on.
    let response = dispatch(unsigned_request(None), &node_id, &keys, &adapter).await;
    assert_permission_denied(&response);
}

#[tokio::test]
async fn cross_purpose_password_envelopes_fail_before_root_effect() {
    let signing = SigningKey::from_bytes(&[19; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let user_secret = SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::UserPassword as i32,
        key_id: "user-key-v1".to_owned(),
        ciphertext: vec![0xa5; 256],
    };
    let mut user_command = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "user.create",
        "ocserv.users.write",
        command_envelope::Payload::UserCreate(UserCreate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 1,
            sealed_password_v1: Some(user_secret),
        }),
    );
    let Some(command_envelope::Payload::UserCreate(payload)) = user_command.payload.as_mut() else {
        unreachable!()
    };
    payload
        .sealed_password_v1
        .as_mut()
        .expect("typed user secret")
        .purpose = SealedSecretPurpose::CertificateP12Password as i32;
    let response = dispatch(command_request(user_command), &node_id, &keys, &adapter).await;
    assert_permission_denied(&response);

    let p12_secret = SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::CertificateP12Password as i32,
        key_id: "p12-key-v1".to_owned(),
        ciphertext: vec![0x5a; 256],
    };
    let mut p12_command = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "certificate.private_key.export",
        "ocserv.certificate.issue",
        command_envelope::Payload::CertificateP12(CertificateP12 {
            certificate_id: Uuid::now_v7().as_bytes().to_vec(),
            certificate_chain_pem: vec![b'A'; 64],
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            artifact_id: Uuid::now_v7().as_bytes().to_vec(),
            sealed_password_v1: Some(p12_secret),
            certificate_version: 1,
            artifact_expires_at: Some(Timestamp {
                seconds: now + 60,
                nanos: 0,
            }),
        }),
    );
    let Some(command_envelope::Payload::CertificateP12(payload)) = p12_command.payload.as_mut()
    else {
        unreachable!()
    };
    payload
        .sealed_password_v1
        .as_mut()
        .expect("typed P12 secret")
        .purpose = SealedSecretPurpose::UserPassword as i32;
    let response = dispatch(command_request(p12_command), &node_id, &keys, &adapter).await;
    assert_permission_denied(&response);
}

#[tokio::test]
async fn signed_config_hash_cannot_authorize_substituted_candidate_bytes() {
    use complete_config_directive::Value::{Literal, Tls};

    let signing = SigningKey::from_bytes(&[14; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let tls = ocservia_contracts::generated::ocserv::platform::agent::v1::NodeLocalTlsReference {
        secret_ref_id: vec![0x12; 16],
        version: "v1".to_owned(),
        certificate_sha256: vec![0x23; 32],
        spki_sha256: vec![0x34; 32],
        ca_sha256: Vec::new(),
    };
    let directive = |name: &str, value| CompleteConfigDirective {
        name: name.to_owned(),
        value: Some(value),
    };
    let candidate = CompleteConfigCandidate {
        node_id: node_id.to_vec(),
        expected_revision: 1,
        directives: vec![
            directive(
                "auth",
                Literal("plain[passwd=/etc/ocserv/ocpasswd]".to_owned()),
            ),
            directive("cookie-timeout", Literal("300".to_owned())),
            directive("device", Literal("vpns".to_owned())),
            directive("dns", Literal("1.1.1.1".to_owned())),
            directive("ipv4-network", Literal("192.0.2.0/24".to_owned())),
            directive("max-clients", Literal("64".to_owned())),
            directive("max-same-clients", Literal("2".to_owned())),
            directive("server-cert", Tls(tls.clone())),
            directive("server-key", Tls(tls)),
            directive("socket-file", Literal("/run/ocserv.socket".to_owned())),
            directive("tcp-port", Literal("443".to_owned())),
            directive("udp-port", Literal("0".to_owned())),
        ],
    };
    let candidate_hash =
        Sha256::digest(config_profile::canonical(&candidate).expect("canonical complete config"))
            .to_vec();
    let mut command = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "config.apply",
        config_profile::APPLY_CAPABILITY,
        command_envelope::Payload::CompleteConfigApply(CompleteConfigApply {
            candidate: Some(candidate),
            candidate_hash,
            materialized_hash: vec![0x33; 32],
            expected_current_hash: vec![0x44; 32],
            desired_revision: 2,
            plan_id: Uuid::now_v7().as_bytes().to_vec(),
            plan_expires_at: Some(Timestamp {
                seconds: now + 60,
                nanos: 0,
            }),
        }),
    );
    let Some(command_envelope::Payload::CompleteConfigApply(payload)) = command.payload.as_mut()
    else {
        panic!("config payload")
    };
    payload.candidate.as_mut().expect("candidate").directives[10].value =
        Some(complete_config_directive::Value::Literal("8443".to_owned()));
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let response = dispatch(command_request(command), &node_id, &keys, &adapter).await;
    assert!(matches!(
        response.result,
        Some(privd_response::Result::Error(ref failure))
            if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                == ErrorKind::PermissionDenied
    ));
}

#[tokio::test]
async fn expired_and_unknown_controller_proofs_are_rejected() {
    let trusted = SigningKey::from_bytes(&[10; 32]);
    let unknown = SigningKey::from_bytes(&[11; 32]);
    let keys = keyring(&trusted);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let expired = signed_service_reload(&trusted, node_id, now - 60, now - 1);
    assert_permission_denied(&dispatch(command_request(expired), &node_id, &keys, &adapter).await);
    let another_node = signed_service_reload(&trusted, *Uuid::now_v7().as_bytes(), now, now + 60);
    assert_permission_denied(
        &dispatch(command_request(another_node), &node_id, &keys, &adapter).await,
    );
    let unknown = signed_service_reload(&unknown, node_id, now, now + 60);
    assert_permission_denied(&dispatch(command_request(unknown), &node_id, &keys, &adapter).await);
}

#[tokio::test]
async fn root_execution_requires_the_commands_own_unexpired_fence() {
    let signing = SigningKey::from_bytes(&[12; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let (adapter, _, counter, directory) = test_adapter();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let capability = command.required_capability.clone();
    let carried = |fence: Option<ConnectionFenceV2>, binding: Option<FenceBindingV2>| {
        let mut changed = command.clone();
        changed.connection_fence = fence;
        changed.fence_binding = binding;
        command_request(changed)
    };
    let bound = |fence: ConnectionFenceV2, operation_id: Vec<u8>, capability: &str| {
        let binding = signed_binding(
            &signing,
            &command,
            &fence,
            FenceOperationKind::Command,
            &operation_id,
            capability,
        );
        carried(Some(fence), Some(binding))
    };
    let current = || signed_fence(&signing, &command, [1; 16], TEST_ENDPOINT_ID, (now + 60, 0));
    let mut newer_revision = command.clone();
    newer_revision.expected_revision += 1;
    let other_command = signed_service_reload(&signing, node_id, now, now + 60);
    let mixed_binding = signed_binding(
        &signing,
        &command,
        &signed_fence(&signing, &command, [4; 16], TEST_ENDPOINT_ID, (now + 60, 0)),
        FenceOperationKind::Command,
        &command.command_id,
        &capability,
    );
    let rejected = [
        carried(None, None),
        carried(command.connection_fence.clone(), None),
        // The proof is still valid; only the signed owner lease ended.
        bound(
            signed_fence(&signing, &command, [1; 16], TEST_ENDPOINT_ID, (now, 0)),
            command.command_id.clone(),
            &capability,
        ),
        bound(
            signed_fence(&signing, &command, [1; 16], [8; 32], (now + 60, 0)),
            command.command_id.clone(),
            &capability,
        ),
        bound(current(), other_command.command_id.clone(), &capability),
        bound(current(), command.command_id.clone(), "ocserv.users.write"),
        bound(
            signed_fence(
                &signing,
                &newer_revision,
                [1; 16],
                TEST_ENDPOINT_ID,
                (now + 60, 0),
            ),
            command.command_id.clone(),
            &capability,
        ),
        carried(Some(current()), Some(mixed_binding)),
    ];
    for request in rejected {
        assert_permission_denied(&dispatch(request, &node_id, &keys, &adapter).await);
    }
    assert!(
        !counter.exists(),
        "a refused fence must not reach the root effect"
    );
    let accepted = dispatch(command_request(command), &node_id, &keys, &adapter).await;
    assert!(matches!(
        accepted.result,
        Some(privd_response::Result::Mutation(ref result)) if result.applied
    ));
    assert_eq!(
        std::fs::read_to_string(counter).expect("effect counter"),
        "x"
    );
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn artifact_read_and_consume_require_their_own_unexpired_fence() {
    let signing = SigningKey::from_bytes(&[12; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let (adapter, _, _, directory) = test_adapter();
    let claims = ArtifactGrantClaimsV1 {
        version: 1,
        key_id: verification_key_id(&signing.verifying_key()),
        node_id,
        artifact_id: *Uuid::now_v7().as_bytes(),
        certificate_id: *Uuid::now_v7().as_bytes(),
        certificate_version: 1,
        operation_id: *Uuid::now_v7().as_bytes(),
        authorized_subject: "user:test".to_owned(),
        purpose: "certificate_p12".to_owned(),
        max_bytes: 4096,
        issued_at_seconds: now - 1,
        issued_at_nanos: 0,
        expires_at_seconds: now + 60,
        expires_at_nanos: 0,
        grant_id: *Uuid::now_v7().as_bytes(),
    };
    let signature = signing.sign(&canonical_artifact_grant_v1(&claims).expect("canonical"));
    let grant = ArtifactGrantV1 {
        version: ArtifactGrantVersion::V1.into(),
        key_id: claims.key_id.clone(),
        node_id: node_id.to_vec(),
        artifact_id: claims.artifact_id.to_vec(),
        certificate_id: claims.certificate_id.to_vec(),
        certificate_version: claims.certificate_version,
        operation_id: claims.operation_id.to_vec(),
        authorized_subject: claims.authorized_subject.clone(),
        purpose: claims.purpose.clone(),
        max_bytes: claims.max_bytes,
        issued_at: Some(Timestamp {
            seconds: claims.issued_at_seconds,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: claims.expires_at_seconds,
            nanos: 0,
        }),
        grant_id: claims.grant_id.to_vec(),
        signature: signature.to_bytes().to_vec(),
    };
    // Only the fence-signing fields of this carrier matter.
    let mut carrier = signed_service_reload(&signing, node_id, now, now + 60);
    carrier.required_capability = CONNECTION_FENCING_CAPABILITY.to_owned();
    let fence =
        |lease_until| signed_fence(&signing, &carrier, [1; 16], TEST_ENDPOINT_ID, lease_until);
    let bound = |fence: ConnectionFenceV2, operation_id: &[u8]| {
        let binding = signed_binding(
            &signing,
            &carrier,
            &fence,
            FenceOperationKind::Artifact,
            operation_id,
            CONNECTION_FENCING_CAPABILITY,
        );
        (Some(Box::new(fence)), Some(Box::new(binding)))
    };
    let read = |(connection_fence, fence_binding)| {
        unsigned_request(Some(privd_request::Operation::ArtifactRead(
            ArtifactReadRequest {
                grant: Some(grant.clone()),
                offset: 0,
                connection_fence,
                fence_binding,
            },
        )))
    };
    let consume = |(connection_fence, fence_binding)| {
        unsigned_request(Some(privd_request::Operation::ArtifactConsume(
            ArtifactConsumeRequest {
                grant: Some(grant.clone()),
                sha256: vec![1; 32],
                size: claims.max_bytes,
                confirm_only: false,
                connection_fence,
                fence_binding,
            },
        )))
    };
    let rejected = [
        read((None, None)),
        consume((None, None)),
        read(bound(fence((now, 0)), &claims.artifact_id)),
        // Fetch binds the artifact ID and consumption binds the grant ID.
        read(bound(fence((now + 60, 0)), &claims.grant_id)),
        consume(bound(fence((now + 60, 0)), &claims.artifact_id)),
    ];
    for request in rejected {
        assert_permission_denied(&dispatch(request, &node_id, &keys, &adapter).await);
    }
    // The same grant with its own live fence passes authorization and only
    // then meets the (absent) artifact lease.
    for request in [
        read(bound(fence((now + 60, 0)), &claims.artifact_id)),
        consume(bound(fence((now + 60, 0)), &claims.grant_id)),
    ] {
        let response = dispatch(request, &node_id, &keys, &adapter).await;
        let Some(privd_response::Result::Error(failure)) = response.result else {
            panic!("absent artifact lease must not succeed")
        };
        assert_ne!(failure.kind, i32::from(ErrorKind::PermissionDenied));
    }
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn every_unsigned_legacy_mutation_family_fails_closed() {
    let signing = SigningKey::from_bytes(&[12; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let mutations = vec![
        privd_request::Operation::ArtifactRead(ArtifactReadRequest::default()),
        privd_request::Operation::ArtifactConsume(ArtifactConsumeRequest::default()),
    ];
    for mutation in mutations {
        let response = dispatch(unsigned_request(Some(mutation)), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);
    }
}

#[tokio::test]
async fn socket_binding_requires_trusted_parent_and_preserves_socket_access() {
    use std::os::unix::fs::{chown, symlink};

    let directory = PathBuf::from("/tmp").join(format!("ocp-socket-{}", Uuid::now_v7().simple()));
    fs::create_dir(&directory).expect("test directory");
    let signing = SigningKey::from_bytes(&[42; 32]);
    let mut config = ServerConfig {
        socket: directory.join("privd.sock"),
        agent_uid: rustix_uid(),
        node_id: *Uuid::now_v7().as_bytes(),
        endpoint_id: TEST_ENDPOINT_ID,
        command_keys: keyring(&signing),
        attestation_key: Arc::new(signing),
        upgrades: UpgradeScheduler::disabled(),
    };
    for mode in [0o755, 0o750] {
        fs::set_permissions(&directory, fs::Permissions::from_mode(mode)).unwrap();
        let listener = bind_socket(&config).expect("trusted parent");
        assert_eq!(fs::metadata(&config.socket).unwrap().mode() & 0o777, 0o660);
        drop(listener);
    }
    remove_socket(&config.socket).unwrap();
    for mode in [0o775, 0o757, 0o777] {
        fs::set_permissions(&directory, fs::Permissions::from_mode(mode)).unwrap();
        assert_eq!(
            bind_socket(&config).unwrap_err().kind(),
            io::ErrorKind::PermissionDenied
        );
        assert!(!config.socket.exists());
    }
    fs::set_permissions(&directory, fs::Permissions::from_mode(0o755)).unwrap();
    // Changing ownership requires root; the BuildServer regression runs as root.
    if rustix_uid() == 0 {
        chown(&directory, Some(65534), None).unwrap();
        let result = bind_socket(&config);
        chown(&directory, Some(0), None).unwrap();
        assert_eq!(result.unwrap_err().kind(), io::ErrorKind::PermissionDenied);
    }
    let alias = directory.with_extension("link");
    symlink(&directory, &alias).unwrap();
    config.socket = alias.join("privd.sock");
    assert_eq!(
        bind_socket(&config).unwrap_err().kind(),
        io::ErrorKind::PermissionDenied
    );
    fs::remove_file(alias).unwrap();
    config.socket = directory.join("privd.sock");
    fs::write(&config.socket, b"do not replace").unwrap();
    assert_eq!(
        bind_socket(&config).unwrap_err().kind(),
        io::ErrorKind::AlreadyExists
    );
    assert_eq!(fs::read(&config.socket).unwrap(), b"do not replace");
    fs::remove_file(&config.socket).unwrap();
    symlink(directory.join("missing"), &config.socket).unwrap();
    assert_eq!(
        bind_socket(&config).unwrap_err().kind(),
        io::ErrorKind::AlreadyExists
    );
    fs::remove_file(&config.socket).unwrap();
    fs::remove_dir(directory).unwrap();
}

#[tokio::test]
async fn saturated_clients_wait_for_capacity_instead_of_being_reset() {
    let signing = SigningKey::from_bytes(&[42; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let (adapter, _, _, directory) = test_adapter();
    let socket = directory.join("privd.sock");
    let config = ServerConfig {
        socket: socket.clone(),
        agent_uid: rustix_uid(),
        node_id,
        endpoint_id: TEST_ENDPOINT_ID,
        command_keys: keys,
        attestation_key: Arc::new(SigningKey::from_bytes(&[43; 32])),
        upgrades: UpgradeScheduler::disabled(),
    };
    let listener = bind_socket(&config).expect("bind privd fixture");
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
    let server_config = config.clone();
    let server = tokio::spawn(async move {
        serve(listener, server_config, adapter, async {
            let _ = shutdown_rx.await;
        })
        .await
    });

    // Keep every active slot inside the bounded request reader. A valid
    // request arriving next must wait for one slot rather than being
    // accepted and reset merely because cleanup of the prior batch has
    // not released its permit yet.
    let mut held = Vec::with_capacity(MAX_CONCURRENT_CLIENTS);
    for _ in 0..MAX_CONCURRENT_CLIENTS {
        held.push(
            UnixStream::connect(&socket)
                .await
                .expect("connect held privd client"),
        );
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    let mut queued = UnixStream::connect(&socket)
        .await
        .expect("connect queued privd client");
    let request = unsigned_request(Some(privd_request::Operation::ServiceStatus(
        ReadRequest {},
    )));
    let request_id = request.request_id.clone();
    write_frame(&mut queued, &request)
        .await
        .expect("write queued read request");
    let response = tokio::spawn(async move {
        let response: Result<PrivdResponse, io::Error> = read_frame(&mut queued).await;
        response
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !response.is_finished(),
        "a saturated but valid local client was reset instead of queued"
    );

    drop(held.pop());
    let response = tokio::time::timeout(Duration::from_secs(2), response)
        .await
        .expect("queued request remained blocked after capacity returned")
        .expect("join queued request")
        .expect("read queued response");
    assert_eq!(response.request_id, request_id);
    assert!(response.result.is_some());

    drop(held);
    shutdown_tx.send(()).expect("stop server");
    server.await.expect("join server").expect("serve fixture");
    remove_socket(&socket).expect("remove fixture socket");
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
async fn same_uid_direct_socket_caller_cannot_forge_root_effect() {
    let signing = SigningKey::from_bytes(&[13; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let (adapter, _, counter, directory) = test_adapter();
    let socket = directory.join("privd.sock");
    let config = ServerConfig {
        socket: socket.clone(),
        agent_uid: rustix_uid(),
        node_id,
        endpoint_id: TEST_ENDPOINT_ID,
        command_keys: keys,
        attestation_key: Arc::new(SigningKey::from_bytes(&[41; 32])),
        upgrades: UpgradeScheduler::disabled(),
    };
    let listener = bind_socket(&config).expect("bind privd fixture");
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
    let server_config = config.clone();
    let server = tokio::spawn(async move {
        serve(listener, server_config, adapter, async {
            let _ = shutdown_rx.await;
        })
        .await
    });
    let mut stream = UnixStream::connect(&socket)
        .await
        .expect("same UID connect");
    let request = unsigned_request(Some(privd_request::Operation::ArtifactRead(
        ArtifactReadRequest::default(),
    )));
    write_frame(&mut stream, &request)
        .await
        .expect("write forged request");
    let response: PrivdResponse = read_frame(&mut stream).await.expect("read rejection");
    assert_permission_denied(&response);
    assert!(!counter.exists());
    shutdown_tx.send(()).expect("stop server");
    server.await.expect("join server").expect("serve fixture");
    remove_socket(&socket).expect("remove fixture socket");
    std::fs::remove_dir_all(directory).expect("cleanup test directory");
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn direct_socket_authorization_and_wire_variants_preserve_effects() {
    use tokio::io::AsyncWriteExt as _;

    let signing = SigningKey::from_bytes(&[13; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let (adapter, _, counter, directory) = test_adapter();
    let now = unix_seconds();
    let command = signed_service_reload(&signing, node_id, now, now + 60);
    let send = |bytes: Vec<u8>, peer_uid| {
        let (mut client, server) = UnixStream::pair().expect("real Unix socket pair");
        let task = tokio::spawn(handle_client(
            server,
            peer_uid,
            node_id,
            TEST_ENDPOINT_ID,
            keys.clone(),
            Arc::new(SigningKey::from_bytes(&[41; 32])),
            UpgradeScheduler::disabled(),
            adapter.clone(),
        ));
        async move {
            let written = client.write_all(&bytes).await;
            let finished = client.shutdown().await;
            let response: Result<PrivdResponse, _> = read_frame(&mut client).await;
            let status = task.await.expect("privd must not panic");
            if peer_uid == rustix_uid() {
                written.expect("write admitted peer request");
                finished.expect("finish admitted peer request");
            }
            (response, status)
        }
    };
    let frame = |body: Vec<u8>| {
        let mut bytes = u32::try_from(body.len()).unwrap().to_be_bytes().to_vec();
        bytes.extend(body);
        bytes
    };
    let request = command_request(command.clone());
    let mut variants = Vec::new();
    for mutate in [
        |value: &mut CommandEnvelope| value.authorization.as_mut().unwrap().signature[0] ^= 1,
        |value: &mut CommandEnvelope| value.action = "config.apply".to_owned(),
        |value: &mut CommandEnvelope| {
            value.required_capability = "ocserv.config.apply".to_owned();
        },
        |value: &mut CommandEnvelope| value.semantic_payload_sha256[0] ^= 1,
        |value: &mut CommandEnvelope| {
            value.payload = Some(command_envelope::Payload::IpBanRemove(
                ocservia_contracts::generated::ocserv::platform::agent::v1::IpBanRemove {
                    ip: "192.0.2.1".to_owned(),
                },
            ));
        },
    ] {
        let mut changed = command.clone();
        mutate(&mut changed);
        variants.push(command_request(changed));
    }
    for changed in [
        signed_service_reload(&signing, *Uuid::now_v7().as_bytes(), now, now + 60),
        signed_service_reload(&signing, node_id, now - 60, now - 1),
        // Stay beyond the 300-second skew even if socket I/O crosses a clock tick.
        signed_service_reload(&signing, node_id, now + 3600, now + 3660),
    ] {
        variants.push(command_request(changed));
    }
    let mut changed = request.clone();
    changed.privileged_mode = 127;
    variants.push(changed);
    let mut changed = request.clone();
    changed.privileged_mode = PrivilegedRequestMode::Reconcile.into();
    variants.push(changed);
    let mut changed = request.clone();
    changed.accepted_at = None;
    variants.push(changed);
    for changed in variants {
        let (response, status) = send(frame(changed.encode_to_vec()), rustix_uid()).await;
        status.expect("decoded request handled");
        let response = response.expect("rejection response");
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(_))
        ));
        assert!(response.privileged_result_proof.is_none());
        assert!(
            !counter.exists(),
            "rejected request performed a root effect"
        );
        assert!(
            !directory.join("effects.sqlite3").exists(),
            "rejected request created an effect record"
        );
    }
    // Unknown fields remain additive; duplicate known fields use prost's
    // decoded value. Neither encoding permits skipping verification.
    let mut duplicate_mode = request.encode_to_vec();
    duplicate_mode.extend([0x40, 0x7f]);
    let (response, _) = send(frame(duplicate_mode), rustix_uid()).await;
    assert!(matches!(
        response.unwrap().result,
        Some(privd_response::Result::Error(_))
    ));
    assert!(!counter.exists());
    for bytes in [vec![0, 0, 0, 0], vec![0xff; 4], vec![0, 0, 0, 2, 0x80]] {
        let (response, status) = send(bytes, rustix_uid()).await;
        assert!(response.is_err());
        assert!(status.is_err());
        assert!(!counter.exists());
    }
    let (response, status) = send(frame(request.encode_to_vec()), rustix_uid() + 1).await;
    assert!(response.is_err());
    assert_eq!(status.unwrap_err().kind(), io::ErrorKind::PermissionDenied);
    assert!(!counter.exists());

    let mut additive = request.encode_to_vec();
    additive.extend([0xf8, 0x07, 0x01, 0x40, 0x01]);
    let (first, status) = send(frame(additive), rustix_uid()).await;
    status.unwrap();
    let first = first.unwrap();
    assert!(
        matches!(first.result, Some(privd_response::Result::Mutation(ref result)) if result.applied)
    );
    assert!(first.privileged_result_proof.is_some());
    for replay in [request, reconcile_request(command.clone(), &signing)] {
        let (response, status) = send(frame(replay.encode_to_vec()), rustix_uid()).await;
        status.unwrap();
        let response = response.unwrap();
        assert_eq!(response.result, first.result);
        assert_eq!(
            response.privileged_result_proof,
            first.privileged_result_proof
        );
        assert_eq!(fs::read(&counter).unwrap(), b"x");
    }
    let mut conflict = command;
    conflict.expected_revision += 1;
    let claims = claims_from_envelope_v1(&conflict).unwrap();
    conflict.authorization.as_mut().unwrap().signature = signing
        .sign(&canonical_v1(&claims).unwrap())
        .to_bytes()
        .to_vec();
    let (response, status) = send(
        frame(command_request(conflict).encode_to_vec()),
        rustix_uid(),
    )
    .await;
    status.unwrap();
    assert!(matches!(
        response.unwrap().result,
        Some(privd_response::Result::Error(_))
    ));
    assert_eq!(
        fs::read(&counter).unwrap(),
        b"x",
        "conflicting signed identity repeated an effect"
    );
    fs::remove_dir_all(directory).unwrap();
}

#[tokio::test]
async fn upgrade_result_list_surfaces_durable_outcomes_read_only() {
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let directory =
        std::env::temp_dir().join(format!("ocservia-privd-upgrade-read-{}", Uuid::now_v7()));
    fs::create_dir(&directory).expect("create test directory");
    let operations = directory.join("var/lib/ocservia-upgrade/operations");
    fs::create_dir_all(&operations).expect("create operations root");
    let scheduler = UpgradeScheduler::new(operations.clone(), UpgradeTrigger::Disabled);
    let intent = UpgradeIntent::new(
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        "2.0.0",
        [0x43; 32],
        ocservia_contracts::agent_upgrade::runtime_architecture().expect("host architecture"),
        [0x55; 32],
    )
    .expect("valid test intent");
    scheduler
        .schedule_and_trigger(&intent)
        .expect("schedule intent");
    // The spool is empty, so the runner durably refuses the upgrade and
    // leaves exactly the terminal evidence the read-only query must show.
    let runner = ocservia_upgrader::UpgradeRunner::new(directory.clone());
    assert!(runner.run(&intent.operation_id.to_string()).is_err());

    let request = PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline(),
        accepted_at: None,
        authorization_command: None,
        privileged_mode: PrivilegedRequestMode::Unspecified.into(),
        operation: Some(privd_request::Operation::UpgradeResultList(
            ocservia_agent_protocol::UpgradeResultListRequest {},
        )),
    };
    let response = dispatch_attested(
        &request,
        &node_id,
        &TEST_ENDPOINT_ID,
        &keys,
        &SigningKey::from_bytes(&[41; 32]),
        &scheduler,
        &adapter,
    )
    .await;
    let Some(privd_response::Result::UpgradeResultList(list)) = response.result else {
        panic!("expected an upgrade result list, got {:?}", response.result);
    };
    assert_eq!(list.results.len(), 1);
    assert_eq!(list.results[0].operation_id, intent.operation_id.as_bytes());
    assert_eq!(list.results[0].state, "failed");
    assert_eq!(list.results[0].target_version, "2.0.0");
    assert!(list.results[0].completed_unix_ms > 0);
    let proof = list.results[0]
        .privileged_result_proof
        .as_ref()
        .expect("root-attested durable result");
    assert_eq!(proof.node_id, node_id);
    assert_eq!(proof.operation_id, intent.operation_id.as_bytes());
    assert_eq!(proof.package_sha256, intent.package_sha256);
    assert_eq!(proof.result_sha256.len(), 32);
    assert_eq!(proof.signature.len(), ed25519_dalek::SIGNATURE_LENGTH);
    std::fs::remove_dir_all(&directory).expect("cleanup test directory");
}
