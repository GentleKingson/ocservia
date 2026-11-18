use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_command_authorization::{
    ControllerCommandKeyring, canonical_v1, claims_from_envelope_v1, verification_key_id,
};
use ocservia_contracts::config_profile;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, CommandAuthorizationProof, CommandAuthorizationVersion, CompleteConfigApply,
    CompleteConfigCandidate, CompleteConfigDirective, ConfigApplyResult, ConfigPlan, GroupApply,
    IpBanRemove, ServiceReload, SessionDisconnect, SessionTerminate, SyntheticEcho, SyntheticNoop,
    UserCreate, UserDisable, UserEnable, UserPasswordRotate, command_envelope,
    complete_config_directive,
};
use prost_types::Timestamp;
use rand::{SeedableRng, rngs::StdRng};
use std::os::unix::process::ExitStatusExt as _;
use std::process::Command;

use super::*;

fn capabilities(values: &[&str]) -> HashSet<String> {
    values.iter().map(|value| (*value).to_owned()).collect()
}

fn user_password(ciphertext: Vec<u8>) -> SealedSecretV1 {
    SealedSecretV1 {
        version: SealedSecretVersion::V1 as i32,
        purpose: SealedSecretPurpose::UserPassword as i32,
        key_id: "node-key-1".to_owned(),
        ciphertext,
    }
}

#[test]
fn desired_privd_deadline_exceeds_each_child_budget() {
    let client = PrivdClient::new(
        std::path::PathBuf::from("/tmp/ocservia-test-privd.sock"),
        Duration::from_secs(5),
    )
    .expect("client");
    assert_eq!(client.timeout, Duration::from_secs(5));
    assert_eq!(client.desired_timeout, Duration::from_secs(20));
    assert!(client.desired_timeout > client.timeout);
}

#[test]
fn oversized_group_apply_is_terminally_rejected_before_privd() {
    let envelope = CommandEnvelope {
        payload: Some(command_envelope::Payload::GroupApply(GroupApply {
            group_name: "staff".to_owned(),
            members: (0..=ocservia_agent_protocol::MAX_MANAGED_RESOURCES)
                .map(|index| format!("member{index:03}"))
                .collect(),
            desired_revision: 1,
        })),
        ..CommandEnvelope::default()
    };
    assert!(matches!(
        validate_payload(&envelope),
        Err(CommandError::Rejected("group_members_invalid"))
    ));
}

#[test]
fn root_is_refused() {
    assert_eq!(
        ensure_unprivileged(0).expect_err("root refused").kind(),
        io::ErrorKind::PermissionDenied
    );
    ensure_unprivileged(65532).expect("service UID allowed");
}

#[test]
fn full_jitter_is_bounded_and_not_constant() {
    let policy = Backoff::default();
    let mut rng = StdRng::seed_from_u64(7);
    let values = (0..64)
        .map(|_| policy.delay(20, &mut rng))
        .collect::<Vec<_>>();
    assert!(values.iter().all(|delay| *delay <= Duration::from_secs(15)));
    assert!(values.windows(2).any(|pair| pair[0] != pair[1]));
}

#[test]
fn legacy_synthetic_payload_hash_matches_cross_language_golden_vectors() {
    let node = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
    let key = *Uuid::now_v7().as_bytes();
    let mut envelope = command(node, key, "", 100);
    let cases = [
        (
            "",
            "2d6daaae892285c786fba378aff37d4d0436dae76699061500b548e939782433",
        ),
        (
            "hello",
            "a7299856cb7fa4e266b1234614361677e1d1b2466608d014e71b4a547c804397",
        ),
        (
            "你好",
            "10f1ae9994bde9ea69de8fb965bf5029e97ad8b1bf1f00834d47831f60adf38d",
        ),
    ];
    for (message, expected) in cases {
        envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: message.to_owned(),
        }));
        assert_eq!(
            hex::encode(legacy_synthetic_payload_hash(&envelope).unwrap()),
            expected
        );
    }
    envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
        message: "x".repeat(4096),
    }));
    assert_eq!(
        hex::encode(legacy_synthetic_payload_hash(&envelope).unwrap()),
        "d6f3a8f8d0fc7ccbbfe6fec9f93bcad544d98fec0f3501aeef8be2a5d2b78daa"
    );
    envelope.payload = Some(command_envelope::Payload::SyntheticNoop(
        SyntheticNoop::default(),
    ));
    assert_eq!(
        hex::encode(legacy_synthetic_payload_hash(&envelope).unwrap()),
        "2e5b198f3c3a2718113a4dbf2a552c730ddede13567ee448c6118459ccfa0d98"
    );
    envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
        message: "hello".to_owned(),
    }));
    envelope.expected_revision = 2;
    assert_eq!(
        hex::encode(legacy_synthetic_payload_hash(&envelope).unwrap()),
        "79659b4e1819080191867174096d8aa5d01a43cb634cab9c51b113391643c343"
    );
    envelope.expected_revision = 1;
    envelope.node_id = (1_u8..=16).collect();
    assert_eq!(
        hex::encode(legacy_synthetic_payload_hash(&envelope).unwrap()),
        "a45222e4babe147a02b9274937f09c337ac56764a56c61a9ebfd901d4fec7afe"
    );
}

fn command(node_id: [u8; 16], key: [u8; 16], message: &str, now: i64) -> CommandEnvelope {
    let mut envelope = CommandEnvelope {
        protocol_version: ocservia_command_authorization::COMMAND_PROTOCOL_VERSION.to_owned(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: key.to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(Timestamp {
            seconds: now,
            nanos: 0,
        }),
        expires_at: Some(Timestamp {
            seconds: now + 60,
            nanos: 0,
        }),
        expected_revision: 1,
        traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01".to_owned(),
        actor_id: "test".to_owned(),
        reason: "fault matrix".to_owned(),
        operation_id: key.to_vec(),
        action: "operation.create".to_owned(),
        required_capability: "synthetic.echo".to_owned(),
        approval_id: Vec::new(),
        approval_request_sha256: Vec::new(),
        connection_fence: None,
        fence_binding: None,
        authorization: None,
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        payload: Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: message.to_owned(),
        })),
        semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
        semantic_payload_sha256: Vec::new(),
    };
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
        .expect("v1 hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    envelope
}

/// Builds a v1 command envelope with a correctly computed semantic hash.
///
/// The `semantic_payload_sha256` is filled from `semantic_payload_hash_v1`
/// so that `validate_command` passes the strict wire-schema check.
fn command_v1(node_id: [u8; 16], key: [u8; 16], message: &str, now: i64) -> CommandEnvelope {
    let mut envelope = command(node_id, key, message, now);
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
        .expect("v1 hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    envelope
}

fn context(node_id: [u8; 16], now: i64) -> CommandContext {
    CommandContext {
        node_id,
        authorization_revision: 1,
        capabilities: capabilities(&["synthetic.noop", "synthetic.echo"]),
        session_expires_at_unix_seconds: now + 300,
        command_keys: test_keyring(),
        now_unix_seconds: now,
        cancelled: false,
    }
}

fn temporary_journal(label: &str) -> std::path::PathBuf {
    std::env::temp_dir()
        .canonicalize()
        .expect("tmp")
        .join(format!("ocservia-agent-{label}-{}.db", Uuid::now_v7()))
}

#[derive(Clone, Copy)]
enum DesiredMutation {
    Create,
    Rotate,
    Disable,
    Enable,
    Group,
}

fn desired_command(
    node_id: [u8; 16],
    mutation: DesiredMutation,
    name: &str,
    _expected_revision: u64,
    desired_revision: u64,
    now: i64,
) -> CommandEnvelope {
    let mut envelope = command(node_id, *Uuid::now_v7().as_bytes(), "", now);
    envelope.expected_revision = 1;
    envelope.payload = Some(match mutation {
        DesiredMutation::Create => command_envelope::Payload::UserCreate(UserCreate {
            username: name.to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision,
            sealed_password_v1: Some(user_password(vec![0xa1; 64])),
        }),
        DesiredMutation::Rotate => {
            command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
                username: name.to_owned(),
                sealed_password: Vec::new(),
                secret_key_id: String::new(),
                desired_revision,
                sealed_password_v1: Some(user_password(vec![
                    u8::try_from(desired_revision)
                        .unwrap_or(0xa5);
                    64
                ])),
            })
        }
        DesiredMutation::Disable => command_envelope::Payload::UserDisable(UserDisable {
            username: name.to_owned(),
            desired_revision,
        }),
        DesiredMutation::Enable => command_envelope::Payload::UserEnable(UserEnable {
            username: name.to_owned(),
            desired_revision,
        }),
        DesiredMutation::Group => command_envelope::Payload::GroupApply(GroupApply {
            group_name: name.to_owned(),
            members: vec!["alice".to_owned()],
            desired_revision,
        }),
    });
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
        .expect("desired semantic hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    envelope
}

fn test_signing_key() -> SigningKey {
    SigningKey::from_bytes(&[
        0x9d, 0x61, 0xb1, 0x9d, 0xef, 0xfd, 0x5a, 0x60, 0xba, 0x84, 0x4a, 0xf4, 0x92, 0xec, 0x2c,
        0xc4, 0x44, 0x49, 0xc5, 0x69, 0x7b, 0x32, 0x69, 0x19, 0x70, 0x3b, 0xac, 0x03, 0x1c, 0xae,
        0x7f, 0x60,
    ])
}

fn test_keyring() -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([test_signing_key().verifying_key()]).expect("test keyring")
}

fn authorize_test_command(envelope: &mut CommandEnvelope) {
    let (action, capability) = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => ("operation.create", "synthetic.noop"),
        Some(command_envelope::Payload::SyntheticEcho(_)) => ("operation.create", "synthetic.echo"),
        Some(command_envelope::Payload::SessionDisconnect(_)) => {
            ("session.disconnect", "ocserv.session.disconnect")
        }
        Some(command_envelope::Payload::SessionTerminate(_)) => {
            ("session.terminate", "ocserv.session.terminate")
        }
        Some(command_envelope::Payload::IpBanRemove(_)) => {
            ("ip_ban.remove", "ocserv.ip_ban.remove")
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            ("service.reload", "ocserv.service.reload")
        }
        Some(command_envelope::Payload::UserCreate(_)) => ("user.create", "ocserv.users.write"),
        Some(command_envelope::Payload::UserDisable(_)) => ("user.disable", "ocserv.users.write"),
        Some(command_envelope::Payload::UserEnable(_)) => ("user.enable", "ocserv.users.write"),
        Some(command_envelope::Payload::UserPasswordRotate(_)) => {
            ("user.password.rotate", "ocserv.users.write")
        }
        Some(command_envelope::Payload::GroupApply(_)) => ("group.apply", "ocserv.groups.write"),
        Some(command_envelope::Payload::CompleteConfigApply(_)) => {
            ("config.apply", config_profile::APPLY_CAPABILITY)
        }
        Some(command_envelope::Payload::AgentUpgrade(_)) => {
            if envelope.required_capability == "ocserv.agent.upgrade.v1" {
                ("agent.upgrade", "ocserv.agent.upgrade.v1")
            } else {
                ("agent.upgrade", "ocserv.agent.upgrade.v2")
            }
        }
        _ => panic!("test command payload is unsupported"),
    };
    envelope.protocol_version = ocservia_command_authorization::COMMAND_PROTOCOL_VERSION.to_owned();
    envelope.operation_id = envelope.idempotency_key.clone();
    envelope.action = action.to_owned();
    envelope.required_capability = capability.to_owned();
    let signing_key = test_signing_key();
    envelope.authorization = Some(CommandAuthorizationProof {
        version: CommandAuthorizationVersion::V1.into(),
        key_id: verification_key_id(&signing_key.verifying_key()),
        signature: Vec::new(),
    });
    let claims = claims_from_envelope_v1(envelope).expect("test authorization claims");
    let canonical = canonical_v1(&claims).expect("test authorization bytes");
    envelope
        .authorization
        .as_mut()
        .expect("test authorization")
        .signature = signing_key.sign(&canonical).to_bytes().to_vec();
}

fn desired_context(node_id: [u8; 16], now: i64) -> CommandContext {
    let mut command_context = context(node_id, now);
    command_context.capabilities = capabilities(&["ocserv.users.write", "ocserv.groups.write"]);
    command_context
}

#[test]
fn expired_signed_commands_only_allow_observation() {
    let node = *Uuid::now_v7().as_bytes();
    let mut envelope = command(node, *Uuid::now_v7().as_bytes(), "expiry", 100);
    let context = context(node, 200);
    for mode in [
        CommandDeliveryMode::ExecuteOrReplay,
        CommandDeliveryMode::ReconcileOnly,
        CommandDeliveryMode::RetryIfEffectAbsent,
    ] {
        envelope.delivery_mode = mode.into();
        authorize_test_command(&mut envelope);
        assert_eq!(
            validate_command(&envelope, &context).is_ok(),
            mode == CommandDeliveryMode::ReconcileOnly
        );
    }
    envelope.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut envelope);
    envelope.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    assert!(validate_command(&envelope, &context).is_err());
}

#[test]
#[allow(clippy::too_many_lines)]
fn config_reconciliation_rebuilds_lost_journal_from_exact_durable_effect() {
    use complete_config_directive::Value::{Literal, Tls};

    let path = temporary_journal("config-journal-rebuild");
    let node = *Uuid::now_v7().as_bytes();
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
        node_id: node.to_vec(),
        expected_revision: 6,
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
    let mut envelope = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    envelope.payload = Some(command_envelope::Payload::CompleteConfigApply(
        CompleteConfigApply {
            candidate: Some(candidate),
            candidate_hash: candidate_hash.clone(),
            materialized_hash: vec![0x55; 32],
            expected_current_hash: vec![0x44; 32],
            desired_revision: 7,
            plan_id: Uuid::now_v7().as_bytes().to_vec(),
            plan_expires_at: Some(Timestamp {
                seconds: 200,
                nanos: 0,
            }),
        },
    ));
    envelope.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V2.into();
    envelope.semantic_payload_sha256 = semantic_payload_hash_v2(&envelope)
        .expect("config semantic hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&[config_profile::APPLY_CAPABILITY]);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("empty journal"));

    let recovered = executor
        .begin_attested_recovery(&envelope, &command_context)
        .expect("accept exact root receipt after journal loss");
    let result = observed_result(&envelope);
    let outcome = executor
        .complete_external_applied_attested(
            &recovered,
            "config",
            "ocserv.conf",
            7,
            &result,
            b"root-receipt",
            101,
        )
        .expect("rebuild exact attested config evidence");
    assert_eq!(outcome.record.state, CommandState::Succeeded);
    assert_eq!(
        executor
            .journal()
            .applied_revision("config", "ocserv.conf")
            .expect("config revision"),
        Some(7)
    );
    let result = ConfigApplyResult::decode(
        outcome
            .record
            .result
            .as_deref()
            .expect("typed config result"),
    )
    .expect("decode config result");
    assert_eq!(result.applied_revision, 7);
    assert_eq!(result.observed_hash, candidate_hash);

    let replay = executor
        .reconcile_external(
            &envelope,
            &command_context,
            ExternalEffectObservation::AppliedExact,
        )
        .expect("strict idempotent reconciliation replay");
    assert_eq!(replay.record.result, outcome.record.result);
    cleanup_journal(&path);
}

fn apply_desired(
    executor: &mut CommandExecutor,
    envelope: &CommandEnvelope,
    context: &CommandContext,
    now: i64,
    privd_calls: &mut usize,
) {
    let ExternalPreparation::Execute(command) = executor
        .prepare_external(envelope, context)
        .expect("desired command preparation")
    else {
        panic!("fresh desired command must execute")
    };
    *privd_calls += 1;
    let (resource_type, resource_key, revision) =
        desired_resource(envelope).expect("desired resource");
    executor
        .complete_external_applied_attested(
            &command,
            resource_type,
            resource_key,
            revision,
            b"applied",
            b"root-receipt",
            now,
        )
        .expect("complete desired command");
}

#[test]
fn stale_cross_kind_user_mutations_are_rejected_before_privd() {
    let path = temporary_journal("stale-cross-kind");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    for (name, applied, stale) in [
        (
            "alice",
            [
                DesiredMutation::Create,
                DesiredMutation::Rotate,
                DesiredMutation::Disable,
            ],
            DesiredMutation::Rotate,
        ),
        (
            "bob",
            [
                DesiredMutation::Create,
                DesiredMutation::Rotate,
                DesiredMutation::Disable,
            ],
            DesiredMutation::Enable,
        ),
        (
            "carol",
            [
                DesiredMutation::Create,
                DesiredMutation::Rotate,
                DesiredMutation::Rotate,
            ],
            DesiredMutation::Disable,
        ),
    ] {
        for (index, mutation) in applied.into_iter().enumerate() {
            let expected = u64::try_from(index).expect("expected revision");
            apply_desired(
                &mut executor,
                &desired_command(node, mutation, name, expected, expected + 1, 100),
                &command_context,
                101 + i64::try_from(index).expect("revision"),
                &mut privd_calls,
            );
        }
        let stale = desired_command(node, stale, name, 1, 2, 100);
        assert!(matches!(
            executor.prepare_external(&stale, &command_context),
            Err(CommandError::Rejected(
                "effect_superseded_by_newer_revision"
            ))
        ));
    }
    assert_eq!(privd_calls, 9);
    for name in ["alice", "bob", "carol"] {
        assert_eq!(
            executor
                .journal()
                .applied_revision("user", name)
                .expect("user revision"),
            Some(3)
        );
    }
    cleanup_journal(&path);
}

#[test]
fn stale_group_apply_is_rejected_before_privd() {
    let path = temporary_journal("stale-group");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    for revision in 1..=3 {
        apply_desired(
            &mut executor,
            &desired_command(
                node,
                DesiredMutation::Group,
                "staff",
                revision - 1,
                revision,
                100,
            ),
            &command_context,
            100 + i64::try_from(revision).expect("revision"),
            &mut privd_calls,
        );
    }
    let stale = desired_command(node, DesiredMutation::Group, "staff", 1, 2, 100);
    assert!(matches!(
        executor.prepare_external(&stale, &command_context),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));
    assert_eq!(privd_calls, 3);
    assert_eq!(
        executor
            .journal()
            .applied_revision("group", "staff")
            .expect("group revision"),
        Some(3)
    );
    cleanup_journal(&path);
}

#[test]
fn durable_revision_fence_survives_agent_restart() {
    let path = temporary_journal("revision-restart");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut privd_calls = 0;
    {
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
        for (mutation, expected, desired) in [
            (DesiredMutation::Create, 0, 1),
            (DesiredMutation::Rotate, 1, 2),
            (DesiredMutation::Disable, 2, 3),
        ] {
            apply_desired(
                &mut executor,
                &desired_command(node, mutation, "alice", expected, desired, 100),
                &command_context,
                101 + i64::try_from(desired).expect("revision"),
                &mut privd_calls,
            );
        }
    }
    let mut restarted = CommandExecutor::new(Journal::open(&path).expect("restart"));
    let stale = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 100);
    assert!(matches!(
        restarted.prepare_external(&stale, &command_context),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));
    assert_eq!(privd_calls, 3);
    cleanup_journal(&path);
}

#[test]
fn revision_gap_is_rejected_before_privd() {
    let path = temporary_journal("revision-gap");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
        &command_context,
        101,
        &mut privd_calls,
    );
    let gap = desired_command(node, DesiredMutation::Disable, "alice", 2, 3, 100);
    assert!(matches!(
        executor.prepare_external(&gap, &command_context),
        Err(CommandError::Rejected("revision_gap"))
    ));
    assert_eq!(privd_calls, 1);
    cleanup_journal(&path);
}

#[test]
fn failed_revision_can_be_replaced_without_claiming_it_was_applied() {
    let path = temporary_journal("failed-revision-replacement");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
        &command_context,
        101,
        &mut privd_calls,
    );

    let failed = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 100);
    let ExternalPreparation::Execute(failed_command) = executor
        .prepare_external(&failed, &command_context)
        .expect("prepare failed revision")
    else {
        panic!("fresh revision must execute")
    };
    executor
        .complete_external_attested(&failed_command, Err("privd_rejected"), b"root-receipt", 102)
        .expect("record deterministic failure");
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("applied revision"),
        Some(1)
    );

    let replacement = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 100);
    apply_desired(
        &mut executor,
        &replacement,
        &command_context,
        103,
        &mut privd_calls,
    );
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("replacement revision"),
        Some(2)
    );
    assert_eq!(privd_calls, 2);
    cleanup_journal(&path);
}

#[test]
fn five_minute_offline_recovery_rejects_expired_command_before_effect() {
    let path = temporary_journal("five-minute-expired-revision");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 1_300);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 1_300),
        &command_context,
        101,
        &mut privd_calls,
    );

    let mut expired = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 1_000);
    expired.expires_at = Some(prost_types::Timestamp {
        seconds: 1_300,
        nanos: 0,
    });
    assert!(matches!(
        executor.prepare_external(&expired, &command_context),
        Err(CommandError::Rejected("command_expired"))
    ));
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("pre-effect rejection revision"),
        Some(1)
    );

    let replacement = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 1_300);
    apply_desired(
        &mut executor,
        &replacement,
        &command_context,
        102,
        &mut privd_calls,
    );
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("replacement revision"),
        Some(2)
    );
    assert_eq!(privd_calls, 2);
    cleanup_journal(&path);
}

#[test]
fn sequential_cross_kind_user_mutations_advance_one_resource_revision() {
    let path = temporary_journal("sequential-cross-kind");
    let node = *Uuid::now_v7().as_bytes();
    let command_context = desired_context(node, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    for (mutation, expected, desired) in [
        (DesiredMutation::Create, 0, 1),
        (DesiredMutation::Rotate, 1, 2),
        (DesiredMutation::Disable, 2, 3),
        (DesiredMutation::Enable, 3, 4),
        (DesiredMutation::Rotate, 4, 5),
    ] {
        apply_desired(
            &mut executor,
            &desired_command(node, mutation, "alice", expected, desired, 100),
            &command_context,
            101 + i64::try_from(desired).expect("revision"),
            &mut privd_calls,
        );
    }
    assert_eq!(privd_calls, 5);
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("user revision"),
        Some(5)
    );
    cleanup_journal(&path);
}

#[test]
fn external_command_is_durable_and_duplicate_delivery_replays() {
    let path = temporary_journal("external");
    let node = [0x41; 16];
    let key = *Uuid::now_v7().as_bytes();
    let mut envelope = command(node, key, "", 100);
    envelope.payload = Some(command_envelope::Payload::SessionDisconnect(
        SessionDisconnect {
            session_id: "42".to_owned(),
            boot_id: Uuid::now_v7().to_string(),
        },
    ));
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
        .expect("session hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&["ocserv.session.disconnect"]);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let ExternalPreparation::Execute(command) = executor
        .prepare_external(&envelope, &command_context)
        .expect("prepare")
    else {
        panic!("first delivery must execute");
    };
    let completed = executor
        .complete_external_attested(&command, Ok(b"applied"), b"root-receipt", 101)
        .expect("complete");
    assert_eq!(completed.record.state, CommandState::Succeeded);
    let ExternalPreparation::Replay(replayed) = executor
        .prepare_external(&envelope, &command_context)
        .expect("replay")
    else {
        panic!("duplicate delivery must not execute");
    };
    assert!(replayed.replayed);
    assert_eq!(replayed.record.state, CommandState::Succeeded);
    cleanup_journal(&path);
}

#[test]
fn desired_unknown_reconcile_recovers_result_and_applied_revision_after_restart() {
    let path = temporary_journal("desired-reconcile");
    let node = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let mut envelope = command(node, key, "", 100);
    envelope.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 2,
            sealed_password_v1: Some(user_password(vec![0xa5; 64])),
        },
    ));
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
        .expect("password hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&["ocserv.users.write"]);
    {
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
        let mut privd_calls = 0;
        apply_desired(
            &mut executor,
            &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
            &command_context,
            100,
            &mut privd_calls,
        );
        let ExternalPreparation::Execute(command) = executor
            .prepare_external(&envelope, &command_context)
            .expect("prepare")
        else {
            panic!("execute preparation")
        };
        let unknown = executor
            .mark_external_unknown(&command, "privd_transport_unknown", 101)
            .expect("unknown after lost response");
        assert_eq!(unknown.record.state, CommandState::Unknown);
    }
    command_context.now_unix_seconds = 102;
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("restart"));
    let recovered_command = executor
        .begin_attested_recovery(&envelope, &command_context)
        .expect("root receipt proves exact effect");
    let recovered = executor
        .complete_external_applied_attested(
            &recovered_command,
            "user",
            "alice",
            2,
            b"applied",
            b"root-receipt",
            102,
        )
        .expect("persist exact result and root receipt");
    assert_eq!(recovered.record.state, CommandState::Succeeded);
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("applied revision"),
        Some(2)
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn password_unknown_remains_nonterminal_until_effect_observation() {
    let path = temporary_journal("password-absence");
    let node = *Uuid::now_v7().as_bytes();
    let mut envelope = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    envelope.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 2,
            sealed_password_v1: Some(user_password(vec![0xa5; 64])),
        },
    ));
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope).unwrap().to_vec();
    authorize_test_command(&mut envelope);
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&["ocserv.users.write"]);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
        &command_context,
        100,
        &mut privd_calls,
    );
    let ExternalPreparation::Execute(command) = executor
        .prepare_external(&envelope, &command_context)
        .expect("prepare")
    else {
        panic!("execute preparation")
    };
    executor
        .mark_external_unknown(&command, "privd_transport_unknown", 101)
        .expect("unknown");
    command_context.now_unix_seconds = 102;
    let reconciled = executor
        .reconcile_external(
            &envelope,
            &command_context,
            ExternalEffectObservation::Absent,
        )
        .expect("effect absence");
    assert_eq!(reconciled.record.state, CommandState::Unknown);
    assert_eq!(
        reconciled.record.error_code.as_deref(),
        Some("effect_absent")
    );
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .unwrap(),
        Some(1)
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
#[allow(clippy::too_many_lines)]
fn older_unknown_password_revision_is_reconciled_before_newer_success() {
    let path = temporary_journal("password-superseded");
    let node = *Uuid::now_v7().as_bytes();
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&["ocserv.users.write"]);
    let mut old = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    old.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 2,
            sealed_password_v1: Some(user_password(vec![0xa2; 64])),
        },
    ));
    old.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    old.semantic_payload_sha256 = semantic_payload_hash_v1(&old).unwrap().to_vec();
    authorize_test_command(&mut old);
    let mut newer = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    newer.expected_revision = 1;
    newer.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 3,
            sealed_password_v1: Some(user_password(vec![0xa3; 64])),
        },
    ));
    newer.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    newer.semantic_payload_sha256 = semantic_payload_hash_v1(&newer).unwrap().to_vec();
    authorize_test_command(&mut newer);

    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
        &command_context,
        100,
        &mut privd_calls,
    );
    let ExternalPreparation::Execute(old_command) = executor
        .prepare_external(&old, &command_context)
        .expect("prepare old")
    else {
        panic!("old execute preparation")
    };
    executor
        .mark_external_unknown(&old_command, "privd_transport_unknown", 101)
        .expect("old unknown");
    assert!(matches!(
        executor.prepare_external(&newer, &command_context),
        Err(CommandError::Rejected("revision_gap"))
    ));
    let recovered_old = executor
        .begin_attested_recovery(&old, &command_context)
        .expect("recover old root receipt");
    executor
        .complete_external_applied_attested(
            &recovered_old,
            "user",
            "alice",
            2,
            b"applied",
            b"root-receipt",
            102,
        )
        .expect("persist old root receipt");
    let ExternalPreparation::Execute(new_command) = executor
        .prepare_external(&newer, &command_context)
        .expect("prepare newer")
    else {
        panic!("new execute preparation")
    };
    executor
        .complete_external_applied_attested(
            &new_command,
            "user",
            "alice",
            3,
            b"applied",
            b"root-receipt",
            102,
        )
        .expect("newer applied");
    command_context.now_unix_seconds = 103;
    let replayed = executor
        .reconcile_external(
            &old,
            &command_context,
            ExternalEffectObservation::SupersededByNewerRevision,
        )
        .expect("old terminal replay");
    assert_eq!(replayed.record.state, CommandState::Succeeded);
    assert_eq!(
        executor
            .journal()
            .applied_revision("user", "alice")
            .expect("applied revision"),
        Some(3)
    );
    cleanup_journal(&path);
}

#[test]
fn stale_effect_absent_retry_is_blocked_by_applied_revision() {
    let path = temporary_journal("password-stale-retry");
    let node = *Uuid::now_v7().as_bytes();
    let mut command_context = context(node, 100);
    command_context.capabilities = capabilities(&["ocserv.users.write"]);
    let mut old = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    old.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 2,
            sealed_password_v1: Some(user_password(vec![0xa2; 64])),
        },
    ));
    old.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    old.semantic_payload_sha256 = semantic_payload_hash_v1(&old).unwrap().to_vec();
    authorize_test_command(&mut old);
    let mut newer = old.clone();
    newer.command_id = Uuid::now_v7().as_bytes().to_vec();
    newer.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
    let Some(command_envelope::Payload::UserPasswordRotate(payload)) = newer.payload.as_mut()
    else {
        unreachable!()
    };
    payload.desired_revision = 3;
    payload
        .sealed_password_v1
        .as_mut()
        .expect("typed password")
        .ciphertext
        .fill(0xa3);
    newer.expected_revision = 1;
    newer.semantic_payload_sha256 = semantic_payload_hash_v1(&newer).unwrap().to_vec();
    authorize_test_command(&mut newer);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
    let mut privd_calls = 0;
    apply_desired(
        &mut executor,
        &desired_command(node, DesiredMutation::Create, "alice", 0, 1, 100),
        &command_context,
        100,
        &mut privd_calls,
    );
    let ExternalPreparation::Execute(old_command) = executor
        .prepare_external(&old, &command_context)
        .expect("prepare old")
    else {
        panic!("old execute preparation")
    };
    executor
        .mark_external_unknown(&old_command, "privd_transport_unknown", 101)
        .expect("old unknown");
    command_context.now_unix_seconds = 102;
    executor
        .reconcile_external(&old, &command_context, ExternalEffectObservation::Absent)
        .expect("old absent");
    let replacement = desired_command(node, DesiredMutation::Rotate, "alice", 1, 2, 100);
    apply_desired(
        &mut executor,
        &replacement,
        &command_context,
        102,
        &mut privd_calls,
    );
    let ExternalPreparation::Execute(new_command) = executor
        .prepare_external(&newer, &command_context)
        .expect("prepare newer")
    else {
        panic!("new execute preparation")
    };
    executor
        .complete_external_applied_attested(
            &new_command,
            "user",
            "alice",
            3,
            b"applied",
            b"root-receipt",
            103,
        )
        .expect("newer applied");
    command_context.now_unix_seconds = 104;
    assert!(matches!(
        executor.retry_external(&old, &command_context),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));
    cleanup_journal(&path);
}

fn cleanup_journal(path: &Path) {
    for suffix in ["", "-wal", "-shm"] {
        let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
    }
}

fn abort_matrix_command() -> (CommandEnvelope, CommandContext) {
    let node_id = [0x11; 16];
    let mut key = [0x22; 16];
    key[6] = 0x72;
    key[8] = 0x82;
    let mut command_id = [0x33; 16];
    command_id[6] = 0x73;
    command_id[8] = 0x83;
    let mut envelope = command(node_id, key, "once", 100);
    envelope.command_id = command_id.to_vec();
    authorize_test_command(&mut envelope);
    (envelope, context(node_id, 100))
}

#[test]
fn abort_crash_child() {
    let Ok(point) = std::env::var("OCSERVIA_ABORT_CRASH_POINT") else {
        return;
    };
    let path = std::env::var_os("OCSERVIA_ABORT_JOURNAL")
        .map(std::path::PathBuf::from)
        .expect("child journal path");
    let crash = match point.as_str() {
        "after-accepted" => CrashPoint::AfterAccepted,
        "before-side-effect" => CrashPoint::BeforeSideEffect,
        "after-side-effect" => CrashPoint::AfterSideEffect,
        "after-result" => CrashPoint::AfterResult,
        _ => panic!("unknown crash point"),
    };
    let (envelope, command_context) = abort_matrix_command();
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("child journal"));
    assert!(matches!(
        executor.execute(&envelope, &command_context, Some(crash)),
        Err(CommandError::InjectedCrash(point)) if point == crash
    ));
    std::process::abort();
}

#[test]
fn process_abort_crash_matrix_recovers_exactly_once() {
    for point in [
        "after-accepted",
        "before-side-effect",
        "after-side-effect",
        "after-result",
    ] {
        let path = temporary_journal(point);
        let status = Command::new(std::env::current_exe().expect("test executable"))
            .args(["--exact", "tests::abort_crash_child", "--nocapture"])
            .env("OCSERVIA_ABORT_CRASH_POINT", point)
            .env("OCSERVIA_ABORT_JOURNAL", &path)
            .status()
            .expect("spawn crash child");
        assert_eq!(status.signal(), Some(6), "{point}: {status}");

        let (envelope, command_context) = abort_matrix_command();
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("reopen"));
        let replayed = executor
            .execute(&envelope, &command_context, None)
            .expect("ordinary replay");
        let reconciled = if replayed.record.state == CommandState::Unknown {
            executor
                .reconcile(&envelope, &command_context)
                .expect("explicit reconcile")
        } else {
            replayed
        };
        let completed = if reconciled.record.state == CommandState::Unknown {
            executor
                .retry_unknown(&envelope, &command_context)
                .expect("safe retry")
        } else {
            reconciled
        };
        assert_eq!(completed.record.state, CommandState::Succeeded, "{point}");
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("effect count"),
            1,
            "{point}"
        );
        executor
            .journal()
            .integrity_check()
            .expect("integrity check");
        drop(executor);
        cleanup_journal(&path);
    }
}

#[test]
fn duplicate_delivery_ack_loss_and_restart_execute_once() {
    let path = temporary_journal("duplicates");
    let node_id = *Uuid::now_v7().as_bytes();
    let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
    let command_context = context(node_id, 100);
    {
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        let first = executor
            .execute(&envelope, &command_context, None)
            .expect("first");
        assert!(!first.replayed);
    }
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("restart"));
    for _ in 0..100 {
        let replay = executor
            .execute(&envelope, &command_context, None)
            .expect("ack-loss replay");
        assert!(replay.replayed);
        assert_eq!(replay.record.state, CommandState::Succeeded);
        assert_eq!(replay.record.result.as_deref(), Some(b"once".as_slice()));
    }
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        1
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn every_crash_boundary_reconciles_before_safe_retry() {
    for crash in [
        CrashPoint::AfterAccepted,
        CrashPoint::BeforeSideEffect,
        CrashPoint::AfterSideEffect,
        CrashPoint::AfterResult,
    ] {
        let path = temporary_journal("crash");
        let node_id = *Uuid::now_v7().as_bytes();
        let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        assert!(matches!(
            executor.execute(&envelope, &command_context, Some(crash)),
            Err(CommandError::InjectedCrash(point)) if point == crash
        ));
        drop(executor);

        let mut executor = CommandExecutor::new(Journal::open(&path).expect("restart"));
        let replayed = executor
            .execute(&envelope, &command_context, None)
            .expect("ordinary replay");
        let reconciled = if replayed.record.state == CommandState::Unknown {
            assert_eq!(
                replayed.record.error_code.as_deref(),
                Some("outcome_requires_reconciliation")
            );
            executor
                .reconcile(&envelope, &command_context)
                .expect("explicit reconcile")
        } else {
            replayed
        };
        let completed = if reconciled.record.state == CommandState::Unknown {
            assert_eq!(
                reconciled.record.error_code.as_deref(),
                Some("effect_absent")
            );
            executor
                .retry_unknown(&envelope, &command_context)
                .expect("explicit safe retry")
        } else {
            reconciled
        };
        assert_eq!(completed.record.state, CommandState::Succeeded);
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("count"),
            1
        );
        drop(executor);
        cleanup_journal(&path);
    }
}

#[test]
fn command_validation_fails_before_side_effect() {
    let path = temporary_journal("validation");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = command(node_id, key, "valid", 100);
    let base_context = context(node_id, 100);
    let cases = [
        ("node", {
            let mut value = base.clone();
            value.node_id = Uuid::now_v7().as_bytes().to_vec();
            value
        }),
        ("expired", {
            let mut value = base.clone();
            value.expires_at = Some(Timestamp {
                seconds: 100,
                nanos: 0,
            });
            value
        }),
        ("clock", {
            let mut value = base.clone();
            value.issued_at = Some(Timestamp {
                seconds: 401,
                nanos: 0,
            });
            value
        }),
        ("revision", {
            let mut value = base.clone();
            value.expected_revision = 2;
            value
        }),
        ("idempotency-key-version", {
            let mut value = base.clone();
            value.idempotency_key = [0_u8; 16].to_vec();
            value
        }),
        ("command-id-version", {
            let mut value = base.clone();
            value.command_id = [0_u8; 16].to_vec();
            value
        }),
    ];
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    for (label, value) in cases {
        assert!(
            matches!(
                executor.execute(&value, &base_context, None),
                Err(CommandError::Rejected(_))
            ),
            "{label}"
        );
    }
    let mut unsupported = base.clone();
    unsupported.payload = None;
    assert!(matches!(
        executor.execute(&unsupported, &base_context, None),
        Err(CommandError::Rejected("capability_rejected"))
    ));
    let mut no_capability = base_context.clone();
    no_capability.capabilities.clear();
    assert!(matches!(
        executor.execute(&base, &no_capability, None),
        Err(CommandError::Rejected("capability_rejected"))
    ));
    let mut expired_session = base_context.clone();
    expired_session.session_expires_at_unix_seconds = expired_session.now_unix_seconds;
    assert!(matches!(
        executor.execute(&base, &expired_session, None),
        Err(CommandError::Rejected("session_grant_expired"))
    ));
    let mut oversized = base.clone();
    oversized.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
        message: "x".repeat(4097),
    }));
    assert!(matches!(
        executor.execute(&oversized, &base_context, None),
        Err(CommandError::Rejected("payload_size_invalid"))
    ));
    let mut cancelled = base_context.clone();
    cancelled.cancelled = true;
    assert!(matches!(
        executor.execute(&base, &cancelled, None),
        Err(CommandError::Rejected("command_cancelled"))
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        0
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
#[allow(clippy::too_many_lines)]
fn controller_authorization_tampering_and_transport_forgery_fail_before_journal() {
    let path = temporary_journal("command-authorization");
    let node_id = *Uuid::now_v7().as_bytes();
    let base = command(node_id, *Uuid::now_v7().as_bytes(), "authorized", 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    let base_context = context(node_id, 100);

    let mut changed_command = base.clone();
    changed_command.command_id = Uuid::now_v7().as_bytes().to_vec();
    assert_rejected(
        &mut executor,
        &changed_command,
        &base_context,
        "command_authorization_signature_invalid",
    );

    let mut changed_idempotency = base.clone();
    changed_idempotency.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
    assert_rejected(
        &mut executor,
        &changed_idempotency,
        &base_context,
        "command_authorization_signature_invalid",
    );

    let mut changed_operation = base.clone();
    changed_operation.operation_id = Uuid::now_v7().as_bytes().to_vec();
    assert_rejected(
        &mut executor,
        &changed_operation,
        &base_context,
        "command_authorization_signature_invalid",
    );

    let mut changed_action = base.clone();
    changed_action.action = "operation.delete".to_owned();
    assert_rejected(
        &mut executor,
        &changed_action,
        &base_context,
        "command_authorization_action_mismatch",
    );

    let mut changed_node = base.clone();
    let changed_node_id = *Uuid::now_v7().as_bytes();
    changed_node.node_id = changed_node_id.to_vec();
    changed_node.semantic_payload_sha256 = semantic_payload_hash_v1(&changed_node)
        .expect("changed node semantic hash")
        .to_vec();
    let mut changed_node_context = base_context.clone();
    changed_node_context.node_id = changed_node_id;
    assert_rejected(
        &mut executor,
        &changed_node,
        &changed_node_context,
        "command_authorization_signature_invalid",
    );

    let mut changed_capability = base.clone();
    changed_capability.required_capability = "synthetic.noop".to_owned();
    assert_rejected(
        &mut executor,
        &changed_capability,
        &base_context,
        "command_authorization_capability_mismatch",
    );

    let mut changed_revision = base.clone();
    changed_revision.expected_revision = 2;
    changed_revision.semantic_payload_sha256 = semantic_payload_hash_v1(&changed_revision)
        .expect("changed revision semantic hash")
        .to_vec();
    assert_rejected(
        &mut executor,
        &changed_revision,
        &base_context,
        "revision_mismatch",
    );

    let mut changed_semantic_hash = base.clone();
    changed_semantic_hash.semantic_payload_sha256[0] ^= 1;
    assert_rejected(
        &mut executor,
        &changed_semantic_hash,
        &base_context,
        "semantic_payload_hash_mismatch",
    );

    let mut approval_bound = base.clone();
    approval_bound.approval_id = Uuid::now_v7().as_bytes().to_vec();
    approval_bound.approval_request_sha256 = vec![0xa5; 32];
    authorize_test_command(&mut approval_bound);
    approval_bound.approval_request_sha256[0] ^= 1;
    assert_rejected(
        &mut executor,
        &approval_bound,
        &base_context,
        "command_authorization_signature_invalid",
    );

    let mut expired = base.clone();
    expired.expires_at = Some(Timestamp {
        seconds: base_context.now_unix_seconds,
        nanos: 0,
    });
    assert_rejected(&mut executor, &expired, &base_context, "command_expired");

    let mut unknown_key = base.clone();
    unknown_key.authorization.as_mut().expect("proof").key_id =
        "ed25519-sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
            .to_owned();
    assert_rejected(
        &mut executor,
        &unknown_key,
        &base_context,
        "command_authorization_key_unknown",
    );

    let mut signature_flip = base.clone();
    signature_flip
        .authorization
        .as_mut()
        .expect("proof")
        .signature[0] ^= 1;
    assert_rejected(
        &mut executor,
        &signature_flip,
        &base_context,
        "command_authorization_signature_invalid",
    );

    let mut privileged = base.clone();
    privileged.payload = Some(command_envelope::Payload::SessionDisconnect(
        SessionDisconnect {
            session_id: "42".to_owned(),
            boot_id: Uuid::now_v7().to_string(),
        },
    ));
    privileged.semantic_payload_sha256 = semantic_payload_hash_v1(&privileged)
        .expect("privileged semantic hash")
        .to_vec();
    authorize_test_command(&mut privileged);
    let mut privileged_context = base_context.clone();
    privileged_context.capabilities = capabilities(&["ocserv.session.disconnect"]);

    let mut unsigned = privileged.clone();
    unsigned.authorization = None;
    assert_rejected(
        &mut executor,
        &unsigned,
        &privileged_context,
        "command_authorization_missing",
    );

    let mut transport_forgery = privileged;
    transport_forgery.command_id = Uuid::now_v7().as_bytes().to_vec();
    transport_forgery
        .authorization
        .as_mut()
        .expect("proof")
        .signature = vec![0; 64];
    assert_rejected(
        &mut executor,
        &transport_forgery,
        &privileged_context,
        "command_authorization_signature_invalid",
    );

    let base_key: [u8; 16] = base
        .idempotency_key
        .as_slice()
        .try_into()
        .expect("base key");
    assert_eq!(executor.journal().command(&base_key).unwrap(), None);
    assert_eq!(executor.journal().synthetic_execution_count().unwrap(), 0);
    drop(executor);
    cleanup_journal(&path);
}

fn assert_rejected(
    executor: &mut CommandExecutor,
    envelope: &CommandEnvelope,
    context: &CommandContext,
    expected: &'static str,
) {
    assert!(
        matches!(
            executor.execute(envelope, context, None),
            Err(CommandError::Rejected(code)) if code == expected
        ),
        "expected rejection {expected}"
    );
}

#[test]
fn v1_command_with_missing_semantic_payload_hash_is_rejected() {
    let path = temporary_journal("v1-missing-hash");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    // Declare v1 but omit the expected hash bytes.
    let mut envelope = command(node_id, key, "hello", 100);
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256.clear();
    assert!(matches!(
        executor.execute(&envelope, &command_context, None),
        Err(CommandError::Rejected("semantic_payload_hash_missing"))
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        0
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn v1_command_with_mismatched_semantic_payload_hash_is_rejected() {
    let path = temporary_journal("v1-hash-mismatch");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    // Declare v1 with a wrong hash (32 bytes of zeros).
    let mut envelope = command(node_id, key, "hello", 100);
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
    envelope.semantic_payload_sha256 = vec![0_u8; 32];
    assert!(matches!(
        executor.execute(&envelope, &command_context, None),
        Err(CommandError::Rejected("semantic_payload_hash_mismatch"))
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        0
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn v1_command_with_unknown_hash_version_is_rejected() {
    let path = temporary_journal("v1-unknown-version");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    // Use an unsupported version number.
    let mut envelope = command(node_id, key, "hello", 100);
    envelope.semantic_payload_hash_version = 99;
    assert!(matches!(
        executor.execute(&envelope, &command_context, None),
        Err(CommandError::Rejected("semantic_hash_version_unsupported"))
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        0
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn v1_command_with_correct_hash_is_accepted() {
    let path = temporary_journal("v1-correct");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    let envelope = command_v1(node_id, key, "hello", 100);
    let outcome = executor
        .execute(&envelope, &command_context, None)
        .expect("accept");
    assert!(!outcome.replayed);
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        1
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn same_key_different_payload_is_rejected_without_second_effect() {
    let path = temporary_journal("conflict");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    let first = command(node_id, key, "first", 100);
    executor
        .execute(&first, &command_context, None)
        .expect("first");
    let mut conflicting = first.clone();
    conflicting.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
        message: "second".to_owned(),
    }));
    conflicting.semantic_payload_sha256 = semantic_payload_hash_v1(&conflicting)
        .expect("conflicting semantic hash")
        .to_vec();
    authorize_test_command(&mut conflicting);
    assert!(matches!(
        executor.execute(&conflicting, &command_context, None),
        Err(CommandError::PayloadConflict)
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        1
    );
    drop(executor);
    cleanup_journal(&path);
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

fn agent_upgrade_envelope(
    node_id: [u8; 16],
    key: [u8; 16],
    target_version: &str,
    package_sha256: Vec<u8>,
    architecture: &str,
    now: i64,
) -> CommandEnvelope {
    let mut envelope = command(node_id, key, "", now);
    envelope.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: target_version.to_owned(),
        package_sha256,
        architecture: architecture.to_owned(),
    }));
    envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V2 as i32;
    envelope.semantic_payload_sha256 = semantic_payload_hash_v2(&envelope)
        .expect("agent upgrade semantic hash")
        .to_vec();
    authorize_test_command(&mut envelope);
    envelope
}

fn upgrade_context(node_id: [u8; 16], now: i64) -> CommandContext {
    let mut command_context = context(node_id, now);
    command_context.capabilities = capabilities(&[AGENT_UPGRADE_CAPABILITY]);
    command_context
}

fn rejected_code(envelope: &CommandEnvelope, command_context: &CommandContext) -> &'static str {
    match validate_command(envelope, command_context) {
        Err(CommandError::Rejected(code)) => code,
        Err(other) => panic!("expected a rejected command, got {other}"),
        Ok(_) => panic!("expected a rejected command, got success"),
    }
}

#[test]
fn agent_upgrade_command_requires_negotiated_capability() {
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let envelope = agent_upgrade_envelope(
        node_id,
        key,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
        100,
    );
    let mut without_capability = context(node_id, 100);
    without_capability.capabilities = capabilities(&["ocserv.service.reload"]);
    assert_eq!(
        rejected_code(&envelope, &without_capability),
        "capability_rejected"
    );
    validate_command(&envelope, &upgrade_context(node_id, 100))
        .expect("typed upgrade command validates with negotiated capability");
    for capability in ocservia_contracts::agent_upgrade::AGENT_UPGRADE_CAPABILITIES {
        let mut signed = envelope.clone();
        signed.required_capability = (*capability).to_owned();
        authorize_test_command(&mut signed);
        let mut negotiated = context(node_id, 100);
        negotiated.capabilities = capabilities(&[capability]);
        validate_command(&signed, &negotiated).expect("actual advertised command");
        let other = if *capability == "ocserv.agent.upgrade.v1" {
            "ocserv.agent.upgrade.v2"
        } else {
            "ocserv.agent.upgrade.v1"
        };
        negotiated.capabilities = capabilities(&[other]);
        assert_eq!(rejected_code(&signed, &negotiated), "capability_rejected");
        signed.required_capability = other.to_owned();
        assert!(
            validate_command(&signed, &negotiated).is_err(),
            "capability is signature-bound"
        );
    }
    let mut unknown = envelope;
    unknown.required_capability = "ocserv.agent.upgrade.v999".to_owned();
    assert!(validate_command(&unknown, &upgrade_context(node_id, 100)).is_err());
}

#[test]
fn agent_upgrade_validation_fails_at_the_payload_layer() {
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = agent_upgrade_envelope(
        node_id,
        key,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
        100,
    );
    let command_context = upgrade_context(node_id, 100);
    let mut cases: Vec<(&'static str, CommandEnvelope)> = Vec::new();
    let mut invalid_semver = base.clone();
    invalid_semver.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "latest".to_owned(),
        package_sha256: vec![0x43; 32],
        architecture: host_architecture().to_owned(),
    }));
    cases.push(("agent_upgrade_version_invalid", invalid_semver));
    let mut short_digest = base.clone();
    short_digest.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "1.2.3".to_owned(),
        package_sha256: vec![0x43; 31],
        architecture: host_architecture().to_owned(),
    }));
    cases.push(("agent_upgrade_digest_invalid", short_digest));
    let mut unsupported_arch = base.clone();
    unsupported_arch.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "1.2.3".to_owned(),
        package_sha256: vec![0x43; 32],
        architecture: "x86_64".to_owned(),
    }));
    cases.push(("agent_upgrade_architecture_invalid", unsupported_arch));
    let mut foreign_arch = base.clone();
    foreign_arch.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "1.2.3".to_owned(),
        package_sha256: vec![0x43; 32],
        architecture: foreign_architecture().to_owned(),
    }));
    cases.push(("agent_upgrade_architecture_mismatch", foreign_arch));
    for (expected_code, envelope) in cases {
        assert_eq!(rejected_code(&envelope, &command_context), expected_code);
    }
}

#[test]
fn agent_upgrade_authorization_boundaries_fail_closed() {
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = agent_upgrade_envelope(
        node_id,
        key,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
        100,
    );
    let command_context = upgrade_context(node_id, 100);

    let mut unsigned = base.clone();
    unsigned.authorization = None;
    assert_eq!(
        rejected_code(&unsigned, &command_context),
        "command_authorization_missing"
    );

    let mut wrong_key = base.clone();
    let attacker = SigningKey::from_bytes(&[0x66; 32]);
    let attacker_claims = claims_from_envelope_v1(&wrong_key).expect("claims");
    let signature = attacker.sign(&canonical_v1(&attacker_claims).expect("canonical"));
    wrong_key.authorization = Some(CommandAuthorizationProof {
        version: CommandAuthorizationVersion::V1.into(),
        key_id: verification_key_id(&attacker.verifying_key()),
        signature: signature.to_bytes().to_vec(),
    });
    assert_eq!(
        rejected_code(&wrong_key, &command_context),
        "command_authorization_key_unknown"
    );

    let mut wrong_capability = base.clone();
    wrong_capability.required_capability = "ocserv.users.write".to_owned();
    assert_eq!(
        rejected_code(&wrong_capability, &command_context),
        "capability_rejected"
    );

    let mut wrong_action = base.clone();
    wrong_action.action = "service.reload".to_owned();
    assert_eq!(
        rejected_code(&wrong_action, &command_context),
        "command_authorization_action_mismatch"
    );

    let mut wrong_node = base.clone();
    wrong_node.node_id = Uuid::now_v7().as_bytes().to_vec();
    assert_eq!(
        rejected_code(&wrong_node, &command_context),
        "node_id_mismatch"
    );

    let mut stale_revision = base.clone();
    stale_revision.expected_revision = 2;
    assert_eq!(
        rejected_code(&stale_revision, &command_context),
        "revision_mismatch"
    );

    let mut mutated_hash = base.clone();
    mutated_hash.semantic_payload_sha256[0] ^= 0xff;
    assert_eq!(
        rejected_code(&mutated_hash, &command_context),
        "semantic_payload_hash_mismatch"
    );
}

#[test]
fn agent_upgrade_idempotency_conflict_never_prepares_twice() {
    let path = temporary_journal("upgrade-conflict");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = upgrade_context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    let first = agent_upgrade_envelope(
        node_id,
        key,
        "1.2.3",
        vec![0x43; 32],
        host_architecture(),
        100,
    );
    assert!(matches!(
        executor.prepare_external(&first, &command_context),
        Ok(ExternalPreparation::Execute(_))
    ));
    let mut conflicting = first.clone();
    conflicting.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "2.0.0".to_owned(),
        package_sha256: vec![0x43; 32],
        architecture: host_architecture().to_owned(),
    }));
    conflicting.semantic_payload_sha256 = semantic_payload_hash_v2(&conflicting)
        .expect("conflicting semantic hash")
        .to_vec();
    authorize_test_command(&mut conflicting);
    assert!(matches!(
        executor.prepare_external(&conflicting, &command_context),
        Err(CommandError::PayloadConflict)
    ));
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn identity_conflicts_never_execute_a_second_effect() {
    let path = temporary_journal("identity-conflict");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let first = command(node_id, key, "first", 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    executor
        .execute(&first, &command_context, None)
        .expect("first");

    let mut different_command = first.clone();
    different_command.command_id = Uuid::now_v7().as_bytes().to_vec();
    authorize_test_command(&mut different_command);
    assert!(matches!(
        executor.execute(&different_command, &command_context, None),
        Err(CommandError::IdentityConflict)
    ));

    let mut different_key = first.clone();
    different_key.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
    authorize_test_command(&mut different_key);
    assert!(matches!(
        executor.execute(&different_key, &command_context, None),
        Err(CommandError::IdentityConflict)
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("count"),
        1
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn reconcile_and_retry_reject_stored_hash_version_conflict() {
    let path = temporary_journal("stored-version-conflict");
    let node_id = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let command_context = context(node_id, 100);
    let envelope = command_v1(node_id, key, "once", 100);
    {
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        assert!(matches!(
            executor.execute(&envelope, &command_context, Some(CrashPoint::AfterAccepted)),
            Err(CommandError::InjectedCrash(CrashPoint::AfterAccepted))
        ));
        executor
            .deliver(&envelope, &command_context)
            .expect("ordinary replay records uncertainty");
    }
    let connection = rusqlite::Connection::open(&path).expect("open journal for mutation");
    connection
        .execute(
            "UPDATE command_journal SET payload_hash_version=99 WHERE idempotency_key=?1",
            [key.as_slice()],
        )
        .expect("install incompatible stored hash version");
    drop(connection);

    let mut executor = CommandExecutor::new(Journal::open(&path).expect("reopen"));
    assert!(matches!(
        executor.reconcile(&envelope, &command_context),
        Err(CommandError::Rejected("semantic_hash_version_conflict"))
    ));
    let mut retry = envelope;
    retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    authorize_test_command(&mut retry);
    assert!(matches!(
        executor.retry_unknown(&retry, &command_context),
        Err(CommandError::Rejected("semantic_hash_version_conflict"))
    ));
    assert_eq!(
        executor
            .journal()
            .synthetic_execution_count()
            .expect("effect count"),
        0
    );
    drop(executor);
    cleanup_journal(&path);
}

#[test]
fn safe_retry_requires_explicit_effect_absence_reconciliation() {
    let path = temporary_journal("reconcile-required");
    let node_id = *Uuid::now_v7().as_bytes();
    let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
    let command_context = context(node_id, 100);
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
    assert!(matches!(
        executor.execute(&envelope, &command_context, Some(CrashPoint::AfterAccepted)),
        Err(CommandError::InjectedCrash(CrashPoint::AfterAccepted))
    ));
    assert!(matches!(
        executor.retry_unknown(&envelope, &command_context),
        Err(CommandError::Rejected("command_not_unknown"))
    ));
    executor
        .deliver(&envelope, &command_context)
        .expect("ordinary replay records uncertainty");
    let mut retry = envelope.clone();
    retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    authorize_test_command(&mut retry);
    assert!(matches!(
        executor.deliver(&retry, &command_context),
        Err(CommandError::Rejected("reconciliation_required"))
    ));
    let mut reconcile = envelope.clone();
    reconcile.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut reconcile);
    executor
        .deliver(&reconcile, &command_context)
        .expect("observe effect absence");
    executor
        .deliver(&retry, &command_context)
        .expect("safe retry");
    assert_eq!(executor.journal().synthetic_execution_count().unwrap(), 1);
    drop(executor);
    cleanup_journal(&path);
}

// ---- Canonical Semantic Payload Hash v1 ----
//
// The tests below pin the v1 algorithm defined in
// `docs/development/command-semantic-hash-v1.md` against the shared golden
// fixture `testdata/semantic-payload-hash-v1.json`. The inlined
// `compute_v1_canonical` mirror is an independent reference that writes each
// field by hand (no Protobuf). The production `semantic_payload_hash_v1`
// function must produce identical bytes.

/// V1 canonical preimage domain separator: ASCII label plus a NUL terminator.
const V1_DOMAIN_SEPARATOR: &[u8] = b"ocservia.command.semantic-hash.v1\0";

/// Independent reference mirror of the v1 canonical hash.
///
/// This intentionally does not use Protobuf serialization. It is the test
/// reference; the production `semantic_payload_hash_v1` must match it.
fn compute_v1_canonical(
    node_id: &[u8],
    expected_revision: u64,
    payload_kind: u32,
    canonical_payload: &[u8],
) -> [u8; 32] {
    let mut hash = Sha256::new();
    hash.update(V1_DOMAIN_SEPARATOR);
    hash.update(node_id);
    hash.update(expected_revision.to_be_bytes());
    hash.update(payload_kind.to_be_bytes());
    hash.update(canonical_payload);
    hash.finalize().into()
}

fn canonical_payload_for(kind: u32, message: &str) -> Vec<u8> {
    match kind {
        107 => Vec::new(), // SyntheticNoop
        108 => {
            // SyntheticEcho: u32_be(len(utf8)) || utf8
            let utf8 = message.as_bytes();
            let mut out = Vec::with_capacity(4 + utf8.len());
            out.extend_from_slice(&(u32::try_from(utf8.len()).unwrap()).to_be_bytes());
            out.extend_from_slice(utf8);
            out
        }
        _ => panic!("unsupported payload_kind in test: {kind}"),
    }
}

fn fixture_string_payload(values: &[&str]) -> Vec<u8> {
    let mut out = Vec::new();
    for value in values {
        out.extend_from_slice(&u32::try_from(value.len()).unwrap().to_be_bytes());
        out.extend_from_slice(value.as_bytes());
    }
    out
}

fn canonical_payload_from_fixture(kind: u32, payload: &serde_json::Value) -> Vec<u8> {
    let field = |name| {
        payload
            .get(name)
            .and_then(serde_json::Value::as_str)
            .unwrap_or("")
    };
    match kind {
        100 | 112 => fixture_string_payload(&[field("session_id"), field("boot_id")]),
        105 => Vec::new(),
        107 | 108 => canonical_payload_for(kind, field("message")),
        113 => fixture_string_payload(&[field("ip")]),
        _ => panic!("unsupported payload_kind in fixture: {kind}"),
    }
}

fn load_v1_fixture() -> serde_json::Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/semantic-payload-hash-v1.json");
    let raw = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("read v1 fixture {}: {e}", path.display()));
    serde_json::from_str(&raw)
        .unwrap_or_else(|e| panic!("parse v1 fixture {}: {e}", path.display()))
}

/// Builds a v1 envelope from fixture vector fields for hash verification.
fn envelope_from_fixture_vector(
    node_id: &[u8],
    expected_revision: u64,
    payload_kind: u32,
    message: &str,
) -> CommandEnvelope {
    let payload = match payload_kind {
        107 => Some(command_envelope::Payload::SyntheticNoop(SyntheticNoop {})),
        108 => Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: message.to_owned(),
        })),
        _ => panic!("unsupported payload_kind in test: {payload_kind}"),
    };
    CommandEnvelope {
        protocol_version: "1.0".to_owned(),
        message_id: Vec::new(),
        command_id: Vec::new(),
        idempotency_key: Vec::new(),
        node_id: node_id.to_vec(),
        sequence: 0,
        issued_at: None,
        expires_at: None,
        expected_revision,
        traceparent: String::new(),
        actor_id: String::new(),
        reason: String::new(),
        delivery_mode: CommandDeliveryMode::Unspecified.into(),
        payload,
        semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
        semantic_payload_sha256: Vec::new(),
        ..CommandEnvelope::default()
    }
}

fn envelope_from_fixture_payload(
    node_id: &[u8],
    expected_revision: u64,
    payload_kind: u32,
    payload: &serde_json::Value,
) -> CommandEnvelope {
    let field = |name| {
        payload
            .get(name)
            .and_then(serde_json::Value::as_str)
            .unwrap_or("")
            .to_owned()
    };
    let message = field("message");
    let mut envelope = envelope_from_fixture_vector(
        node_id,
        expected_revision,
        if matches!(payload_kind, 107 | 108) {
            payload_kind
        } else {
            107
        },
        &message,
    );
    envelope.payload = Some(match payload_kind {
        100 => command_envelope::Payload::SessionDisconnect(SessionDisconnect {
            session_id: field("session_id"),
            boot_id: field("boot_id"),
        }),
        105 => command_envelope::Payload::ServiceReload(ServiceReload {}),
        107 | 108 => return envelope,
        112 => command_envelope::Payload::SessionTerminate(SessionTerminate {
            session_id: field("session_id"),
            boot_id: field("boot_id"),
        }),
        113 => command_envelope::Payload::IpBanRemove(IpBanRemove { ip: field("ip") }),
        _ => panic!("unsupported payload_kind in fixture: {payload_kind}"),
    });
    envelope
}

#[test]
fn canonical_semantic_hash_v1_matches_shared_fixture() {
    let fixture = load_v1_fixture();
    for vector in fixture
        .get("vectors")
        .and_then(serde_json::Value::as_array)
        .expect("vectors array")
    {
        let name = vector
            .get("name")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("<unnamed>");
        let node_id = hex::decode(
            vector
                .get("node_id_hex")
                .and_then(serde_json::Value::as_str)
                .expect("node_id_hex"),
        )
        .expect("node_id hex");
        let expected_revision = vector
            .get("expected_revision")
            .and_then(serde_json::Value::as_u64)
            .expect("expected_revision");
        let payload_kind = vector
            .get("payload_kind")
            .and_then(serde_json::Value::as_u64)
            .expect("payload_kind");
        let payload_kind = u32::try_from(payload_kind).expect("payload_kind fits u32");
        let payload_fields = vector.get("payload").expect("payload");
        let expected = vector
            .get("expected_sha256")
            .and_then(serde_json::Value::as_str)
            .expect("expected_sha256");

        // Cross-check: the test-only mirror must agree with the fixture.
        let payload = canonical_payload_from_fixture(payload_kind, payload_fields);
        let mirror = compute_v1_canonical(&node_id, expected_revision, payload_kind, &payload);
        assert_eq!(
            hex::encode(mirror),
            expected,
            "vector {name:?} mirror digest mismatch"
        );

        // Production function must agree with the mirror.
        let envelope = envelope_from_fixture_payload(
            &node_id,
            expected_revision,
            payload_kind,
            payload_fields,
        );
        let production = semantic_payload_hash_v1(&envelope).expect("v1 hash");
        assert_eq!(
            hex::encode(production),
            expected,
            "vector {name:?} production digest mismatch"
        );
    }
}

#[test]
fn canonical_semantic_hash_v2_matches_shared_config_plan_vector() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/semantic-payload-hash-v2.json");
    let fixture: serde_json::Value = serde_json::from_str(
        &std::fs::read_to_string(&path)
            .unwrap_or_else(|error| panic!("read {}: {error}", path.display())),
    )
    .unwrap_or_else(|error| panic!("parse {}: {error}", path.display()));
    let vector = fixture.get("vector").expect("vector");
    let node_id =
        hex::decode(vector["node_id_hex"].as_str().expect("node ID")).expect("node ID hex");
    let candidate_hash = hex::decode(
        vector["candidate_hash_hex"]
            .as_str()
            .expect("candidate hash"),
    )
    .expect("candidate hash hex");
    let expected_revision = vector["config_expected_revision"]
        .as_u64()
        .expect("config revision");
    let mut envelope = CommandEnvelope {
        node_id,
        expected_revision: vector["authorization_revision"]
            .as_u64()
            .expect("authorization revision"),
        payload: Some(command_envelope::Payload::ConfigPlan(ConfigPlan {
            candidate_hash,
            expected_revision,
            ..ConfigPlan::default()
        })),
        ..CommandEnvelope::default()
    };
    let digest = semantic_payload_hash_v2(&envelope).expect("v2 hash");
    assert_eq!(
        hex::encode(digest),
        vector["expected_sha256"].as_str().expect("expected hash")
    );
    let Some(command_envelope::Payload::ConfigPlan(payload)) = envelope.payload.as_mut() else {
        panic!("ConfigPlan payload");
    };
    payload.expected_revision += 1;
    assert_ne!(
        semantic_payload_hash_v2(&envelope).expect("changed v2 hash"),
        digest
    );
}

#[test]
fn canonical_semantic_hash_v1_excludes_delivery_metadata() {
    let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
    let message = "hello";
    // Build two envelopes that differ only in delivery/audit metadata.
    let mut a = envelope_from_fixture_vector(&node, 1, 108, message);
    let mut b = envelope_from_fixture_vector(&node, 1, 108, message);
    a.message_id = vec![1_u8; 16];
    a.command_id = vec![2_u8; 16];
    a.idempotency_key = vec![3_u8; 16];
    a.sequence = 42;
    a.traceparent = "00-aaa".to_owned();
    a.actor_id = "alice".to_owned();
    a.reason = "ticket-1".to_owned();
    a.delivery_mode = CommandDeliveryMode::ExecuteOrReplay.into();
    b.message_id = vec![9_u8; 16];
    b.command_id = vec![8_u8; 16];
    b.idempotency_key = vec![7_u8; 16];
    b.sequence = 99;
    b.traceparent = "00-bbb".to_owned();
    b.actor_id = "bob".to_owned();
    b.reason = "ticket-2".to_owned();
    b.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    // Only node_id, revision, kind, and payload bytes enter the preimage,
    // so changing delivery/audit fields cannot affect the digest.
    assert_eq!(
        semantic_payload_hash_v1(&a).expect("v1 hash a"),
        semantic_payload_hash_v1(&b).expect("v1 hash b"),
    );
}

#[test]
fn canonical_semantic_hash_v1_changes_on_semantic_input() {
    let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
    let node_shifted = [1_u8, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16];

    let hello = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "hello"))
        .expect("v1 hash");
    // message change
    let world = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "world"))
        .expect("v1 hash");
    assert_ne!(hello, world);
    // node_id change
    let shifted = semantic_payload_hash_v1(&envelope_from_fixture_vector(
        &node_shifted,
        1,
        108,
        "hello",
    ))
    .expect("v1 hash");
    assert_ne!(hello, shifted);
    // revision change
    let rev2 = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 2, 108, "hello"))
        .expect("v1 hash");
    assert_ne!(hello, rev2);
    // payload kind change (noop vs echo)
    let noop = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 107, ""))
        .expect("v1 hash");
    assert_ne!(hello, noop);
}

#[test]
fn canonical_semantic_hash_v1_rejects_unicode_normalization() {
    let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
    // Precomposed é (U+00E9) vs decomposed e + combining acute (U+0301).
    let composed =
        semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "\u{00e9}"))
            .expect("v1 hash");
    let decomposed =
        semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "e\u{0301}"))
            .expect("v1 hash");
    assert_ne!(
        composed, decomposed,
        "no Unicode normalization: different bytes must hash differently"
    );
}
