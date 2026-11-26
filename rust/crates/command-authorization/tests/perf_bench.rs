#![allow(clippy::missing_panics_doc)]

use std::time::Instant;

use ed25519_dalek::SigningKey;
use ocservia_command_authorization::{ControllerCommandKeyring, semantic_payload_hash_v2};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CertificateP12, CommandEnvelope, SealedSecretPurpose, SealedSecretV1, command_envelope,
};

#[test]
#[ignore = "run explicitly via scripts/perf-bench.sh"]
fn perf_semantic_payload_hash_and_keyring_clone() {
    let envelope = CommandEnvelope {
        protocol_version: "1.2".to_owned(),
        command_id: vec![1; 16],
        operation_id: vec![2; 16],
        node_id: vec![3; 16],
        idempotency_key: b"bench-key".to_vec(),
        message_id: vec![4; 16],
        payload: Some(command_envelope::Payload::CertificateP12(CertificateP12 {
            certificate_id: vec![5; 16],
            certificate_chain_pem: vec![b'A'; 128 * 1024],
            artifact_id: vec![6; 16],
            sealed_password_v1: Some(SealedSecretV1 {
                version: 1,
                purpose: SealedSecretPurpose::CertificateP12Password as i32,
                key_id: "seal-key-1".to_owned(),
                ciphertext: vec![7; 64],
            }),
            certificate_version: 1,
            artifact_expires_at: Some(prost_types::Timestamp {
                seconds: 1_800_000_000,
                nanos: 0,
            }),
        })),
        ..Default::default()
    };

    for _ in 0..20 {
        let _ = semantic_payload_hash_v2(&envelope).expect("hash");
    }
    let hash_iters = 1_000u32;
    let start = Instant::now();
    for _ in 0..hash_iters {
        let h = semantic_payload_hash_v2(&envelope).expect("hash");
        std::hint::black_box(h);
    }
    let elapsed = start.elapsed();
    println!(
        "RUST_BENCH semantic_payload_hash_v2_128kib iterations={hash_iters} total_ms={:.3} ns_per_op={}",
        elapsed.as_secs_f64() * 1000.0,
        elapsed.as_nanos() / u128::from(hash_iters)
    );

    let keys = (1u8..=4)
        .map(|seed| SigningKey::from_bytes(&[seed; 32]).verifying_key())
        .collect::<Vec<_>>();
    let keyring = ControllerCommandKeyring::new(keys).expect("keyring");
    let clone_iters = 100_000u32;
    let start = Instant::now();
    for _ in 0..clone_iters {
        let cloned = keyring.clone();
        std::hint::black_box(cloned);
    }
    let elapsed = start.elapsed();
    println!(
        "RUST_BENCH keyring_clone_4keys iterations={clone_iters} total_ms={:.3} ns_per_op={}",
        elapsed.as_secs_f64() * 1000.0,
        elapsed.as_nanos() / u128::from(clone_iters)
    );
}
