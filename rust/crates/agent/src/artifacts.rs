use crate::session::{SessionGrantAuthority, refresh_fence_epoch_floor, verify_fenced_operation};
use crate::startup::invalid;
use crate::{CONNECTION_FENCING_CAPABILITY, HANDSHAKE_TIMEOUT};
use iroh::EndpointId;
use ocservia_agent::PrivdClient;
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, privd_request, privd_response,
};
use ocservia_command_authorization::{ControllerCommandKeyring, VerifiedConnectionFenceV2};
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactChunk, ArtifactConsumeRequest as ArtifactConsumeFinalizeRequest,
    ArtifactConsumeResponse as ArtifactConsumeFinalizeResponse, ArtifactFetchRequest,
    FenceOperationKind,
};
use prost::Message;
use sha2::Digest;
use std::time::{Duration, SystemTime};
use uuid::Uuid;

#[allow(clippy::too_many_arguments, clippy::too_many_lines)]
pub(super) async fn handle_artifact_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
    authority: &SessionGrantAuthority,
    active_fence: &VerifiedConnectionFenceV2,
    journal: &mut Journal,
    fence_epoch_floor: &mut u64,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let length = usize::try_from(length)?;
    if length == 0 || length > 64 * 1024 {
        return Err(invalid("artifact request size invalid").into());
    }
    let mut bytes = vec![0; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| invalid("artifact request timed out"))??;
    let request = ArtifactFetchRequest::decode(bytes.as_slice())?;
    let artifact =
        Uuid::from_slice(&request.artifact_id).map_err(|_| invalid("artifact ID invalid"))?;
    let grant = request
        .grant
        .as_ref()
        .ok_or_else(|| invalid("artifact grant required"))?;
    if artifact.get_version_num() != 7
        || request.purpose != "certificate_p12"
        || request.max_bytes == 0
        || request.max_bytes > 64 * 1024 * 1024
    {
        return Err(invalid("artifact request invalid").into());
    }
    refresh_fence_epoch_floor(journal, fence_epoch_floor)?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    verify_fenced_operation(
        command_keys,
        authority,
        active_fence,
        *fence_epoch_floor,
        &node_id,
        &endpoint_id,
        request.connection_fence.as_ref(),
        request.fence_binding.as_ref(),
        FenceOperationKind::Artifact,
        artifact.as_bytes(),
        CONNECTION_FENCING_CAPABILITY,
        now_unix_seconds,
        now.subsec_nanos(),
    )?;
    command_keys.verify_artifact_grant(
        grant,
        node_id.as_bytes(),
        artifact.as_bytes(),
        &request.purpose,
        request.max_bytes,
        now_unix_seconds,
    )?;
    let mut offset = 0_u64;
    let mut hasher = sha2::Sha256::new();
    let digest;
    loop {
        let response = privd
            .call(privd_request::Operation::ArtifactRead(
                ArtifactReadRequest {
                    grant: Some(grant.clone()),
                    offset,
                    connection_fence: request.connection_fence.clone().map(Box::new),
                    fence_binding: request.fence_binding.clone().map(Box::new),
                },
            ))
            .await?;
        let data = match response.result {
            Some(privd_response::Result::ArtifactData(value)) => value,
            Some(privd_response::Result::Error(error)) => {
                return Err(invalid(&error.detail).into());
            }
            _ => return Err(invalid("privd artifact response invalid").into()),
        };
        if data.artifact_id != request.artifact_id
            || data.grant_id != grant.grant_id
            || data.offset != offset
            || data.data.is_empty()
            || data.data.len() > 256 * 1024
            || offset.saturating_add(data.data.len() as u64) > request.max_bytes
            || (data.eof && data.sha256.len() != 32)
            || (!data.eof && !data.sha256.is_empty())
        {
            return Err(invalid("privd artifact chunk invalid").into());
        }
        hasher.update(&data.data);
        let next_offset = offset.saturating_add(data.data.len() as u64);
        let chunk = ArtifactChunk {
            artifact_id: request.artifact_id.clone(),
            offset,
            data: data.data,
            eof: data.eof,
            sha256: data.sha256.clone(),
        }
        .encode_to_vec();
        send.write_all(&u32::try_from(chunk.len())?.to_be_bytes())
            .await?;
        send.write_all(&chunk).await?;
        offset = next_offset;
        if data.eof {
            digest = data.sha256;
            break;
        }
    }
    send.finish()?;
    let stopped = tokio::time::timeout(Duration::from_secs(30), send.stopped())
        .await
        .map_err(|_| invalid("artifact delivery acknowledgement timed out"))??;
    if stopped.is_some() || hasher.finalize().as_slice() != digest {
        return Err(invalid("artifact delivery integrity failed").into());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn handle_artifact_consume(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
    authority: &SessionGrantAuthority,
    active_fence: &VerifiedConnectionFenceV2,
    journal: &mut Journal,
    fence_epoch_floor: &mut u64,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let length = usize::try_from(length)?;
    if length == 0 || length > 64 * 1024 {
        return Err(invalid("artifact finalize request size invalid").into());
    }
    let mut bytes = vec![0; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| invalid("artifact finalize request timed out"))??;
    let request = ArtifactConsumeFinalizeRequest::decode(bytes.as_slice())?;
    let grant = request
        .grant
        .as_ref()
        .ok_or_else(|| invalid("artifact grant required"))?;
    let artifact =
        Uuid::from_slice(&grant.artifact_id).map_err(|_| invalid("artifact ID invalid"))?;
    let grant_id =
        Uuid::from_slice(&grant.grant_id).map_err(|_| invalid("artifact grant ID invalid"))?;
    if artifact.get_version_num() != 7
        || grant_id.get_version_num() != 7
        || request.sha256.len() != 32
        || request.size == 0
        || request.size != grant.max_bytes
        || grant.purpose != "certificate_p12"
    {
        return Err(invalid("artifact finalize request invalid").into());
    }
    refresh_fence_epoch_floor(journal, fence_epoch_floor)?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    verify_fenced_operation(
        command_keys,
        authority,
        active_fence,
        *fence_epoch_floor,
        &node_id,
        &endpoint_id,
        request.connection_fence.as_ref(),
        request.fence_binding.as_ref(),
        FenceOperationKind::Artifact,
        grant_id.as_bytes(),
        CONNECTION_FENCING_CAPABILITY,
        now_unix_seconds,
        now.subsec_nanos(),
    )?;
    if request.confirm_only {
        command_keys.verify_artifact_grant_for_confirmation(
            grant,
            node_id.as_bytes(),
            artifact.as_bytes(),
            "certificate_p12",
            request.size,
            now_unix_seconds,
        )?;
    } else {
        command_keys.verify_artifact_grant(
            grant,
            node_id.as_bytes(),
            artifact.as_bytes(),
            "certificate_p12",
            request.size,
            now_unix_seconds,
        )?;
    }
    let response = privd
        .call(privd_request::Operation::ArtifactConsume(
            ArtifactConsumeRequest {
                grant: Some(grant.clone()),
                sha256: request.sha256,
                size: request.size,
                confirm_only: request.confirm_only,
                connection_fence: request.connection_fence.map(Box::new),
                fence_binding: request.fence_binding.map(Box::new),
            },
        ))
        .await?;
    let Some(privd_response::Result::Mutation(result)) = response.result else {
        return Err(invalid("artifact consumption failed").into());
    };
    if !request.confirm_only && !result.applied {
        return Err(invalid("artifact consumption failed").into());
    }
    let response = ArtifactConsumeFinalizeResponse {
        artifact_id: grant.artifact_id.clone(),
        grant_id: grant.grant_id.clone(),
        consumed: result.applied,
    }
    .encode_to_vec();
    send.write_all(&u32::try_from(response.len())?.to_be_bytes())
        .await?;
    send.write_all(&response).await?;
    send.finish()?;
    Ok(())
}
