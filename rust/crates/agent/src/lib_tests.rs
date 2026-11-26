#![allow(
    clippy::many_single_char_names,
    clippy::semicolon_if_nothing_returned,
    clippy::similar_names,
    clippy::too_many_lines,
    clippy::type_complexity
)]

use std::io;
use std::os::unix::process::ExitStatusExt as _;
use std::path::Path;
use std::process::Command;
use std::time::Duration;

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_command_authorization::{
    COMMAND_PROTOCOL_VERSION, ControllerCommandKeyring, canonical_v1, claims_from_envelope_v1,
    verification_key_id,
};
use ocservia_contracts::config_profile;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, CommandAuthorizationProof, CompleteConfigApply, CompleteConfigCandidate,
    CompleteConfigDirective, ConfigApplyResult, ConfigPlan, GroupApply, NodeLocalTlsReference,
    SemanticPayloadHashVersion, SessionDisconnect, SyntheticEcho, SyntheticNoop, UserCreate,
    UserDisable, UserEnable, UserPasswordRotate, command_envelope, complete_config_directive,
};
use ocservia_test_support::{
    PrivateTempDir, foreign_architecture, host_architecture, keyring, ts,
    user_password_secret as user_password,
};
use prost::Message as _;
use rand::{SeedableRng, rngs::StdRng};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::*;
use crate::validation::validate_payload;

struct TempJournal {
    _dir: PrivateTempDir,
    path: std::path::PathBuf,
}

impl TempJournal {
    fn new(label: &str) -> Self {
        let dir = PrivateTempDir::new("agt-j");
        let path = dir.join(format!("{label}.db"));
        Self { _dir: dir, path }
    }

    fn path(&self) -> &Path {
        &self.path
    }

    fn open(&self) -> CommandExecutor {
        CommandExecutor::new(Journal::open(&self.path).expect("open journal"))
    }
}

fn capabilities(values: &[&str]) -> HashSet<String> {
    values.iter().map(|v| (*v).to_owned()).collect()
}

fn test_signing_key() -> SigningKey {
    SigningKey::from_bytes(&[
        0x9d, 0x61, 0xb1, 0x9d, 0xef, 0xfd, 0x5a, 0x60, 0xba, 0x84, 0x4a, 0xf4, 0x92, 0xec, 0x2c,
        0xc4, 0x44, 0x49, 0xc5, 0x69, 0x7b, 0x32, 0x69, 0x19, 0x70, 0x3b, 0xac, 0x03, 0x1c, 0xae,
        0x7f, 0x60,
    ])
}

fn test_keyring() -> ControllerCommandKeyring {
    keyring(&test_signing_key())
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
    envelope.protocol_version = COMMAND_PROTOCOL_VERSION.into();
    envelope.operation_id.clone_from(&envelope.idempotency_key);
    envelope.action = action.into();
    envelope.required_capability = capability.into();
    let key = test_signing_key();
    envelope.authorization = Some(CommandAuthorizationProof {
        version: 1,
        key_id: verification_key_id(&key.verifying_key()),
        signature: Vec::new(),
    });
    let claims = claims_from_envelope_v1(envelope).expect("claims");
    let canonical = canonical_v1(&claims).expect("canonical");
    envelope.authorization.as_mut().expect("auth").signature =
        key.sign(&canonical).to_bytes().to_vec();
}

fn command(node_id: [u8; 16], key: [u8; 16], message: &str, now: i64) -> CommandEnvelope {
    let mut env = CommandEnvelope {
        protocol_version: COMMAND_PROTOCOL_VERSION.into(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: key.to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(ts(now, 0)),
        expires_at: Some(ts(now + 60, 0)),
        expected_revision: 1,
        traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01".into(),
        actor_id: "test".into(),
        reason: "fault matrix".into(),
        operation_id: key.to_vec(),
        action: "operation.create".into(),
        required_capability: "synthetic.echo".into(),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        payload: Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: message.into(),
        })),
        semantic_payload_hash_version: SemanticPayloadHashVersion::V2 as i32,
        ..CommandEnvelope::default()
    };
    env.semantic_payload_sha256 = semantic_payload_hash_v2(&env).expect("v2 hash").to_vec();
    authorize_test_command(&mut env);
    env
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

fn desired_context(node_id: [u8; 16], now: i64) -> CommandContext {
    let mut ctx = context(node_id, now);
    ctx.capabilities = capabilities(&["ocserv.users.write", "ocserv.groups.write"]);
    ctx
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
    desired_revision: u64,
    now: i64,
) -> CommandEnvelope {
    let mut env = command(node_id, *Uuid::now_v7().as_bytes(), "", now);
    env.payload = Some(match mutation {
        DesiredMutation::Create => command_envelope::Payload::UserCreate(UserCreate {
            username: name.into(),
            desired_revision,
            sealed_password_v1: Some(user_password(vec![0xa1; 64])),
        }),
        DesiredMutation::Rotate => {
            command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
                username: name.into(),
                desired_revision,
                sealed_password_v1: Some(user_password(vec![
                    u8::try_from(desired_revision)
                        .unwrap_or(0xa5);
                    64
                ])),
            })
        }
        DesiredMutation::Disable => command_envelope::Payload::UserDisable(UserDisable {
            username: name.into(),
            desired_revision,
        }),
        DesiredMutation::Enable => command_envelope::Payload::UserEnable(UserEnable {
            username: name.into(),
            desired_revision,
        }),
        DesiredMutation::Group => command_envelope::Payload::GroupApply(GroupApply {
            group_name: name.into(),
            members: vec!["alice".into()],
            desired_revision,
        }),
    });
    env.semantic_payload_hash_version = SemanticPayloadHashVersion::V2 as i32;
    env.semantic_payload_sha256 = semantic_payload_hash_v2(&env).expect("hash").to_vec();
    authorize_test_command(&mut env);
    env
}

fn apply_desired(
    executor: &mut CommandExecutor,
    envelope: &CommandEnvelope,
    ctx: &CommandContext,
    now: i64,
    privd_calls: &mut usize,
) {
    let ExternalPreparation::Execute(cmd) = executor.prepare_external(envelope, ctx).expect("prep")
    else {
        panic!("fresh desired command must execute");
    };
    *privd_calls += 1;
    let (rtype, rkey, rev) = desired_resource(envelope).expect("resource");
    executor
        .complete_external_applied_attested(
            &cmd,
            rtype,
            rkey,
            rev,
            b"applied",
            b"root-receipt",
            now,
        )
        .expect("complete");
}

fn agent_upgrade_envelope(
    node_id: [u8; 16],
    key: [u8; 16],
    version: &str,
    sha256: Vec<u8>,
    arch: &str,
    now: i64,
) -> CommandEnvelope {
    let mut env = command(node_id, key, "", now);
    env.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: version.into(),
        package_sha256: sha256,
        architecture: arch.into(),
    }));
    env.semantic_payload_hash_version = SemanticPayloadHashVersion::V2 as i32;
    env.semantic_payload_sha256 = semantic_payload_hash_v2(&env).expect("v2 hash").to_vec();
    authorize_test_command(&mut env);
    env
}

fn rejected_code(env: &CommandEnvelope, ctx: &CommandContext) -> &'static str {
    match validate_command(env, ctx) {
        Err(CommandError::Rejected(code)) => code,
        Err(other) => panic!("expected rejection, got {other}"),
        Ok(_) => panic!("expected rejection, got Ok"),
    }
}

fn assert_exec_rejected(
    executor: &mut CommandExecutor,
    env: &CommandEnvelope,
    ctx: &CommandContext,
    expected: &'static str,
) {
    assert!(
        matches!(executor.execute(env, ctx, None), Err(CommandError::Rejected(c)) if c == expected),
        "expected rejection {expected}"
    );
}

#[test]
fn basic_executor_limits_uid_jitter_and_expiry_rules() {
    let client = PrivdClient::new(
        "/tmp/ocservia-test-privd.sock".into(),
        Duration::from_secs(5),
    )
    .expect("client");
    assert_eq!(
        (client.timeout, client.desired_timeout),
        (Duration::from_secs(5), Duration::from_secs(20))
    );
    let regular_cmd = CommandEnvelope {
        payload: Some(command_envelope::Payload::ServiceReload(
            ocservia_contracts::generated::ocserv::platform::agent::v1::ServiceReload {},
        )),
        ..CommandEnvelope::default()
    };
    let apply_cmd = CommandEnvelope {
        payload: Some(command_envelope::Payload::CompleteConfigApply(
            ocservia_contracts::generated::ocserv::platform::agent::v1::CompleteConfigApply {
                ..Default::default()
            },
        )),
        ..CommandEnvelope::default()
    };
    assert_eq!(
        client.command_timeout(&regular_cmd),
        Duration::from_secs(20)
    );
    assert_eq!(client.command_timeout(&apply_cmd), Duration::from_secs(75));

    let oversized_group = CommandEnvelope {
        payload: Some(command_envelope::Payload::GroupApply(GroupApply {
            group_name: "staff".into(),
            members: (0..=ocservia_agent_protocol::MAX_MANAGED_RESOURCES)
                .map(|i| format!("member{i:03}"))
                .collect(),
            desired_revision: 1,
        })),
        ..CommandEnvelope::default()
    };
    assert!(matches!(
        validate_payload(&oversized_group),
        Err(CommandError::Rejected("group_members_invalid"))
    ));

    assert_eq!(
        ensure_unprivileged(0).expect_err("root").kind(),
        io::ErrorKind::PermissionDenied
    );
    ensure_unprivileged(65532).expect("service UID");

    let mut rng = StdRng::seed_from_u64(7);
    let delays: Vec<_> = (0..64)
        .map(|_| Backoff::default().delay(20, &mut rng))
        .collect();
    assert!(delays.iter().all(|d| *d <= Duration::from_secs(15)));
    assert!(delays.windows(2).any(|p| p[0] != p[1]));

    // Expired signed commands only allow ReconcileOnly; mutating delivery_mode invalidates signature.
    let node = *Uuid::now_v7().as_bytes();
    let mut env = command(node, *Uuid::now_v7().as_bytes(), "expiry", 100);
    let ctx = context(node, 200);
    for mode in [
        CommandDeliveryMode::ExecuteOrReplay,
        CommandDeliveryMode::ReconcileOnly,
        CommandDeliveryMode::RetryIfEffectAbsent,
    ] {
        env.delivery_mode = mode.into();
        authorize_test_command(&mut env);
        assert_eq!(
            validate_command(&env, &ctx).is_ok(),
            mode == CommandDeliveryMode::ReconcileOnly
        );
    }
    env.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut env);
    env.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    assert!(validate_command(&env, &ctx).is_err());
}

#[test]
fn config_reconciliation_rebuilds_lost_journal_from_exact_durable_effect() {
    use complete_config_directive::Value::{Literal, Tls};

    let journal = TempJournal::new("config-journal-rebuild");
    let node = *Uuid::now_v7().as_bytes();
    let tls = NodeLocalTlsReference {
        secret_ref_id: vec![0x12; 16],
        version: "v1".into(),
        certificate_sha256: vec![0x23; 32],
        spki_sha256: vec![0x34; 32],
        ca_sha256: Vec::new(),
    };
    let mut directives: Vec<_> = [
        ("auth", "plain[passwd=/etc/ocserv/ocpasswd]"),
        ("cookie-timeout", "300"),
        ("device", "vpns"),
        ("dns", "1.1.1.1"),
        ("ipv4-network", "192.0.2.0/24"),
        ("max-clients", "64"),
        ("max-same-clients", "2"),
        ("socket-file", "/run/ocserv.socket"),
        ("tcp-port", "443"),
        ("udp-port", "0"),
    ]
    .into_iter()
    .map(|(k, v)| CompleteConfigDirective {
        name: k.into(),
        value: Some(Literal(v.into())),
    })
    .collect();
    directives.insert(
        7,
        CompleteConfigDirective {
            name: "server-cert".into(),
            value: Some(Tls(tls.clone())),
        },
    );
    directives.insert(
        8,
        CompleteConfigDirective {
            name: "server-key".into(),
            value: Some(Tls(tls)),
        },
    );
    let candidate = CompleteConfigCandidate {
        node_id: node.to_vec(),
        expected_revision: 6,
        directives,
    };
    let candidate_hash =
        Sha256::digest(config_profile::canonical(&candidate).expect("canonical")).to_vec();

    let mut env = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    env.payload = Some(command_envelope::Payload::CompleteConfigApply(
        CompleteConfigApply {
            candidate: Some(candidate),
            candidate_hash: candidate_hash.clone(),
            materialized_hash: vec![0x55; 32],
            expected_current_hash: vec![0x44; 32],
            desired_revision: 7,
            plan_id: Uuid::now_v7().as_bytes().to_vec(),
            plan_expires_at: Some(ts(200, 0)),
        },
    ));
    env.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    env.semantic_payload_hash_version = SemanticPayloadHashVersion::V2.into();
    env.semantic_payload_sha256 = semantic_payload_hash_v2(&env).expect("v2").to_vec();
    authorize_test_command(&mut env);

    let mut ctx = context(node, 100);
    ctx.capabilities = capabilities(&[config_profile::APPLY_CAPABILITY]);
    let mut executor = journal.open();
    let ExternalPreparation::Execute(recovered) = executor
        .begin_attested_recovery(&env, &ctx)
        .expect("recover")
    else {
        panic!("expected recovery execution");
    };
    let res_bytes = observed_result(&env);
    let outcome = executor
        .complete_external_applied_attested(
            &recovered,
            "config",
            "ocserv.conf",
            7,
            &res_bytes,
            b"root-receipt",
            101,
        )
        .expect("rebuild");
    assert_eq!(outcome.record.state, CommandState::Succeeded);
    // A subsequent RECONCILE_ONLY delivery for an already-settled command must
    // replay the persisted attested outcome instead of rejecting with
    // reconciliation_state_invalid.
    let ExternalPreparation::Replay(replayed) = executor
        .begin_attested_recovery(&env, &ctx)
        .expect("replay settled attested command")
    else {
        panic!("expected replay for already-succeeded command");
    };
    assert!(replayed.replayed);
    assert_eq!(replayed.record.state, CommandState::Succeeded);
    assert_eq!(
        replayed.record.privileged_result_proof.as_deref(),
        Some(b"root-receipt".as_slice())
    );
    assert_eq!(
        executor
            .journal()
            .applied_revision("config", "ocserv.conf")
            .expect("rev"),
        Some(7)
    );
    let decoded =
        ConfigApplyResult::decode(outcome.record.result.as_deref().expect("res")).expect("decode");
    assert_eq!(
        (decoded.applied_revision, decoded.observed_hash),
        (7, candidate_hash)
    );
    let replay = executor
        .reconcile_external(&env, &ctx, ExternalEffectObservation::AppliedExact)
        .expect("replay");
    assert_eq!(replay.record.result, outcome.record.result);
}

#[test]
fn desired_revision_fences_cross_kind_restart_and_gap_enforcement() {
    use DesiredMutation::{Create, Disable, Enable, Group, Rotate};

    let journal = TempJournal::new("desired-fences");
    let node = *Uuid::now_v7().as_bytes();
    let ctx = desired_context(node, 100);
    let mut calls = 0;

    {
        let mut ex = journal.open();
        // Cross-kind stale user mutations rejected before privd.
        for (user, seq, stale) in [
            ("alice", [Create, Rotate, Disable], Rotate),
            ("bob", [Create, Rotate, Disable], Enable),
            ("carol", [Create, Rotate, Rotate], Disable),
        ] {
            for (idx, m) in seq.into_iter().enumerate() {
                let rev = u64::try_from(idx + 1).expect("rev");
                apply_desired(
                    &mut ex,
                    &desired_command(node, m, user, rev, 100),
                    &ctx,
                    101 + i64::try_from(idx).expect("i"),
                    &mut calls,
                );
            }
            assert!(matches!(
                ex.prepare_external(&desired_command(node, stale, user, 2, 100), &ctx),
                Err(CommandError::Rejected(
                    "effect_superseded_by_newer_revision"
                ))
            ));
            assert_eq!(
                ex.journal().applied_revision("user", user).expect("u"),
                Some(3)
            );
        }

        // Sequential cross-kind mutations advance a single resource revision monotonically.
        for (m, rev) in [(Enable, 4), (Rotate, 5)] {
            apply_desired(
                &mut ex,
                &desired_command(node, m, "alice", rev, 100),
                &ctx,
                105,
                &mut calls,
            );
        }
        assert_eq!(
            ex.journal().applied_revision("user", "alice").expect("u"),
            Some(5)
        );

        // Stale group apply rejected before privd.
        for rev in 1..=3 {
            apply_desired(
                &mut ex,
                &desired_command(node, Group, "staff", rev, 100),
                &ctx,
                100 + i64::try_from(rev).expect("r"),
                &mut calls,
            );
        }
        assert!(matches!(
            ex.prepare_external(&desired_command(node, Group, "staff", 2, 100), &ctx),
            Err(CommandError::Rejected(
                "effect_superseded_by_newer_revision"
            ))
        ));
        assert_eq!(
            ex.journal().applied_revision("group", "staff").expect("g"),
            Some(3)
        );

        // Revision gap rejected before privd.
        apply_desired(
            &mut ex,
            &desired_command(node, Create, "dave", 1, 100),
            &ctx,
            101,
            &mut calls,
        );
        assert!(matches!(
            ex.prepare_external(&desired_command(node, Disable, "dave", 3, 100), &ctx),
            Err(CommandError::Rejected("revision_gap"))
        ));
    }

    // Durable revision fence survives agent restart.
    let mut restarted = journal.open();
    assert!(matches!(
        restarted.prepare_external(&desired_command(node, Rotate, "alice", 2, 100), &ctx),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));
    assert_eq!(calls, 15);
}

#[test]
fn failed_and_expired_desired_revisions_allow_replacement() {
    use DesiredMutation::{Create, Rotate};

    let journal = TempJournal::new("desired-replace");
    let node = *Uuid::now_v7().as_bytes();
    let mut calls = 0;

    // Deterministic failure does not advance applied_revision and allows replacement.
    let ctx1 = desired_context(node, 100);
    let mut ex = journal.open();
    apply_desired(
        &mut ex,
        &desired_command(node, Create, "alice", 1, 100),
        &ctx1,
        101,
        &mut calls,
    );
    let failed = desired_command(node, Rotate, "alice", 2, 100);
    let ExternalPreparation::Execute(cmd) = ex.prepare_external(&failed, &ctx1).expect("prep")
    else {
        panic!("must execute");
    };
    ex.complete_external_attested(&cmd, Err("privd_rejected"), b"root-receipt", 102)
        .expect("fail");
    assert_eq!(
        ex.journal().applied_revision("user", "alice").expect("rev"),
        Some(1)
    );
    apply_desired(
        &mut ex,
        &desired_command(node, Rotate, "alice", 2, 100),
        &ctx1,
        103,
        &mut calls,
    );
    assert_eq!(
        ex.journal().applied_revision("user", "alice").expect("rev"),
        Some(2)
    );

    // Offline 5-minute expiry rejects before effect and allows replacement.
    let ctx2 = desired_context(node, 1_300);
    apply_desired(
        &mut ex,
        &desired_command(node, Create, "bob", 1, 1_300),
        &ctx2,
        101,
        &mut calls,
    );
    let mut expired = desired_command(node, Rotate, "bob", 2, 1_000);
    expired.expires_at = Some(ts(1_300, 0));
    assert!(matches!(
        ex.prepare_external(&expired, &ctx2),
        Err(CommandError::Rejected("command_expired"))
    ));
    assert_eq!(
        ex.journal().applied_revision("user", "bob").expect("rev"),
        Some(1)
    );
    apply_desired(
        &mut ex,
        &desired_command(node, Rotate, "bob", 2, 1_300),
        &ctx2,
        102,
        &mut calls,
    );
    assert_eq!(
        ex.journal().applied_revision("user", "bob").expect("rev"),
        Some(2)
    );
    assert_eq!(calls, 4);
}

#[test]
fn external_command_replay_and_password_reconcile_recovery() {
    use DesiredMutation::{Create, Rotate};

    // External command durability and duplicate replay.
    let j1 = TempJournal::new("external-replay");
    let node = [0x41; 16];
    let mut disc = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    disc.payload = Some(command_envelope::Payload::SessionDisconnect(
        SessionDisconnect {
            session_id: "42".into(),
            boot_id: Uuid::now_v7().to_string(),
        },
    ));
    disc.semantic_payload_sha256 = semantic_payload_hash_v2(&disc).expect("h").to_vec();
    authorize_test_command(&mut disc);
    let mut dctx = context(node, 100);
    dctx.capabilities = capabilities(&["ocserv.session.disconnect"]);
    let mut ex1 = j1.open();
    let ExternalPreparation::Execute(c) = ex1.prepare_external(&disc, &dctx).expect("prep") else {
        panic!("execute");
    };
    assert_eq!(
        ex1.complete_external_attested(&c, Ok(b"applied"), b"root-receipt", 101)
            .expect("ok")
            .record
            .state,
        CommandState::Succeeded
    );
    let ExternalPreparation::Replay(rep) = ex1.prepare_external(&disc, &dctx).expect("replay")
    else {
        panic!("replay");
    };
    assert!(rep.replayed && rep.record.state == CommandState::Succeeded);

    // Unknown desired password recovers attested result & applied_revision after restart.
    let j2 = TempJournal::new("desired-reconcile");
    let node2 = *Uuid::now_v7().as_bytes();
    let mut ctx = desired_context(node2, 100);
    let rot2 = desired_command(node2, Rotate, "alice", 2, 100);
    {
        let mut ex = j2.open();
        let mut calls = 0;
        apply_desired(
            &mut ex,
            &desired_command(node2, Create, "alice", 1, 100),
            &ctx,
            100,
            &mut calls,
        );
        let ExternalPreparation::Execute(c) = ex.prepare_external(&rot2, &ctx).expect("prep")
        else {
            panic!("exec");
        };
        assert_eq!(
            ex.mark_external_unknown(&c, "privd_transport_unknown", 101)
                .expect("u")
                .record
                .state,
            CommandState::Unknown
        );
    }
    ctx.now_unix_seconds = 102;
    let mut ex2 = j2.open();
    let ExternalPreparation::Execute(rec_cmd) =
        ex2.begin_attested_recovery(&rot2, &ctx).expect("attested")
    else {
        panic!("expected attested recovery execution");
    };
    let rec = ex2
        .complete_external_applied_attested(
            &rec_cmd,
            "user",
            "alice",
            2,
            b"applied",
            b"root-receipt",
            102,
        )
        .expect("complete");
    assert_eq!(rec.record.state, CommandState::Succeeded);
    assert_eq!(
        ex2.journal()
            .applied_revision("user", "alice")
            .expect("rev"),
        Some(2)
    );

    // Absent password remains Unknown (effect_absent) without advancing revision.
    let rot3 = desired_command(node2, Rotate, "alice", 3, 102);
    let ExternalPreparation::Execute(c3) = ex2.prepare_external(&rot3, &ctx).expect("prep3") else {
        panic!("exec3");
    };
    ex2.mark_external_unknown(&c3, "privd_transport_unknown", 103)
        .expect("u3");
    ctx.now_unix_seconds = 104;
    let abs = ex2
        .reconcile_external(&rot3, &ctx, ExternalEffectObservation::Absent)
        .expect("absent");
    assert_eq!(abs.record.state, CommandState::Unknown);
    assert_eq!(abs.record.error_code.as_deref(), Some("effect_absent"));
    assert_eq!(
        ex2.journal()
            .applied_revision("user", "alice")
            .expect("rev"),
        Some(2)
    );
}

#[test]
fn reconcile_only_restores_receipt_from_a_running_command_after_restart() {
    let journal = TempJournal::new("reconcile-running");
    let node = *Uuid::now_v7().as_bytes();
    let mut ctx = desired_context(node, 100);
    let mut cmd = desired_command(node, DesiredMutation::Create, "alice", 1, 100);
    {
        let mut executor = journal.open();
        assert!(matches!(
            executor.prepare_external(&cmd, &ctx).expect("execute"),
            ExternalPreparation::Execute(_)
        ));
    }
    ctx.now_unix_seconds += 1;
    cmd.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut cmd);
    let mut executor = journal.open();
    let ExternalPreparation::Execute(command) = executor
        .begin_attested_recovery(&cmd, &ctx)
        .expect("running recovery")
    else {
        panic!("resume running command");
    };
    let outcome = executor
        .complete_external_applied_attested(
            &command,
            "user",
            "alice",
            1,
            b"applied",
            b"root-receipt",
            101,
        )
        .expect("recovered terminal proof");
    assert_eq!(outcome.record.state, CommandState::Succeeded);
    assert_eq!(outcome.record.accepted_at, 100);
    let ExternalPreparation::Replay(replay) = executor
        .begin_attested_recovery(&cmd, &ctx)
        .expect("Controller lost the result")
    else {
        panic!("replay the settled command");
    };
    assert!(replay.replayed);
    assert_eq!(replay.record, outcome.record);
}

#[test]
fn password_revision_supersession_and_stale_retry_block() {
    use DesiredMutation::{Create, Rotate};

    // Older unknown revision must be reconciled before newer revision can advance; superseded replays Succeeded.
    let j1 = TempJournal::new("pw-superseded");
    let node = *Uuid::now_v7().as_bytes();
    let mut ctx = desired_context(node, 100);
    let old = desired_command(node, Rotate, "alice", 2, 100);
    let newer = desired_command(node, Rotate, "alice", 3, 100);
    let mut ex = j1.open();
    let mut calls = 0;
    apply_desired(
        &mut ex,
        &desired_command(node, Create, "alice", 1, 100),
        &ctx,
        100,
        &mut calls,
    );
    let ExternalPreparation::Execute(old_c) = ex.prepare_external(&old, &ctx).expect("old") else {
        panic!("exec");
    };
    ex.mark_external_unknown(&old_c, "privd_transport_unknown", 101)
        .expect("unk");
    assert!(matches!(
        ex.prepare_external(&newer, &ctx),
        Err(CommandError::Rejected("revision_gap"))
    ));
    let ExternalPreparation::Execute(rec_old) =
        ex.begin_attested_recovery(&old, &ctx).expect("rec old")
    else {
        panic!("expected old attested recovery execution");
    };
    ex.complete_external_applied_attested(
        &rec_old,
        "user",
        "alice",
        2,
        b"applied",
        b"root-receipt",
        102,
    )
    .expect("ok2");
    apply_desired(&mut ex, &newer, &ctx, 102, &mut calls);
    ctx.now_unix_seconds = 103;
    let rep = ex
        .reconcile_external(
            &old,
            &ctx,
            ExternalEffectObservation::SupersededByNewerRevision,
        )
        .expect("replay");
    assert_eq!(rep.record.state, CommandState::Succeeded);
    assert_eq!(
        ex.journal().applied_revision("user", "alice").expect("rev"),
        Some(3)
    );

    // Stale effect_absent retry blocked once applied_revision moves past it.
    let j2 = TempJournal::new("pw-stale-retry");
    let mut ex2 = j2.open();
    ctx.now_unix_seconds = 100;
    apply_desired(
        &mut ex2,
        &desired_command(node, Create, "alice", 1, 100),
        &ctx,
        100,
        &mut calls,
    );
    let old2 = desired_command(node, Rotate, "alice", 2, 100);
    let ExternalPreparation::Execute(oc2) = ex2.prepare_external(&old2, &ctx).expect("old2") else {
        panic!("exec");
    };
    ex2.mark_external_unknown(&oc2, "privd_transport_unknown", 101)
        .expect("unk");
    ctx.now_unix_seconds = 102;
    ex2.reconcile_external(&old2, &ctx, ExternalEffectObservation::Absent)
        .expect("absent");
    apply_desired(
        &mut ex2,
        &desired_command(node, Rotate, "alice", 2, 100),
        &ctx,
        102,
        &mut calls,
    );
    apply_desired(
        &mut ex2,
        &desired_command(node, Rotate, "alice", 3, 100),
        &ctx,
        103,
        &mut calls,
    );
    ctx.now_unix_seconds = 104;
    assert!(matches!(
        ex2.retry_external(&old2, &ctx),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));
}

fn abort_matrix_command() -> (CommandEnvelope, CommandContext) {
    let node_id = [0x11; 16];
    let mut key = [0x22; 16];
    key[6] = 0x72;
    key[8] = 0x82;
    let mut command_id = [0x33; 16];
    command_id[6] = 0x73;
    command_id[8] = 0x83;
    let mut env = command(node_id, key, "once", 100);
    env.command_id = command_id.to_vec();
    authorize_test_command(&mut env);
    (env, context(node_id, 100))
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
    let (env, ctx) = abort_matrix_command();
    let mut executor = CommandExecutor::new(Journal::open(&path).expect("child journal"));
    assert!(
        matches!(executor.execute(&env, &ctx, Some(crash)), Err(CommandError::InjectedCrash(p)) if p == crash)
    );
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
        let journal = TempJournal::new(point);
        let status = Command::new(std::env::current_exe().expect("test exe"))
            .args(["--exact", "tests::abort_crash_child", "--nocapture"])
            .env("OCSERVIA_ABORT_CRASH_POINT", point)
            .env("OCSERVIA_ABORT_JOURNAL", journal.path())
            .status()
            .expect("spawn crash child");
        assert_eq!(status.signal(), Some(6), "{point}: {status}");

        let (env, ctx) = abort_matrix_command();
        let mut ex = journal.open();
        let rep = ex.execute(&env, &ctx, None).expect("replay");
        let rec = if rep.record.state == CommandState::Unknown {
            ex.reconcile(&env, &ctx).expect("reconcile")
        } else {
            rep
        };
        let done = if rec.record.state == CommandState::Unknown {
            ex.retry_unknown(&env, &ctx).expect("retry")
        } else {
            rec
        };
        assert_eq!(done.record.state, CommandState::Succeeded, "{point}");
        assert_eq!(
            ex.journal().synthetic_execution_count().expect("count"),
            1,
            "{point}"
        );
        ex.journal().integrity_check().expect("integrity");
    }
}

#[test]
fn crash_boundaries_ack_loss_and_safe_retry_reconciliation() {
    // Ack-loss duplicate delivery executes once across restart.
    let j_dup = TempJournal::new("duplicates");
    let node = *Uuid::now_v7().as_bytes();
    let env = command(node, *Uuid::now_v7().as_bytes(), "once", 100);
    let ctx = context(node, 100);
    assert!(
        !j_dup
            .open()
            .execute(&env, &ctx, None)
            .expect("first")
            .replayed
    );
    let mut ex_dup = j_dup.open();
    for _ in 0..16 {
        let rep = ex_dup.execute(&env, &ctx, None).expect("replay");
        assert!(rep.replayed && rep.record.state == CommandState::Succeeded);
        assert_eq!(rep.record.result.as_deref(), Some(b"once".as_slice()));
    }
    assert_eq!(ex_dup.journal().synthetic_execution_count().expect("c"), 1);

    // Every in-process crash boundary reconciles before safe retry.
    for crash in [
        CrashPoint::AfterAccepted,
        CrashPoint::BeforeSideEffect,
        CrashPoint::AfterSideEffect,
        CrashPoint::AfterResult,
    ] {
        let j = TempJournal::new("crash");
        let e = command(node, *Uuid::now_v7().as_bytes(), "once", 100);
        assert!(
            matches!(j.open().execute(&e, &ctx, Some(crash)), Err(CommandError::InjectedCrash(p)) if p == crash)
        );
        let mut ex = j.open();
        let rep = ex.execute(&e, &ctx, None).expect("replay");
        let rec = if rep.record.state == CommandState::Unknown {
            assert_eq!(
                rep.record.error_code.as_deref(),
                Some("outcome_requires_reconciliation")
            );
            ex.reconcile(&e, &ctx).expect("reconcile")
        } else {
            rep
        };
        let done = if rec.record.state == CommandState::Unknown {
            assert_eq!(rec.record.error_code.as_deref(), Some("effect_absent"));
            ex.retry_unknown(&e, &ctx).expect("retry")
        } else {
            rec
        };
        assert_eq!(done.record.state, CommandState::Succeeded);
        assert_eq!(ex.journal().synthetic_execution_count().expect("c"), 1);
    }

    // Safe retry requires explicit effect_absent reconciliation first.
    let j_req = TempJournal::new("reconcile-required");
    let e = command(node, *Uuid::now_v7().as_bytes(), "once", 100);
    let mut ex = j_req.open();
    assert!(matches!(
        ex.execute(&e, &ctx, Some(CrashPoint::AfterAccepted)),
        Err(CommandError::InjectedCrash(_))
    ));
    assert!(matches!(
        ex.retry_unknown(&e, &ctx),
        Err(CommandError::Rejected("command_not_unknown"))
    ));
    ex.deliver(&e, &ctx).expect("uncertainty");
    let mut retry = e.clone();
    retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    authorize_test_command(&mut retry);
    assert!(matches!(
        ex.deliver(&retry, &ctx),
        Err(CommandError::Rejected("reconciliation_required"))
    ));
    let mut reconcile = e;
    reconcile.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut reconcile);
    ex.deliver(&reconcile, &ctx).expect("observe absent");
    ex.deliver(&retry, &ctx).expect("safe retry");
    assert_eq!(ex.journal().synthetic_execution_count().expect("c"), 1);
}

#[test]
fn command_validation_authorization_tampering_and_v1_hash_rules() {
    let journal = TempJournal::new("validation-auth");
    let node = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = command(node, key, "authorized", 100);
    let ctx = context(node, 100);
    let mut ex = journal.open();

    // Context-level rejections.
    let mut no_cap = ctx.clone();
    no_cap.capabilities.clear();
    assert_exec_rejected(&mut ex, &base, &no_cap, "capability_rejected");
    let mut exp_sess = ctx.clone();
    exp_sess.session_expires_at_unix_seconds = ctx.now_unix_seconds;
    assert_exec_rejected(&mut ex, &base, &exp_sess, "session_grant_expired");
    let mut cancelled = ctx.clone();
    cancelled.cancelled = true;
    assert_exec_rejected(&mut ex, &base, &cancelled, "command_cancelled");

    // Envelope validation, semantic hash v2, and signature tampering matrix.
    let mut changed_node = base.clone();
    let other_node = *Uuid::now_v7().as_bytes();
    changed_node.node_id = other_node.to_vec();
    changed_node.semantic_payload_sha256 =
        semantic_payload_hash_v2(&changed_node).expect("h").to_vec();
    let mut other_ctx = ctx.clone();
    other_ctx.node_id = other_node;
    assert_exec_rejected(
        &mut ex,
        &changed_node,
        &other_ctx,
        "command_authorization_signature_invalid",
    );

    let cases: &[(&str, fn(&mut CommandEnvelope))] = &[
        ("node_id_mismatch", |c| {
            c.node_id = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("command_expired", |c| c.expires_at = Some(ts(100, 0))),
        ("clock_skew", |c| c.issued_at = Some(ts(401, 0))),
        ("revision_mismatch", |c| {
            c.expected_revision = 2;
            c.semantic_payload_sha256 = semantic_payload_hash_v2(c).expect("h").to_vec();
        }),
        ("idempotency_key_invalid", |c| {
            c.idempotency_key = vec![0; 16]
        }),
        ("command_id_invalid", |c| c.command_id = vec![0; 16]),
        ("capability_rejected", |c| c.payload = None),
        ("payload_size_invalid", |c| {
            c.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: "x".repeat(4097),
            }));
        }),
        ("command_authorization_signature_invalid", |c| {
            c.command_id = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("command_authorization_signature_invalid", |c| {
            c.idempotency_key = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("command_authorization_signature_invalid", |c| {
            c.operation_id = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("command_authorization_action_mismatch", |c| {
            c.action = "operation.delete".into()
        }),
        ("command_authorization_capability_mismatch", |c| {
            c.required_capability = "synthetic.noop".into()
        }),
        ("semantic_payload_hash_missing", |c| {
            c.semantic_payload_sha256.clear()
        }),
        ("semantic_payload_hash_mismatch", |c| {
            c.semantic_payload_sha256[0] ^= 1
        }),
        ("semantic_hash_version_unsupported", |c| {
            c.semantic_payload_hash_version = 99
        }),
        ("command_authorization_key_unknown", |c| {
            c.authorization.as_mut().expect("p").key_id =
                "ed25519-sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
                    .into();
        }),
        ("command_authorization_signature_invalid", |c| {
            c.authorization.as_mut().expect("p").signature[0] ^= 1
        }),
    ];
    for (expected, mutate) in cases {
        let mut env = base.clone();
        mutate(&mut env);
        assert_exec_rejected(&mut ex, &env, &ctx, expected);
    }

    // Privileged unsigned & forged transport commands fail before journal.
    let mut priv_cmd = base.clone();
    priv_cmd.payload = Some(command_envelope::Payload::SessionDisconnect(
        SessionDisconnect {
            session_id: "42".into(),
            boot_id: Uuid::now_v7().to_string(),
        },
    ));
    priv_cmd.semantic_payload_sha256 = semantic_payload_hash_v2(&priv_cmd).expect("h").to_vec();
    authorize_test_command(&mut priv_cmd);
    let mut pctx = ctx.clone();
    pctx.capabilities = capabilities(&["ocserv.session.disconnect"]);
    let mut unsigned = priv_cmd.clone();
    unsigned.authorization = None;
    assert_exec_rejected(&mut ex, &unsigned, &pctx, "command_authorization_missing");
    let mut forged = priv_cmd;
    forged.command_id = Uuid::now_v7().as_bytes().to_vec();
    forged.authorization.as_mut().expect("p").signature = vec![0; 64];
    assert_exec_rejected(
        &mut ex,
        &forged,
        &pctx,
        "command_authorization_signature_invalid",
    );

    assert_eq!(ex.journal().command(&key).expect("cmd"), None);
    assert_eq!(ex.journal().synthetic_execution_count().expect("c"), 0);

    // Valid v1 command is accepted and executes once.
    assert!(!ex.execute(&base, &ctx, None).expect("accept").replayed);
    assert_eq!(ex.journal().synthetic_execution_count().expect("c"), 1);
}

#[test]
fn idempotency_identity_and_stored_hash_version_conflicts() {
    let journal = TempJournal::new("conflicts");
    let node = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let ctx = context(node, 100);
    let first = command(node, key, "first", 100);
    let mut ex = journal.open();
    ex.execute(&first, &ctx, None).expect("first");

    // Same key + different payload => PayloadConflict.
    let mut diff_payload = first.clone();
    diff_payload.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
        message: "second".into(),
    }));
    diff_payload.semantic_payload_sha256 =
        semantic_payload_hash_v2(&diff_payload).expect("h").to_vec();
    authorize_test_command(&mut diff_payload);
    assert!(matches!(
        ex.execute(&diff_payload, &ctx, None),
        Err(CommandError::PayloadConflict)
    ));

    // Same key + different command_id, or same command_id + different key => IdentityConflict.
    let mut diff_cmd = first.clone();
    diff_cmd.command_id = Uuid::now_v7().as_bytes().to_vec();
    authorize_test_command(&mut diff_cmd);
    assert!(matches!(
        ex.execute(&diff_cmd, &ctx, None),
        Err(CommandError::IdentityConflict)
    ));

    let mut diff_key = first;
    diff_key.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
    authorize_test_command(&mut diff_key);
    assert!(matches!(
        ex.execute(&diff_key, &ctx, None),
        Err(CommandError::IdentityConflict)
    ));
    assert_eq!(ex.journal().synthetic_execution_count().expect("c"), 1);

    // Stored hash version mismatch rejects reconcile & retry without executing.
    let j2 = TempJournal::new("stored-version");
    let key2 = *Uuid::now_v7().as_bytes();
    let env2 = command(node, key2, "once", 100);
    {
        let mut ex2 = j2.open();
        assert!(matches!(
            ex2.execute(&env2, &ctx, Some(CrashPoint::AfterAccepted)),
            Err(CommandError::InjectedCrash(_))
        ));
        ex2.deliver(&env2, &ctx).expect("unknown");
    }
    rusqlite::Connection::open(j2.path())
        .expect("sql")
        .execute(
            "UPDATE command_journal SET payload_hash_version=99 WHERE idempotency_key=?1",
            [key2.as_slice()],
        )
        .expect("mutate");
    let mut ex2 = j2.open();
    assert!(matches!(
        ex2.reconcile(&env2, &ctx),
        Err(CommandError::Rejected("semantic_hash_version_conflict"))
    ));
    let mut retry = env2;
    retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
    authorize_test_command(&mut retry);
    assert!(matches!(
        ex2.retry_unknown(&retry, &ctx),
        Err(CommandError::Rejected("semantic_hash_version_conflict"))
    ));
    assert_eq!(ex2.journal().synthetic_execution_count().expect("c"), 0);
}

#[test]
fn agent_upgrade_capability_payload_auth_and_conflict() {
    let node = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = agent_upgrade_envelope(node, key, "1.2.3", vec![0x43; 32], host_architecture(), 100);
    let mut ctx = context(node, 100);
    ctx.capabilities = capabilities(&[AGENT_UPGRADE_CAPABILITY]);

    // Capability negotiation & signature binding across v1/v2 upgrade capabilities.
    let mut no_upgrade = context(node, 100);
    no_upgrade.capabilities = capabilities(&["ocserv.service.reload"]);
    assert_eq!(rejected_code(&base, &no_upgrade), "capability_rejected");
    validate_command(&base, &ctx).expect("valid upgrade");
    for cap in ocservia_contracts::agent_upgrade::AGENT_UPGRADE_CAPABILITIES {
        let mut signed = base.clone();
        signed.required_capability = (*cap).into();
        authorize_test_command(&mut signed);
        let mut neg = context(node, 100);
        neg.capabilities = capabilities(&[cap]);
        validate_command(&signed, &neg).expect("negotiated");
        let other = if *cap == "ocserv.agent.upgrade.v1" {
            "ocserv.agent.upgrade.v2"
        } else {
            "ocserv.agent.upgrade.v1"
        };
        neg.capabilities = capabilities(&[other]);
        assert_eq!(rejected_code(&signed, &neg), "capability_rejected");
        signed.required_capability = other.into();
        assert!(validate_command(&signed, &neg).is_err());
    }
    let mut unknown_cap = base.clone();
    unknown_cap.required_capability = "ocserv.agent.upgrade.v999".into();
    assert!(validate_command(&unknown_cap, &ctx).is_err());

    // Payload-layer rejections.
    for (expected, ver, digest, arch) in [
        (
            "agent_upgrade_version_invalid",
            "latest",
            vec![0x43; 32],
            host_architecture(),
        ),
        (
            "agent_upgrade_digest_invalid",
            "1.2.3",
            vec![0x43; 31],
            host_architecture(),
        ),
        (
            "agent_upgrade_architecture_invalid",
            "1.2.3",
            vec![0x43; 32],
            "x86_64",
        ),
        (
            "agent_upgrade_architecture_mismatch",
            "1.2.3",
            vec![0x43; 32],
            foreign_architecture(),
        ),
    ] {
        let mut env = base.clone();
        env.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
            target_version: ver.into(),
            package_sha256: digest,
            architecture: arch.into(),
        }));
        assert_eq!(rejected_code(&env, &ctx), expected);
    }

    // Authorization boundary rejections.
    let mut wrong_key = base.clone();
    let attacker = SigningKey::from_bytes(&[0x66; 32]);
    let claims = claims_from_envelope_v1(&wrong_key).expect("claims");
    wrong_key.authorization = Some(CommandAuthorizationProof {
        version: 1,
        key_id: verification_key_id(&attacker.verifying_key()),
        signature: attacker
            .sign(&canonical_v1(&claims).expect("c"))
            .to_bytes()
            .to_vec(),
    });
    assert_eq!(
        rejected_code(&wrong_key, &ctx),
        "command_authorization_key_unknown"
    );

    let auth_cases: &[(&str, fn(&mut CommandEnvelope))] = &[
        ("command_authorization_missing", |c| c.authorization = None),
        ("capability_rejected", |c| {
            c.required_capability = "ocserv.users.write".into()
        }),
        ("command_authorization_action_mismatch", |c| {
            c.action = "service.reload".into()
        }),
        ("node_id_mismatch", |c| {
            c.node_id = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("revision_mismatch", |c| c.expected_revision = 2),
        ("semantic_payload_hash_mismatch", |c| {
            c.semantic_payload_sha256[0] ^= 0xff
        }),
    ];
    for (expected, mutate) in auth_cases {
        let mut env = base.clone();
        mutate(&mut env);
        assert_eq!(rejected_code(&env, &ctx), *expected);
    }

    // Idempotency conflict never prepares twice.
    let journal = TempJournal::new("upgrade-conflict");
    let mut ex = journal.open();
    assert!(matches!(
        ex.prepare_external(&base, &ctx),
        Ok(ExternalPreparation::Execute(_))
    ));
    let mut conflicting = base;
    conflicting.payload = Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
        target_version: "2.0.0".into(),
        package_sha256: vec![0x43; 32],
        architecture: host_architecture().into(),
    }));
    conflicting.semantic_payload_sha256 =
        semantic_payload_hash_v2(&conflicting).expect("v2").to_vec();
    authorize_test_command(&mut conflicting);
    assert!(matches!(
        ex.prepare_external(&conflicting, &ctx),
        Err(CommandError::PayloadConflict)
    ));
}

// ---- Canonical Semantic Payload Hash v2 Golden Vectors ----

fn envelope_from_fixture_vector(
    node_id: &[u8],
    expected_revision: u64,
    kind: u32,
    message: &str,
) -> CommandEnvelope {
    let payload = match kind {
        107 => Some(command_envelope::Payload::SyntheticNoop(SyntheticNoop {})),
        108 => Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: message.into(),
        })),
        _ => panic!("unsupported payload_kind: {kind}"),
    };
    CommandEnvelope {
        protocol_version: "1.0".into(),
        node_id: node_id.to_vec(),
        expected_revision,
        payload,
        ..CommandEnvelope::default()
    }
}

#[test]
fn canonical_semantic_hash_v2_matches_shared_config_plan_vector() {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/semantic-payload-hash-v2.json");
    let fixture: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&path).expect("read v2 fixture"))
            .expect("parse");
    let v = &fixture["vector"];
    let mut env = CommandEnvelope {
        node_id: hex::decode(v["node_id_hex"].as_str().expect("node")).expect("hex"),
        expected_revision: v["authorization_revision"].as_u64().expect("auth rev"),
        payload: Some(command_envelope::Payload::ConfigPlan(ConfigPlan {
            candidate_hash: hex::decode(v["candidate_hash_hex"].as_str().expect("cand"))
                .expect("hex"),
            expected_revision: v["config_expected_revision"].as_u64().expect("cfg rev"),
            ..ConfigPlan::default()
        })),
        ..CommandEnvelope::default()
    };
    let digest = semantic_payload_hash_v2(&env).expect("v2");
    assert_eq!(
        hex::encode(digest),
        v["expected_sha256"].as_str().expect("sha256")
    );
    let Some(command_envelope::Payload::ConfigPlan(p)) = env.payload.as_mut() else {
        panic!("ConfigPlan");
    };
    p.expected_revision += 1;
    assert_ne!(semantic_payload_hash_v2(&env).expect("changed"), digest);
}

#[test]
fn canonical_semantic_hash_v2_invariance_sensitivity_and_no_unicode_normalization() {
    let node = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
    let shifted = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16];

    // Excludes delivery/audit metadata.
    let mut a = envelope_from_fixture_vector(&node, 1, 108, "hello");
    let mut b = envelope_from_fixture_vector(&node, 1, 108, "hello");
    a.message_id = vec![1; 16];
    a.command_id = vec![2; 16];
    a.idempotency_key = vec![3; 16];
    a.sequence = 42;
    a.traceparent = "00-aaa".into();
    a.actor_id = "alice".into();
    a.reason = "ticket-1".into();
    a.delivery_mode = CommandDeliveryMode::ExecuteOrReplay.into();
    b.message_id = vec![9; 16];
    b.command_id = vec![8; 16];
    b.idempotency_key = vec![7; 16];
    b.sequence = 99;
    b.traceparent = "00-bbb".into();
    b.actor_id = "bob".into();
    b.reason = "ticket-2".into();
    b.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    let hello = semantic_payload_hash_v2(&a).expect("a");
    assert_eq!(hello, semantic_payload_hash_v2(&b).expect("b"));

    // Changes on every semantic input and does not normalize Unicode.
    for other in [
        envelope_from_fixture_vector(&node, 1, 108, "world"),
        envelope_from_fixture_vector(&shifted, 1, 108, "hello"),
        envelope_from_fixture_vector(&node, 2, 108, "hello"),
        envelope_from_fixture_vector(&node, 1, 107, ""),
    ] {
        assert_ne!(hello, semantic_payload_hash_v2(&other).expect("other"));
    }
    assert_ne!(
        semantic_payload_hash_v2(&envelope_from_fixture_vector(&node, 1, 108, "\u{00e9}"))
            .expect("c"),
        semantic_payload_hash_v2(&envelope_from_fixture_vector(&node, 1, 108, "e\u{0301}"))
            .expect("d"),
    );
}
