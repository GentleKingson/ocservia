use std::fs;
use std::os::unix::fs::{DirBuilderExt as _, MetadataExt as _, symlink};
use std::path::PathBuf;
use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::SigningKey;
use ed25519_dalek::pkcs8::{EncodePublicKey as _, spki::der::pem::LineEnding};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, CommandAuthorizationProof, CompleteConfigApply, ConfigApply, ConfigPlan,
    SyntheticEcho, command_envelope, complete_config_directive,
};
use prost_types::Timestamp;
use serde_json::Value;

use super::*;

type ArtifactMutation = (&'static str, fn(&mut ArtifactGrantV1));

fn load_fixture(name: &str) -> Value {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata")
        .join(name);
    let raw = fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    serde_json::from_str(&raw).expect("fixture json")
}
fn s<'a>(v: &'a Value, k: &str) -> &'a str {
    v[k].as_str().unwrap_or_else(|| panic!("{k} string"))
}
fn u(v: &Value, k: &str) -> u64 {
    v[k].as_u64().unwrap_or_else(|| panic!("{k} u64"))
}
fn i(v: &Value, k: &str) -> i64 {
    v[k].as_i64().unwrap_or_else(|| panic!("{k} i64"))
}
fn u32v(v: &Value, k: &str) -> u32 {
    u32::try_from(u(v, k)).expect("u32")
}
fn fixed<const N: usize>(hex_s: &str) -> [u8; N] {
    hex::decode(hex_s).expect("hex").try_into().expect("len")
}
#[allow(clippy::unnecessary_wraps)]
fn ts(sec: i64, nanos: u32) -> Option<Timestamp> {
    Some(Timestamp {
        seconds: sec,
        nanos: i32::try_from(nanos).expect("nanos"),
    })
}
fn fixture_keyring(f: &Value) -> ControllerCommandKeyring {
    let pk = VerifyingKey::from_bytes(&fixed::<32>(s(f, "public_key_hex"))).expect("pk");
    ControllerCommandKeyring::new([pk]).expect("keyring")
}
fn str_vec(v: &Value, k: &str) -> Vec<String> {
    v[k].as_array()
        .expect("arr")
        .iter()
        .map(|x| x.as_str().expect("s").to_owned())
        .collect()
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
    let mut bad = candidate.clone();
    bad.directives.swap(0, 1);
    assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());
    bad = candidate.clone();
    bad.directives.remove(2);
    assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());
    bad = candidate;
    if let Some(complete_config_directive::Value::Tls(v)) = bad.directives[7].value.as_mut() {
        v.version = "../escape".into();
    }
    assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());

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
    // Command authorization v1 vectors.
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
            s(v, "canonical_preimage_hex"),
            "{}",
            s(v, "name")
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
            issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
            expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
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
        kr.verify(&env)
            .unwrap_or_else(|e| panic!("{} Go sig: {e}", s(v, "name")));
        if c.payload_kind == 108 {
            let mut legacy_proto = env.clone();
            legacy_proto.protocol_version = "1.1".into();
            assert_eq!(
                claims_from_envelope_v1(&legacy_proto),
                Err(AuthorizationError::ClaimsInvalid(
                    "command_protocol_version_unsupported"
                ))
            );
            for legacy_payload in [
                command_envelope::Payload::ConfigPlan(ConfigPlan::default()),
                command_envelope::Payload::ConfigApply(ConfigApply::default()),
            ] {
                let mut unsupported = env.clone();
                unsupported.payload = Some(legacy_payload);
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

    // Session grant v1 vector (including session_epoch).
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
        negotiated_capabilities: str_vec(&sf, "negotiated_capabilities"),
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
        issued_at: ts(sc.issued_at_seconds, sc.issued_at_nanos),
        expires_at: ts(sc.expires_at_seconds, sc.expires_at_nanos),
        signature: hex::decode(s(&sf, "signature_hex")).expect("sig"),
    };
    let verified = skr
        .verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds)
        .expect("sg");
    assert_eq!(verified.authorization_revision, sc.authorization_revision);
    assert_eq!(verified.session_epoch, sc.session_epoch);
    assert_eq!(
        skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.expires_at_seconds),
        Err(AuthorizationError::ClaimsInvalid("session_grant_expired"))
    );
    let mut bad_sg = sg.clone();
    bad_sg.key_id = "ed25519-sha256:unknown".into();
    assert_eq!(
        skr.verify_session_grant(&bad_sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds),
        Err(AuthorizationError::UnknownKey)
    );
    sg.session_epoch += 1;
    assert_eq!(
        skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds),
        Err(AuthorizationError::SignatureInvalid)
    );

    // Artifact grant v1 vector.
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
    let ag = ArtifactGrantV1 {
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
        issued_at: ts(ac.issued_at_seconds, ac.issued_at_nanos),
        expires_at: ts(ac.expires_at_seconds, ac.expires_at_nanos),
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
    let mut forged = ag.clone();
    forged.signature[0] ^= 1;
    assert_eq!(
        akr.verify_artifact_grant_for_confirmation(
            &forged,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.expires_at_seconds
        ),
        Err(AuthorizationError::SignatureInvalid)
    );
    let mut unk = ag.clone();
    unk.key_id = "ed25519-sha256:unknown".into();
    assert_eq!(
        akr.verify_artifact_grant(
            &unk,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.issued_at_seconds
        ),
        Err(AuthorizationError::UnknownKey)
    );
    let mut wrong_node = ac.node_id;
    wrong_node[0] ^= 1;
    assert_eq!(
        akr.verify_artifact_grant(
            &ag,
            &wrong_node,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.issued_at_seconds
        ),
        Err(AuthorizationError::ClaimsInvalid(
            "artifact_grant_node_mismatch"
        ))
    );
    let mutations: [ArtifactMutation; 4] = [
        ("cert_ver", |v| v.certificate_version += 1),
        ("op", |v| v.operation_id[0] ^= 1),
        ("grant_id", |v| v.grant_id[0] ^= 1),
        ("sig", |v| v.signature[0] ^= 1),
    ];
    for (name, m) in mutations {
        let mut mod_g = ag.clone();
        m(&mut mod_g);
        assert_eq!(
            akr.verify_artifact_grant(
                &mod_g,
                &ac.node_id,
                &ac.artifact_id,
                &ac.purpose,
                ac.max_bytes,
                ac.issued_at_seconds
            ),
            Err(AuthorizationError::SignatureInvalid),
            "{name}"
        );
    }
}

struct PrivateTempDir(PathBuf);

impl PrivateTempDir {
    fn new(prefix: &str) -> Self {
        let uid = rustix::process::geteuid().as_raw();
        let repo = Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .and_then(Path::parent)
            .and_then(Path::parent)
            .and_then(|p| fs::canonicalize(p).ok());
        let mut candidates = Vec::with_capacity(3);
        if let Some(dir) = std::env::var_os("TMPDIR").filter(|v| !v.is_empty()) {
            candidates.push(PathBuf::from(dir));
        }
        if let Some(dir) = std::env::var_os("XDG_RUNTIME_DIR").filter(|v| !v.is_empty()) {
            candidates.push(PathBuf::from(dir));
        }
        if let Some(home) = std::env::var_os("HOME").filter(|v| !v.is_empty()) {
            let home_path = PathBuf::from(home);
            let cache = home_path.join(".cache");
            if home_path.is_dir() && !cache.exists() {
                let _ = fs::DirBuilder::new().mode(0o700).create(&cache);
                let _ = fs::set_permissions(&cache, fs::Permissions::from_mode(0o700));
            }
            candidates.push(cache);
        }
        let base = candidates
            .into_iter()
            .filter_map(|candidate| fs::canonicalize(candidate).ok())
            .find(|resolved| {
                if let Some(root) = repo.as_deref()
                    && resolved.starts_with(root)
                {
                    return false;
                }
                let mut current = Some(resolved.as_path());
                while let Some(dir) = current {
                    let Ok(meta) = fs::symlink_metadata(dir) else {
                        return false;
                    };
                    if !meta.is_dir()
                        || meta.file_type().is_symlink()
                        || (meta.uid() != 0 && meta.uid() != uid)
                        || meta.permissions().mode() & 0o022 != 0
                    {
                        return false;
                    }
                    current = dir.parent();
                }
                true
            })
            .expect("private temp directory outside repository");
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let dir = base.join(format!("{prefix}-{}-{suffix}", std::process::id()));
        fs::DirBuilder::new()
            .mode(0o700)
            .create(&dir)
            .expect("mkdir");
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).expect("700");
        Self(dir)
    }

    fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for PrivateTempDir {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

#[test]
fn verification_key_loader_and_shared_metadata_policy() {
    let dir = PrivateTempDir::new("cmd-key-test");
    let key_path = dir.path().join("controller.pem");
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

    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(load_verification_key(&key_path, uid, gid).is_err());
    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("600");
    let link = dir.path().join("controller-link.pem");
    symlink(&key_path, &link).expect("symlink");
    assert!(load_verification_key(&link, uid, gid).is_err());
    fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o770)).expect("770");
    assert!(load_verification_key(&key_path, uid, gid).is_err());
    drop(dir);

    let (agent_user_id, agent_group_id) = (997, 998);
    for mode in [0o440, 0o640] {
        assert!(verification_key_metadata_allowed(
            0,
            agent_group_id,
            mode,
            agent_user_id,
            agent_group_id
        ));
        assert!(verification_key_metadata_allowed(
            0,
            agent_group_id,
            mode,
            0,
            agent_group_id
        ));
    }
    for mode in [0o400, 0o600] {
        assert!(verification_key_metadata_allowed(
            agent_user_id,
            agent_group_id,
            mode,
            agent_user_id,
            agent_group_id
        ));
        assert!(!verification_key_metadata_allowed(
            agent_user_id,
            agent_group_id,
            mode,
            0,
            agent_group_id
        ));
        assert!(!verification_key_metadata_allowed(
            0,
            agent_group_id,
            mode,
            agent_user_id,
            agent_group_id
        ));
        assert!(verification_key_metadata_allowed(
            0,
            agent_group_id,
            mode,
            0,
            agent_group_id
        ));
    }
    assert!(!verification_key_metadata_allowed(
        0,
        agent_group_id + 1,
        0o640,
        agent_user_id,
        agent_group_id
    ));
    assert!(!verification_key_metadata_allowed(
        0,
        agent_group_id + 1,
        0o640,
        0,
        agent_group_id
    ));
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
    for (name, val, reject) in [
        ("ver", "1.2.4", false),
        ("arch", "amd64", false),
        ("ver", "latest", true),
        ("ver", "1.2", true),
    ] {
        let mut c = env.clone();
        if let Some(command_envelope::Payload::AgentUpgrade(p)) = c.payload.as_mut() {
            if name == "ver" {
                p.target_version = val.into();
            } else {
                p.architecture = val.into();
            }
        }
        if reject {
            assert_eq!(
                semantic_payload_hash_v2(&c),
                Err(AuthorizationError::ClaimsInvalid("agent_upgrade_invalid"))
            );
        } else {
            assert_ne!(semantic_payload_hash_v2(&c).expect("ok"), digest);
        }
    }
    let mut short = env;
    if let Some(command_envelope::Payload::AgentUpgrade(p)) = short.payload.as_mut() {
        p.package_sha256.pop();
    }
    assert_eq!(
        semantic_payload_hash_v2(&short),
        Err(AuthorizationError::ClaimsInvalid("agent_upgrade_invalid"))
    );
}
