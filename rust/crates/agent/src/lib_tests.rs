#![allow(
    clippy::many_single_char_names,
    clippy::semicolon_if_nothing_returned,
    clippy::similar_names,
    clippy::too_many_lines,
    clippy::type_complexity
)]

use std::path::Path;

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_command_authorization::{
    COMMAND_PROTOCOL_VERSION, ControllerCommandKeyring, canonical_v1, claims_from_envelope_v1,
    verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandAuthorizationProof, ConfigPlan, SemanticPayloadHashVersion, SessionDisconnect,
    SyntheticEcho, UserCreate, UserDisable, UserPasswordRotate, command_envelope,
};
use ocservia_test_support::{PrivateTempDir, keyring, ts, user_password_secret as user_password};
use uuid::Uuid;

use super::*;

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
        Some(command_envelope::Payload::SyntheticEcho(_)) => ("operation.create", "synthetic.echo"),
        Some(command_envelope::Payload::SessionDisconnect(_)) => {
            ("session.disconnect", "ocserv.session.disconnect")
        }
        Some(command_envelope::Payload::UserCreate(_)) => ("user.create", "ocserv.users.write"),
        Some(command_envelope::Payload::UserPasswordRotate(_)) => {
            ("user.password.rotate", "ocserv.users.write")
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
        capabilities: capabilities(&["synthetic.noop", "synthetic.echo", "ocserv.users.write"]),
        session_expires_at_unix_seconds: now + 300,
        command_keys: test_keyring(),
        now_unix_seconds: now,
        cancelled: false,
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
fn external_command_replay_and_reconcile_only_recovery() {
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

    let j2 = TempJournal::new("reconcile-running");
    let node2 = *Uuid::now_v7().as_bytes();
    let mut ctx = context(node2, 100);
    let mut cmd = command(node2, *Uuid::now_v7().as_bytes(), "", 100);
    cmd.payload = Some(command_envelope::Payload::UserCreate(UserCreate {
        username: "alice".into(),
        desired_revision: 1,
        sealed_password_v1: Some(user_password(vec![0xa1; 64])),
    }));
    cmd.semantic_payload_sha256 = semantic_payload_hash_v2(&cmd).expect("h").to_vec();
    authorize_test_command(&mut cmd);
    {
        let mut ex = j2.open();
        assert!(matches!(
            ex.prepare_external(&cmd, &ctx).expect("exec"),
            ExternalPreparation::Execute(_)
        ));
    }
    ctx.now_unix_seconds += 1;
    cmd.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
    authorize_test_command(&mut cmd);
    let mut ex2 = j2.open();
    let ExternalPreparation::Execute(command) =
        ex2.begin_attested_recovery(&cmd, &ctx).expect("recover")
    else {
        panic!("resume");
    };
    let outcome = ex2
        .complete_external_applied_attested(
            &command,
            "user",
            "alice",
            1,
            b"applied",
            b"root-receipt",
            101,
        )
        .expect("ok");
    assert_eq!(outcome.record.state, CommandState::Succeeded);
    assert!(
        matches!(ex2.begin_attested_recovery(&cmd, &ctx).expect("replay"), ExternalPreparation::Replay(r) if r.replayed)
    );
}

#[test]
fn desired_revision_fences_and_idempotency_conflicts() {
    let journal = TempJournal::new("fences-conflicts");
    let node = *Uuid::now_v7().as_bytes();
    let mut ctx = context(node, 100);
    let mut create = command(node, *Uuid::now_v7().as_bytes(), "", 100);
    create.payload = Some(command_envelope::Payload::UserCreate(UserCreate {
        username: "alice".into(),
        desired_revision: 1,
        sealed_password_v1: Some(user_password(vec![0xa1; 64])),
    }));
    create.semantic_payload_sha256 = semantic_payload_hash_v2(&create).expect("h").to_vec();
    authorize_test_command(&mut create);

    let mut ex = journal.open();
    let ExternalPreparation::Execute(c1) = ex.prepare_external(&create, &ctx).expect("c1") else {
        panic!("c1");
    };
    ex.complete_external_applied_attested(&c1, "user", "alice", 1, b"ok", b"proof", 100)
        .expect("done");

    let mut stale = command(node, *Uuid::now_v7().as_bytes(), "", 101);
    stale.payload = Some(command_envelope::Payload::UserPasswordRotate(
        UserPasswordRotate {
            username: "alice".into(),
            desired_revision: 1,
            sealed_password_v1: Some(user_password(vec![0xa2; 64])),
        },
    ));
    stale.semantic_payload_sha256 = semantic_payload_hash_v2(&stale).expect("h").to_vec();
    authorize_test_command(&mut stale);
    ctx.now_unix_seconds = 101;
    assert!(matches!(
        ex.prepare_external(&stale, &ctx),
        Err(CommandError::Rejected(
            "effect_superseded_by_newer_revision"
        ))
    ));

    let key = *Uuid::now_v7().as_bytes();
    let first = command(node, key, "first", 101);
    ex.execute(&first, &ctx, None).expect("first");
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

    let mut diff_cmd = first;
    diff_cmd.command_id = Uuid::now_v7().as_bytes().to_vec();
    authorize_test_command(&mut diff_cmd);
    assert!(matches!(
        ex.execute(&diff_cmd, &ctx, None),
        Err(CommandError::IdentityConflict)
    ));
}

#[test]
fn command_validation_authorization_tampering_and_semantic_hash_vector() {
    let journal = TempJournal::new("validation-auth");
    let node = *Uuid::now_v7().as_bytes();
    let key = *Uuid::now_v7().as_bytes();
    let base = command(node, key, "authorized", 100);
    let ctx = context(node, 100);
    let mut ex = journal.open();

    let mut no_cap = ctx.clone();
    no_cap.capabilities.clear();
    assert_exec_rejected(&mut ex, &base, &no_cap, "capability_rejected");
    let mut exp_sess = ctx.clone();
    exp_sess.session_expires_at_unix_seconds = ctx.now_unix_seconds;
    assert_exec_rejected(&mut ex, &base, &exp_sess, "session_grant_expired");

    let cases: &[(&str, fn(&mut CommandEnvelope))] = &[
        ("node_id_mismatch", |c| {
            c.node_id = Uuid::now_v7().as_bytes().to_vec()
        }),
        ("command_expired", |c| c.expires_at = Some(ts(100, 0))),
        ("clock_skew", |c| c.issued_at = Some(ts(401, 0))),
        ("command_authorization_action_mismatch", |c| {
            c.action = "operation.delete".into()
        }),
        ("semantic_payload_hash_mismatch", |c| {
            c.semantic_payload_sha256[0] ^= 1
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

    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/semantic-payload-hash-v2.json");
    let fixture: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("fixture")).expect("json");
    let v = &fixture["vector"];
    assert_eq!(v["payload_kind"].as_u64(), Some(102));
    let mut env = CommandEnvelope {
        node_id: hex::decode(v["node_id_hex"].as_str().expect("node")).expect("hex"),
        expected_revision: v["authorization_revision"].as_u64().expect("auth rev"),
        payload: Some(command_envelope::Payload::UserDisable(UserDisable {
            username: v["username"].as_str().expect("username").into(),
            desired_revision: v["desired_revision"].as_u64().expect("rev"),
        })),
        ..CommandEnvelope::default()
    };
    let digest = semantic_payload_hash_v2(&env).expect("v2");
    assert_eq!(
        hex::encode(digest),
        v["expected_sha256"].as_str().expect("sha256")
    );
    let Some(command_envelope::Payload::UserDisable(p)) = env.payload.as_mut() else {
        panic!("UserDisable");
    };
    p.desired_revision += 1;
    assert_ne!(semantic_payload_hash_v2(&env).expect("changed"), digest);
    let legacy = CommandEnvelope {
        node_id: env.node_id,
        expected_revision: 1,
        payload: Some(command_envelope::Payload::ConfigPlan(ConfigPlan::default())),
        ..CommandEnvelope::default()
    };
    assert!(semantic_payload_hash_v2(&legacy).is_err());
}
