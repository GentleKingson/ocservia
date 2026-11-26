#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::os::unix::fs::symlink;

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_agent_protocol::{
    ArtifactReadRequest, PrivilegedRequestMode, ReadRequest, privd_request,
};
use ocservia_command_authorization::{canonical_v1, claims_from_envelope_v1};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope, IpBanRemove, SealedSecretPurpose, SealedSecretV1,
    ServiceReload, UserCreate, command_envelope,
};
use ocservia_ocserv_adapter::{FixedResources, Limits};
use ocservia_test_support::{
    deadline_ms, keyring, sign_command_v2, sign_session_grant, unix_seconds,
};

use super::*;

const TEST_ENDPOINT_ID: [u8; 32] = [9; 32];

async fn dispatch(
    request: PrivdRequest,
    node_id: &[u8; 16],
    keys: &ControllerCommandKeyring,
    adapter: &Adapter,
) -> PrivdResponse {
    let ak = Arc::new(SigningKey::from_bytes(&[41; 32]));
    let ctx = PrivdContext {
        exec_permits: Semaphore::new(MAX_CONCURRENT_CLIENTS),
        agent_uid: rustix_uid(),
        node_id: *node_id,
        endpoint_id: TEST_ENDPOINT_ID,
        command_keys: keys.clone(),
        attestation_key_id: Arc::from(key_id(&ak.verifying_key())),
        attestation_key: ak,
        upgrades: UpgradeScheduler::disabled(),
        adapter: adapter.clone(),
    };
    Box::pin(dispatch_attested(&request, &ctx)).await
}

struct PrivdFixture {
    dir: ocservia_test_support::PrivateTempDir,
    adapter: Adapter,
    resources: FixedResources,
    counter: PathBuf,
}

impl PrivdFixture {
    fn new() -> Self {
        let dir = ocservia_test_support::PrivateTempDir::new("ocp");
        let (counter, systemctl) = (dir.join("reload-count"), dir.join("systemctl"));
        fs::write(
            &systemctl,
            format!("#!/bin/sh\nprintf x >> '{}'\n", counter.display()),
        )
        .expect("sc");
        fs::set_permissions(&systemctl, fs::Permissions::from_mode(0o700)).expect("chmod");
        let (config, boot) = (dir.join("ocserv.conf"), dir.join("boot_id"));
        fs::write(&config, b"fixture\n").expect("cfg");
        fs::write(&boot, Uuid::now_v7().to_string()).expect("boot");
        let res = FixedResources::new(
            systemctl,
            PathBuf::from("/bin/true"),
            PathBuf::from("/bin/true"),
            config,
            boot,
        )
        .expect("res")
        .with_effect_store(dir.join("effects.sqlite3"), dir.join("effects.key"))
        .expect("eff");
        Self {
            dir,
            adapter: Adapter::new(res.clone(), Limits::default()),
            resources: res,
            counter,
        }
    }
    fn server_config(&self, node_id: [u8; 16], keys: ControllerCommandKeyring) -> ServerConfig {
        ServerConfig {
            socket: self.dir.join("privd.sock"),
            agent_uid: rustix_uid(),
            node_id,
            endpoint_id: TEST_ENDPOINT_ID,
            command_keys: keys,
            attestation_key: Arc::new(SigningKey::from_bytes(&[43; 32])),
            upgrades: UpgradeScheduler::disabled(),
        }
    }
}

fn reload_cmd(key: &SigningKey, node_id: [u8; 16], issued: i64, expires: i64) -> CommandEnvelope {
    sign_command_v2(
        key,
        node_id,
        issued,
        expires,
        7,
        "service.reload",
        "ocserv.service.reload",
        command_envelope::Payload::ServiceReload(ServiceReload {}),
    )
}

fn resign(
    mut cmd: CommandEnvelope,
    key: &SigningKey,
    mode: CommandDeliveryMode,
) -> CommandEnvelope {
    cmd.delivery_mode = mode.into();
    cmd.message_id = Uuid::now_v7().as_bytes().to_vec();
    let claims = claims_from_envelope_v1(&cmd).expect("claims");
    cmd.authorization.as_mut().expect("auth").signature = key
        .sign(&canonical_v1(&claims).expect("canon"))
        .to_bytes()
        .to_vec();
    cmd
}

fn req_epoch(command: CommandEnvelope, key: &SigningKey, epoch: u64) -> PrivdRequest {
    let (issued, expires) = (
        command.issued_at.expect("i").seconds,
        command.expires_at.expect("e").seconds,
    );
    let grant = sign_session_grant(
        key,
        command.node_id.as_slice().try_into().expect("n"),
        TEST_ENDPOINT_ID,
        command.expected_revision,
        epoch,
        vec![command.required_capability.clone()],
        issued - 1,
        expires.max(unix_seconds() + 60),
    );
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        accepted_at: command.issued_at,
        authorization_command: Some(command),
        privileged_mode: PrivilegedRequestMode::Execute.into(),
        session_grant: Some(Box::new(grant)),
        ..PrivdRequest::default()
    }
}

fn unsigned(operation: Option<privd_request::Operation>) -> PrivdRequest {
    PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        operation,
        ..PrivdRequest::default()
    }
}

fn assert_denied(resp: &PrivdResponse) {
    let Some(privd_response::Result::Error(f)) = resp.result.as_ref() else {
        panic!("expected error");
    };
    assert_eq!(
        ErrorKind::try_from(f.kind).unwrap_or(ErrorKind::Unspecified),
        ErrorKind::PermissionDenied
    );
}

#[tokio::test]
async fn rejects_expired_unsigned_tampered_and_cross_purpose_before_root_effect() {
    let (trusted, unknown) = (
        SigningKey::from_bytes(&[7; 32]),
        SigningKey::from_bytes(&[11; 32]),
    );
    let (keys, node_id, now, fx) = (
        keyring(&trusted),
        *Uuid::now_v7().as_bytes(),
        unix_seconds(),
        PrivdFixture::new(),
    );

    assert_denied(&dispatch(unsigned(None), &node_id, &keys, &fx.adapter).await);
    assert_denied(
        &dispatch(
            unsigned(Some(privd_request::Operation::ArtifactRead(
                ArtifactReadRequest::default(),
            ))),
            &node_id,
            &keys,
            &fx.adapter,
        )
        .await,
    );
    for bad in [
        reload_cmd(&trusted, node_id, now - 60, now - 1),
        reload_cmd(&trusted, *Uuid::now_v7().as_bytes(), now, now + 60),
        reload_cmd(&unknown, node_id, now, now + 60),
    ] {
        assert_denied(&dispatch(req_epoch(bad, &trusted, 1), &node_id, &keys, &fx.adapter).await);
    }
    let mut forged = reload_cmd(&trusted, node_id, now, now + 60);
    forged.payload = Some(command_envelope::Payload::IpBanRemove(IpBanRemove {
        ip: "192.0.2.1".into(),
    }));
    assert_denied(&dispatch(req_epoch(forged, &trusted, 1), &node_id, &keys, &fx.adapter).await);

    let mut bad_user = sign_command_v2(
        &trusted,
        node_id,
        now,
        now + 60,
        1,
        "user.create",
        "ocserv.users.write",
        command_envelope::Payload::UserCreate(UserCreate {
            username: "alice".into(),
            desired_revision: 1,
            sealed_password_v1: Some(SealedSecretV1 {
                version: 1,
                purpose: SealedSecretPurpose::UserPassword as i32,
                key_id: "user-key-v1".into(),
                ciphertext: vec![0xa5; 256],
            }),
        }),
    );
    if let Some(command_envelope::Payload::UserCreate(p)) = bad_user.payload.as_mut() {
        p.sealed_password_v1.as_mut().expect("s").purpose =
            SealedSecretPurpose::CertificateP12Password as i32;
    }
    assert_denied(
        &dispatch(
            req_epoch(bad_user, &trusted, 1),
            &node_id,
            &keys,
            &fx.adapter,
        )
        .await,
    );
    assert!(!fx.counter.exists());
}

#[tokio::test]
async fn signed_effect_executes_once_replays_and_enforces_owner_epoch_floor() {
    let signing = SigningKey::from_bytes(&[8; 32]);
    let (keys, node_id, now, fx) = (
        keyring(&signing),
        *Uuid::now_v7().as_bytes(),
        unix_seconds(),
        PrivdFixture::new(),
    );
    let cmd = reload_cmd(&signing, node_id, now, now + 60);

    let first = dispatch(
        req_epoch(cmd.clone(), &signing, 1),
        &node_id,
        &keys,
        &fx.adapter,
    )
    .await;
    assert!(matches!(first.result, Some(privd_response::Result::Mutation(ref r)) if r.applied));
    let proof = first
        .privileged_result_proof
        .clone()
        .expect("attested receipt");
    ocservia_privd_attestation::verify_receipt(
        &proof,
        &SigningKey::from_bytes(&[41; 32]).verifying_key(),
    )
    .expect("receipt");

    let restarted = Adapter::new(fx.resources.clone(), Limits::default());
    let reconcile = PrivdRequest {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        deadline_unix_ms: deadline_ms(5_000),
        authorization_command: Some(resign(
            cmd.clone(),
            &signing,
            CommandDeliveryMode::ReconcileOnly,
        )),
        privileged_mode: PrivilegedRequestMode::Reconcile.into(),
        ..PrivdRequest::default()
    };
    let retry = req_epoch(
        resign(
            cmd.clone(),
            &signing,
            CommandDeliveryMode::RetryIfEffectAbsent,
        ),
        &signing,
        1,
    );
    for req in [reconcile, req_epoch(cmd, &signing, 1), retry] {
        let replay = dispatch(req, &node_id, &keys, &restarted).await;
        assert_eq!(
            (replay.privileged_result_proof, replay.result),
            (Some(proof.clone()), first.result.clone())
        );
    }
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "x");

    let newer = reload_cmd(&signing, node_id, now, now + 60);
    assert!(
        matches!(dispatch(req_epoch(newer, &signing, 2), &node_id, &keys, &restarted).await.result, Some(privd_response::Result::Mutation(ref r)) if r.applied)
    );
    let stale = reload_cmd(&signing, node_id, now, now + 60);
    assert_denied(&dispatch(req_epoch(stale, &signing, 1), &node_id, &keys, &restarted).await);
    assert_eq!(fs::read_to_string(&fx.counter).expect("counter"), "xx");
}

#[tokio::test]
async fn grants_and_socket_binding_enforce_trust_boundaries() {
    let signing = SigningKey::from_bytes(&[12; 32]);
    let (keys, node_id, now, fx) = (
        keyring(&signing),
        *Uuid::now_v7().as_bytes(),
        unix_seconds(),
        PrivdFixture::new(),
    );
    let cmd = reload_cmd(&signing, node_id, now, now + 60);

    for grant in [
        None,
        Some(sign_session_grant(
            &signing,
            node_id,
            TEST_ENDPOINT_ID,
            cmd.expected_revision,
            1,
            vec![cmd.required_capability.clone()],
            now - 60,
            now - 1,
        )),
        Some(sign_session_grant(
            &signing,
            node_id,
            TEST_ENDPOINT_ID,
            cmd.expected_revision,
            1,
            vec!["ocserv.users.write".into()],
            now - 1,
            now + 60,
        )),
    ] {
        let req = PrivdRequest {
            session_grant: grant.map(Box::new),
            ..req_epoch(cmd.clone(), &signing, 1)
        };
        assert_denied(&dispatch(req, &node_id, &keys, &fx.adapter).await);
    }

    let mut cfg = fx.server_config(node_id, keys);
    let alias = fx.dir.join("alias");
    symlink(fx.dir.path(), &alias).expect("symlink");
    cfg.socket = alias.join("privd.sock");
    assert_eq!(
        bind_socket(&cfg).unwrap_err().kind(),
        io::ErrorKind::PermissionDenied
    );

    cfg.socket = fx.dir.join("privd.sock");
    let socket = cfg.socket.clone();
    let listener = bind_socket(&cfg).expect("bind");
    let (tx, rx) = tokio::sync::oneshot::channel();
    let srv = tokio::spawn(serve(listener, cfg, fx.adapter.clone(), async {
        let _ = rx.await;
    }));
    let mut forge = UnixStream::connect(&socket).await.expect("connect");
    write_frame(
        &mut forge,
        &unsigned(Some(privd_request::Operation::ArtifactRead(
            ArtifactReadRequest::default(),
        ))),
    )
    .await
    .expect("write");
    assert_denied(
        &read_frame::<PrivdResponse, _>(&mut forge)
            .await
            .expect("read"),
    );
    let mut valid = UnixStream::connect(&socket).await.expect("valid");
    let req = unsigned(Some(privd_request::Operation::ServiceStatus(
        ReadRequest {},
    )));
    let req_id = req.request_id.clone();
    write_frame(&mut valid, &req).await.expect("write");
    let resp = tokio::time::timeout(
        Duration::from_secs(2),
        read_frame::<PrivdResponse, _>(&mut valid),
    )
    .await
    .expect("fast")
    .expect("resp");
    assert_eq!(resp.request_id, req_id);
    tx.send(()).expect("stop");
    srv.await.expect("join").expect("serve");
    remove_socket(&socket).expect("rm");
}
