use std::fs;
use std::os::unix::fs::{DirBuilderExt as _, symlink};
use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::SigningKey;
use ed25519_dalek::pkcs8::{EncodePublicKey as _, spki::der::pem::LineEnding};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, CommandAuthorizationProof, CompleteConfigApply, ConfigApply, ConfigPlan,
    SyntheticEcho, command_envelope,
};
use prost_types::Timestamp;
use serde_json::Value;

use super::*;

fn load_fixture(name: &str) -> Value {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata")
        .join(name);
    serde_json::from_str(&fs::read_to_string(path).expect("fixture")).expect("json")
}
fn s<'a>(v: &'a Value, k: &str) -> &'a str {
    v[k].as_str().expect("str")
}
fn u(v: &Value, k: &str) -> u64 {
    v[k].as_u64().expect("u64")
}
fn i(v: &Value, k: &str) -> i64 {
    v[k].as_i64().expect("i64")
}
fn u32v(v: &Value, k: &str) -> u32 {
    u32::try_from(u(v, k)).expect("u32")
}
fn fixed<const N: usize>(hex_s: &str) -> [u8; N] {
    hex::decode(hex_s).expect("hex").try_into().expect("len")
}
fn ts(sec: i64, nanos: u32) -> Timestamp {
    Timestamp {
        seconds: sec,
        nanos: i32::try_from(nanos).expect("nanos"),
    }
}
fn fixture_keyring(f: &Value) -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([
        VerifyingKey::from_bytes(&fixed::<32>(s(f, "public_key_hex"))).expect("pk"),
    ])
    .expect("kr")
}

#[test]
fn complete_config_matches_go_identity_vectors() {
    let f = load_fixture("complete-config-v1.json");
    let bytes = |name: &str| hex::decode(s(&f, name)).unwrap();
    let plan = ocservia_contracts::decode_strict_command_envelope(&bytes("plan_proto")).unwrap();
    let Some(command_envelope::Payload::CompleteConfigPlan(plan)) = plan.payload else {
        panic!("plan fixture");
    };
    let candidate = plan.candidate.unwrap();
    let canonical = ocservia_contracts::config_profile::canonical(&candidate).unwrap();
    assert_eq!(canonical, bytes("canonical"));
    assert_eq!(
        Sha256::digest(canonical).as_slice(),
        bytes("candidate_hash")
    );
    for kind in ["plan", "apply"] {
        let env =
            ocservia_contracts::decode_strict_command_envelope(&bytes(&format!("{kind}_proto")))
                .unwrap();
        assert_eq!(
            semantic_payload_hash_v2(&env).unwrap().as_slice(),
            bytes(&format!("{kind}_semantic_hash"))
        );
        assert_eq!(
            payload_authorization(&env).unwrap().2,
            format!("ocserv.config.complete.{kind}")
        );
        let mut changed = env;
        changed.node_id[0] ^= 1;
        assert!(semantic_payload_hash_v2(&changed).is_err());
    }
    let mut env =
        ocservia_contracts::decode_strict_command_envelope(&bytes("apply_proto")).unwrap();
    let direct = semantic_payload_hash_v2(&env).unwrap();
    if let Some(command_envelope::Payload::CompleteConfigApply(p)) = &mut env.payload {
        p.materialized_hash[0] ^= 1;
    }
    assert_ne!(semantic_payload_hash_v2(&env).unwrap(), direct);
}

#[test]
fn rust_verifies_go_command_session_and_artifact_vectors() {
    let f = load_fixture("command-authorization-v1.json");
    assert_eq!(u(&f, "version"), 1);
    let pk = VerifyingKey::from_bytes(&fixed::<32>(s(&f, "public_key_hex"))).expect("pk");
    assert_eq!(verification_key_id(&pk), s(&f, "key_id"));
    let kr = ControllerCommandKeyring::new([pk]).expect("kr");
    for v in f["vectors"].as_array().expect("vectors") {
        let c = CommandAuthorizationV1 {
            authorization_version: 1,
            key_id: s(&f, "key_id").into(),
            protocol_version: s(v, "protocol_version").into(),
            command_id: fixed(s(v, "command_id_hex")),
            idempotency_key: fixed(s(v, "idempotency_key_hex")),
            node_id: fixed(s(v, "node_id_hex")),
            operation_id: fixed(s(v, "operation_id_hex")),
            actor_identity: s(v, "actor_identity").into(),
            action: s(v, "action").into(),
            required_capability: s(v, "required_capability").into(),
            expected_revision: u(v, "expected_revision"),
            semantic_hash_version: u32v(v, "semantic_hash_version"),
            semantic_payload_sha256: fixed(s(v, "semantic_payload_sha256_hex")),
            payload_kind: u32v(v, "payload_kind"),
            delivery_mode: u32v(v, "delivery_mode"),
            issued_at_seconds: i(v, "issued_at_seconds"),
            issued_at_nanos: u32v(v, "issued_at_nanos"),
            expires_at_seconds: i(v, "expires_at_seconds"),
            expires_at_nanos: u32v(v, "expires_at_nanos"),
        };
        assert_eq!(
            hex::encode(canonical_v1(&c).expect("canon")),
            s(v, "canonical_preimage_hex")
        );
        let payload = match c.payload_kind {
            130 => command_envelope::Payload::CompleteConfigApply(CompleteConfigApply::default()),
            108 => command_envelope::Payload::SyntheticEcho(SyntheticEcho::default()),
            k => panic!("unsupported payload kind {k}"),
        };
        let env = CommandEnvelope {
            protocol_version: c.protocol_version,
            command_id: c.command_id.to_vec(),
            idempotency_key: c.idempotency_key.to_vec(),
            node_id: c.node_id.to_vec(),
            operation_id: c.operation_id.to_vec(),
            issued_at: Some(ts(c.issued_at_seconds, c.issued_at_nanos)),
            expires_at: Some(ts(c.expires_at_seconds, c.expires_at_nanos)),
            expected_revision: c.expected_revision,
            actor_id: c.actor_identity,
            action: c.action,
            required_capability: c.required_capability,
            semantic_payload_hash_version: i32::try_from(c.semantic_hash_version).expect("v"),
            semantic_payload_sha256: c.semantic_payload_sha256.to_vec(),
            delivery_mode: i32::try_from(c.delivery_mode).expect("d"),
            payload: Some(payload),
            authorization: Some(CommandAuthorizationProof {
                version: 1,
                key_id: c.key_id,
                signature: hex::decode(s(v, "signature_hex")).expect("sig"),
            }),
            ..CommandEnvelope::default()
        };
        kr.verify(&env).expect("Go sig");
        if c.payload_kind == 108 {
            let mut bad = env.clone();
            bad.protocol_version = "1.1".into();
            assert_eq!(
                claims_from_envelope_v1(&bad),
                Err(AuthorizationError::ClaimsInvalid(
                    "command_protocol_version_unsupported"
                ))
            );
            for legacy in [
                command_envelope::Payload::ConfigPlan(ConfigPlan::default()),
                command_envelope::Payload::ConfigApply(ConfigApply::default()),
            ] {
                let mut unsupported = env.clone();
                unsupported.payload = Some(legacy);
                assert_eq!(
                    claims_from_envelope_v1(&unsupported),
                    Err(AuthorizationError::PayloadUnsupported)
                );
                assert_eq!(
                    semantic_payload_hash_v2(&unsupported),
                    Err(AuthorizationError::PayloadUnsupported)
                );
            }
        }
    }

    let sf = load_fixture("session-grant-v1.json");
    let skr = fixture_keyring(&sf);
    let sc = SessionGrantClaimsV1 {
        version: u32v(&sf, "version"),
        key_id: s(&sf, "key_id").into(),
        protocol_major: u32v(&sf, "protocol_major"),
        protocol_minor: u32v(&sf, "protocol_minor"),
        node_id: fixed(s(&sf, "node_id_hex")),
        endpoint_id: fixed(s(&sf, "endpoint_id_hex")),
        authorization_revision: u(&sf, "authorization_revision"),
        session_epoch: u(&sf, "session_epoch"),
        negotiated_capabilities: sf["negotiated_capabilities"]
            .as_array()
            .expect("arr")
            .iter()
            .map(|x| x.as_str().expect("s").to_owned())
            .collect(),
        issued_at_seconds: i(&sf, "issued_at_seconds"),
        issued_at_nanos: u32v(&sf, "issued_at_nanos"),
        expires_at_seconds: i(&sf, "expires_at_seconds"),
        expires_at_nanos: u32v(&sf, "expires_at_nanos"),
    };
    assert_eq!(
        hex::encode(canonical_session_grant_v1(&sc).expect("sg")),
        s(&sf, "canonical_preimage_hex")
    );
    let mut sg = SessionGrantV1 {
        version: 1,
        key_id: sc.key_id.clone(),
        protocol_major: sc.protocol_major,
        protocol_minor: sc.protocol_minor,
        node_id: sc.node_id.to_vec(),
        endpoint_id: sc.endpoint_id.to_vec(),
        authorization_revision: sc.authorization_revision,
        session_epoch: sc.session_epoch,
        negotiated_capabilities: sc.negotiated_capabilities.clone(),
        issued_at: Some(ts(sc.issued_at_seconds, sc.issued_at_nanos)),
        expires_at: Some(ts(sc.expires_at_seconds, sc.expires_at_nanos)),
        signature: hex::decode(s(&sf, "signature_hex")).expect("sig"),
    };
    let verified = skr
        .verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds)
        .expect("sg");
    assert_eq!(
        (verified.authorization_revision, verified.session_epoch),
        (sc.authorization_revision, sc.session_epoch)
    );
    assert_eq!(
        skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.expires_at_seconds),
        Err(AuthorizationError::ClaimsInvalid("session_grant_expired"))
    );
    sg.session_epoch += 1;
    assert_eq!(
        skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds),
        Err(AuthorizationError::SignatureInvalid)
    );

    let af = load_fixture("artifact-grant-v1.json");
    let akr = fixture_keyring(&af);
    let ac = ArtifactGrantClaimsV1 {
        version: u32v(&af, "version"),
        key_id: s(&af, "key_id").into(),
        node_id: fixed(s(&af, "node_id_hex")),
        artifact_id: fixed(s(&af, "artifact_id_hex")),
        certificate_id: fixed(s(&af, "certificate_id_hex")),
        certificate_version: u(&af, "certificate_version"),
        operation_id: fixed(s(&af, "operation_id_hex")),
        authorized_subject: s(&af, "authorized_subject").into(),
        purpose: s(&af, "purpose").into(),
        max_bytes: u(&af, "max_bytes"),
        issued_at_seconds: i(&af, "issued_at_seconds"),
        issued_at_nanos: u32v(&af, "issued_at_nanos"),
        expires_at_seconds: i(&af, "expires_at_seconds"),
        expires_at_nanos: u32v(&af, "expires_at_nanos"),
        grant_id: fixed(s(&af, "grant_id_hex")),
    };
    assert_eq!(
        hex::encode(canonical_artifact_grant_v1(&ac).expect("ag")),
        s(&af, "canonical_preimage_hex")
    );
    let mut ag = ArtifactGrantV1 {
        version: 1,
        key_id: ac.key_id.clone(),
        node_id: ac.node_id.to_vec(),
        artifact_id: ac.artifact_id.to_vec(),
        certificate_id: ac.certificate_id.to_vec(),
        certificate_version: ac.certificate_version,
        operation_id: ac.operation_id.to_vec(),
        authorized_subject: ac.authorized_subject.clone(),
        purpose: ac.purpose.clone(),
        max_bytes: ac.max_bytes,
        issued_at: Some(ts(ac.issued_at_seconds, ac.issued_at_nanos)),
        expires_at: Some(ts(ac.expires_at_seconds, ac.expires_at_nanos)),
        grant_id: ac.grant_id.to_vec(),
        signature: hex::decode(s(&af, "signature_hex")).expect("sig"),
    };
    akr.verify_artifact_grant(
        &ag,
        &ac.node_id,
        &ac.artifact_id,
        &ac.purpose,
        ac.max_bytes,
        ac.issued_at_seconds,
    )
    .expect("ag");
    assert_eq!(
        akr.verify_artifact_grant(
            &ag,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.expires_at_seconds
        ),
        Err(AuthorizationError::ClaimsInvalid("artifact_grant_expired"))
    );
    akr.verify_artifact_grant_for_confirmation(
        &ag,
        &ac.node_id,
        &ac.artifact_id,
        &ac.purpose,
        ac.max_bytes,
        ac.expires_at_seconds,
    )
    .expect("confirm");
    ag.signature[0] ^= 1;
    assert_eq!(
        akr.verify_artifact_grant(
            &ag,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.issued_at_seconds
        ),
        Err(AuthorizationError::SignatureInvalid)
    );
}

#[test]
fn verification_key_loader_and_shared_metadata_policy() {
    let base = std::env::var_os("XDG_RUNTIME_DIR")
        .map(std::path::PathBuf::from)
        .filter(|p| p.is_dir())
        .unwrap_or_else(|| {
            let p =
                std::path::PathBuf::from(std::env::var_os("HOME").expect("HOME")).join(".cache");
            let _ = fs::DirBuilder::new().mode(0o700).create(&p);
            let _ = fs::set_permissions(&p, fs::Permissions::from_mode(0o700));
            p
        });
    let dir = base.join(format!(
        "cmd-key-{}-{}",
        std::process::id(),
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos()
    ));
    fs::DirBuilder::new()
        .mode(0o700)
        .create(&dir)
        .expect("mkdir");
    let key_path = dir.join("controller.pem");
    let key = SigningKey::from_bytes(&[7; 32]).verifying_key();
    fs::write(
        &key_path,
        key.to_public_key_pem(LineEnding::LF).expect("pem"),
    )
    .expect("write");
    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("600");
    let (uid, gid) = (
        rustix::process::geteuid().as_raw(),
        rustix::process::getegid().as_raw(),
    );
    assert_eq!(
        load_verification_key(&key_path, uid, gid).expect("safe"),
        key
    );
    let link = dir.join("controller-link.pem");
    symlink(&key_path, &link).expect("symlink");
    assert!(load_verification_key(&link, uid, gid).is_err());
    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(load_verification_key(&key_path, uid, gid).is_err());
    let _ = fs::remove_dir_all(&dir);
    assert!(verification_key_metadata_allowed(0, 998, 0o640, 997, 998));
    assert!(!verification_key_metadata_allowed(0, 999, 0o640, 997, 998));
}

#[test]
fn canonical_semantic_hash_v2_matches_shared_agent_upgrade_vector() {
    let f = load_fixture("semantic-payload-hash-v2-agent-upgrade.json");
    let v = &f["vector"];
    assert_eq!(u(v, "payload_kind"), 128);
    let env = CommandEnvelope {
        node_id: hex::decode(s(v, "node_id_hex")).expect("node"),
        expected_revision: u(v, "authorization_revision"),
        payload: Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
            target_version: s(v, "target_version").into(),
            package_sha256: hex::decode(s(v, "package_sha256_hex")).expect("sha"),
            architecture: s(v, "architecture").into(),
        })),
        ..CommandEnvelope::default()
    };
    let digest = semantic_payload_hash_v2(&env).expect("v2");
    assert_eq!(hex::encode(digest), s(v, "expected_sha256"));
    let mut bad = env;
    if let Some(command_envelope::Payload::AgentUpgrade(p)) = bad.payload.as_mut() {
        p.target_version = "latest".into();
    }
    assert_eq!(
        semantic_payload_hash_v2(&bad),
        Err(AuthorizationError::ClaimsInvalid("agent_upgrade_invalid"))
    );
}
