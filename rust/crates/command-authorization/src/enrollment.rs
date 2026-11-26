use std::io;

use ocservia_canonical_cbor::Bytes;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    EnrollRequest, EnrollmentProofV1, SealedSecretPurpose,
};
use serde::Serialize;
use sha2::{Digest as _, Sha256};

const ENROLLMENT_PROOF_DOMAIN_V1: &str = "ocservia/agent-enrollment/cbor.v1\0";
pub const ENROLLMENT_PROOF_VERSION_V1: u32 = 1;
pub const ENROLLMENT_PROTOCOL_MAJOR: u32 = 1;
pub const ENROLLMENT_PROTOCOL_MINOR: u32 = 1;

#[derive(Serialize)]
struct CborSealingKey<'a> {
    version: u32,
    purpose: u32,
    key_id: &'a str,
    public_key_sha256: Bytes<'a>,
}

#[derive(Serialize)]
struct CborEnrollmentProofV1<'a> {
    proof_version: u32,
    protocol_major: u32,
    protocol_minor: u32,
    token_sha256: Bytes<'a>,
    endpoint_id: Bytes<'a>,
    agent_version: &'a str,
    os_release: &'a str,
    ocserv_version: &'a str,
    boot_id: &'a str,
    agent_instance_id: Bytes<'a>,
    capabilities: &'a [String],
    sealing_keys: Vec<CborSealingKey<'a>>,
    environment: &'a str,
    nonce: Bytes<'a>,
    time_seconds: i64,
    time_nanos: u32,
}

/// Signs an enrollment request using the caller-provided 32-byte endpoint ID
/// and signature callback.
///
/// # Errors
///
/// Returns an error when the request endpoint differs from the supplied
/// endpoint ID or a canonical claim is invalid.
pub fn authorize_enrollment_with<F>(
    request: &mut EnrollRequest,
    endpoint_id: &[u8; 32],
    sign: F,
) -> Result<(), io::Error>
where
    F: FnOnce(&[u8]) -> [u8; 64],
{
    if request.endpoint_id != endpoint_id {
        return Err(invalid("enrollment endpoint does not match local identity"));
    }
    request.enrollment_protocol_major = ENROLLMENT_PROTOCOL_MAJOR;
    request.enrollment_protocol_minor = ENROLLMENT_PROTOCOL_MINOR;
    request.capabilities.sort();
    request.sealing_keys.sort_by_key(|key| key.purpose);
    request.proof = None;
    let canonical = enrollment_canonical_v1(request)?;
    request.proof = Some(EnrollmentProofV1 {
        version: ENROLLMENT_PROOF_VERSION_V1,
        signature: sign(&canonical).to_vec(),
    });
    Ok(())
}

/// Produces the cross-language canonical `EnrollmentProofV1` signing input.
///
/// # Errors
///
/// Returns an error for a missing, malformed, duplicate, or out-of-range
/// canonical claim.
pub fn enrollment_canonical_v1(request: &EnrollRequest) -> Result<Vec<u8>, io::Error> {
    let timestamp = request
        .time
        .as_ref()
        .ok_or_else(|| invalid("enrollment proof timestamp is missing"))?;
    if request.enrollment_protocol_major != ENROLLMENT_PROTOCOL_MAJOR
        || request.enrollment_protocol_minor != ENROLLMENT_PROTOCOL_MINOR
        || request.endpoint_id.len() != 32
        || request.agent_instance_id.len() != 16
        || !(16..=64).contains(&request.nonce.len())
        || !(0..1_000_000_000).contains(&timestamp.nanos)
        || request.capabilities.is_empty()
        || request.capabilities.len() > 128
        || request.sealing_keys.len() != 2
    {
        return Err(invalid("enrollment proof claims are invalid"));
    }
    let mut sealing_keys = request.sealing_keys.clone();
    sealing_keys.sort_by_key(|key| key.purpose);
    if sealing_keys.iter().enumerate().any(|(index, key)| {
        key.version != 1
            || key.purpose
                != i32::from(if index == 0 {
                    SealedSecretPurpose::UserPassword
                } else {
                    SealedSecretPurpose::CertificateP12Password
                })
            || key.key_id.is_empty()
            || key.key_id.len() > 128
            || key.public_key_sha256.len() != 32
    }) || sealing_keys[0].key_id == sealing_keys[1].key_id
        || sealing_keys[0].public_key_sha256 == sealing_keys[1].public_key_sha256
    {
        return Err(invalid("enrollment sealing keys are invalid"));
    }
    let mut capabilities = request.capabilities.clone();
    capabilities.sort();
    if capabilities.windows(2).any(|pair| pair[0] == pair[1])
        || capabilities
            .iter()
            .any(|value| value.is_empty() || value.len() > 128)
    {
        return Err(invalid("enrollment proof capabilities are invalid"));
    }
    let token_hash = Sha256::digest(request.token.as_bytes());
    let cbor_keys = sealing_keys
        .iter()
        .map(|key| {
            Ok(CborSealingKey {
                version: key.version,
                purpose: u32::try_from(key.purpose)
                    .map_err(|_| invalid("sealing key purpose invalid"))?,
                key_id: &key.key_id,
                public_key_sha256: Bytes(&key.public_key_sha256),
            })
        })
        .collect::<Result<Vec<_>, io::Error>>()?;
    let time_nanos =
        u32::try_from(timestamp.nanos).map_err(|_| invalid("timestamp nanos are invalid"))?;
    ocservia_canonical_cbor::marshal(
        ENROLLMENT_PROOF_DOMAIN_V1,
        &CborEnrollmentProofV1 {
            proof_version: ENROLLMENT_PROOF_VERSION_V1,
            protocol_major: request.enrollment_protocol_major,
            protocol_minor: request.enrollment_protocol_minor,
            token_sha256: Bytes(token_hash.as_slice()),
            endpoint_id: Bytes(&request.endpoint_id),
            agent_version: &request.agent_version,
            os_release: &request.os_release,
            ocserv_version: &request.ocserv_version,
            boot_id: &request.boot_id,
            agent_instance_id: Bytes(&request.agent_instance_id),
            capabilities: &capabilities,
            sealing_keys: cbor_keys,
            environment: &request.environment,
            nonce: Bytes(&request.nonce),
            time_seconds: timestamp.seconds,
            time_nanos,
        },
    )
    .map_err(|_| invalid("enrollment proof canonical encoding failed"))
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer as _, SigningKey};
    use ocservia_contracts::generated::ocserv::platform::agent::v1::SealingKeyDescriptorV1;

    #[test]
    fn enrollment_proof_matches_the_go_golden_vector() {
        let mut seed = [0_u8; 32];
        for (index, byte) in seed.iter_mut().enumerate() {
            *byte = u8::try_from(index).expect("seed index");
        }
        let signing_key = SigningKey::from_bytes(&seed);
        let endpoint_id = signing_key.verifying_key().to_bytes();
        let mut request = EnrollRequest {
            token: "enrollment-token-fixture".into(),
            endpoint_id: endpoint_id.to_vec(),
            agent_version: "agent-1.2.3".into(),
            os_release: "FixtureOS 9".into(),
            ocserv_version: "1.3.0".into(),
            boot_id: "boot-fixture".into(),
            agent_instance_id: hex::decode("00112233445566778899aabbccddeeff")
                .expect("instance fixture"),
            capabilities: vec!["ocserv.users.write".into(), "ocserv.status.read".into()],
            environment: "production".into(),
            nonce: hex::decode("ffeeddccbbaa99887766554433221100").expect("nonce fixture"),
            time: Some(prost_types::Timestamp {
                seconds: 1_700_000_000,
                nanos: 123_456_789,
            }),
            enrollment_protocol_major: ENROLLMENT_PROTOCOL_MAJOR,
            enrollment_protocol_minor: ENROLLMENT_PROTOCOL_MINOR,
            proof: None,
            sealing_keys: vec![
                SealingKeyDescriptorV1 {
                    version: 1,
                    purpose: SealedSecretPurpose::UserPassword as i32,
                    key_id: "fixture-user-key-v1".into(),
                    public_key_sha256: vec![0x11; 32],
                },
                SealingKeyDescriptorV1 {
                    version: 1,
                    purpose: SealedSecretPurpose::CertificateP12Password as i32,
                    key_id: "fixture-p12-key-v1".into(),
                    public_key_sha256: vec![0x22; 32],
                },
            ],
        };
        let fixture = include_str!("../../../../testdata/enrollment-proof-v1.txt");
        let fields: Vec<_> = fixture.split_whitespace().collect();
        assert_eq!(fields.len(), 2);
        assert_eq!(
            hex::encode(enrollment_canonical_v1(&request).expect("canonical enrollment proof")),
            fields[0]
        );
        authorize_enrollment_with(&mut request, &endpoint_id, |canonical| {
            signing_key.sign(canonical).to_bytes()
        })
        .expect("sign enrollment proof");
        assert_eq!(
            hex::encode(&request.proof.expect("proof").signature),
            fields[1]
        );
    }
}
