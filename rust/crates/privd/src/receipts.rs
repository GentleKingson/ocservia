use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::SigningKey;
use ocservia_agent_protocol::{ErrorKind, PrivdError, privd_response};
use ocservia_command_authorization::CommandAuthorizationV1;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, CommandResultState, PrivdCertificateReceiptBindingV1, PrivdReceiptVersion,
    PrivdResultReceiptV1, PrivilegedCommandKind, PrivilegedResultKind, PrivilegedResultProof,
    command_envelope,
};
use ocservia_privd_attestation::{requested_subject_digest, sign_receipt_with_key_id};
use prost::Message as _;
use sha2::{Digest as _, Sha256};

use crate::effect_binding::CommandEffectBinding;

#[allow(clippy::too_many_arguments)]
pub(crate) fn build_result_proof(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    accepted_at: &prost_types::Timestamp,
    node_id: &[u8; 16],
    binding: &CommandEffectBinding,
    result: &privd_response::Result,
    key: &SigningKey,
    attestation_key_id: &str,
) -> Result<PrivilegedResultProof, ()> {
    let completed = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| ())?;
    let completed_at = prost_types::Timestamp {
        seconds: i64::try_from(completed.as_secs()).map_err(|_| ())?,
        nanos: i32::try_from(completed.subsec_nanos()).map_err(|_| ())?,
    };
    let result_bytes = exact_result_bytes(result).ok_or(())?;
    let mut effect = Sha256::new();
    effect.update(b"ocservia/privd-root-effect-record/v1\0");
    effect.update(node_id);
    effect.update(claims.command_id);
    effect.update(claims.operation_id);
    effect.update(claims.idempotency_key);
    effect.update(binding.effect_kind.as_bytes());
    effect.update((binding.resource_key.len() as u64).to_be_bytes());
    effect.update(binding.resource_key.as_bytes());
    effect.update(binding.effect_revision.to_be_bytes());
    let effect_record_id = effect.finalize().to_vec();
    let certificate = certificate_binding(command, result, &effect_record_id)?;
    let receipt = PrivdResultReceiptV1 {
        receipt_version: PrivdReceiptVersion::V1.into(),
        node_id: node_id.to_vec(),
        privd_attestation_key_id: attestation_key_id.to_owned(),
        command_id: claims.command_id.to_vec(),
        operation_id: claims.operation_id.to_vec(),
        idempotency_key: claims.idempotency_key.to_vec(),
        semantic_payload_hash_version: i32::try_from(claims.semantic_hash_version)
            .map_err(|_| ())?,
        semantic_payload_sha256: claims.semantic_payload_sha256.to_vec(),
        command_kind: privileged_command_kind(command).ok_or(())?.into(),
        result_kind: privileged_result_kind(result).ok_or(())?.into(),
        terminal_state: match result {
            privd_response::Result::Error(_) => CommandResultState::Failed.into(),
            _ => CommandResultState::Succeeded.into(),
        },
        result_bytes_sha256: Sha256::digest(&result_bytes).to_vec(),
        error_code_sha256: Sha256::digest(match result {
            privd_response::Result::Error(error) => {
                terminal_result_error_code(error).ok_or(())?.as_bytes()
            }
            _ => &[],
        })
        .to_vec(),
        effect_record_id,
        effect_sequence: binding.effect_revision.max(claims.expected_revision).max(1),
        accepted_at: Some(*accepted_at),
        completed_at: Some(completed_at),
        replayed: false,
        certificate,
    };
    sign_receipt_with_key_id(receipt, key, attestation_key_id).map_err(|_| ())
}

fn exact_result_bytes(result: &privd_response::Result) -> Option<Vec<u8>> {
    match result {
        privd_response::Result::Mutation(value) => Some(value.encode_to_vec()),
        privd_response::Result::ConfigPlan(value) => Some(value.encode_to_vec()),
        privd_response::Result::ConfigApply(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateCsr(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateP12(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateRevoke(value) => Some(value.encode_to_vec()),
        privd_response::Result::AgentUpgradeScheduled(value) => Some(value.encode_to_vec()),
        privd_response::Result::Error(error) if terminal_result_error_code(error).is_some() => {
            Some(Vec::new())
        }
        _ => None,
    }
}

fn privileged_result_kind(result: &privd_response::Result) -> Option<PrivilegedResultKind> {
    match result {
        privd_response::Result::Mutation(_) => Some(PrivilegedResultKind::Mutation),
        privd_response::Result::ConfigPlan(_) => Some(PrivilegedResultKind::ConfigPlan),
        privd_response::Result::ConfigApply(_) => Some(PrivilegedResultKind::ConfigApply),
        privd_response::Result::CertificateCsr(_) => Some(PrivilegedResultKind::CertificateCsr),
        privd_response::Result::CertificateP12(_) => Some(PrivilegedResultKind::CertificateP12),
        privd_response::Result::CertificateRevoke(_) => {
            Some(PrivilegedResultKind::CertificateRevoke)
        }
        privd_response::Result::AgentUpgradeScheduled(_) => {
            Some(PrivilegedResultKind::AgentUpgradeScheduled)
        }
        privd_response::Result::Error(error) if terminal_result_error_code(error).is_some() => {
            Some(PrivilegedResultKind::Error)
        }
        _ => None,
    }
}

fn privileged_command_kind(command: &CommandEnvelope) -> Option<PrivilegedCommandKind> {
    match command.payload.as_ref()? {
        command_envelope::Payload::SessionDisconnect(_) => {
            Some(PrivilegedCommandKind::SessionDisconnect)
        }
        command_envelope::Payload::SessionTerminate(_) => {
            Some(PrivilegedCommandKind::SessionTerminate)
        }
        command_envelope::Payload::IpBanRemove(_) => Some(PrivilegedCommandKind::IpBanRemove),
        command_envelope::Payload::ServiceReload(_) => Some(PrivilegedCommandKind::ServiceReload),
        command_envelope::Payload::UserCreate(_) => Some(PrivilegedCommandKind::UserCreate),
        command_envelope::Payload::UserDisable(_) => Some(PrivilegedCommandKind::UserDisable),
        command_envelope::Payload::UserEnable(_) => Some(PrivilegedCommandKind::UserEnable),
        command_envelope::Payload::UserPasswordRotate(_) => {
            Some(PrivilegedCommandKind::UserPasswordRotate)
        }
        command_envelope::Payload::GroupApply(_) => Some(PrivilegedCommandKind::GroupApply),
        command_envelope::Payload::CompleteConfigPlan(_) => Some(PrivilegedCommandKind::ConfigPlan),
        command_envelope::Payload::CompleteConfigApply(_) => {
            Some(PrivilegedCommandKind::ConfigApply)
        }
        command_envelope::Payload::CertificateCsr(_) => Some(PrivilegedCommandKind::CertificateCsr),
        command_envelope::Payload::CertificateP12(_) => Some(PrivilegedCommandKind::CertificateP12),
        command_envelope::Payload::CertificateRevoke(_) => {
            Some(PrivilegedCommandKind::CertificateRevoke)
        }
        command_envelope::Payload::AgentUpgrade(_) => Some(PrivilegedCommandKind::AgentUpgrade),
        _ => None,
    }
}

fn certificate_binding(
    command: &CommandEnvelope,
    result: &privd_response::Result,
    effect_record_id: &[u8],
) -> Result<Option<PrivdCertificateReceiptBindingV1>, ()> {
    let certificate_id = match (command.payload.as_ref(), result) {
        (
            Some(command_envelope::Payload::CertificateCsr(request)),
            privd_response::Result::CertificateCsr(response),
        ) => {
            return Ok(Some(PrivdCertificateReceiptBindingV1 {
                certificate_id: request.certificate_id.clone(),
                csr_der_sha256: Sha256::digest(&response.csr_der).to_vec(),
                public_key_sha256: response.public_key_sha256.clone(),
                requested_subject_sha256: requested_subject_digest(request)
                    .map_err(|_| ())?
                    .to_vec(),
                root_effect_record_id: effect_record_id.to_vec(),
            }));
        }
        (
            Some(command_envelope::Payload::CertificateCsr(request)),
            privd_response::Result::Error(error),
        ) if terminal_result_error_code(error).is_some() => &request.certificate_id,
        (Some(command_envelope::Payload::CertificateP12(request)), _) => &request.certificate_id,
        (Some(command_envelope::Payload::CertificateRevoke(request)), _) => &request.certificate_id,
        (Some(command_envelope::Payload::CertificateCsr(_)), _) => return Err(()),
        _ => return Ok(None),
    };
    Ok(Some(PrivdCertificateReceiptBindingV1 {
        certificate_id: certificate_id.clone(),
        csr_der_sha256: Vec::new(),
        public_key_sha256: Vec::new(),
        requested_subject_sha256: Vec::new(),
        root_effect_record_id: effect_record_id.to_vec(),
    }))
}

pub(crate) fn terminal_result_error_code(error: &PrivdError) -> Option<&'static str> {
    match ErrorKind::try_from(error.kind).unwrap_or(ErrorKind::Unspecified) {
        ErrorKind::CapacityExceeded => Some("capacity_exceeded"),
        ErrorKind::InvalidRequest | ErrorKind::PermissionDenied | ErrorKind::MalformedOutput => {
            Some("privd_rejected")
        }
        _ => None,
    }
}
