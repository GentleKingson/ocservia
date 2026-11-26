use super::*;
use ocservia_test_support::PrivateTempDir;
use prost_types::Timestamp;
use std::os::unix::fs::symlink;

fn fixture() -> (PrivdResultReceiptV1, SigningKey) {
    let key = SigningKey::from_bytes(&[7; 32]);
    let id = hex::decode("018f2a3b4c5d70008000000000000001").expect("id");
    (
        PrivdResultReceiptV1 {
            receipt_version: 1,
            node_id: id.clone(),
            privd_attestation_key_id: key_id(&key.verifying_key()),
            command_id: hex::decode("018f2a3b4c5d70008000000000000002").expect("command"),
            operation_id: hex::decode("018f2a3b4c5d70008000000000000003").expect("operation"),
            idempotency_key: hex::decode("018f2a3b4c5d70008000000000000004").expect("idempotency"),
            semantic_payload_hash_version: SemanticPayloadHashVersion::V2.into(),
            semantic_payload_sha256: vec![0x11; 32],
            command_kind: PrivilegedCommandKind::CertificateCsr.into(),
            result_kind: PrivilegedResultKind::CertificateCsr.into(),
            terminal_state: CommandResultState::Succeeded.into(),
            result_cbor_sha256: vec![0x22; 32],
            error_code: String::new(),
            effect_sequence: 9,
            accepted_at: Some(Timestamp {
                seconds: 1_700_000_000,
                nanos: 123,
            }),
            completed_at: Some(Timestamp {
                seconds: 1_700_000_001,
                nanos: 456,
            }),
            replayed: false,
            certificate: Some(PrivdCertificateReceiptBindingV1 {
                certificate_id: id,
                csr_der_sha256: vec![0x33; 32],
                public_key_sha256: vec![0x44; 32],
            }),
        },
        key,
    )
}

#[test]
fn empty_result_encodes_as_cbor_empty_map() {
    let encoded = ocservia_canonical_cbor::marshal("", &CborEmptyResult {}).expect("encode");
    assert_eq!(encoded, vec![0xa0]);
    assert!(result_payload_cbor_hash(PrivilegedResultKind::Mutation, &[]).is_ok());
    assert_eq!(
        result_payload_cbor_hash(PrivilegedResultKind::Mutation, &[0x01]),
        Err(ReceiptError::Malformed)
    );
}

#[test]
fn receipt_signature_binds_every_security_identity() {
    let (receipt, key) = fixture();
    let canonical = canonical_receipt_v1(&receipt).expect("canonical");
    assert!(canonical.starts_with(RECEIPT_DOMAIN.as_bytes()));
    assert_eq!(
        canonical,
        canonical_receipt_v1(&receipt).expect("determinism")
    );
    let proof = sign_receipt(receipt, &key).expect("sign");
    verify_receipt(&proof, &key.verifying_key()).expect("verify");
    for mutate in [
        |value: &mut PrivdResultReceiptV1| value.node_id[15] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.command_id[15] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.operation_id[15] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.idempotency_key[15] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.semantic_payload_sha256[0] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.result_cbor_sha256[0] ^= 1,
        |value: &mut PrivdResultReceiptV1| value.error_code = "tampered".to_owned(),
    ] {
        let mut tampered = proof.clone();
        mutate(tampered.receipt_v1.as_mut().expect("receipt"));
        assert_eq!(
            verify_receipt(&tampered, &key.verifying_key()),
            Err(ReceiptError::SignatureInvalid)
        );
    }
}

#[test]
fn upgrade_result_signature_binds_durable_identity() {
    let key = SigningKey::from_bytes(&[9; 32]);
    let proof = AgentUpgradeResultProof {
        version: 1,
        node_id: hex::decode("018f2a3b4c5d70008000000000000001").expect("node"),
        privd_attestation_key_id: key_id(&key.verifying_key()),
        operation_id: hex::decode("018f2a3b4c5d70008000000000000002").expect("operation"),
        target_version: "1.2.3".to_owned(),
        package_sha256: vec![0x11; 32],
        state: AgentUpgradeOutcomeState::Succeeded.into(),
        completed_unix_ms: 1_700_000_000_000,
        result_sha256: vec![0x22; 32],
        attested_unix_ms: 1_700_000_000_000 + 60_000,
        signature: Vec::new(),
    };
    let signed = sign_upgrade_result(proof, &key).expect("sign");
    verify_upgrade_result(&signed, &key.verifying_key()).expect("verify");

    let mut tampered = signed.clone();
    tampered.package_sha256[0] ^= 1;
    assert_eq!(
        verify_upgrade_result(&tampered, &key.verifying_key()),
        Err(ReceiptError::SignatureInvalid)
    );
    let mut backdated = signed.clone();
    backdated.attested_unix_ms = signed.completed_unix_ms - 1;
    assert_eq!(
        canonical_upgrade_result_v1(&backdated),
        Err(ReceiptError::Malformed)
    );
    let mut shifted = signed.clone();
    shifted.attested_unix_ms += 1;
    assert_eq!(
        verify_upgrade_result(&shifted, &key.verifying_key()),
        Err(ReceiptError::SignatureInvalid)
    );
}

#[test]
fn unknown_version_and_malformed_signature_fail_closed() {
    let (receipt, key) = fixture();
    let mut proof = sign_receipt(receipt, &key).expect("sign");
    proof.version = 77;
    assert_eq!(
        verify_receipt(&proof, &key.verifying_key()),
        Err(ReceiptError::UnsupportedVersion)
    );
    proof.version = 1;
    proof.signature.push(0);
    assert_eq!(
        verify_receipt(&proof, &key.verifying_key()),
        Err(ReceiptError::Malformed)
    );
}

#[test]
fn key_creation_is_atomic_owner_only_and_symlinks_fail_closed() {
    let directory = PrivateTempDir::new("att-key");
    let uid = rustix::process::geteuid().as_raw();
    let path = directory.join("attestation.key");
    let first = load_or_create_signing_key(&path, uid).expect("create key");
    let second = load_or_create_signing_key(&path, uid).expect("reload key");
    assert_eq!(first.to_bytes(), second.to_bytes());
    let metadata = fs::symlink_metadata(&path).expect("metadata");
    assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
    assert_eq!(metadata.len(), 32);

    let target = directory.join("target.key");
    fs::rename(&path, &target).expect("move key");
    symlink(&target, &path).expect("create symlink");
    assert_eq!(
        load_or_create_signing_key(&path, uid)
            .expect_err("symlink must fail")
            .kind(),
        io::ErrorKind::PermissionDenied
    );
}
