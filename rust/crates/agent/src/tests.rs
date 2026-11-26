#![allow(clippy::many_single_char_names, clippy::similar_names)]

use ed25519_dalek::SigningKey;
use iroh::EndpointId;
use ocservia_agent_protocol::{ConfigApplyResult, ErrorKind, PrivdResponse, privd_response};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, HandshakeResult, SessionHandshakeResponse, UserPasswordRotate,
    command_envelope,
};
use ocservia_test_support::{
    PrivateTempDir, keyring, sign_session_grant, ts, user_password_secret as test_user_password,
};
use uuid::Uuid;

use crate::commands::{
    map_external_effect, response_proof, settle_privd_response, terminal_privd_error_code,
};
use crate::session::{
    activate_session_epoch, negotiate_session_mode, negotiate_session_mode_at_instant,
    refresh_fence_epoch_floor, supported_capabilities,
};

const NOW: i64 = 1_700_000_050;

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
            .expect("fixture");
    let bytes = hex::decode(fixture["apply_proto"].as_str().expect("apply")).expect("hex");
    let fixture = CommandEnvelope::decode(bytes.as_slice()).expect("cmd");
    let node = fixture.node_id.as_slice().try_into().expect("node");
    let payload = fixture.payload.expect("payload");
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

    for (rolled_back, failed_critical, applied_revision) in
        [(true, false, 8), (false, true, 8), (false, false, 8)]
    {
        let dir = PrivateTempDir::new("config-result");
        let mut executor =
            CommandExecutor::new(Journal::open(&dir.join("journal.sqlite3")).expect("open"));
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
            panic!("resume");
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
            .expect("settle");
        assert_eq!(result.record.state, CommandState::Succeeded);
        assert_eq!(
            executor
                .journal()
                .applied_revision("config", "ocserv.conf")
                .expect("rev"),
            (!rolled_back && !failed_critical).then_some(applied_revision)
        );
    }
}

#[test]
fn overlapping_agent_floor_and_handshake_activation_enforce_monotonic_epoch() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = vec!["synthetic.noop".to_owned()];

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
            .expect("auth");
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
fn agent_protocol_session_grant_validation() {
    let node = Uuid::now_v7();
    let ep = iroh::SecretKey::generate().public();
    let signing = test_key();
    let kr = test_keyring();

    let grantless = SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".into(),
        negotiated_capabilities: vec!["ocserv.status.read".into()],
        session_grant: None,
    };
    assert!(negotiate_session_mode(&grantless, &supported_capabilities(), node, ep, &kr).is_err());

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
    let sig = resp
        .session_grant
        .as_ref()
        .expect("grant")
        .signature
        .clone();
    resp.session_grant.as_mut().expect("grant").signature = vec![0; 64];
    assert!(negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).is_err());
    resp.session_grant.as_mut().expect("grant").signature = sig;
    assert_eq!(
        negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr)
            .expect("valid")
            .session_epoch,
        1
    );
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
    assert!(terminal_privd_error_code(ErrorKind::Unavailable.into()).is_none());

    let payload = command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
        username: "alice".into(),
        desired_revision: 7,
        sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
    });
    let mutation =
        privd_response::Result::Mutation(ocservia_agent_protocol::MutationResult { applied: true });
    assert_eq!(
        map_external_effect(&payload, mutation.clone()),
        ocservia_agent::ExternalEffectObservation::Unknown
    );
    let response = PrivdResponse {
        request_id: Uuid::now_v7().as_bytes().to_vec(),
        privileged_result_proof: None,
        result: Some(mutation),
    };
    assert!(response_proof(&response).is_none());
}
