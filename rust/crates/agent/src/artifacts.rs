use crate::HANDSHAKE_TIMEOUT;
use crate::session::{SessionGrantAuthority, refresh_fence_epoch_floor};
use crate::startup::invalid;
use ocservia_agent::PrivdClient;
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, privd_request, privd_response,
};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactConsumeRequest as ArtifactConsumeFinalizeRequest,
    ArtifactConsumeResponse as ArtifactConsumeFinalizeResponse, ArtifactFetchRequest,
    ArtifactPayload,
};
use prost::Message;
use std::time::{Duration, SystemTime};
use uuid::Uuid;

#[allow(clippy::too_many_arguments)]
pub(super) async fn handle_artifact_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
    authority: &SessionGrantAuthority,
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
        || request.max_bytes > 256 * 1024
    {
        return Err(invalid("artifact request invalid").into());
    }
    refresh_fence_epoch_floor(journal, fence_epoch_floor)?;
    if authority.session_epoch != *fence_epoch_floor {
        return Err(invalid("artifact fetch from stale session epoch rejected").into());
    }
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    command_keys.verify_artifact_grant(
        grant,
        node_id.as_bytes(),
        artifact.as_bytes(),
        &request.purpose,
        request.max_bytes,
        now_unix_seconds,
    )?;
    let response = privd
        .call_with_session(
            privd_request::Operation::ArtifactRead(ArtifactReadRequest {
                grant: Some(grant.clone()),
            }),
            &authority.raw_grant,
        )
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
        || data.data.is_empty()
        || data.data.len() > 256 * 1024
        || data.data.len() as u64 > request.max_bytes
        || data.sha256.len() != 32
    {
        return Err(invalid("privd artifact payload invalid").into());
    }
    let payload = ArtifactPayload {
        artifact_id: request.artifact_id,
        data: data.data,
        sha256: data.sha256,
    }
    .encode_to_vec();
    send.write_all(&u32::try_from(payload.len())?.to_be_bytes())
        .await?;
    send.write_all(&payload).await?;
    send.finish()?;
    let stopped = tokio::time::timeout(Duration::from_secs(30), send.stopped())
        .await
        .map_err(|_| invalid("artifact delivery acknowledgement timed out"))??;
    if stopped.is_some() {
        return Err(invalid("artifact delivery cancelled by peer").into());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn handle_artifact_consume(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
    authority: &SessionGrantAuthority,
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
    if authority.session_epoch != *fence_epoch_floor {
        return Err(invalid("artifact consume from stale session epoch rejected").into());
    }
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
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
        .call_with_session(
            privd_request::Operation::ArtifactConsume(ArtifactConsumeRequest {
                grant: Some(grant.clone()),
                sha256: request.sha256,
                size: request.size,
                confirm_only: request.confirm_only,
            }),
            &authority.raw_grant,
        )
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
