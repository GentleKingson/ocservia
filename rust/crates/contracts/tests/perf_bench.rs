#![allow(clippy::missing_panics_doc)]

use std::time::Instant;

use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CertificateP12, CommandEnvelope, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
    command_envelope,
};
use ocservia_contracts::strict_wire::validate_strict_command_envelope;
use prost::Message;

#[test]
#[ignore = "run explicitly via scripts/perf-bench.sh"]
fn perf_strict_wire_validate_certificate_p12() {
    let envelope = CommandEnvelope {
        protocol_version: "1.1".to_owned(),
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
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::CertificateP12Password as i32,
                key_id: "seal-key-1".to_owned(),
                ciphertext: vec![7; 64],
            }),
            certificate_version: 1,
            artifact_expires_at: Some(prost_types::Timestamp {
                seconds: 1_800_000_000,
                nanos: 0,
            }),
            ..Default::default()
        })),
        ..Default::default()
    };
    let wire = envelope.encode_to_vec();

    for _ in 0..50 {
        validate_strict_command_envelope(&wire).expect("valid wire");
    }

    let iterations = 2_000u32;
    let start = Instant::now();
    for _ in 0..iterations {
        validate_strict_command_envelope(&wire).expect("valid wire");
    }
    let elapsed = start.elapsed();
    let ns_per_op = elapsed.as_nanos() / u128::from(iterations);
    println!(
        "RUST_BENCH strict_wire_validate_128kib iterations={iterations} total_ms={:.3} ns_per_op={ns_per_op}",
        elapsed.as_secs_f64() * 1000.0
    );
}
