use std::fs;
use std::os::unix::fs::symlink;
use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::pkcs8::{EncodePublicKey as _, spki::der::pem::LineEnding};
use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, CommandAuthorizationProof, ConfigApply, SyntheticEcho, command_envelope,
    complete_config_directive,
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
fn opt_fixed<const N: usize>(hex_s: &str) -> Option<[u8; N]> {
    (!hex_s.is_empty()).then(|| fixed(hex_s))
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

fn fence_from_fixture(f: &Value) -> (ConnectionFenceClaimsV2, ConnectionFenceV2) {
    let c = ConnectionFenceClaimsV2 {
        signature_version: u32v(f, "signature_version"),
        key_id: s(f, "key_id").into(),
        fence_id: fixed(s(f, "fence_id_hex")),
        node_id: fixed(s(f, "node_id_hex")),
        endpoint_id: fixed(s(f, "endpoint_id_hex")),
        owner_instance_id: fixed(s(f, "owner_instance_id_hex")),
        owner_incarnation: u(f, "owner_incarnation"),
        owner_epoch: u(f, "owner_epoch"),
        connection_id: fixed(s(f, "connection_id_hex")),
        authorization_revision: u(f, "authorization_revision"),
        capabilities: str_vec(f, "capabilities"),
        lease_until_seconds: i(f, "lease_until_seconds"),
        lease_until_nanos: u32v(f, "lease_until_nanos"),
        issued_at_seconds: i(f, "issued_at_seconds"),
        issued_at_nanos: u32v(f, "issued_at_nanos"),
        expires_at_seconds: i(f, "expires_at_seconds"),
        expires_at_nanos: u32v(f, "expires_at_nanos"),
    };
    let m = ConnectionFenceV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: c.key_id.clone(),
        fence_id: c.fence_id.to_vec(),
        node_id: c.node_id.to_vec(),
        endpoint_id: c.endpoint_id.to_vec(),
        owner_instance_id: c.owner_instance_id.to_vec(),
        owner_incarnation: c.owner_incarnation,
        owner_epoch: c.owner_epoch,
        connection_id: c.connection_id.to_vec(),
        authorization_revision: c.authorization_revision,
        capabilities: c.capabilities.clone(),
        lease_until: ts(c.lease_until_seconds, c.lease_until_nanos),
        issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
        expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
        signature: hex::decode(s(f, "signature_hex")).expect("sig"),
    };
    (c, m)
}

fn binding_from_fixture(f: &Value) -> (FenceBindingClaimsV2, FenceBindingV2) {
    let c = FenceBindingClaimsV2 {
        signature_version: u32v(f, "signature_version"),
        key_id: s(f, "key_id").into(),
        operation_kind: u32v(f, "operation_kind"),
        operation_id: fixed(s(f, "operation_id_hex")),
        fence_id: fixed(s(f, "fence_id_hex")),
        node_id: fixed(s(f, "node_id_hex")),
        endpoint_id: fixed(s(f, "endpoint_id_hex")),
        owner_instance_id: fixed(s(f, "owner_instance_id_hex")),
        owner_incarnation: u(f, "owner_incarnation"),
        owner_epoch: u(f, "owner_epoch"),
        connection_id: fixed(s(f, "connection_id_hex")),
        authorization_revision: u(f, "authorization_revision"),
        capability: s(f, "capability").into(),
        issued_at_seconds: i(f, "issued_at_seconds"),
        issued_at_nanos: u32v(f, "issued_at_nanos"),
        expires_at_seconds: i(f, "expires_at_seconds"),
        expires_at_nanos: u32v(f, "expires_at_nanos"),
    };
    let m = FenceBindingV2 {
        signature_version: FenceSignatureVersion::Ed25519V1.into(),
        key_id: c.key_id.clone(),
        operation_kind: FenceOperationKind::try_from(i32::try_from(c.operation_kind).expect("k"))
            .expect("k")
            .into(),
        operation_id: c.operation_id.to_vec(),
        fence_id: c.fence_id.to_vec(),
        node_id: c.node_id.to_vec(),
        endpoint_id: c.endpoint_id.to_vec(),
        owner_instance_id: c.owner_instance_id.to_vec(),
        owner_incarnation: c.owner_incarnation,
        owner_epoch: c.owner_epoch,
        connection_id: c.connection_id.to_vec(),
        authorization_revision: c.authorization_revision,
        capability: c.capability.clone(),
        issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
        expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
        signature: hex::decode(s(f, "signature_hex")).expect("sig"),
    };
    (c, m)
}

#[test]
fn complete_config_matches_go_identity_and_approval_vectors() {
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
        assert!(semantic_payload_hash_v1(&env).is_err());
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

    // Single-admin vs legacy approval binding.
    let mut env =
        ocservia_contracts::decode_strict_command_envelope(&bytes("apply_proto")).unwrap();
    let legacy = semantic_payload_hash_v2(&env).unwrap();
    env.approval_id.clear();
    assert!(semantic_payload_hash_v2(&env).is_err());
    env.approval_request_sha256.clear();
    let direct = semantic_payload_hash_v2(&env).unwrap();
    assert_ne!(direct, legacy);
    env.approval_id = vec![0; 16];
    assert!(semantic_payload_hash_v2(&env).is_err());
    env.approval_id.clear();
    if let Some(command_envelope::Payload::CompleteConfigApply(p)) = &mut env.payload {
        p.materialized_hash[0] ^= 1;
    }
    assert_ne!(semantic_payload_hash_v2(&env).unwrap(), direct);
}

#[test]
fn state_update_operation_identity_matches_the_shared_vector() {
    let node: [u8; 16] = core::array::from_fn(|i| u8::try_from(i + 1).expect("b"));
    let ep: [u8; 32] = core::array::from_fn(|i| u8::try_from(i).expect("b"));
    let id = state_update_operation_id(&node, &ep, 2, 7, "review fixture");
    assert_eq!(hex::encode(id), "f0229ecacf9bb65589e1897c668a48f7");
    assert_ne!(
        state_update_operation_id(&node, &ep, 1, 7, "review fixture"),
        id
    );
    assert_ne!(
        state_update_operation_id(&node, &ep, 2, 8, "review fixture"),
        id
    );
    assert_ne!(
        state_update_operation_id(&node, &ep, 2, 7, "review fixture "),
        id
    );
    let mut other_ep = ep;
    other_ep[0] ^= 1;
    assert_ne!(
        state_update_operation_id(&node, &other_ep, 2, 7, "review fixture"),
        id
    );
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
            approval_id: opt_fixed(s(v, "approval_id_hex")),
            approval_request_sha256: opt_fixed(s(v, "approval_request_sha256_hex")),
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
            104 => command_envelope::Payload::ConfigApply(ConfigApply::default()),
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
            approval_id: c.approval_id.map_or_else(Vec::new, |x| x.to_vec()),
            approval_request_sha256: c
                .approval_request_sha256
                .map_or_else(Vec::new, |x| x.to_vec()),
            semantic_payload_hash_version: i32::try_from(c.semantic_hash_version).expect("v"),
            semantic_payload_sha256: c.semantic_payload_sha256.to_vec(),
            delivery_mode: i32::try_from(c.delivery_mode).expect("d"),
            payload: Some(payload),
            authorization: Some(CommandAuthorizationProof {
                version: CommandAuthorizationVersion::V1.into(),
                key_id: c.key_id,
                signature: hex::decode(s(v, "signature_hex")).expect("sig"),
            }),
            ..CommandEnvelope::default()
        };
        kr.verify(&env)
            .unwrap_or_else(|e| panic!("{} Go sig: {e}", s(v, "name")));
    }

    // Session grant v1 vector.
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
        version: SessionGrantVersion::V1.into(),
        key_id: sc.key_id.clone(),
        protocol_major: sc.protocol_major,
        protocol_minor: sc.protocol_minor,
        node_id: sc.node_id.to_vec(),
        endpoint_id: sc.endpoint_id.to_vec(),
        authorization_revision: sc.authorization_revision,
        negotiated_capabilities: sc.negotiated_capabilities.clone(),
        issued_at: ts(sc.issued_at_seconds, sc.issued_at_nanos),
        expires_at: ts(sc.expires_at_seconds, sc.expires_at_nanos),
        signature: hex::decode(s(&sf, "signature_hex")).expect("sig"),
    };
    assert_eq!(
        skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds)
            .expect("sg")
            .authorization_revision,
        sc.authorization_revision
    );
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
    sg.authorization_revision += 1;
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
        version: ArtifactGrantVersion::V1.into(),
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

#[test]
fn rust_verifies_go_fence_v2_binding_nanosecond_bounds_and_domain_separation() {
    let ff = load_fixture("connection-fence-v2.json");
    let kr = fixture_keyring(&ff);
    let (fc, fence) = fence_from_fixture(&ff);
    let fcanon = canonical_connection_fence_v2(&fc).expect("canon fence");
    assert_eq!(hex::encode(&fcanon), s(&ff, "canonical_preimage_hex"));
    let vf = kr
        .verify_connection_fence_v2_at(&fence, &fc.node_id, &fc.endpoint_id, 1_700_000_100, 0)
        .expect("vf");
    assert_eq!(
        (
            vf.fence_id,
            vf.owner_instance_id,
            vf.owner_incarnation,
            vf.owner_epoch,
            vf.connection_id,
            vf.authorization_revision,
            vf.lease_until_seconds
        ),
        (
            fc.fence_id,
            fc.owner_instance_id,
            fc.owner_incarnation,
            fc.owner_epoch,
            fc.connection_id,
            fc.authorization_revision,
            fc.lease_until_seconds
        )
    );
    assert_eq!(vf.capabilities, fc.capabilities);
    assert_eq!(
        kr.verify_connection_fence_v2_at(&fence, &[9; 16], &fc.endpoint_id, 1_700_000_100, 0),
        Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_node_mismatch"
        ))
    );
    assert_eq!(
        kr.verify_connection_fence_v2_at(&fence, &fc.node_id, &[9; 32], 1_700_000_100, 0),
        Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_endpoint_mismatch"
        ))
    );
    let mut tamp = fence.clone();
    tamp.owner_epoch += 1;
    assert_eq!(
        kr.verify_connection_fence_v2_at(&tamp, &fc.node_id, &fc.endpoint_id, 1_700_000_100, 0),
        Err(AuthorizationError::SignatureInvalid)
    );
    assert_eq!(
        kr.verify_connection_fence_v2_at(&fence, &fc.node_id, &fc.endpoint_id, 1_700_000_301, 0),
        Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_expired"
        ))
    );

    // Nanosecond bounds on fence v2.
    let skew = fc
        .issued_at_seconds
        .checked_sub(MAX_FUTURE_SKEW_SECONDS)
        .expect("skew");
    kr.verify_connection_fence_v2_at(
        &fence,
        &fc.node_id,
        &fc.endpoint_id,
        fc.expires_at_seconds,
        fc.expires_at_nanos - 1,
    )
    .expect("1ns before");
    assert_eq!(
        kr.verify_connection_fence_v2_at(
            &fence,
            &fc.node_id,
            &fc.endpoint_id,
            fc.expires_at_seconds,
            fc.expires_at_nanos
        ),
        Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_expired"
        ))
    );
    kr.verify_connection_fence_v2_at(
        &fence,
        &fc.node_id,
        &fc.endpoint_id,
        skew,
        fc.issued_at_nanos,
    )
    .expect("exact skew");
    assert_eq!(
        kr.verify_connection_fence_v2_at(
            &fence,
            &fc.node_id,
            &fc.endpoint_id,
            skew,
            fc.issued_at_nanos - 1
        ),
        Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_clock_skew"
        ))
    );
    kr.verify_connection_fence_v2_at(
        &fence,
        &fc.node_id,
        &fc.endpoint_id,
        fc.expires_at_seconds,
        0,
    )
    .expect("sec-only at ns=0");

    // Domain separation vs V1.
    let signing = SigningKey::from_bytes(&fixed::<32>(s(&ff, "test_seed_hex")));
    let sig = signing.sign(&fcanon).to_bytes();
    for v1 in [
        DOMAIN_SEPARATOR,
        SESSION_GRANT_DOMAIN_SEPARATOR,
        ARTIFACT_GRANT_DOMAIN_SEPARATOR,
    ] {
        let mut forged = v1.to_vec();
        forged.extend_from_slice(&fcanon[CONNECTION_FENCE_V2_DOMAIN_SEPARATOR.len()..]);
        assert_ne!(forged, fcanon);
        assert_ne!(signing.sign(&forged).to_bytes(), sig);
    }
    let mut cross = fence;
    let mut v1_body = SESSION_GRANT_DOMAIN_SEPARATOR.to_vec();
    v1_body.extend_from_slice(&fcanon[CONNECTION_FENCE_V2_DOMAIN_SEPARATOR.len()..]);
    cross.signature = signing.sign(&v1_body).to_bytes().to_vec();
    assert_eq!(
        kr.verify_connection_fence_v2_at(&cross, &fc.node_id, &fc.endpoint_id, 1_700_000_100, 0),
        Err(AuthorizationError::SignatureInvalid)
    );

    // Fence binding v2 vector + term matching + nanosecond bounds.
    let bf = load_fixture("fence-binding-v2.json");
    let bkr = fixture_keyring(&bf);
    let (bc, binding) = binding_from_fixture(&bf);
    assert_eq!(
        hex::encode(canonical_fence_binding_v2(&bc).expect("canon binding")),
        s(&bf, "canonical_preimage_hex")
    );
    let vb = bkr
        .verify_fence_binding_v2_at(&binding, &bc.node_id, &bc.endpoint_id, 1_700_000_100, 0)
        .expect("vb");
    assert!(vb.matches_fence(&vf));
    for mutate in [
        (|f: &mut VerifiedConnectionFenceV2| f.owner_epoch += 1)
            as fn(&mut VerifiedConnectionFenceV2),
        |f| f.connection_id = [9; 16],
        |f| f.fence_id = [9; 16],
    ] {
        let mut drifted = vf.clone();
        mutate(&mut drifted);
        assert!(!vb.matches_fence(&drifted));
    }
    let mut tb = binding.clone();
    tb.operation_id = vec![9; 16];
    assert_eq!(
        bkr.verify_fence_binding_v2_at(&tb, &bc.node_id, &bc.endpoint_id, 1_700_000_100, 0),
        Err(AuthorizationError::SignatureInvalid)
    );
    let mut dg = binding.clone();
    dg.signature_version = FenceSignatureVersion::Unspecified.into();
    assert_eq!(
        bkr.verify_fence_binding_v2_at(&dg, &bc.node_id, &bc.endpoint_id, 1_700_000_100, 0),
        Err(AuthorizationError::UnsupportedVersion)
    );

    let bskew = bc
        .issued_at_seconds
        .checked_sub(MAX_FUTURE_SKEW_SECONDS)
        .expect("bskew");
    bkr.verify_fence_binding_v2_at(
        &binding,
        &bc.node_id,
        &bc.endpoint_id,
        bc.expires_at_seconds,
        bc.expires_at_nanos - 1,
    )
    .expect("1ns");
    assert_eq!(
        bkr.verify_fence_binding_v2_at(
            &binding,
            &bc.node_id,
            &bc.endpoint_id,
            bc.expires_at_seconds,
            bc.expires_at_nanos
        ),
        Err(AuthorizationError::ClaimsInvalid("fence_binding_expired"))
    );
    bkr.verify_fence_binding_v2_at(
        &binding,
        &bc.node_id,
        &bc.endpoint_id,
        bskew,
        bc.issued_at_nanos,
    )
    .expect("exact");
    assert_eq!(
        bkr.verify_fence_binding_v2_at(
            &binding,
            &bc.node_id,
            &bc.endpoint_id,
            bskew,
            bc.issued_at_nanos - 1
        ),
        Err(AuthorizationError::ClaimsInvalid(
            "fence_binding_clock_skew"
        ))
    );
    bkr.verify_fence_binding_v2_at(
        &binding,
        &bc.node_id,
        &bc.endpoint_id,
        bc.expires_at_seconds,
        0,
    )
    .expect("sec-only");
}

#[test]
fn verification_key_loader_and_shared_metadata_policy() {
    let suffix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let dir = std::env::current_dir()
        .expect("cwd")
        .join(format!(".command-key-test-{}-{suffix}", std::process::id()));
    fs::create_dir(&dir).expect("mkdir");
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).expect("700");
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

    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o644)).expect("644");
    assert!(load_verification_key(&key_path, uid, gid).is_err());
    fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("600");
    let link = dir.join("controller-link.pem");
    symlink(&key_path, &link).expect("symlink");
    assert!(load_verification_key(&link, uid, gid).is_err());
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o770)).expect("770");
    assert!(load_verification_key(&key_path, uid, gid).is_err());
    let _ = fs::remove_dir_all(&dir);

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
