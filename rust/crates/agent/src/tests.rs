#![allow(
    clippy::many_single_char_names,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use ed25519_dalek::{Signature, Signer as _, SigningKey, Verifier as _, VerifyingKey};
use iroh::EndpointId;
use iroh::endpoint::RelayMode;
use ocservia_agent::{ExternalEffectObservation, PrivdClient};
use ocservia_agent_protocol::{
    DesiredEffectObservation, DesiredEffectState, ErrorKind, OcservVersion, PrivdRequest,
    PrivdResponse, PrivilegedRequestMode, privd_request, privd_response, read_frame, write_frame,
};
use ocservia_command_authorization::{
    ControllerCommandKeyring, canonical_session_grant_v1, session_grant_claims_v1,
    verification_key_id,
};
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, CompleteConfigApply, CompleteConfigPlan, ConnectionFenceV2, EnrollRequest,
    FenceOperationKind, HandshakeResult, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
    SealingKeyDescriptorV1, SessionGrantV1, SessionGrantVersion, SessionHandshakeResponse,
    UserPasswordRotate, command_envelope,
};
use ocservia_contracts::session::{
    ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME, READ_ONLY_SESSION_CAPABILITIES,
};
use ocservia_test_support::{FenceSpec, keyring, sign_fence_binding, ts};
use uuid::Uuid;

use crate::CONNECTION_FENCING_CAPABILITY;
use crate::commands::{
    map_external_effect, observe_external_effect_with_reconcile, response_proof,
    terminal_privd_error_code,
};
use crate::session::{
    ActiveSessionAuthority, FenceDecision, SessionGrantAuthority,
    activate_session_connection_fence, fenced_artifact_authority,
    gate_signed_read_only_stale_diagnostic, negotiate_session_mode,
    negotiate_session_mode_at_instant, refresh_fence_epoch_floor, session_capabilities,
    supported_capabilities, verify_fenced_operation,
};
use crate::startup::{
    Config, build_relay_mode, build_sealing_keys, load_controller_command_keys, prepare_enrollment,
    read_enrollment_token, validate_enrollment_mode,
};

const NOW: i64 = 1_700_000_050;

struct TempDir(PathBuf);

impl TempDir {
    fn new(prefix: &str) -> Self {
        let dir = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!(
                "ocservia-agent-{prefix}-{}",
                Uuid::now_v7().simple()
            ));
        std::fs::create_dir_all(&dir).expect("mkdir");
        Self(dir)
    }

    fn path(&self) -> &Path {
        &self.0
    }

    fn join(&self, name: &str) -> PathBuf {
        self.0.join(name)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn test_sealing_keys() -> Vec<SealingKeyDescriptorV1> {
    vec![
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "node-key-1".into(),
            public_key_sha256: vec![0x11; 32],
        },
        SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::CertificateP12Password as i32,
            key_id: "node-p12-key-1".into(),
            public_key_sha256: vec![0x22; 32],
        },
    ]
}

fn test_user_password(ciphertext: Vec<u8>) -> SealedSecretV1 {
    SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::UserPassword as i32,
        key_id: "node-key-1".into(),
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
    capabilities: Vec<String>,
    connection_fence: Option<ConnectionFenceV2>,
    issued_at: i64,
    expires_at: i64,
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
        issued_at: Some(ts(issued_at, 0)),
        expires_at: Some(ts(expires_at, 0)),
        signature: Vec::new(),
    };
    let claims = session_grant_claims_v1(&grant).expect("grant claims");
    let canonical = canonical_session_grant_v1(&claims).expect("canonical grant");
    grant.signature = signing.sign(&canonical).to_bytes().to_vec();
    SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: 1,
        protocol_minor: 1,
        max_message_size: 1024 * 1024,
        controller_version: "test".into(),
        negotiated_capabilities: capabilities,
        session_grant: Some(grant),
        connection_fence,
    }
}

fn serve_privd_once<F>(handle: F) -> (PathBuf, tokio::task::JoinHandle<()>)
where
    F: FnOnce(PrivdRequest) -> privd_response::Result + Send + 'static,
{
    let socket = PathBuf::from(format!("/tmp/ocsa-{}.sock", Uuid::now_v7().simple()));
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
    (socket, task)
}

#[test]
fn agent_advertises_connection_owner_fencing_and_read_only_blocks_artifacts() {
    assert_eq!(
        supported_capabilities()
            .iter()
            .filter(|c| c.as_str() == CONNECTION_FENCING_CAPABILITY)
            .count(),
        1
    );
    let authority = ActiveSessionAuthority::SignedReadOnlyV11(SessionGrantAuthority {
        negotiated_capabilities: ["ocserv.status.read".to_owned()].into_iter().collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    });
    for frame in [ARTIFACT_FETCH_FRAME | 32, ARTIFACT_CONSUME_FRAME | 32] {
        assert!(fenced_artifact_authority(&authority, frame).is_err());
    }
}

#[test]
fn signed_read_only_stale_diagnostic_never_advances_the_floor() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let cmd = [5_u8; 16];
    let stale = FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 7).sign(&signing);
    assert!(matches!(
        gate_signed_read_only_stale_diagnostic(&kr, 8, &node, &ep, &stale, &cmd, NOW, 0)
            .expect("classified"),
        FenceDecision::RejectedStaleOwnerEpoch
    ));
    for f in [
        FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 8).sign(&signing),
        FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 9).sign(&signing),
        FenceSpec::new(*Uuid::now_v7().as_bytes(), *ep.as_bytes(), 7).sign(&signing),
    ] {
        assert!(
            gate_signed_read_only_stale_diagnostic(&kr, 8, &node, &ep, &f, &cmd, NOW, 0).is_err()
        );
    }
}

#[test]
fn fenced_operation_requires_the_active_term_and_never_advances_the_floor() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = [CONNECTION_FENCING_CAPABILITY, "synthetic.noop"];
    let grant = SessionGrantAuthority {
        negotiated_capabilities: caps.iter().map(|s| (*s).to_owned()).collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    };
    let cmd = [5_u8; 16];
    let base = || {
        FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 8)
            .authority(3)
            .capabilities(caps)
    };
    let active = base().lease(NOW, 100).sign(&signing);
    let active_verified = kr
        .verify_connection_fence_v2_at(&active, node.as_bytes(), ep.as_bytes(), NOW, 0)
        .expect("active fence");
    let binding = sign_fence_binding(
        &signing,
        &active,
        FenceOperationKind::Command,
        cmd,
        "synthetic.noop",
        (1_700_000_000, 0),
        (1_700_000_300, 0),
    );
    let verify =
        |f: Option<&ConnectionFenceV2>,
         b: Option<&ocservia_contracts::generated::ocserv::platform::agent::v1::FenceBindingV2>,
         kind: FenceOperationKind,
         op: &[u8; 16],
         cap: &str,
         nanos: u32| {
            verify_fenced_operation(
                &kr,
                &grant,
                &active_verified,
                8,
                &node,
                &ep,
                f,
                b,
                kind,
                op,
                cap,
                NOW,
                nanos,
            )
        };

    // Exact nanosecond boundaries for lease, proof expiry, and binding expiry.
    verify(
        Some(&active),
        Some(&binding),
        FenceOperationKind::Command,
        &cmd,
        "synthetic.noop",
        99,
    )
    .expect("live 1ns before lease");
    assert!(
        verify(
            Some(&active),
            Some(&binding),
            FenceOperationKind::Command,
            &cmd,
            "synthetic.noop",
            100
        )
        .is_err()
    );

    let proof_boundary = base().lease(NOW, 100).expires(NOW, 100).sign(&signing);
    verify(
        Some(&proof_boundary),
        Some(&binding),
        FenceOperationKind::Command,
        &cmd,
        "synthetic.noop",
        99,
    )
    .expect("live 1ns before proof expiry");
    assert!(
        verify(
            Some(&proof_boundary),
            Some(&binding),
            FenceOperationKind::Command,
            &cmd,
            "synthetic.noop",
            100
        )
        .is_err()
    );

    let live_refresh = base().lease(NOW + 1, 0).sign(&signing);
    let binding_boundary = sign_fence_binding(
        &signing,
        &active,
        FenceOperationKind::Command,
        cmd,
        "synthetic.noop",
        (1_700_000_000, 0),
        (NOW, 100),
    );
    verify(
        Some(&live_refresh),
        Some(&binding_boundary),
        FenceOperationKind::Command,
        &cmd,
        "synthetic.noop",
        99,
    )
    .expect("live 1ns before binding expiry");
    assert!(
        verify(
            Some(&live_refresh),
            Some(&binding_boundary),
            FenceOperationKind::Command,
            &cmd,
            "synthetic.noop",
            100
        )
        .is_err()
    );

    // Missing carrier, successor epoch, and different immutable term all fail closed.
    let higher = FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 9)
        .authority(3)
        .capabilities(caps)
        .sign(&signing);
    let higher_b = sign_fence_binding(
        &signing,
        &higher,
        FenceOperationKind::Command,
        cmd,
        "synthetic.noop",
        (1_700_000_000, 0),
        (1_700_000_300, 0),
    );
    let diff = base().ids([9; 16], [2; 16], 1, [4; 16]).sign(&signing);
    let diff_b = sign_fence_binding(
        &signing,
        &diff,
        FenceOperationKind::Command,
        cmd,
        "synthetic.noop",
        (1_700_000_000, 0),
        (1_700_000_300, 0),
    );
    for (f, b) in [
        (None, Some(&binding)),
        (Some(&active), None),
        (Some(&higher), Some(&higher_b)),
        (Some(&diff), Some(&diff_b)),
    ] {
        assert!(verify(f, b, FenceOperationKind::Command, &cmd, "synthetic.noop", 0).is_err());
    }

    // Artifact binding grounded in active term, cannot replay for another op.
    let art_id = [6_u8; 16];
    let art_b = sign_fence_binding(
        &signing,
        &active,
        FenceOperationKind::Artifact,
        art_id,
        CONNECTION_FENCING_CAPABILITY,
        (1_700_000_000, 0),
        (1_700_000_300, 0),
    );
    verify(
        Some(&active),
        Some(&art_b),
        FenceOperationKind::Artifact,
        &art_id,
        CONNECTION_FENCING_CAPABILITY,
        99,
    )
    .expect("artifact binding accepted");
    assert!(
        verify(
            Some(&active),
            Some(&art_b),
            FenceOperationKind::Artifact,
            &[7; 16],
            CONNECTION_FENCING_CAPABILITY,
            99
        )
        .is_err()
    );
}

#[test]
fn overlapping_agent_floor_and_handshake_activation_enforce_monotonic_epoch() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];

    // Overlapping journal processes: successor raises floor, old session retires before dispatch.
    let dir = TempDir::new("floor-overlap");
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

    let old_fence = FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 5)
        .authority(3)
        .capabilities(caps.clone())
        .sign(&signing);
    let old_verified = kr
        .verify_connection_fence_v2_at(&old_fence, node.as_bytes(), ep.as_bytes(), NOW, 0)
        .expect("old");
    let cmd = [5_u8; 16];
    let old_b = sign_fence_binding(
        &signing,
        &old_fence,
        FenceOperationKind::Command,
        cmd,
        "synthetic.noop",
        (1_700_000_000, 0),
        (1_700_000_300, 0),
    );
    let grant = SessionGrantAuthority {
        negotiated_capabilities: caps.iter().cloned().collect(),
        authorization_revision: 3,
        expires_at_unix_seconds: 1_700_000_300,
    };
    assert!(
        verify_fenced_operation(
            &kr,
            &grant,
            &old_verified,
            cached,
            &node,
            &ep,
            Some(&old_fence),
            Some(&old_b),
            FenceOperationKind::Command,
            &cmd,
            "synthetic.noop",
            NOW,
            0,
        )
        .is_err()
    );
    assert_eq!(active_agent.owner_fence_epoch_floor().expect("floor"), 10);
    drop((successor, active_agent));

    // Handshake persists new epoch before activation and rejects replay across reopen.
    let dir2 = TempDir::new("handshake-fence");
    let jpath2 = dir2.join("journal.db");
    let mut journal = Journal::open(&jpath2).expect("journal");
    journal.raise_owner_fence_epoch_floor(7).expect("preseed");
    let mut floor = 7_u64;
    let fence = FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 8)
        .authority(3)
        .capabilities(caps.clone())
        .lease(NOW, 100)
        .sign(&signing);
    let resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        3,
        caps,
        Some(fence),
        1_700_000_000,
        1_700_000_300,
    );
    let mode =
        negotiate_session_mode_at_instant(&resp, &supported_capabilities(), node, ep, &kr, NOW, 0)
            .expect("mode");
    assert!(
        matches!(&mode, ActiveSessionAuthority::FencedV11 { connection_fence, .. } if connection_fence.owner_epoch == 8)
    );
    activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99).expect("activate");
    assert_eq!(floor, 8);
    assert!(activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99).is_err());
    drop(journal);
    assert_eq!(
        Journal::open(&jpath2)
            .expect("reopen")
            .owner_fence_epoch_floor()
            .expect("f"),
        8
    );

    // Proof expiry exact nanosecond boundary on negotiate + activate.
    let boundary_caps = vec![CONNECTION_FENCING_CAPABILITY.to_owned()];
    let b_fence = FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 1)
        .authority(3)
        .capabilities(boundary_caps.clone())
        .lease(NOW, 100)
        .expires(NOW, 100)
        .sign(&signing);
    let b_resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        3,
        boundary_caps,
        Some(b_fence),
        1_700_000_000,
        1_700_000_300,
    );
    let b_mode = negotiate_session_mode_at_instant(
        &b_resp,
        &supported_capabilities(),
        node,
        ep,
        &kr,
        NOW,
        99,
    )
    .expect("live 1ns before expiry");
    assert!(
        negotiate_session_mode_at_instant(
            &b_resp,
            &supported_capabilities(),
            node,
            ep,
            &kr,
            NOW,
            100
        )
        .is_err()
    );
    let dir3 = TempDir::new("proof-boundary");
    let mut j3 = Journal::open(&dir3.join("journal.db")).expect("j3");
    let mut f3 = 0_u64;
    assert!(activate_session_connection_fence(&b_mode, &mut j3, &mut f3, NOW, 100).is_err());
    assert_eq!((f3, j3.owner_fence_epoch_floor().expect("f3")), (0, 0));
}

#[test]
fn fenced_handshake_failures_do_not_advance_the_durable_epoch() {
    let signing = test_key();
    let kr = keyring(&signing);
    let node = Uuid::now_v7();
    let ep = test_endpoint();
    let caps = vec![
        CONNECTION_FENCING_CAPABILITY.to_owned(),
        "synthetic.noop".to_owned(),
    ];
    let mk_fence = |epoch: u64| {
        FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), epoch)
            .authority(3)
            .capabilities(caps.clone())
    };
    let mk_resp = |f: Option<ConnectionFenceV2>| {
        signed_handshake_response(
            &signing,
            &node,
            &ep,
            3,
            caps.clone(),
            f,
            1_700_000_000,
            1_700_000_300,
        )
    };
    let non_fencing = vec!["synthetic.noop".to_owned()];
    let mut bad_sig = mk_fence(11).sign(&signing);
    bad_sig.signature[0] ^= 1;

    let cases = vec![
        ("missing fence", mk_resp(None)),
        (
            "mutation without fencing",
            signed_handshake_response(
                &signing,
                &node,
                &ep,
                3,
                non_fencing.clone(),
                None,
                1_700_000_000,
                1_700_000_300,
            ),
        ),
        (
            "unexpected fence",
            signed_handshake_response(
                &signing,
                &node,
                &ep,
                3,
                non_fencing.clone(),
                Some(
                    FenceSpec::new(*node.as_bytes(), *ep.as_bytes(), 11)
                        .authority(3)
                        .capabilities(non_fencing)
                        .sign(&signing),
                ),
                1_700_000_000,
                1_700_000_300,
            ),
        ),
        ("lower epoch", mk_resp(Some(mk_fence(9).sign(&signing)))),
        ("equal epoch", mk_resp(Some(mk_fence(10).sign(&signing)))),
        (
            "expired proof",
            mk_resp(Some(
                mk_fence(11).lease(NOW, 0).expires(NOW, 0).sign(&signing),
            )),
        ),
        (
            "expired lease",
            mk_resp(Some(mk_fence(11).lease(NOW, 99).sign(&signing))),
        ),
        ("bad signature", mk_resp(Some(bad_sig))),
        (
            "revision mismatch",
            mk_resp(Some(mk_fence(11).authority(4).sign(&signing))),
        ),
        (
            "capability mismatch",
            mk_resp(Some(
                mk_fence(11)
                    .capabilities([CONNECTION_FENCING_CAPABILITY])
                    .sign(&signing),
            )),
        ),
        (
            "node mismatch",
            mk_resp(Some(
                FenceSpec::new(*Uuid::now_v7().as_bytes(), *ep.as_bytes(), 11)
                    .authority(3)
                    .capabilities(caps.clone())
                    .sign(&signing),
            )),
        ),
        (
            "endpoint mismatch",
            mk_resp(Some(
                FenceSpec::new(
                    *node.as_bytes(),
                    *iroh::SecretKey::from_bytes(&[8; 32]).public().as_bytes(),
                    11,
                )
                .authority(3)
                .capabilities(caps.clone())
                .sign(&signing),
            )),
        ),
    ];

    let dir = TempDir::new("fence-negative");
    let jpath = dir.join("journal.db");
    let mut journal = Journal::open(&jpath).expect("journal");
    journal.raise_owner_fence_epoch_floor(10).expect("preseed");
    let mut floor = 10_u64;

    // Read-only signed session activates without raising floor.
    let ro_resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        3,
        vec!["ocserv.status.read".into()],
        None,
        1_700_000_000,
        1_700_000_300,
    );
    let ro_mode = negotiate_session_mode_at_instant(
        &ro_resp,
        &supported_capabilities(),
        node,
        ep,
        &kr,
        NOW,
        0,
    )
    .expect("ro");
    assert!(matches!(
        &ro_mode,
        ActiveSessionAuthority::SignedReadOnlyV11(_)
    ));
    activate_session_connection_fence(&ro_mode, &mut journal, &mut floor, NOW, 100)
        .expect("ro active");
    assert_eq!(floor, 10);

    for (name, resp) in cases {
        if let Ok(mode) = negotiate_session_mode_at_instant(
            &resp,
            &supported_capabilities(),
            node,
            ep,
            &kr,
            NOW,
            0,
        ) {
            assert!(
                activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 100)
                    .is_err(),
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

    // Grantless v1.0 (read-only & mutating) and grantless v1.1 all rejected.
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
            connection_fence: None,
        };
        assert!(negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).is_err());
    }

    // Invalid v1.1 grant signature rejected; valid signature accepted as SignedReadOnlyV11.
    let now_sec = ocservia_test_support::unix_seconds();
    let mut resp = signed_handshake_response(
        &signing,
        &node,
        &ep,
        1,
        vec!["ocserv.status.read".into()],
        None,
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
    assert!(matches!(
        negotiate_session_mode(&resp, &supported_capabilities(), node, ep, &kr).expect("valid"),
        ActiveSessionAuthority::SignedReadOnlyV11(_)
    ));
}

#[tokio::test]
async fn session_capabilities_withhold_terminate_below_ocserv_1_4_2() {
    for (version, terminate) in [(Some("1.2.4"), false), (Some("1.4.2"), true), (None, false)] {
        let (socket, server) = serve_privd_once(move |req| {
            assert!(matches!(
                req.operation,
                Some(privd_request::Operation::OcservVersion(_))
            ));
            version.map_or_else(
                || privd_response::Result::Error(ocservia_agent_protocol::PrivdError::default()),
                |v| privd_response::Result::OcservVersion(OcservVersion { version: v.into() }),
            )
        });
        let privd = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
        let caps = session_capabilities(&privd).await;
        server.await.expect("server");
        std::fs::remove_file(&socket).expect("rm sock");
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
    let prep_dir = TempDir::new("prepare");
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
    let identity =
        ocservia_agent_protocol::identity::Identity::provision(&identity_dir, controller)
            .expect("reload");
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
        ocservia_agent_protocol::identity::enrollment_canonical_v1(&req).expect("canonical");
    let sig = Signature::from_slice(&req.proof.expect("proof").signature).expect("sig");
    VerifyingKey::from_bytes(identity.endpoint_id().as_bytes())
        .expect("vk")
        .verify(&canonical, &sig)
        .expect("verify");

    // Enrollment token file requires strict mode & format.
    let tok_dir = TempDir::new("enrollment");
    let token = tok_dir.join("enrollment.token");
    let valid = "0".repeat(43);
    std::fs::write(&token, format!("{valid}\n")).expect("write");
    std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("mode");
    for v in [format!("obt2_{valid}"), valid.clone()] {
        std::fs::write(&token, format!("{v}\r\n")).expect("write");
        assert_eq!(read_enrollment_token(&token).expect("valid"), v);
    }
    for bad in [
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
}

#[test]
fn custom_relay_count_and_security_contract() {
    let dir = TempDir::new("relay-contract");
    let token = dir.join("token");
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
        sealed_password: Vec::new(),
        secret_key_id: String::new(),
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
    let (socket, server) = serve_privd_once(|req| {
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
    let client = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
    let pw_env = CommandEnvelope {
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        semantic_payload_sha256: vec![0x5a; 32],
        expires_at: Some(ts(i64::MAX, 0)),
        payload: Some(command_envelope::Payload::UserPasswordRotate(
            UserPasswordRotate {
                username: "alice".into(),
                sealed_password: Vec::new(),
                secret_key_id: String::new(),
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
    std::fs::remove_file(socket).expect("rm sock");

    // Interrupted config plan is safe to retry (Absent even when socket missing).
    let missing_client = PrivdClient::new(
        std::env::temp_dir().join(format!("missing-{}.sock", Uuid::now_v7())),
        Duration::from_millis(20),
    )
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
        let (sock, srv) = serve_privd_once(move |req| {
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
        let c = PrivdClient::new(sock.clone(), Duration::from_secs(2)).expect("client");
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
        std::fs::remove_file(sock).expect("rm sock");
    }
}
