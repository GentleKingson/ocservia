#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::os::unix::fs::PermissionsExt as _;
use std::path::PathBuf;
use std::time::{Duration, SystemTime};

use ed25519_dalek::{Signature, SigningKey, Verifier as _, VerifyingKey};
use iroh::EndpointId;
use iroh::endpoint::RelayMode;
use ocservia_agent::{ExternalEffectObservation, PrivdClient};
use ocservia_agent_protocol::{
    ConfigApplyResult, DesiredEffectObservation, DesiredEffectState, ErrorKind, OcservVersion,
    PrivdRequest, PrivdResponse, PrivilegedRequestMode, privd_request, privd_response, read_frame,
    write_frame,
};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, CompleteConfigApply, CompleteConfigPlan, EnrollRequest, HandshakeResult,
    SealedSecretPurpose, SealingKeyDescriptorV1, SessionHandshakeResponse, UserPasswordRotate,
    command_envelope,
};
use ocservia_contracts::session::READ_ONLY_SESSION_CAPABILITIES;
use ocservia_test_support::{
    PrivateTempDir, keyring, sign_session_grant, ts, user_password_secret as test_user_password,
};
use uuid::Uuid;

use crate::commands::{
    map_external_effect, observe_external_effect_with_reconcile, response_proof,
    settle_privd_response, terminal_privd_error_code,
};
use crate::session::{
    activate_session_epoch, negotiate_session_mode, negotiate_session_mode_at_instant,
    refresh_fence_epoch_floor, session_capabilities, supported_capabilities,
};
use crate::startup::{
    Config, build_relay_mode, build_sealing_keys, load_controller_command_keys, prepare_enrollment,
    read_enrollment_token, validate_enrollment_mode,
};

const NOW: i64 = 1_700_000_050;

#[test]
fn only_successful_config_apply_advances_the_applied_revision() {
    use ocservia_agent::{CommandContext, CommandExecutor, ExternalPreparation};
    use ocservia_command_journal::CommandState;
    use ocservia_contracts::config_profile::APPLY_CAPABILITY;
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        CommandDeliveryMode, PrivdResultReceiptV1, PrivilegedResultProof,
    };
    use ocservia_test_support::{sign_command_envelope_v2, sign_command_v2};
    use prost::Message as _;

    let fixture: serde_json::Value =
        serde_json::from_str(include_str!("../../../../testdata/complete-config-v1.json"))
            .expect("config fixture");
    let bytes = hex::decode(fixture["apply_proto"].as_str().expect("apply fixture")).expect("hex");
    let fixture = CommandEnvelope::decode(bytes.as_slice()).expect("fixture command");
    let node = fixture.node_id.as_slice().try_into().expect("node");
    let payload = fixture.payload.expect("config payload");
    let signing = SigningKey::from_bytes(&[49; 32]);
    let context = CommandContext {
        node_id: node,
        authorization_revision: fixture.expected_revision,
        capabilities: [APPLY_CAPABILITY.to_owned()].into(),
        session_expires_at_unix_seconds: NOW + 300,
        command_keys: keyring(&signing),
        now_unix_seconds: NOW,
        cancelled: false,
    };

    for (rolled_back, failed_critical, applied_revision) in [
        (true, false, 8),
        (false, true, 8),
        (false, false, 0),
        (false, false, 8),
    ] {
        let directory = PrivateTempDir::new("config-result");
        let mut executor = CommandExecutor::new(
            Journal::open(&directory.join("journal.sqlite3")).expect("open journal"),
        );
        let mut envelope = sign_command_v2(
            &signing,
            node,
            NOW,
            NOW + 60,
            fixture.expected_revision,
            "config.apply",
            APPLY_CAPABILITY,
            payload.clone(),
        );
        envelope.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
        sign_command_envelope_v2(&signing, &mut envelope);
        let ExternalPreparation::Execute(command) = executor
            .begin_attested_recovery(&envelope, &context)
            .expect("recover")
        else {
            panic!("resume configuration command");
        };
        let response = PrivdResponse {
            result: Some(privd_response::Result::ConfigApply(ConfigApplyResult {
                rolled_back,
                failed_critical,
                applied_revision,
                ..ConfigApplyResult::default()
            })),
            privileged_result_proof: Some(PrivilegedResultProof {
                version: 1,
                receipt_v1: Some(PrivdResultReceiptV1 {
                    receipt_version: 1,
                    completed_at: Some(ts(NOW + 1, 0)),
                    ..PrivdResultReceiptV1::default()
                }),
                signature: vec![0; 64],
            }),
            ..PrivdResponse::default()
        };
        let result = settle_privd_response(&mut executor, &command, &envelope, response, NOW + 1)
            .expect("settle attested response");
        assert_eq!(result.record.state, CommandState::Succeeded);
        assert_eq!(
            executor
                .journal()
                .applied_revision("config", "ocserv.conf")
                .expect("revision"),
            (!rolled_back && !failed_critical && applied_revision != 0).then_some(applied_revision)
        );
    }
}

fn test_sealing_keys() -> Vec<SealingKeyDescriptorV1> {
    vec![
        SealingKeyDescriptorV1 {
            version: 1,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "node-key-1".into(),
            public_key_sha256: vec![0x11; 32],
        },
        SealingKeyDescriptorV1 {
            version: 1,
            purpose: SealedSecretPurpose::CertificateP12Password as i32,
            key_id: "node-p12-key-1".into(),
            public_key_sha256: vec![0x22; 32],
        },
    ]
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
    }
}

fn test_key() -> SigningKey {
    SigningKey::from_bytes(&[7; 32])
}

fn test_keyring() -> ControllerCommandKeyring {
    keyring(&test_key())
}

fn test_endpoint() -> EndpointId {
    iroh::SecretKey::from_bytes(&[9; 32]).public()
}

#[allow(clippy::too_many_arguments)]
fn signed_handshake_response(
    signing: &SigningKey,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    authorization_revision: u64,
    session_epoch: u64,
    capabilities: Vec<String>,
    issued_at: i64,
    expires_at: i64,
) -> SessionHandshakeResponse {
    let grant = sign_session_grant(
        signing,
        *node_id.as_bytes(),
        *endpoint_id.as_bytes(),
        authorization_revision,
        session_epoch,
        capabilities.clone(),
        issued_at,
        expires_at,
    );
    SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".into(),
        negotiated_capabilities: capabilities,
        session_grant: Some(grant),
    }
}

fn serve_privd_once<F>(handle: F) -> (PrivateTempDir, PathBuf, tokio::task::JoinHandle<()>)
where
    F: FnOnce(PrivdRequest) -> privd_response::Result + Send + 'static,
{
    let dir = PrivateTempDir::new("ocsa");
    let socket = dir.join("privd.sock");
    let listener = tokio::net::UnixListener::bind(&socket).expect("bind privd fixture");
    let task = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.expect("accept");
        let req: PrivdRequest = read_frame(&mut stream).await.expect("read req");
        let id = req.request_id.clone();
        let res = handle(req);
        write_frame(
            &mut stream,
            &PrivdResponse {
                request_id: id,
                privileged_result_proof: None,
                result: Some(res),
            },
        )
        .await
        .expect("write resp");
    });
    (dir, socket, task)
}

#[test]
fn overlapping_agent_floor_and_handshake_activation_enforce_monotonic_epoch() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = vec!["synthetic.noop".to_owned()];

    // Overlapping journal processes: successor raises floor, old cached floor refreshes.
    let dir = PrivateTempDir::new("floor-overlap");
    let jpath = dir.join("journal.db");
    let active_agent = Journal::open(&jpath).expect("active");
    let successor = Journal::open(&jpath).expect("successor");
    assert_eq!(
        active_agent.raise_owner_fence_epoch_floor(5).expect("e5"),
        5
    );
    let mut cached = 5_u64;
    assert_eq!(
        successor.raise_owner_fence_epoch_floor(10).expect("e10"),
        10
    );
    refresh_fence_epoch_floor(&active_agent, &mut cached).expect("refresh");
    assert_eq!(cached, 10);
    assert_eq!(active_agent.owner_fence_epoch_floor().expect("floor"), 10);
    drop((successor, active_agent));

    // Handshake persists new session_epoch before activation and rejects replay across reopen.
    let dir2 = PrivateTempDir::new("handshake-epoch");
    let jpath2 = dir2.join("journal.db");
    let mut journal = Journal::open(&jpath2).expect("journal");
    journal.raise_owner_fence_epoch_floor(7).expect("preseed");
    let mut floor = 7_u64;
    let resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        3,
        8,
        caps,
        1_700_000_000,
        1_700_000_300,
    );
    let authority =
        negotiate_session_mode_at_instant(&resp, &supported_capabilities(), node, ep, &kr, NOW)
            .expect("authority");
    assert_eq!(authority.session_epoch, 8);
    activate_session_epoch(&authority, &mut journal, &mut floor).expect("activate");
    assert_eq!(floor, 8);
    assert!(activate_session_epoch(&authority, &mut journal, &mut floor).is_err());
    drop(journal);
    assert_eq!(
        Journal::open(&jpath2)
            .expect("reopen")
            .owner_fence_epoch_floor()
            .expect("f"),
        8
    );
}

#[test]
fn invalid_session_handshakes_do_not_advance_the_durable_epoch() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = vec!["synthetic.noop".to_owned()];
    let mk_resp = |epoch: u64, issued: i64, expires: i64| {
        signed_handshake_response(
            &signing,
            &node,
            &ep,
            3,
            epoch,
            caps.clone(),
            issued,
            expires,
        )
    };
    let mut bad_sig = mk_resp(11, 1_700_000_000, 1_700_000_300);
    bad_sig.session_grant.as_mut().expect("grant").signature[0] ^= 1;

    let cases = vec![
        ("lower epoch", mk_resp(9, 1_700_000_000, 1_700_000_300)),
        ("equal epoch", mk_resp(10, 1_700_000_000, 1_700_000_300)),
        ("expired grant", mk_resp(11, 1_700_000_000, NOW)),
        ("bad signature", bad_sig),
        (
            "node mismatch",
            signed_handshake_response(
                &signing,
                &Uuid::now_v7(),
                &ep,
                3,
                11,
                caps.clone(),
                1_700_000_000,
                1_700_000_300,
            ),
        ),
        (
            "endpoint mismatch",
            signed_handshake_response(
                &signing,
                &node,
                &iroh::SecretKey::from_bytes(&[8; 32]).public(),
                3,
                11,
                caps,
                1_700_000_000,
                1_700_000_300,
            ),
        ),
    ];

    let dir = PrivateTempDir::new("epoch-negative");
    let jpath = dir.join("journal.db");
    let mut journal = Journal::open(&jpath).expect("journal");
    journal.raise_owner_fence_epoch_floor(10).expect("preseed");
    let mut floor = 10_u64;

    for (name, resp) in cases {
        if let Ok(authority) =
            negotiate_session_mode_at_instant(&resp, &supported_capabilities(), node, ep, &kr, NOW)
        {
            assert!(
                activate_session_epoch(&authority, &mut journal, &mut floor).is_err(),
                "{name} must fail closed"
            );
        }
        assert_eq!(floor, 10, "{name} changed in-memory floor");
        assert_eq!(
            journal.owner_fence_epoch_floor().expect("durable"),
            10,
            "{name} changed durable floor"
        );
    }
    drop(journal);
    assert_eq!(
        Journal::open(&jpath)
            .expect("reopen")
            .owner_fence_epoch_floor()
            .expect("f"),
        10
    );
}

#[test]
fn agent_protocol_session_grant_validation() {
    let node = Uuid::now_v7();
    let ep = iroh::SecretKey::generate().public();
    let signing = test_key();
    let kr = test_keyring();

    // Grantless v1.0 and grantless v1.1 all rejected.
    for (minor, caps) in [
        (
            0,
            READ_ONLY_SESSION_CAPABILITIES
                .iter()
                .map(|c| (*c).to_owned())
                .collect(),
        ),
        (0, vec!["synthetic.noop".to_owned()]),
        (1, vec!["ocserv.status.read".to_owned()]),
    ] {
        let resp = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: minor,
            max_message_size: 1024 * 1024,
            controller_version: "test".into(),
            negotiated_capabilities: caps,
            session_grant: None,
        };
        assert!(negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).is_err());
    }

    // Invalid v1.1 grant signature rejected; valid signature accepted.
    let now_sec = ocservia_test_support::unix_seconds();
    let mut resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        1,
        1,
        vec!["ocserv.status.read".into()],
        now_sec - 1,
        now_sec + 30,
    );
    let good_sig = resp
        .session_grant
        .as_ref()
        .expect("grant")
        .signature
        .clone();
    resp.session_grant.as_mut().expect("grant").signature = vec![0; 64];
    assert!(negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).is_err());
    resp.session_grant.as_mut().expect("grant").signature = good_sig;
    let authority =
        negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).expect("valid");
    assert_eq!(authority.session_epoch, 1);
}

#[tokio::test]
async fn session_capabilities_withhold_terminate_below_ocserv_1_4_2() {
    for (version, terminate) in [(Some("1.2.4"), false), (Some("1.4.2"), true), (None, false)] {
        let (_dir, socket, server) = serve_privd_once(move |req| {
            assert!(matches!(
                req.operation,
                Some(privd_request::Operation::OcservVersion(_))
            ));
            version.map_or_else(
                || privd_response::Result::Error(ocservia_agent_protocol::PrivdError::default()),
                |v| privd_response::Result::OcservVersion(OcservVersion { version: v.into() }),
            )
        });
        let privd = PrivdClient::new(socket, Duration::from_secs(2)).expect("client");
        let caps = session_capabilities(&privd).await;
        server.await.expect("server");
        let mut expected = supported_capabilities();
        expected.retain(|c| terminate || c != "ocserv.session.terminate");
        assert_eq!(caps, expected, "{version:?}");
    }
}

#[test]
fn startup_key_enrollment_and_token_requirements() {
    let cfg = command_key_config(false);
    assert!(load_controller_command_keys(&cfg).is_err());
    assert!(
        load_controller_command_keys(&command_key_config(true))
            .expect("probe")
            .is_none()
    );

    // Two distinct password sealing keys required.
    assert!(build_sealing_keys(&cfg, None, None, None, None).is_err());
    assert!(
        build_sealing_keys(
            &cfg,
            Some("s".into()),
            Some("11".repeat(32)),
            Some("s".into()),
            Some("22".repeat(32))
        )
        .is_err()
    );
    assert!(
        build_sealing_keys(
            &cfg,
            Some("u".into()),
            Some("11".repeat(32)),
            Some("p".into()),
            Some("11".repeat(32))
        )
        .is_err()
    );

    // Enrollment mode requires no command keys.
    let mut enroll = command_key_config(false);
    enroll.enrollment_token_file = Some(PathBuf::from("/run/secrets/enrollment-token"));
    enroll.enrollment_environment = Some("production".into());
    assert!(
        load_controller_command_keys(&enroll)
            .expect("enroll")
            .is_none()
    );

    // Fresh enrollment preparation reuses endpoint key for proof.
    let prep_dir = PrivateTempDir::new("prepare");
    let identity_dir = prep_dir.path().join("identity");
    let controller = iroh::SecretKey::generate().public();
    let mut prep = command_key_config(false);
    prep.identity_dir = identity_dir.clone();
    prep.controller = Some(controller);
    prep.prepare_enrollment = true;
    validate_enrollment_mode(&prep).expect("prep mode");
    assert!(
        load_controller_command_keys(&prep)
            .expect("no cmd keys")
            .is_none()
    );
    let ep_hex = prepare_enrollment(&prep).expect("prepare");
    assert_eq!(ep_hex.len(), 64);
    assert!(
        ep_hex
            .bytes()
            .all(|b| b.is_ascii_digit() || matches!(b, b'a'..=b'f'))
    );
    let identity = crate::identity::Identity::provision(&identity_dir, controller).expect("reload");
    assert_eq!(ep_hex, hex::encode(identity.endpoint_id().as_bytes()));
    let mut req = EnrollRequest {
        token: "a".repeat(43),
        endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
        agent_version: "test".into(),
        os_release: "test".into(),
        ocserv_version: "test".into(),
        boot_id: "boot".into(),
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: supported_capabilities(),
        environment: "production".into(),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
        time: Some(SystemTime::now().into()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: prep.sealing_keys.clone(),
    };
    identity.authorize_enrollment(&mut req).expect("authorize");
    let canonical =
        ocservia_command_authorization::enrollment_canonical_v1(&req).expect("canonical");
    let sig = Signature::from_slice(&req.proof.expect("proof").signature).expect("sig");
    VerifyingKey::from_bytes(identity.endpoint_id().as_bytes())
        .expect("vk")
        .verify(&canonical, &sig)
        .expect("verify");

    // Enrollment token file requires strict mode & format.
    let tok_dir = PrivateTempDir::new("enrollment");
    let token = tok_dir.join("enrollment.token");
    let valid = "0".repeat(43);
    let valid_token = format!("obt2_{valid}");
    std::fs::write(&token, format!("{valid_token}\r\n")).expect("write");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("mode");
    assert_eq!(read_enrollment_token(&token).expect("valid"), valid_token);
    for bad in [
        valid.clone(),
        format!("obt1_{valid}"),
        format!("oit1_{valid}"),
        format!("obt3_{valid}"),
        format!("obt2_{valid}extra"),
        "obt2_short".to_owned(),
    ] {
        std::fs::write(&token, bad).expect("bad");
        assert!(read_enrollment_token(&token).is_err());
    }
    std::fs::write(&token, format!("obt2_{valid}")).expect("restore");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644)).expect("insecure");
    assert!(read_enrollment_token(&token).is_err());
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("restore mode");
    let token_link = tok_dir.join("enrollment.link");
    std::os::unix::fs::symlink(&token, &token_link).expect("symlink enrollment token");
    assert!(read_enrollment_token(&token_link).is_err());
}

#[test]
fn custom_relay_count_and_security_contract() {
    let dir = PrivateTempDir::new("relay-contract");
    let token = dir.join("token");
    std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("token");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("mode");
    let token_link = dir.join("token.link");
    std::os::unix::fs::symlink(&token, &token_link).expect("symlink relay token");
    assert!(
        build_relay_mode(
            "custom",
            Some("https://relay-0.example.test"),
            Some(&token_link)
        )
        .is_err()
    );

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
            "{invalid_url:?}"
        );
    }
    let url = "https://relay.example.test";
    assert!(build_relay_mode("custom", Some(url), None).is_err());
    assert!(build_relay_mode("custom", Some(url), Some(&dir.join("missing"))).is_err());
    assert!(build_relay_mode("disabled", None, None).is_ok());
    assert!(build_relay_mode("disabled", Some(url), None).is_err());
    assert!(build_relay_mode("disabled", None, Some(&token)).is_err());
    assert!(build_relay_mode("default", None, None).is_err());

    for invalid_token in ["short", "0123456789abcdef 123456789abcdef0"] {
        std::fs::write(&token, invalid_token).expect("bad token");
        assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
    }
    std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("valid");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644)).expect("mode");
    assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
}

#[test]
fn privd_error_receipt_and_unexpected_result_classification() {
    assert_eq!(
        terminal_privd_error_code(ErrorKind::InvalidRequest.into()),
        Some("privd_rejected")
    );
    assert_eq!(
        terminal_privd_error_code(ErrorKind::CapacityExceeded.into()),
        Some("capacity_exceeded")
    );
    for non_terminal in [
        ErrorKind::OutputLimit,
        ErrorKind::Unavailable,
        ErrorKind::CommandFailed,
    ] {
        assert!(terminal_privd_error_code(non_terminal.into()).is_none());
    }

    let payload = command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
        username: "alice".into(),
        desired_revision: 7,
        sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
    });
    let mutation =
        privd_response::Result::Mutation(ocservia_agent_protocol::MutationResult { applied: true });
    assert_eq!(
        map_external_effect(&payload, mutation.clone()),
        ExternalEffectObservation::Unknown
    );

    let response = PrivdResponse {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        privileged_result_proof: None,
        result: Some(mutation),
    };
    assert!(response_proof(&response).is_none());
}

#[tokio::test]
async fn reconcile_observes_password_and_config_effects_and_retries_plan() {
    // Password reconcile uses non-secret authoritative effect store.
    let (_dir, socket, server) = serve_privd_once(|req| {
        assert!(req.operation.is_none());
        assert_eq!(
            PrivilegedRequestMode::try_from(req.privileged_mode).expect("mode"),
            PrivilegedRequestMode::Reconcile
        );
        let Some(CommandEnvelope {
            payload: Some(command_envelope::Payload::UserPasswordRotate(obs)),
            ..
        }) = req.authorization_command
        else {
            panic!("desired effect observation required");
        };
        assert_eq!((obs.username.as_str(), obs.desired_revision), ("alice", 7));
        privd_response::Result::DesiredEffectObservation(DesiredEffectObservation {
            state: DesiredEffectState::AppliedExact.into(),
            observed_revision: obs.desired_revision,
        })
    });
    let client = PrivdClient::new(socket, Duration::from_secs(2)).expect("client");
    let pw_env = CommandEnvelope {
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        semantic_payload_sha256: vec![0x5a; 32],
        expires_at: Some(ts(i64::MAX, 0)),
        payload: Some(command_envelope::Payload::UserPasswordRotate(
            UserPasswordRotate {
                username: "alice".into(),
                desired_revision: 7,
                sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
            },
        )),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        observe_external_effect_with_reconcile(&client, &pw_env, None).await,
        ExternalEffectObservation::AppliedExact
    );
    server.await.expect("server");

    // Interrupted config plan is safe to retry (Absent even when socket missing).
    let missing_dir = PrivateTempDir::new("agt-miss");
    let missing_client =
        PrivdClient::new(missing_dir.join("missing.sock"), Duration::from_millis(20))
            .expect("client");
    let plan_env = CommandEnvelope {
        payload: Some(command_envelope::Payload::CompleteConfigPlan(
            CompleteConfigPlan::default(),
        )),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        observe_external_effect_with_reconcile(&missing_client, &plan_env, None).await,
        ExternalEffectObservation::Absent
    );

    // Interrupted config apply maps all 4 durable observation states.
    for (state, rev, expected) in [
        (
            DesiredEffectState::AppliedExact,
            2,
            ExternalEffectObservation::AppliedExact,
        ),
        (
            DesiredEffectState::Absent,
            2,
            ExternalEffectObservation::Absent,
        ),
        (
            DesiredEffectState::SupersededByNewerRevision,
            3,
            ExternalEffectObservation::SupersededByNewerRevision,
        ),
        (
            DesiredEffectState::Unknown,
            0,
            ExternalEffectObservation::Unknown,
        ),
    ] {
        let (_dir, sock, srv) = serve_privd_once(move |req| {
            assert!(req.operation.is_none());
            let Some(CommandEnvelope {
                payload: Some(command_envelope::Payload::CompleteConfigApply(obs)),
                ..
            }) = req.authorization_command
            else {
                panic!("config observation required");
            };
            assert_eq!(obs.desired_revision, 2);
            privd_response::Result::DesiredEffectObservation(DesiredEffectObservation {
                state: state.into(),
                observed_revision: rev,
            })
        });
        let c = PrivdClient::new(sock, Duration::from_secs(2)).expect("client");
        let env = CommandEnvelope {
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            semantic_payload_sha256: vec![0x6a; 32],
            expires_at: Some(ts(i64::MAX, 0)),
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
        assert_eq!(
            observe_external_effect_with_reconcile(&c, &env, None).await,
            expected
        );
        srv.await.expect("srv");
    }
}
