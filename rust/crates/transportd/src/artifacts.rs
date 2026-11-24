use super::{
    ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME, ARTIFACT_STREAM_ABORT,
    AgentArtifactConsumeResponse, ArtifactGrantV1, ArtifactPayload, Connection, MAX_FRAME_BYTES,
    Status, SystemTime,
};
use prost::Message;
use std::time::Duration;

/// Runs one artifact finalize exchange with the Agent and validates the
/// response against the exact consumed grant carrier.
pub(super) async fn finalize_artifact_consumption(
    connection: Connection,
    payload: Vec<u8>,
    expected_artifact_id: Vec<u8>,
    expected_grant_id: Vec<u8>,
    confirm_only: bool,
) -> Result<AgentArtifactConsumeResponse, Status> {
    let (mut send, mut recv) = connection
        .open_bi()
        .await
        .map_err(|_| Status::unavailable("failed to open artifact finalize stream"))?;
    let length = u32::try_from(payload.len())
        .map_err(|_| Status::resource_exhausted("artifact finalize request is too large"))?
        | ARTIFACT_CONSUME_FRAME;
    send.write_all(&length.to_be_bytes())
        .await
        .map_err(|_| Status::unavailable("failed to write artifact finalize length"))?;
    send.write_all(&payload)
        .await
        .map_err(|_| Status::unavailable("failed to write artifact finalize request"))?;
    send.finish()
        .map_err(|_| Status::unavailable("failed to finish artifact finalize request"))?;
    let mut size = [0_u8; 4];
    recv.read_exact(&mut size)
        .await
        .map_err(|_| Status::unavailable("artifact finalize response ended"))?;
    let size = u32::from_be_bytes(size) as usize;
    if size == 0 || size > 64 * 1024 {
        return Err(Status::data_loss("artifact finalize response is invalid"));
    }
    let mut bytes = vec![0; size];
    recv.read_exact(&mut bytes)
        .await
        .map_err(|_| Status::unavailable("artifact finalize response ended"))?;
    let response = AgentArtifactConsumeResponse::decode(bytes.as_slice())
        .map_err(|_| Status::data_loss("artifact finalize response is invalid"))?;
    if response.artifact_id != expected_artifact_id
        || response.grant_id != expected_grant_id
        || (!confirm_only && !response.consumed)
    {
        return Err(Status::data_loss(
            "artifact finalize response is inconsistent",
        ));
    }
    Ok(response)
}

/// Converts the grant expiry into one absolute deadline.
pub(super) fn artifact_stream_deadline(
    grant: &ArtifactGrantV1,
) -> Result<tokio::time::Instant, Status> {
    let expires_at = grant
        .expires_at
        .as_ref()
        .and_then(|expires_at| SystemTime::try_from(*expires_at).ok())
        .ok_or_else(|| Status::invalid_argument("artifact grant expiry is invalid"))?;
    let remaining = expires_at
        .duration_since(SystemTime::now())
        .map_err(|_| Status::deadline_exceeded("artifact grant has expired"))?;
    Ok(tokio::time::Instant::now() + remaining)
}

struct ArtifactStreamGuard {
    streams: Option<(iroh::endpoint::SendStream, iroh::endpoint::RecvStream)>,
    completed: bool,
}

impl Drop for ArtifactStreamGuard {
    fn drop(&mut self) {
        if !self.completed
            && let Some((mut send, mut recv)) = self.streams.take()
        {
            let _ = send.reset(ARTIFACT_STREAM_ABORT);
            let _ = recv.stop(ARTIFACT_STREAM_ABORT);
        }
    }
}

/// Executes one unary artifact fetch over the authenticated Agent connection,
/// bounded by the signed grant deadline.
pub(super) async fn fetch_artifact_payload(
    connection: Connection,
    request_bytes: Vec<u8>,
    expected_artifact_id: &[u8],
    max_bytes: u64,
    deadline: tokio::time::Instant,
) -> Result<ArtifactPayload, Status> {
    let mut guard = ArtifactStreamGuard {
        streams: None,
        completed: false,
    };
    let outcome = tokio::select! {
        outcome = exchange_artifact_payload(
            &connection,
            &request_bytes,
            expected_artifact_id,
            max_bytes,
            &mut guard.streams,
        ) => outcome,
        () = tokio::time::sleep_until(deadline) => {
            Err(Status::deadline_exceeded("artifact grant expired"))
        }
    };
    if outcome.is_ok() {
        guard.completed = true;
    }
    outcome
}

async fn exchange_artifact_payload(
    connection: &Connection,
    request_bytes: &[u8],
    expected_artifact_id: &[u8],
    max_bytes: u64,
    streams: &mut Option<(iroh::endpoint::SendStream, iroh::endpoint::RecvStream)>,
) -> Result<ArtifactPayload, Status> {
    let (send, recv) = streams.insert(
        connection
            .open_bi()
            .await
            .map_err(|_| Status::unavailable("failed to open artifact stream"))?,
    );
    let length = u32::try_from(request_bytes.len())
        .map_err(|_| Status::resource_exhausted("artifact request is too large"))?
        | ARTIFACT_FETCH_FRAME;
    send.write_all(&length.to_be_bytes())
        .await
        .map_err(|_| Status::unavailable("failed to write artifact request length"))?;
    send.write_all(request_bytes)
        .await
        .map_err(|_| Status::unavailable("failed to write artifact request"))?;
    send.finish()
        .map_err(|_| Status::unavailable("failed to finish artifact request"))?;
    let mut size = [0_u8; 4];
    tokio::time::timeout(Duration::from_secs(30), recv.read_exact(&mut size))
        .await
        .map_err(|_| Status::deadline_exceeded("artifact fetch timed out"))?
        .map_err(|_| Status::unavailable("artifact stream ended"))?;
    let size = u32::from_be_bytes(size) as usize;
    if size == 0 || size > MAX_FRAME_BYTES {
        return Err(Status::resource_exhausted(
            "artifact payload size is invalid",
        ));
    }
    let mut bytes = vec![0; size];
    recv.read_exact(&mut bytes)
        .await
        .map_err(|_| Status::unavailable("artifact payload ended"))?;
    let payload = ArtifactPayload::decode(bytes.as_slice())
        .map_err(|_| Status::data_loss("artifact payload protobuf is invalid"))?;
    if payload.artifact_id != expected_artifact_id
        || payload.data.is_empty()
        || payload.data.len() > 256 * 1024
        || payload.data.len() as u64 > max_bytes
        || payload.sha256.len() != 32
    {
        return Err(Status::data_loss("artifact payload is inconsistent"));
    }
    // Consume QUIC FIN so the Agent's `send.stopped()` acknowledgement succeeds.
    tokio::time::timeout(Duration::from_secs(30), recv.read_to_end(0))
        .await
        .map_err(|_| Status::deadline_exceeded("artifact stream termination timed out"))?
        .map_err(|_| Status::data_loss("artifact stream has invalid trailing data"))?;
    Ok(payload)
}
