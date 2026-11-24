#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::fs::OpenOptions;
use std::os::unix::fs::{OpenOptionsExt as _, chown, symlink};

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, PrivilegedRequestMode, ReadRequest,
    UpgradeResultListRequest, privd_request,
};
use ocservia_command_authorization::{
    ArtifactGrantClaimsV1, canonical_artifact_grant_v1, canonical_v1, claims_from_envelope_v1,
    verification_key_id,
};
use ocservia_contracts::config_profile;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, ArtifactGrantV1, ArtifactGrantVersion, CertificateCsr, CertificateP12,
    CommandDeliveryMode, CommandResultState, CompleteConfigApply, CompleteConfigCandidate,
    CompleteConfigDirective, ConnectionFenceV2, FenceBindingV2, FenceOperationKind, IpBanRemove,
    NodeLocalTlsReference, PrivilegedCommandKind, PrivilegedResultKind, SealedSecretPurpose,
    SealedSecretV1, SealedSecretVersion, ServiceReload, SessionDisconnect, SessionTerminate,
    UserCreate, command_envelope, complete_config_directive,
};
use ocservia_ocserv_adapter::{FixedResources, Limits};
use ocservia_test_support::{
    FenceSpec, attach_command_fence, deadline_ms, foreign_architecture, host_architecture, keyring,
    sign_command_v2, sign_fence_binding, ts, unix_seconds,
};
use ocservia_upgrader::UpgradeTrigger;
use sha2::{Digest as _, Sha256};
use tokio::io::AsyncWriteExt as _;

use super::*;

const TEST_ENDPOINT_ID: [u8; 32] = [9; 32];

async fn dispatch(
    request: PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
    adapter: &Adapter,
) -> PrivdResponse {
    dispatch_upgrade(
        request,
        node_id,
        command_keys,
        adapter,
        &UpgradeScheduler::disabled(),
    )
    .await
}

async fn dispatch_upgrade(
    request: PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
    adapter: &Adapter,
    upgrades: &UpgradeScheduler,
) -> PrivdResponse {
    let key = Arc::new(SigningKey::from_bytes(&[41; 32]));
    let kid: Arc<str> = Arc::from(key_id(&key.verifying_key()));
    Box::pin(dispatch_attested(
        &request,
        node_id,
        &TEST_ENDPOINT_ID,
        command_keys,
        &key,
        &kid,
        upgrades,
        adapter,
    ))
    .await
}

struct PrivdFixture {
    dir: PathBuf,
    adapter: Adapter,
    resources: FixedResources,
    counter: PathBuf,
}

impl PrivdFixture {
    fn new() -> Self {
        let dir = PathBuf::from("/tmp").join(format!("ocp-{}", Uuid::now_v7().simple()));
        fs::create_dir(&dir).expect("create test dir");
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).expect("chmod dir");
        let counter = dir.join("reload-count");
        let systemctl = dir.join("systemctl");
        fs::write(
            &systemctl,
            format!("#!/bin/sh\nprintf x >> '{}'\n", counter.display()),
        )
        .expect("write systemctl");
        fs::set_permissions(&systemctl, fs::Permissions::from_mode(0o700)).expect("chmod");
        let config = dir.join("ocserv.conf");
        let boot = dir.join("boot_id");
        fs::write(&config, b"fixture\n").expect("config");
        fs::write(&boot, Uuid::now_v7().to_string()).expect("boot");
        let resources = FixedResources::new(
            systemctl,
            PathBuf::from("/bin/true"),
            PathBuf::from("/bin/true"),
            config,
            boot,
        )
        .expect("fixed resources")
        .with_effect_store(dir.join("effects.sqlite3"), dir.join("effects.key"))
        .expect("effect resources");
        let adapter = Adapter::new(resources.clone(), Limits::default());
        Self {
            dir,
            adapter,
            resources,
            counter,
        }
    }

    fn upgrade_scheduler(&self) -> (UpgradeScheduler, PathBuf) {
        let ops = self.dir.join("upgrade-operations");
        fs::create_dir(&ops).expect("ops dir");
        fs::set_permissions(&ops, fs::Permissions::from_mode(0o700)).expect("chmod ops");
        (
            UpgradeScheduler::new(ops.clone(), UpgradeTrigger::Disabled),
            ops,
        )
    }

    fn server_config(
        &self,
        node_id: [u8; 16],
        keys: ControllerCommandKeyring,
        attest: [u8; 32],
    ) -> ServerConfig {
        ServerConfig {
            socket: self.dir.join("privd.sock"),
            agent_uid: rustix_uid(),
            node_id,
            endpoint_id: TEST_ENDPOINT_ID,
            command_keys: keys,
            attestation_key: Arc::new(SigningKey::from_bytes(&attest)),
            upgrades: UpgradeScheduler::disabled(),
        }
    }
}

impl Drop for PrivdFixture {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.dir);
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
    let mut cmd = sign_command_v2(
        key, node_id, issued_at, expires_at, 7, action, capability, payload,
    );
    attach_command_fence(key, &mut cmd, [1; 16], TEST_ENDPOINT_ID, (expires_at, 0));
    cmd
}

fn signed_service_reload(
    key: &SigningKey,
    node_id: [u8; 16],
    issued: i64,
    expires: i64,
) -> CommandEnvelope {
    signed_command(
        key,
        node_id,
        issued,
        expires,
        "service.reload",
        "ocserv.service.reload",
        command_envelope::Payload::ServiceReload(ServiceReload {}),
    )
}

fn signed_agent_upgrade(
    key: &SigningKey,
    node_id: [u8; 16],
    issued: i64,
    expires: i64,
    ver: &str,
    sha: Vec<u8>,
    arch: &str,
) -> CommandEnvelope {
    signed_command(
        key,
        node_id,
        issued,
        expires,
        "agent.upgrade",
        "ocserv.agent.upgrade.v2",
        command_envelope::Payload::AgentUpgrade(AgentUpgrade {
            target_version: ver.to_owned(),
            package_sha256: sha,
            architecture: arch.to_owned(),
        }),
    )
}

fn signed_fence(
    key: &SigningKey,
    cmd: &CommandEnvelope,
    fence_id: [u8; 16],
    endpoint_id: [u8; 32],
    lease_until: (i64, u32),
) -> ConnectionFenceV2 {
    signed_fence_at_epoch(key, cmd, fence_id, endpoint_id, lease_until, 1)
}

fn signed_fence_at_epoch(
    key: &SigningKey,
    cmd: &CommandEnvelope,
    fence_id: [u8; 16],
    endpoint_id: [u8; 32],
    lease_until: (i64, u32),
    owner_epoch: u64,
) -> ConnectionFenceV2 {
    let issued = cmd.issued_at.expect("issued").seconds;
    let expires = cmd.expires_at.expect("expires").seconds;
    FenceSpec::new(
        cmd.node_id.as_slice().try_into().expect("node"),
        endpoint_id,
        owner_epoch,
    )
    .ids(fence_id, [2; 16], 1, [3; 16])
    .authority(cmd.expected_revision)
    .capabilities([cmd.required_capability.clone()])
    .lease(lease_until.0, lease_until.1)
    .issued(issued - 1, 0)
    .expires(expires.max(lease_until.0 + 1), 0)
    .sign(key)
}

fn with_owner_epoch(
    mut cmd: CommandEnvelope,
    key: &SigningKey,
    owner_epoch: u64,
) -> CommandEnvelope {
    let fence = signed_fence_at_epoch(
        key,
        &cmd,
        [1; 16],
        TEST_ENDPOINT_ID,
        (cmd.expires_at.expect("expires").seconds, 0),
        owner_epoch,
    );
    let binding = signed_binding(
        key,
        &cmd,
        &fence,
        FenceOperationKind::Command,
        &cmd.command_id,
        &cmd.required_capability,
    );
    cmd.connection_fence = Some(fence);
    cmd.fence_binding = Some(binding);
    cmd
}

fn signed_binding(
    key: &SigningKey,
    cmd: &CommandEnvelope,
    fence: &ConnectionFenceV2,
    kind: FenceOperationKind,
    op_id: &[u8],
    cap: &str,
) -> FenceBindingV2 {
    sign_fence_binding(
        key,
        fence,
        kind,
        op_id.try_into().expect("op ID"),
        cap,
        (cmd.issued_at.expect("issued").seconds, 0),
        (fence.expires_at.expect("fence expiry").seconds, 0),
    )
}

fn resign_mode(
    mut cmd: CommandEnvelope,
    key: &SigningKey,
    mode: CommandDeliveryMode,
) -> CommandEnvelope {
    cmd.delivery_mode = mode.into();
    cmd.message_id = Uuid::now_v7().as_bytes().to_vec();
    let claims = claims_from_envelope_v1(&cmd).expect("claims");
    cmd.authorization.as_mut().expect("auth").signature = key
        .sign(&canonical_v1(&claims).expect("canonical"))
        .to_bytes()
        .to_vec();
    cmd
}

fn command_request(command: CommandEnvelope) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        accepted_at: command.issued_at,
        authorization_command: Some(command),
        privileged_mode: PrivilegedRequestMode::Execute.into(),
        operation: None,
    }
}

fn reconcile_request(command: CommandEnvelope, key: &SigningKey) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        accepted_at: None,
        authorization_command: Some(resign_mode(
            command,
            key,
            CommandDeliveryMode::ReconcileOnly,
        )),
        privileged_mode: PrivilegedRequestMode::Reconcile.into(),
        operation: None,
    }
}

fn retry_request(command: CommandEnvelope, key: &SigningKey) -> PrivdRequest {
    command_request(resign_mode(
        command,
        key,
        CommandDeliveryMode::RetryIfEffectAbsent,
    ))
}

fn unsigned_request(operation: Option<privd_request::Operation>) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        accepted_at: None,
        authorization_command: None,
        privileged_mode: PrivilegedRequestMode::Unspecified.into(),
        operation,
    }
}

fn assert_error_kind(response: &PrivdResponse, want: ErrorKind) {
    let Some(privd_response::Result::Error(failure)) = response.result.as_ref() else {
        panic!("expected error {want:?}, got {:?}", response.result);
    };
    assert_eq!(
        ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified),
        want
    );
}

fn assert_permission_denied(response: &PrivdResponse) {
    assert_error_kind(response, ErrorKind::PermissionDenied);
}

fn assert_requires_reconciliation(response: &PrivdResponse) {
    assert!(response.privileged_result_proof.is_none());
    assert_error_kind(response, ErrorKind::Unavailable);
}

fn durable_operation_state(ops_dir: &Path, operation_id: &[u8]) -> String {
    let id = Uuid::from_slice(operation_id).expect("op UUID");
    fs::read_to_string(ops_dir.join(id.to_string()).join("state"))
        .expect("durable state")
        .trim_end_matches('\n')
        .to_owned()
}

#[tokio::test]
async fn rejects_expired_missing_unknown_and_unsigned_operations() {
    let trusted = SigningKey::from_bytes(&[7; 32]);
    let unknown = SigningKey::from_bytes(&[11; 32]);
    let keys = keyring(&trusted);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());

    let expired_req = PrivdRequest {
        deadline_unix_ms: 1,
        ..unsigned_request(Some(privd_request::Operation::ServiceStatus(
            ReadRequest {},
        )))
    };
    assert!(matches!(
        dispatch(expired_req, &node_id, &keys, &adapter)
            .await
            .result,
        Some(privd_response::Result::Error(_))
    ));
    assert_permission_denied(&dispatch(unsigned_request(None), &node_id, &keys, &adapter).await);

    for bad_cmd in [
        signed_service_reload(&trusted, node_id, now - 60, now - 1),
        signed_service_reload(&trusted, *Uuid::now_v7().as_bytes(), now, now + 60),
        signed_service_reload(&unknown, node_id, now, now + 60),
    ] {
        assert_permission_denied(
            &dispatch(command_request(bad_cmd), &node_id, &keys, &adapter).await,
        );
    }
    for op in [
        privd_request::Operation::ArtifactRead(ArtifactReadRequest::default()),
        privd_request::Operation::ArtifactConsume(ArtifactConsumeRequest::default()),
    ] {
        assert_permission_denied(
            &dispatch(unsigned_request(Some(op)), &node_id, &keys, &adapter).await,
        );
    }
}

#[tokio::test]
async fn rebound_authority_denies_old_node_key_and_quarantines_uncertain_effects() {
    let old_key = SigningKey::from_bytes(&[8; 32]);
    let new_key = SigningKey::from_bytes(&[9; 32]);
    let keys = keyring(&new_key);
    let old_node = *Uuid::now_v7().as_bytes();
    let new_node = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fx = PrivdFixture::new();
    for (signing, node) in [
        (&old_key, old_node),
        (&old_key, new_node),
        (&new_key, old_node),
    ] {
        let cmd = signed_service_reload(signing, node, now, now + 60);
        assert_permission_denied(
            &dispatch(command_request(cmd), &new_node, &keys, &fx.adapter).await,
        );
        assert!(!fx.counter.exists());
    }
    let cmd = signed_service_reload(&new_key, new_node, now, now + 60);
    let ok = dispatch(command_request(cmd), &new_node, &keys, &fx.adapter).await;
    assert!(matches!(ok.result, Some(privd_response::Result::Mutation(ref r)) if r.applied));
    assert_eq!(fs::read(&fx.counter).expect("counter"), b"x");
}

#[tokio::test]
async fn journal_acceptance_allows_five_minute_skew_and_receipts_survive_clock_rollback() {
    let signing = SigningKey::from_bytes(&[8; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fixture = PrivdFixture::new();
    for (accepted_at, allowed) in [
        (ts(now - 301, 0), false),
        (ts(now - 300, 0), true),
        (ts(now + 30, 900_000_000), true),
        (ts(now + 600, 0), false),
    ] {
        let mut request = command_request(signed_service_reload(&signing, node_id, now, now + 60));
        request.accepted_at = Some(accepted_at);
        let result = dispatch(request, &node_id, &keys, &fixture.adapter).await;
        assert_eq!(result.privileged_result_proof.is_some(), allowed);
        if let Some(proof) = result.privileged_result_proof {
            ocservia_privd_attestation::verify_receipt(
                &proof,
                &SigningKey::from_bytes(&[41; 32]).verifying_key(),
            )
            .expect("valid receipt");
            let receipt = proof.receipt_v1.expect("receipt");
            assert_eq!(receipt.accepted_at, Some(accepted_at));
            if accepted_at.seconds > now {
                assert_eq!(receipt.completed_at, Some(accepted_at));
            }
        }
    }
    assert_eq!(fs::read(&fixture.counter).expect("executed twice"), b"xx");
}

#[tokio::test]
async fn signed_effect_executes_once_replays_after_restart_and_fails_on_receipt_fault() {
    let signing = SigningKey::from_bytes(&[8; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let cmd = signed_service_reload(&signing, node_id, now, now + 60);
    let fx = PrivdFixture::new();

    let first = dispatch(command_request(cmd.clone()), &node_id, &keys, &fx.adapter).await;
    assert!(matches!(first.result, Some(privd_response::Result::Mutation(ref r)) if r.applied));
    let proof = first
        .privileged_result_proof
        .clone()
        .expect("attested receipt");
    ocservia_privd_attestation::verify_receipt(
        &proof,
        &SigningKey::from_bytes(&[41; 32]).verifying_key(),
    )
    .expect("valid receipt");

    let restarted = Adapter::new(fx.resources.clone(), Limits::default());
    for req in [
        reconcile_request(cmd.clone(), &signing),
        command_request(cmd.clone()),
        retry_request(cmd, &signing),
    ] {
        let replay = dispatch(req, &node_id, &keys, &restarted).await;
        assert_eq!(replay.privileged_result_proof, Some(proof.clone()));
        assert_eq!(replay.result, first.result);
    }
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "x");

    // Receipt persistence failure cannot return privileged success or repeat side effect.
    let fault_fx = PrivdFixture::new();
    let init = signed_service_reload(&signing, node_id, now, now + 60);
    let claims = claims_from_envelope_v1(&init).expect("claims");
    let binding = effect_binding(&init, &claims).expect("binding");
    assert_eq!(
        fault_fx
            .adapter
            .prepare_authorized_effect(
                authorized_effect(&init, &claims, &binding),
                owner_fence(&node_id, &TEST_ENDPOINT_ID, 1),
            )
            .expect("init"),
        AuthorizedEffectDecision::Execute
    );
    rusqlite::Connection::open(fault_fx.dir.join("effects.sqlite3"))
        .expect("sqlite")
        .execute_batch(
            "CREATE TRIGGER fail_attested_response BEFORE UPDATE OF response ON authorized_effects BEGIN SELECT RAISE(ABORT, 'injected'); END;",
        )
        .expect("trigger");
    let failed = dispatch(
        command_request(signed_service_reload(&signing, node_id, now, now + 60)),
        &node_id,
        &keys,
        &fault_fx.adapter,
    )
    .await;
    assert_requires_reconciliation(&failed);
    assert_eq!(fs::read_to_string(&fault_fx.counter).expect("counter"), "x");
}

#[tokio::test]
async fn newer_owner_epoch_persists_and_rejects_stale_root_effects() {
    let signing = SigningKey::from_bytes(&[47; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fx = PrivdFixture::new();

    let stale = signed_service_reload(&signing, node_id, now, now + 60);
    let newer = with_owner_epoch(
        signed_service_reload(&signing, node_id, now, now + 60),
        &signing,
        2,
    );
    let accepted = dispatch(command_request(newer), &node_id, &keys, &fx.adapter).await;
    assert!(matches!(
        accepted.result,
        Some(privd_response::Result::Mutation(ref result)) if result.applied
    ));

    let restarted = Adapter::new(fx.resources.clone(), Limits::default());
    assert_permission_denied(
        &dispatch(
            retry_request(stale.clone(), &signing),
            &node_id,
            &keys,
            &restarted,
        )
        .await,
    );
    assert_permission_denied(&dispatch(command_request(stale), &node_id, &keys, &restarted).await);
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "x");

    let current = with_owner_epoch(
        signed_service_reload(&signing, node_id, now, now + 60),
        &signing,
        2,
    );
    let allowed = dispatch(command_request(current), &node_id, &keys, &restarted).await;
    assert!(matches!(
        allowed.result,
        Some(privd_response::Result::Mutation(ref result)) if result.applied
    ));
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "xx");
}

#[tokio::test]
async fn unattestable_csr_is_rejected_before_effect_reservation() {
    let signing = SigningKey::from_bytes(&[44; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fx = PrivdFixture::new();
    let certificate_id = Uuid::now_v7();
    let command = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "certificate.issue",
        "ocserv.certificate.issue",
        command_envelope::Payload::CertificateCsr(CertificateCsr {
            certificate_id: certificate_id.as_bytes().to_vec(),
            common_name: "a".repeat(254),
            dns_names: Vec::new(),
            key_bits: 2048,
        }),
    );

    let rejected = dispatch(command_request(command), &node_id, &keys, &fx.adapter).await;
    assert_error_kind(&rejected, ErrorKind::InvalidRequest);
    assert!(!fx.dir.join("effects.sqlite3").exists());
}

#[tokio::test]
async fn expired_and_retry_grants_enforce_reconciliation_and_fixed_host_rules() {
    let signing = SigningKey::from_bytes(&[45; 32]);
    let keys = keyring(&signing);
    let node = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();

    // Fixed-host classification check.
    for payload in [
        command_envelope::Payload::ServiceReload(ServiceReload {}),
        command_envelope::Payload::IpBanRemove(IpBanRemove {
            ip: "192.0.2.1".to_owned(),
        }),
        command_envelope::Payload::SessionDisconnect(SessionDisconnect {
            session_id: "s".into(),
            boot_id: "b".into(),
        }),
        command_envelope::Payload::SessionTerminate(SessionTerminate {
            session_id: "s".into(),
            boot_id: "b".into(),
        }),
    ] {
        assert!(fixed_host_effect(&CommandEnvelope {
            payload: Some(payload),
            ..CommandEnvelope::default()
        }));
    }
    assert!(!fixed_host_effect(&CommandEnvelope {
        payload: Some(command_envelope::Payload::AgentUpgrade(
            AgentUpgrade::default()
        )),
        ..CommandEnvelope::default()
    }));

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

    // Legacy retry without completed result requires reconciliation.
    let fx = PrivdFixture::new();
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
    assert_requires_reconciliation(
        &dispatch(retry_request(legacy, &signing), &node, &keys, &fx.adapter).await,
    );

    // Retry grant never re-executes fixed host effects when pending or pruned.
    for pending in [true, false] {
        let retry_fx = PrivdFixture::new();
        let cmd = signed_service_reload(&signing, node, now, now + 60);
        if pending {
            let claims = claims_from_envelope_v1(&cmd).expect("claims");
            let binding = effect_binding(&cmd, &claims).expect("binding");
            assert_eq!(
                retry_fx
                    .adapter
                    .prepare_authorized_effect(
                        authorized_effect(&cmd, &claims, &binding),
                        owner_fence(&node, &TEST_ENDPOINT_ID, 1),
                    )
                    .expect("prep"),
                AuthorizedEffectDecision::Execute
            );
        }
        for _ in 0..2 {
            assert_requires_reconciliation(
                &dispatch(
                    retry_request(cmd.clone(), &signing),
                    &node,
                    &keys,
                    &retry_fx.adapter,
                )
                .await,
            );
            assert!(!retry_fx.counter.exists());
        }
    }
}

#[tokio::test]
async fn tampering_cross_purpose_and_substituted_config_fail_before_root_effect() {
    use complete_config_directive::Value::{Literal, Tls};

    let signing = SigningKey::from_bytes(&[9; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fx = PrivdFixture::new();
    let cmd = signed_service_reload(&signing, node_id, now, now + 60);

    let mutations: [fn(&mut CommandEnvelope); 10] = [
        |c| c.node_id = Uuid::now_v7().as_bytes().to_vec(),
        |c| c.expected_revision += 1,
        |c| c.command_id = Uuid::now_v7().as_bytes().to_vec(),
        |c| c.idempotency_key = Uuid::now_v7().as_bytes().to_vec(),
        |c| c.required_capability = "ocserv.config.apply".to_owned(),
        |c| c.semantic_payload_sha256[0] ^= 1,
        |c| c.operation_id = Uuid::now_v7().as_bytes().to_vec(),
        |c| c.actor_id = "forged-controller".to_owned(),
        |c| {
            c.payload = Some(command_envelope::Payload::IpBanRemove(IpBanRemove {
                ip: "192.0.2.1".into(),
            }));
        },
        |c| c.authorization.as_mut().expect("auth").signature[0] ^= 1,
    ];
    for mutate in mutations {
        let mut changed = cmd.clone();
        mutate(&mut changed);
        assert_permission_denied(
            &dispatch(command_request(changed), &node_id, &keys, &fx.adapter).await,
        );
    }
    let mut agent_selected = command_request(cmd);
    agent_selected.operation = Some(privd_request::Operation::ServiceStatus(ReadRequest {}));
    assert_error_kind(
        &dispatch(agent_selected, &node_id, &keys, &fx.adapter).await,
        ErrorKind::InvalidRequest,
    );
    assert!(!fx.counter.exists());

    // Cross-purpose sealed passwords fail closed.
    let mut user_cmd = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "user.create",
        "ocserv.users.write",
        command_envelope::Payload::UserCreate(UserCreate {
            username: "alice".into(),
            desired_revision: 1,
            sealed_password_v1: Some(SealedSecretV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::UserPassword as i32,
                key_id: "user-key-v1".into(),
                ciphertext: vec![0xa5; 256],
            }),
            ..UserCreate::default()
        }),
    );
    if let Some(command_envelope::Payload::UserCreate(p)) = user_cmd.payload.as_mut() {
        p.sealed_password_v1.as_mut().expect("secret").purpose =
            SealedSecretPurpose::CertificateP12Password as i32;
    }
    assert_permission_denied(
        &dispatch(command_request(user_cmd), &node_id, &keys, &fx.adapter).await,
    );

    let mut p12_cmd = signed_command(
        &signing,
        node_id,
        now,
        now + 60,
        "certificate.private_key.export",
        "ocserv.certificate.issue",
        command_envelope::Payload::CertificateP12(CertificateP12 {
            certificate_id: Uuid::now_v7().as_bytes().to_vec(),
            certificate_chain_pem: vec![b'A'; 64],
            artifact_id: Uuid::now_v7().as_bytes().to_vec(),
            sealed_password_v1: Some(SealedSecretV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::CertificateP12Password as i32,
                key_id: "p12-key-v1".into(),
                ciphertext: vec![0x5a; 256],
            }),
            certificate_version: 1,
            artifact_expires_at: Some(ts(now + 60, 0)),
            ..CertificateP12::default()
        }),
    );
    if let Some(command_envelope::Payload::CertificateP12(p)) = p12_cmd.payload.as_mut() {
        p.sealed_password_v1.as_mut().expect("secret").purpose =
            SealedSecretPurpose::UserPassword as i32;
    }
    assert_permission_denied(
        &dispatch(command_request(p12_cmd), &node_id, &keys, &fx.adapter).await,
    );

    // Substituted complete config candidate bytes fail closed.
    let tls = NodeLocalTlsReference {
        secret_ref_id: vec![0x12; 16],
        version: "v1".into(),
        certificate_sha256: vec![0x23; 32],
        spki_sha256: vec![0x34; 32],
        ca_sha256: Vec::new(),
    };
    let dir = |name: &str, value| CompleteConfigDirective {
        name: name.into(),
        value: Some(value),
    };
    let candidate = CompleteConfigCandidate {
        node_id: node_id.to_vec(),
        expected_revision: 1,
        directives: vec![
            dir("auth", Literal("plain[passwd=/etc/ocserv/ocpasswd]".into())),
            dir("cookie-timeout", Literal("300".into())),
            dir("device", Literal("vpns".into())),
            dir("dns", Literal("1.1.1.1".into())),
            dir("ipv4-network", Literal("192.0.2.0/24".into())),
            dir("max-clients", Literal("64".into())),
            dir("max-same-clients", Literal("2".into())),
            dir("server-cert", Tls(tls.clone())),
            dir("server-key", Tls(tls)),
            dir("socket-file", Literal("/run/ocserv.socket".into())),
            dir("tcp-port", Literal("443".into())),
            dir("udp-port", Literal("0".into())),
        ],
    };
    let candidate_hash =
        Sha256::digest(config_profile::canonical(&candidate).expect("canonical")).to_vec();
    let mut cfg_cmd = signed_command(
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
            plan_expires_at: Some(ts(now + 60, 0)),
        }),
    );
    if let Some(command_envelope::Payload::CompleteConfigApply(p)) = cfg_cmd.payload.as_mut() {
        p.candidate.as_mut().expect("cand").directives[10].value = Some(Literal("8443".into()));
    }
    assert_permission_denied(
        &dispatch(command_request(cfg_cmd), &node_id, &keys, &fx.adapter).await,
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn agent_upgrade_prepare_attestation_concurrency_conflict_and_validation() {
    let signing = SigningKey::from_bytes(&[29; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();

    // Release identity validation: foreign arch, unknown key, unsigned.
    let no_io = Adapter::new(FixedResources::default(), Limits::default());
    let foreign = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        foreign_architecture(),
    );
    assert_error_kind(
        &dispatch(command_request(foreign), &node_id, &keys, &no_io).await,
        ErrorKind::InvalidRequest,
    );
    let forged = signed_agent_upgrade(
        &SigningKey::from_bytes(&[31; 32]),
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
    );
    assert_permission_denied(&dispatch(command_request(forged), &node_id, &keys, &no_io).await);

    // Concurrent duplicate schedules both attest the same durable intent; second active op is refused.
    let fx = PrivdFixture::new();
    let (upgrades, ops_dir) = fx.upgrade_scheduler();
    let cmd = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
    );
    let (r1, r2) = tokio::join!(
        dispatch_upgrade(
            command_request(cmd.clone()),
            &node_id,
            &keys,
            &fx.adapter,
            &upgrades
        ),
        dispatch_upgrade(
            command_request(cmd.clone()),
            &node_id,
            &keys,
            &fx.adapter,
            &upgrades
        ),
    );
    for resp in [&r1, &r2] {
        let Some(privd_response::Result::AgentUpgradeScheduled(ref s)) = resp.result else {
            panic!("expected scheduled intent, got {:?}", resp.result);
        };
        assert_eq!(
            (
                s.target_version.as_str(),
                s.package_sha256.as_slice(),
                s.operation_id.as_slice()
            ),
            ("1.2.3", [0x43; 32].as_slice(), cmd.operation_id.as_slice())
        );
        let proof = resp.privileged_result_proof.as_ref().expect("proof");
        ocservia_privd_attestation::verify_receipt(
            proof,
            &SigningKey::from_bytes(&[41; 32]).verifying_key(),
        )
        .expect("receipt");
        let rec = proof.receipt_v1.as_ref().expect("receipt_v1");
        assert_eq!(
            PrivilegedCommandKind::try_from(rec.command_kind),
            Ok(PrivilegedCommandKind::AgentUpgrade)
        );
        assert_eq!(
            PrivilegedResultKind::try_from(rec.result_kind),
            Ok(PrivilegedResultKind::AgentUpgradeScheduled)
        );
        assert_eq!(
            CommandResultState::try_from(rec.terminal_state),
            Ok(CommandResultState::Succeeded)
        );
    }
    assert_eq!(
        durable_operation_state(&ops_dir, &cmd.operation_id),
        "accepted"
    );
    let restarted = Adapter::new(fx.resources.clone(), Limits::default());
    let replay = dispatch_upgrade(
        reconcile_request(cmd, &signing),
        &node_id,
        &keys,
        &restarted,
        &upgrades,
    )
    .await;
    assert_eq!(replay.privileged_result_proof, r1.privileged_result_proof);
    assert_eq!(replay.result, r1.result);

    let second_cmd = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.4",
        vec![0x52; 32],
        host_architecture(),
    );
    let refused = dispatch_upgrade(
        command_request(second_cmd),
        &node_id,
        &keys,
        &restarted,
        &upgrades,
    )
    .await;
    assert_error_kind(&refused, ErrorKind::InvalidRequest);
    if let Some(proof) = refused.privileged_result_proof.as_ref() {
        assert_eq!(
            CommandResultState::try_from(proof.receipt_v1.as_ref().expect("rec").terminal_state),
            Ok(CommandResultState::Failed)
        );
    }
    let durable_count = fs::read_dir(&ops_dir)
        .expect("ops")
        .filter_map(Result::ok)
        .filter(|e| Uuid::parse_str(&e.file_name().to_string_lossy()).is_ok())
        .count();
    assert_eq!(durable_count, 1);

    // Pending journal replays while runner holds execution lock.
    let lock_fx = PrivdFixture::new();
    let (lock_up, lock_ops) = lock_fx.upgrade_scheduler();
    let lock_cmd = signed_agent_upgrade(
        &signing,
        node_id,
        now,
        now + 60,
        "1.2.3",
        vec![0x61; 32],
        host_architecture(),
    );
    let claims = claims_from_envelope_v1(&lock_cmd).expect("claims");
    let binding = effect_binding(&lock_cmd, &claims).expect("binding");
    assert_eq!(
        lock_fx
            .adapter
            .prepare_authorized_effect(
                authorized_effect(&lock_cmd, &claims, &binding),
                owner_fence(&node_id, &TEST_ENDPOINT_ID, 1),
            )
            .expect("prep"),
        AuthorizedEffectDecision::Execute
    );
    let Some(command_envelope::Payload::AgentUpgrade(payload)) = lock_cmd.payload.as_ref() else {
        unreachable!()
    };
    let intent = UpgradeIntent::new(
        claims.operation_id,
        claims.command_id,
        &payload.target_version,
        payload.package_sha256.clone().try_into().expect("sha"),
        &payload.architecture,
        claims.semantic_payload_sha256,
    )
    .expect("intent");
    assert_eq!(
        lock_up.schedule_and_trigger(&intent).expect("schedule"),
        ScheduleOutcome::Scheduled
    );
    fs::write(
        lock_ops.join(intent.operation_id.to_string()).join("state"),
        b"running\n",
    )
    .expect("state");
    let exec_lock = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(lock_ops.join(".run.lock"))
        .expect("lock");
    rustix::fs::flock(
        &exec_lock,
        rustix::fs::FlockOperation::NonBlockingLockExclusive,
    )
    .expect("flock");
    let lock_replay = dispatch_upgrade(
        command_request(lock_cmd.clone()),
        &node_id,
        &keys,
        &lock_fx.adapter,
        &lock_up,
    )
    .await;
    assert!(matches!(
        lock_replay.result,
        Some(privd_response::Result::AgentUpgradeScheduled(_))
    ));
    assert_eq!(
        durable_operation_state(&lock_ops, &lock_cmd.operation_id),
        "running"
    );
    drop(exec_lock);
}

#[tokio::test]
async fn root_execution_and_artifacts_require_their_own_unexpired_fence() {
    let signing = SigningKey::from_bytes(&[12; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let now = unix_seconds();
    let fx = PrivdFixture::new();
    let cmd = signed_service_reload(&signing, node_id, now, now + 60);
    let cap = cmd.required_capability.clone();
    let carried = |fence: Option<ConnectionFenceV2>, binding: Option<FenceBindingV2>| {
        let mut c = cmd.clone();
        c.connection_fence = fence;
        c.fence_binding = binding;
        command_request(c)
    };
    let bound = |fence: ConnectionFenceV2, op_id: Vec<u8>, c: &str| {
        let b = signed_binding(
            &signing,
            &cmd,
            &fence,
            FenceOperationKind::Command,
            &op_id,
            c,
        );
        carried(Some(fence), Some(b))
    };
    let current = || signed_fence(&signing, &cmd, [1; 16], TEST_ENDPOINT_ID, (now + 60, 0));
    let mut newer_rev = cmd.clone();
    newer_rev.expected_revision += 1;
    let other_cmd = signed_service_reload(&signing, node_id, now, now + 60);
    let mixed_b = signed_binding(
        &signing,
        &cmd,
        &signed_fence(&signing, &cmd, [4; 16], TEST_ENDPOINT_ID, (now + 60, 0)),
        FenceOperationKind::Command,
        &cmd.command_id,
        &cap,
    );

    for req in [
        carried(None, None),
        carried(cmd.connection_fence.clone(), None),
        bound(
            signed_fence(&signing, &cmd, [1; 16], TEST_ENDPOINT_ID, (now, 0)),
            cmd.command_id.clone(),
            &cap,
        ),
        bound(
            signed_fence(&signing, &cmd, [1; 16], [8; 32], (now + 60, 0)),
            cmd.command_id.clone(),
            &cap,
        ),
        bound(current(), other_cmd.command_id.clone(), &cap),
        bound(current(), cmd.command_id.clone(), "ocserv.users.write"),
        bound(
            signed_fence(
                &signing,
                &newer_rev,
                [1; 16],
                TEST_ENDPOINT_ID,
                (now + 60, 0),
            ),
            cmd.command_id.clone(),
            &cap,
        ),
        carried(Some(current()), Some(mixed_b)),
    ] {
        assert_permission_denied(&dispatch(req, &node_id, &keys, &fx.adapter).await);
    }
    assert!(!fx.counter.exists());
    let ok = dispatch(command_request(cmd), &node_id, &keys, &fx.adapter).await;
    assert!(matches!(ok.result, Some(privd_response::Result::Mutation(ref r)) if r.applied));
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "x");

    // Artifact read and consume each require their own live fence binding.
    let claims = ArtifactGrantClaimsV1 {
        version: 1,
        key_id: verification_key_id(&signing.verifying_key()),
        node_id,
        artifact_id: *Uuid::now_v7().as_bytes(),
        certificate_id: *Uuid::now_v7().as_bytes(),
        certificate_version: 1,
        operation_id: *Uuid::now_v7().as_bytes(),
        authorized_subject: "user:test".into(),
        purpose: "certificate_p12".into(),
        max_bytes: 4096,
        issued_at_seconds: now - 1,
        issued_at_nanos: 0,
        expires_at_seconds: now + 60,
        expires_at_nanos: 0,
        grant_id: *Uuid::now_v7().as_bytes(),
    };
    let sig = signing.sign(&canonical_artifact_grant_v1(&claims).expect("canonical"));
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
        issued_at: Some(ts(claims.issued_at_seconds, 0)),
        expires_at: Some(ts(claims.expires_at_seconds, 0)),
        grant_id: claims.grant_id.to_vec(),
        signature: sig.to_bytes().to_vec(),
    };
    let mut carrier = signed_service_reload(&signing, node_id, now, now + 60);
    carrier.required_capability = CONNECTION_FENCING_CAPABILITY.into();
    let fence_at_epoch = |lease, owner_epoch| {
        signed_fence_at_epoch(
            &signing,
            &carrier,
            [1; 16],
            TEST_ENDPOINT_ID,
            lease,
            owner_epoch,
        )
    };
    let fence = |lease| fence_at_epoch(lease, 1);
    let art_bound = |f: ConnectionFenceV2, op_id: &[u8]| {
        let b = signed_binding(
            &signing,
            &carrier,
            &f,
            FenceOperationKind::Artifact,
            op_id,
            CONNECTION_FENCING_CAPABILITY,
        );
        (Some(Box::new(f)), Some(Box::new(b)))
    };
    let read = |(cf, fb)| {
        unsigned_request(Some(privd_request::Operation::ArtifactRead(
            ArtifactReadRequest {
                grant: Some(grant.clone()),
                offset: 0,
                connection_fence: cf,
                fence_binding: fb,
            },
        )))
    };
    let consume = |(cf, fb)| {
        unsigned_request(Some(privd_request::Operation::ArtifactConsume(
            ArtifactConsumeRequest {
                grant: Some(grant.clone()),
                sha256: vec![1; 32],
                size: claims.max_bytes,
                confirm_only: false,
                connection_fence: cf,
                fence_binding: fb,
            },
        )))
    };
    for req in [
        read((None, None)),
        consume((None, None)),
        read(art_bound(fence((now, 0)), &claims.artifact_id)),
        read(art_bound(fence((now + 60, 0)), &claims.grant_id)),
        consume(art_bound(fence((now + 60, 0)), &claims.artifact_id)),
    ] {
        assert_permission_denied(&dispatch(req, &node_id, &keys, &fx.adapter).await);
    }
    for req in [
        read(art_bound(fence((now + 60, 0)), &claims.artifact_id)),
        consume(art_bound(fence((now + 60, 0)), &claims.grant_id)),
    ] {
        let resp = dispatch(req, &node_id, &keys, &fx.adapter).await;
        let Some(privd_response::Result::Error(f)) = resp.result else {
            panic!("expected absent lease error")
        };
        assert_ne!(f.kind, i32::from(ErrorKind::PermissionDenied));
    }

    let newer = read(art_bound(
        fence_at_epoch((now + 60, 0), 2),
        &claims.artifact_id,
    ));
    let response = dispatch(newer, &node_id, &keys, &fx.adapter).await;
    let Some(privd_response::Result::Error(failure)) = response.result else {
        panic!("expected absent lease error")
    };
    assert_ne!(failure.kind, i32::from(ErrorKind::PermissionDenied));

    let restarted = Adapter::new(fx.resources.clone(), Limits::default());
    assert_permission_denied(
        &dispatch(
            read(art_bound(fence((now + 60, 0)), &claims.artifact_id)),
            &node_id,
            &keys,
            &restarted,
        )
        .await,
    );
    assert_permission_denied(
        &dispatch(
            consume(art_bound(fence((now + 60, 0)), &claims.grant_id)),
            &node_id,
            &keys,
            &restarted,
        )
        .await,
    );
}

#[tokio::test]
async fn socket_binding_requires_trusted_parent_and_preserves_socket_access() {
    let dir = PathBuf::from("/tmp").join(format!("ocp-socket-{}", Uuid::now_v7().simple()));
    fs::create_dir(&dir).expect("dir");
    let signing = SigningKey::from_bytes(&[42; 32]);
    let mut cfg = ServerConfig {
        socket: dir.join("privd.sock"),
        agent_uid: rustix_uid(),
        node_id: *Uuid::now_v7().as_bytes(),
        endpoint_id: TEST_ENDPOINT_ID,
        command_keys: keyring(&signing),
        attestation_key: Arc::new(signing),
        upgrades: UpgradeScheduler::disabled(),
    };
    for mode in [0o755, 0o750] {
        fs::set_permissions(&dir, fs::Permissions::from_mode(mode)).expect("chmod");
        let listener = bind_socket(&cfg).expect("trusted parent");
        assert_eq!(
            fs::metadata(&cfg.socket).expect("meta").mode() & 0o777,
            0o660
        );
        drop(listener);
    }
    remove_socket(&cfg.socket).expect("rm");
    for mode in [0o775, 0o757, 0o777] {
        fs::set_permissions(&dir, fs::Permissions::from_mode(mode)).expect("chmod");
        assert_eq!(
            bind_socket(&cfg).unwrap_err().kind(),
            io::ErrorKind::PermissionDenied
        );
        assert!(!cfg.socket.exists());
    }
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).expect("chmod");
    if rustix_uid() == 0 {
        chown(&dir, Some(65534), None).expect("chown");
        let res = bind_socket(&cfg);
        chown(&dir, Some(0), None).expect("restore");
        assert_eq!(res.unwrap_err().kind(), io::ErrorKind::PermissionDenied);
    }
    let alias = dir.with_extension("link");
    symlink(&dir, &alias).expect("symlink");
    cfg.socket = alias.join("privd.sock");
    assert_eq!(
        bind_socket(&cfg).unwrap_err().kind(),
        io::ErrorKind::PermissionDenied
    );
    fs::remove_file(alias).expect("rm alias");
    cfg.socket = dir.join("privd.sock");
    fs::write(&cfg.socket, b"do not replace").expect("file");
    assert_eq!(
        bind_socket(&cfg).unwrap_err().kind(),
        io::ErrorKind::AlreadyExists
    );
    assert_eq!(fs::read(&cfg.socket).expect("kept"), b"do not replace");
    fs::remove_file(&cfg.socket).expect("rm");
    symlink(dir.join("missing"), &cfg.socket).expect("dangling");
    assert_eq!(
        bind_socket(&cfg).unwrap_err().kind(),
        io::ErrorKind::AlreadyExists
    );
    fs::remove_file(&cfg.socket).expect("rm");
    fs::remove_dir(dir).expect("rmdir");
}

#[tokio::test]
async fn saturated_clients_wait_for_capacity_and_direct_socket_blocks_forgery() {
    let signing = SigningKey::from_bytes(&[42; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let fx = PrivdFixture::new();
    let cfg = fx.server_config(node_id, keys, [43; 32]);
    let socket = cfg.socket.clone();
    let listener = bind_socket(&cfg).expect("bind");
    let (tx, rx) = tokio::sync::oneshot::channel();
    let srv = tokio::spawn(serve(listener, cfg, fx.adapter.clone(), async {
        let _ = rx.await;
    }));

    // Same-UID forged request is denied before root effect.
    let mut forge = UnixStream::connect(&socket).await.expect("connect");
    let forge_req = unsigned_request(Some(privd_request::Operation::ArtifactRead(
        ArtifactReadRequest::default(),
    )));
    write_frame(&mut forge, &forge_req).await.expect("write");
    assert_permission_denied(
        &read_frame::<PrivdResponse, _>(&mut forge)
            .await
            .expect("read"),
    );
    assert!(!fx.counter.exists());
    drop(forge);

    // Saturated slots queue valid clients instead of resetting.
    let mut held = Vec::with_capacity(MAX_CONCURRENT_CLIENTS);
    for _ in 0..MAX_CONCURRENT_CLIENTS {
        held.push(UnixStream::connect(&socket).await.expect("held"));
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let mut queued = UnixStream::connect(&socket).await.expect("queued");
    let req = unsigned_request(Some(privd_request::Operation::ServiceStatus(
        ReadRequest {},
    )));
    let req_id = req.request_id.clone();
    write_frame(&mut queued, &req).await.expect("write");
    let task = tokio::spawn(async move { read_frame::<PrivdResponse, _>(&mut queued).await });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !task.is_finished(),
        "saturated client was reset instead of queued"
    );
    drop(held.pop());
    let resp = tokio::time::timeout(Duration::from_secs(2), task)
        .await
        .expect("timeout")
        .expect("join")
        .expect("read");
    assert_eq!(resp.request_id, req_id);
    assert!(resp.result.is_some());

    drop(held);
    tx.send(()).expect("stop");
    srv.await.expect("join").expect("serve");
    remove_socket(&socket).expect("rm sock");
}

#[tokio::test]
async fn direct_socket_authorization_and_wire_variants_preserve_effects() {
    let signing = SigningKey::from_bytes(&[13; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let fx = PrivdFixture::new();
    let now = unix_seconds();
    let cmd = signed_service_reload(&signing, node_id, now, now + 60);
    let send = |bytes: Vec<u8>, peer_uid| {
        let (mut client, server) = UnixStream::pair().expect("pair");
        let attestation_key = Arc::new(SigningKey::from_bytes(&[41; 32]));
        let attestation_key_id: Arc<str> =
            Arc::from(verification_key_id(&attestation_key.verifying_key()));
        let task = tokio::spawn(handle_client(
            server,
            peer_uid,
            node_id,
            TEST_ENDPOINT_ID,
            keys.clone(),
            attestation_key,
            attestation_key_id,
            UpgradeScheduler::disabled(),
            fx.adapter.clone(),
        ));
        async move {
            let written = client.write_all(&bytes).await;
            let finished = client.shutdown().await;
            let response: Result<PrivdResponse, _> = read_frame(&mut client).await;
            let status = task.await.expect("no panic");
            if peer_uid == rustix_uid() {
                written.expect("write");
                finished.expect("shutdown");
            }
            (response, status)
        }
    };
    let frame = |body: Vec<u8>| {
        let mut out = u32::try_from(body.len())
            .expect("len")
            .to_be_bytes()
            .to_vec();
        out.extend(body);
        out
    };
    let request = command_request(cmd.clone());
    let mut bad_reqs: Vec<PrivdRequest> = [
        (|c: &mut CommandEnvelope| c.authorization.as_mut().expect("a").signature[0] ^= 1)
            as fn(&mut CommandEnvelope),
        |c| c.action = "config.apply".into(),
        |c| c.required_capability = "ocserv.config.apply".into(),
        |c| c.semantic_payload_sha256[0] ^= 1,
        |c| {
            c.payload = Some(command_envelope::Payload::IpBanRemove(IpBanRemove {
                ip: "192.0.2.1".into(),
            }));
        },
    ]
    .into_iter()
    .map(|f| {
        let mut c = cmd.clone();
        f(&mut c);
        command_request(c)
    })
    .collect();
    for c in [
        signed_service_reload(&signing, *Uuid::now_v7().as_bytes(), now, now + 60),
        signed_service_reload(&signing, node_id, now - 60, now - 1),
        signed_service_reload(&signing, node_id, now + 3600, now + 3660),
    ] {
        bad_reqs.push(command_request(c));
    }
    bad_reqs.push(PrivdRequest {
        privileged_mode: 127,
        ..request.clone()
    });
    bad_reqs.push(PrivdRequest {
        privileged_mode: PrivilegedRequestMode::Reconcile.into(),
        ..request.clone()
    });
    bad_reqs.push(PrivdRequest {
        accepted_at: None,
        ..request.clone()
    });

    for bad in bad_reqs {
        let (resp, status) = send(frame(bad.encode_to_vec()), rustix_uid()).await;
        status.expect("handled");
        let resp = resp.expect("resp");
        assert!(
            matches!(resp.result, Some(privd_response::Result::Error(_)))
                && resp.privileged_result_proof.is_none()
        );
        assert!(!fx.counter.exists() && !fx.dir.join("effects.sqlite3").exists());
    }

    let mut dup_mode = request.encode_to_vec();
    dup_mode.extend([0x40, 0x7f]);
    assert!(matches!(
        send(frame(dup_mode), rustix_uid())
            .await
            .0
            .expect("resp")
            .result,
        Some(privd_response::Result::Error(_))
    ));
    for malformed in [vec![0, 0, 0, 0], vec![0xff; 4], vec![0, 0, 0, 2, 0x80]] {
        let (r, s) = send(malformed, rustix_uid()).await;
        assert!(r.is_err() && s.is_err() && !fx.counter.exists());
    }
    let (r, s) = send(frame(request.encode_to_vec()), rustix_uid() + 1).await;
    assert!(
        r.is_err()
            && s.unwrap_err().kind() == io::ErrorKind::PermissionDenied
            && !fx.counter.exists()
    );

    let mut additive = request.encode_to_vec();
    additive.extend([0xf8, 0x07, 0x01, 0x40, 0x01]);
    let (first, status) = send(frame(additive), rustix_uid()).await;
    status.expect("ok");
    let first = first.expect("first");
    assert!(
        matches!(first.result, Some(privd_response::Result::Mutation(ref r)) if r.applied)
            && first.privileged_result_proof.is_some()
    );
    for replay in [request, reconcile_request(cmd.clone(), &signing)] {
        let (resp, st) = send(frame(replay.encode_to_vec()), rustix_uid()).await;
        st.expect("ok");
        let resp = resp.expect("resp");
        assert_eq!(
            (resp.result, resp.privileged_result_proof),
            (first.result.clone(), first.privileged_result_proof.clone())
        );
        assert_eq!(fs::read(&fx.counter).expect("cnt"), b"x");
    }
    let mut conflict = cmd;
    conflict.expected_revision += 1;
    let claims = claims_from_envelope_v1(&conflict).expect("claims");
    conflict.authorization.as_mut().expect("auth").signature = signing
        .sign(&canonical_v1(&claims).expect("c"))
        .to_bytes()
        .to_vec();
    let (resp, st) = send(
        frame(command_request(conflict).encode_to_vec()),
        rustix_uid(),
    )
    .await;
    st.expect("ok");
    assert!(matches!(
        resp.expect("r").result,
        Some(privd_response::Result::Error(_))
    ));
    assert_eq!(fs::read(&fx.counter).expect("cnt"), b"x");
}

#[tokio::test]
async fn upgrade_result_list_surfaces_durable_outcomes_read_only() {
    let signing = SigningKey::from_bytes(&[7; 32]);
    let keys = keyring(&signing);
    let node_id = *Uuid::now_v7().as_bytes();
    let adapter = Adapter::new(FixedResources::default(), Limits::default());
    let dir = std::env::temp_dir().join(format!("ocservia-privd-upgrade-read-{}", Uuid::now_v7()));
    fs::create_dir(&dir).expect("dir");
    let ops = dir.join("var/lib/ocservia-upgrade/operations");
    fs::create_dir_all(&ops).expect("ops");
    let scheduler = UpgradeScheduler::new(ops, UpgradeTrigger::Disabled);
    let intent = UpgradeIntent::new(
        *Uuid::now_v7().as_bytes(),
        *Uuid::now_v7().as_bytes(),
        "2.0.0",
        [0x43; 32],
        host_architecture(),
        [0x55; 32],
    )
    .expect("intent");
    scheduler.schedule_and_trigger(&intent).expect("schedule");
    assert!(
        ocservia_upgrader::UpgradeRunner::new(dir.clone())
            .run(&intent.operation_id.to_string())
            .is_err()
    );

    let req = unsigned_request(Some(privd_request::Operation::UpgradeResultList(
        UpgradeResultListRequest {},
    )));
    let attestation_key = Arc::new(SigningKey::from_bytes(&[41; 32]));
    let attestation_key_id: Arc<str> =
        Arc::from(verification_key_id(&attestation_key.verifying_key()));
    let resp = dispatch_attested(
        &req,
        &node_id,
        &TEST_ENDPOINT_ID,
        &keys,
        &attestation_key,
        &attestation_key_id,
        &scheduler,
        &adapter,
    )
    .await;
    let Some(privd_response::Result::UpgradeResultList(list)) = resp.result else {
        panic!("expected upgrade result list, got {:?}", resp.result);
    };
    assert_eq!(list.results.len(), 1);
    assert_eq!(
        (
            list.results[0].operation_id.as_slice(),
            list.results[0].state.as_str(),
            list.results[0].target_version.as_str()
        ),
        (intent.operation_id.as_bytes().as_slice(), "failed", "2.0.0")
    );
    let proof = list.results[0]
        .privileged_result_proof
        .as_ref()
        .expect("proof");
    assert_eq!(
        (
            proof.node_id.as_slice(),
            proof.operation_id.as_slice(),
            proof.package_sha256.as_slice()
        ),
        (
            node_id.as_slice(),
            intent.operation_id.as_bytes().as_slice(),
            intent.package_sha256.as_slice()
        )
    );
    assert_eq!(
        (proof.result_sha256.len(), proof.signature.len()),
        (32, ed25519_dalek::SIGNATURE_LENGTH)
    );
    fs::remove_dir_all(&dir).expect("cleanup");
}
