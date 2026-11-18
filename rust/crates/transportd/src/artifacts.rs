use super::{
    ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME, ARTIFACT_STREAM_ABORT,
    AgentArtifactConsumeResponse, ArtifactChunk, ArtifactGrantV1, Connection, MAX_FRAME_BYTES,
    Status, SystemTime, mpsc,
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

/// Converts the grant expiry into one absolute stream deadline. The grant is
/// authoritative for how long this export may run; chunks never extend it.
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

/// Streams one artifact fetch from the Agent to the Controller consumer,
/// validating every chunk against the exact request carrier before it is
/// forwarded. Consumer cancellation and the grant deadline bound the whole
/// forwarding future; the Agent stream is then reset/stopped without closing
/// the shared Agent connection.
pub(super) async fn relay_artifact_stream(
    connection: Connection,
    payload: Vec<u8>,
    artifact_id: Vec<u8>,
    max_bytes: u64,
    sender: mpsc::Sender<Result<ArtifactChunk, Status>>,
    deadline: tokio::time::Instant,
) {
    let mut streams = None;
    let outcome = tokio::select! {
        outcome = forward_artifact_chunks(
            &connection,
            &payload,
            &artifact_id,
            max_bytes,
            &sender,
            &mut streams,
        ) => outcome,
        () = sender.closed() => Err(Status::cancelled("artifact consumer disconnected")),
        () = tokio::time::sleep_until(deadline) => {
            Err(Status::deadline_exceeded("artifact grant expired"))
        }
    };
    let Err(error) = outcome else { return };
    if let Some((mut send, mut recv)) = streams {
        let _ = send.reset(ARTIFACT_STREAM_ABORT);
        let _ = recv.stop(ARTIFACT_STREAM_ABORT);
    }
    // Never wait for a consumer that is not draining the channel.
    let _ = sender.try_send(Err(error));
}

async fn forward_artifact_chunks(
    connection: &Connection,
    payload: &[u8],
    artifact_id: &[u8],
    max_bytes: u64,
    sender: &mpsc::Sender<Result<ArtifactChunk, Status>>,
    streams: &mut Option<(iroh::endpoint::SendStream, iroh::endpoint::RecvStream)>,
) -> Result<(), Status> {
    let (send, recv) = streams.insert(
        connection
            .open_bi()
            .await
            .map_err(|_| Status::unavailable("failed to open artifact stream"))?,
    );
    let length = u32::try_from(payload.len())
        .map_err(|_| Status::resource_exhausted("artifact request is too large"))?
        | ARTIFACT_FETCH_FRAME;
    send.write_all(&length.to_be_bytes())
        .await
        .map_err(|_| Status::unavailable("failed to write artifact request length"))?;
    send.write_all(payload)
        .await
        .map_err(|_| Status::unavailable("failed to write artifact request"))?;
    send.finish()
        .map_err(|_| Status::unavailable("failed to finish artifact request"))?;
    let mut expected_offset = 0_u64;
    loop {
        let mut size = [0_u8; 4];
        tokio::time::timeout(Duration::from_secs(30), recv.read_exact(&mut size))
            .await
            .map_err(|_| Status::deadline_exceeded("artifact stream timed out"))?
            .map_err(|_| Status::unavailable("artifact stream ended"))?;
        let size = u32::from_be_bytes(size) as usize;
        if size == 0 || size > MAX_FRAME_BYTES {
            return Err(Status::resource_exhausted("artifact chunk is invalid"));
        }
        let mut bytes = vec![0; size];
        recv.read_exact(&mut bytes)
            .await
            .map_err(|_| Status::unavailable("artifact chunk ended"))?;
        let chunk = ArtifactChunk::decode(bytes.as_slice())
            .map_err(|_| Status::data_loss("artifact chunk protobuf is invalid"))?;
        if chunk.artifact_id != artifact_id
            || chunk.offset != expected_offset
            || chunk.data.len() > 256 * 1024
            || expected_offset.saturating_add(chunk.data.len() as u64) > max_bytes
            || (!chunk.eof && !chunk.sha256.is_empty())
            || (chunk.eof && chunk.sha256.len() != 32)
        {
            return Err(Status::data_loss("artifact chunk is inconsistent"));
        }
        expected_offset = expected_offset.saturating_add(chunk.data.len() as u64);
        let eof = chunk.eof;
        if eof {
            // Consume QUIC FIN before forwarding the terminal chunk. Dropping
            // an unread stream sends STOP_SENDING and invalidates the Agent's
            // delivery acknowledgement even when every payload byte arrived.
            tokio::time::timeout(Duration::from_secs(30), recv.read_to_end(0))
                .await
                .map_err(|_| Status::deadline_exceeded("artifact stream termination timed out"))?
                .map_err(|_| Status::data_loss("artifact stream has invalid trailing data"))?;
        }
        sender
            .send(Ok(chunk))
            .await
            .map_err(|_| Status::cancelled("artifact consumer disconnected"))?;
        if eof {
            return Ok(());
        }
    }
}
