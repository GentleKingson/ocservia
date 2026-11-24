use std::collections::HashSet;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_util::StreamExt as _;
use iroh::EndpointId;
use iroh::endpoint::{Connection, PathEvent, VarInt};
use iroh::protocol::{AcceptError, ProtocolHandler};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    EnrollRequest, HandshakeResult, SessionHandshake, SessionHandshakeResponse, TelemetryBatch,
    TelemetryPriority,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    ConnectionPath, NodeConnection, TransportEventType,
};
use prost::Message;
use tonic::Status;
use uuid::Uuid;

use crate::admission::trust_rejection_error;
use crate::command_stream::{event, now_timestamp};
use crate::registry::finish_session;
use crate::trust::{IdentityPolicy, TrustAuthority, TrustClass};
use crate::{
    MAX_FRAME_BYTES, MAX_HANDSHAKE_BYTES, MAX_STREAMS, PROTOCOL_MAJOR, PROTOCOL_MINOR,
    STREAM_TIMEOUT, Shared, unix_now_seconds, validate_uuid,
};

#[derive(Clone, Copy, Debug)]
pub(crate) enum ProtocolKind {
    Enroll,
    Agent,
}

#[derive(Clone)]
pub(crate) struct SessionHandler {
    pub(crate) shared: Shared,
    pub(crate) policy: IdentityPolicy,
    pub(crate) kind: ProtocolKind,
    pub(crate) trust: TrustAuthority,
    /// One absolute budget for the whole pre-registration handshake.
    pub(crate) handshake_timeout: Duration,
}

pub(crate) struct NegotiatedSession {
    pub(crate) handshake: SessionHandshake,
    pub(crate) negotiated_capabilities: HashSet<String>,
    pub(crate) authorization_revision: u64,
    pub(crate) session_epoch: u64,
    pub(crate) connection_id: [u8; 16],
    pub(crate) session_expires_at: SystemTime,
}

impl std::fmt::Debug for SessionHandler {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("SessionHandler")
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

impl SessionHandler {
    #[allow(clippy::too_many_lines)]
    async fn negotiate(
        &self,
        connection: &Connection,
        send: &mut iroh::endpoint::SendStream,
        recv: &mut iroh::endpoint::RecvStream,
    ) -> Result<Option<NegotiatedSession>, AcceptError> {
        if matches!(self.kind, ProtocolKind::Enroll) {
            let request: EnrollRequest = read_message(recv, "enrollment").await?;
            validate_enrollment(&request, connection.remote_id())?;
            let response = self.trust.enroll(request).await?;
            write_message(send, &response).await?;
            wait_for_delivery(send).await?;
            let reason = if response.result == i32::from(HandshakeResult::Accepted) {
                b"enrollment recovered".as_slice()
            } else {
                b"enrollment pending".as_slice()
            };
            connection.close(VarInt::from_u32(0), reason);
            return Ok(None);
        }
        let handshake: SessionHandshake = read_message(recv, "handshake").await?;
        validate_handshake(&handshake, connection.remote_id())?;
        validate_protocol_version(&handshake)?;
        let connection_id = *Uuid::now_v7().as_bytes();
        let response = self
            .trust
            .authorize(connection.remote_id(), handshake.clone(), connection_id)
            .await?;
        write_message(send, &response).await?;
        let result =
            HandshakeResult::try_from(response.result).unwrap_or(HandshakeResult::Unspecified);
        if result != HandshakeResult::Accepted {
            wait_for_delivery(send).await?;
            connection.close(VarInt::from_u32(0x101), b"session rejected");
            return Ok(None);
        }
        if response.max_message_size == 0
            || response.max_message_size > handshake.max_message_size
            || response.max_message_size as usize > MAX_FRAME_BYTES
        {
            return Err(protocol_error("negotiated message size is invalid"));
        }
        let negotiated_capabilities =
            validate_negotiated_session(&handshake, connection.remote_id(), &response)?;
        let grant = response
            .session_grant
            .as_ref()
            .ok_or_else(|| protocol_error("Controller session grant is required"))?;
        let node_id: [u8; 16] = handshake
            .node_id
            .as_slice()
            .try_into()
            .map_err(|_| protocol_error("handshake node id is invalid"))?;
        let endpoint_id: [u8; 32] = *connection.remote_id().as_bytes();
        let verified = self
            .shared
            .keyring()
            .verify_session_grant(grant, &node_id, &endpoint_id, unix_now_seconds())
            .map_err(|_| protocol_error("Controller session grant is invalid"))?;
        let session_expires_at = UNIX_EPOCH
            + Duration::from_secs(
                u64::try_from(verified.expires_at_seconds)
                    .map_err(|_| protocol_error("session grant expiry is invalid"))?,
            );
        let mut handshake = handshake;
        handshake.max_message_size = response.max_message_size;
        Ok(Some(NegotiatedSession {
            handshake,
            negotiated_capabilities,
            authorization_revision: verified.authorization_revision,
            session_epoch: verified.session_epoch,
            connection_id,
            session_expires_at,
        }))
    }
}

impl ProtocolHandler for SessionHandler {
    #[allow(clippy::too_many_lines)]
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        let permits = match self.kind {
            ProtocolKind::Enroll => &self.shared.inner.enrollment_connection_permits,
            ProtocolKind::Agent => &self.shared.inner.agent_connection_permits,
        };
        let permit = permits
            .clone()
            .try_acquire_owned()
            .map_err(|_| protocol_error("connection capacity reached"))?;
        let mut streams = None;
        let negotiated = tokio::time::timeout(self.handshake_timeout, async {
            let (send, recv) = streams.insert(
                connection
                    .accept_bi()
                    .await
                    .map_err(|_| protocol_error("handshake stream failed"))?,
            );
            self.negotiate(&connection, send, recv).await
        })
        .await;
        let Ok(negotiated) = negotiated else {
            connection.close(VarInt::from_u32(0x10c), b"handshake timed out");
            return Err(protocol_error("handshake timed out"));
        };
        let Some(session) = negotiated? else {
            drop(permit);
            return Ok(());
        };
        let max_message_size = session.handshake.max_message_size as usize;

        connection.set_max_concurrent_bi_streams(VarInt::from_u32(MAX_STREAMS));
        connection.set_max_concurrent_uni_streams(VarInt::from_u32(2));
        let metadata = metadata(&session, &connection).await;
        let node_id = metadata.node_id.clone();
        let registration_token = self.shared.registration_token(&node_id).await;
        match self
            .trust
            .check_agent_endpoint(connection.remote_id(), TrustClass::AgentRegistrationRecheck)
            .await
        {
            Ok(true) => {}
            Ok(false) => {
                connection.close(VarInt::from_u32(0x101), b"session trust changed");
                return Err(protocol_error(
                    "endpoint trust changed during session registration",
                ));
            }
            Err(rejection) => {
                connection.close(VarInt::from_u32(0x102), b"trust authority unavailable");
                return Err(trust_rejection_error(rejection));
            }
        }
        let connection_id = session.connection_id;
        let session_epoch = session.session_epoch;
        let session_expires_at = session.session_expires_at;
        self.shared
            .register_session(
                &self.policy,
                &connection,
                metadata,
                session,
                &registration_token,
            )
            .await?;
        let monitors = (
            monitor_paths(self.shared.clone(), node_id.clone(), connection.clone()),
            monitor_audit(
                self.shared.clone(),
                self.trust.clone(),
                node_id.clone(),
                connection.clone(),
                connection_id,
                session_epoch,
            ),
            monitor_telemetry(
                self.shared.clone(),
                node_id.clone(),
                connection.clone(),
                max_message_size,
            ),
            monitor_session_expiry(connection.clone(), session_expires_at),
        );
        let _closed = connection.closed().await;
        finish_session(&self.shared, &node_id, &connection, monitors, permit).await;
        Ok(())
    }
}

fn monitor_session_expiry(
    connection: Connection,
    expires_at: SystemTime,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        if let Ok(remaining) = expires_at.duration_since(SystemTime::now()) {
            tokio::time::sleep(remaining).await;
        }
        connection.close(VarInt::from_u32(0x109), b"session grant expired");
    })
}

fn monitor_audit(
    shared: Shared,
    authority: TrustAuthority,
    node: Vec<u8>,
    connection: Connection,
    connection_id: [u8; 16],
    session_epoch: u64,
) -> tokio::task::JoinHandle<()> {
    use ocservia_contracts::generated::ocserv::platform::{
        agent::v1::VpnAuditBatch, transport::v1::StoreVpnAuditRequest,
    };
    tokio::spawn(async move {
        while let Ok((mut send, mut recv)) = connection.accept_bi().await {
            let _ = tokio::time::timeout(STREAM_TIMEOUT, async {
                let mut prefix = [0u8; 4];
                recv.read_exact(&mut prefix)
                    .await
                    .map_err(|_| protocol_error("audit length missing"))?;
                let size = u32::from_be_bytes(prefix) as usize;
                if size == 0
                    || size > MAX_HANDSHAKE_BYTES
                    || !shared.admit_telemetry(&node, size).await
                {
                    return Err(protocol_error("audit frame limit exceeded"));
                }
                let mut payload = vec![0u8; size];
                recv.read_exact(&mut payload)
                    .await
                    .map_err(|_| protocol_error("incomplete audit frame"))?;
                let batch = VpnAuditBatch::decode(payload.as_slice())
                    .map_err(|_| protocol_error("invalid audit frame"))?;
                if batch.records.is_empty() || batch.records.len() > 32 {
                    return Err(protocol_error("audit batch limit exceeded"));
                }
                let receipt = authority
                    .store_vpn_audit(StoreVpnAuditRequest {
                        node_id: node.clone(),
                        endpoint_id: connection.remote_id().as_bytes().to_vec(),
                        connection_id: connection_id.to_vec(),
                        session_epoch,
                        batch: Some(batch),
                    })
                    .await?;
                write_message(&mut send, &receipt).await
            })
            .await;
        }
    })
}

fn monitor_telemetry(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
    max_message_size: usize,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        while let Ok(mut recv) = connection.accept_uni().await {
            let mut length = [0_u8; 4];
            if !matches!(
                tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut length)).await,
                Ok(Ok(()))
            ) {
                continue;
            }
            let length = u32::from_be_bytes(length) as usize;
            if length == 0 || length > 512 * 1024 || length > max_message_size {
                connection.close(VarInt::from_u32(0x107), b"telemetry frame size invalid");
                break;
            }
            if !shared.admit_telemetry(&node_id, length).await {
                tracing::warn!(
                    event_type = "telemetry_budget_exceeded",
                    node_id = %hex::encode(&node_id),
                    "closing connection that exceeded its telemetry budget"
                );
                connection.close(VarInt::from_u32(0x10d), b"telemetry budget exceeded");
                break;
            }
            let mut payload = vec![0_u8; length];
            if !matches!(
                tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut payload)).await,
                Ok(Ok(()))
            ) {
                continue;
            }
            let Ok(batch) = TelemetryBatch::decode(payload.as_slice()) else {
                connection.close(VarInt::from_u32(0x107), b"telemetry protobuf invalid");
                break;
            };
            if batch.node_id != node_id
                || validate_uuid(&batch.batch_id, "telemetry batch_id").is_err()
            {
                connection.close(VarInt::from_u32(0x107), b"telemetry identity invalid");
                break;
            }
            let security = (batch.priority == i32::from(TelemetryPriority::Security)
                || !batch.security_events.is_empty())
                && shared.claim_security_priority(&node_id).await;
            shared
                .publish_telemetry(
                    event(
                        &node_id,
                        connection.remote_id().as_bytes(),
                        TransportEventType::Telemetry,
                        payload,
                    ),
                    security,
                )
                .await;
        }
    })
}

fn monitor_paths(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut events = connection.path_events();
        while let Some(path_event) = events.next().await {
            if !matches!(
                path_event,
                PathEvent::Selected { .. } | PathEvent::Lagged { .. }
            ) {
                continue;
            }
            let next = metadata_path(&connection);
            let mut registry = shared.inner.connections.lock().await;
            let Some(entry) = registry.get_mut(&node_id) else {
                break;
            };
            if entry.connection.stable_id() != connection.stable_id() {
                break;
            }
            entry.metadata.path = next.0.into();
            entry.metadata.path_detail.clone_from(&next.1);
            entry.metadata.round_trip_time_millis = next.2;
            entry.metadata.last_seen = Some(now_timestamp());
            shared
                .publish(event(
                    &node_id,
                    connection.remote_id().as_bytes(),
                    TransportEventType::PathChanged,
                    next.1.into_bytes(),
                ))
                .await;
            drop(registry);
        }
    })
}

async fn read_message<M: Message + Default>(
    recv: &mut iroh::endpoint::RecvStream,
    kind: &str,
) -> Result<M, AcceptError> {
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .map_err(|_| protocol_error(&format!("{kind} length invalid")))?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_HANDSHAKE_BYTES {
        return Err(protocol_error(&format!("{kind} size invalid")));
    }
    let mut bytes = vec![0_u8; length];
    recv.read_exact(&mut bytes)
        .await
        .map_err(|_| protocol_error(&format!("{kind} body invalid")))?;
    M::decode(bytes.as_slice()).map_err(|_| protocol_error(&format!("{kind} protobuf invalid")))
}

async fn write_message(
    send: &mut iroh::endpoint::SendStream,
    response: &impl Message,
) -> Result<(), AcceptError> {
    let response = response.encode_to_vec();
    let response_len = u32::try_from(response.len())
        .map_err(|_| protocol_error("handshake response length exceeds u32"))?;
    send.write_all(&response_len.to_be_bytes())
        .await
        .map_err(|_| protocol_error("handshake response length failed"))?;
    send.write_all(&response)
        .await
        .map_err(|_| protocol_error("handshake response failed"))?;
    send.finish()
        .map_err(|_| protocol_error("handshake response finish failed"))?;
    Ok(())
}

async fn wait_for_delivery(send: &iroh::endpoint::SendStream) -> Result<(), AcceptError> {
    let stopped = send
        .stopped()
        .await
        .map_err(|_| protocol_error("handshake response acknowledgement failed"))?;
    if stopped.is_some() {
        return Err(protocol_error("peer rejected handshake response"));
    }
    Ok(())
}

pub(crate) fn validate_negotiated_session(
    handshake: &SessionHandshake,
    endpoint: EndpointId,
    response: &SessionHandshakeResponse,
) -> Result<HashSet<String>, AcceptError> {
    if response.protocol_major != PROTOCOL_MAJOR
        || response.protocol_minor != PROTOCOL_MINOR
        || response.protocol_minor > handshake.protocol_minor
    {
        return Err(protocol_error("negotiated protocol version is invalid"));
    }
    let supported = handshake.capabilities.iter().collect::<HashSet<_>>();
    let mut previous: Option<&str> = None;
    for capability in &response.negotiated_capabilities {
        if capability.is_empty()
            || capability.len() > 128
            || !supported.contains(capability)
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(protocol_error("negotiated capability set is invalid"));
        }
        previous = Some(capability);
    }
    let grant = response
        .session_grant
        .as_ref()
        .ok_or_else(|| protocol_error("Controller session grant is required"))?;
    if grant.protocol_major != response.protocol_major
        || grant.protocol_minor != response.protocol_minor
        || grant.node_id != handshake.node_id
        || grant.endpoint_id != endpoint.as_bytes()
        || grant.authorization_revision == 0
        || grant.session_epoch == 0
        || grant.key_id.is_empty()
        || grant.signature.len() != 64
        || grant.negotiated_capabilities != response.negotiated_capabilities
        || grant.issued_at.is_none()
        || grant.expires_at.is_none()
    {
        return Err(protocol_error(
            "Controller session grant claims are inconsistent",
        ));
    }
    Ok(response.negotiated_capabilities.iter().cloned().collect())
}

pub(crate) fn validate_enrollment(
    request: &EnrollRequest,
    remote: EndpointId,
) -> Result<(), AcceptError> {
    validate_uuid(&request.agent_instance_id, "agent_instance_id")
        .map_err(|status| status_to_accept(&status))?;
    if request.endpoint_id != remote.as_bytes()
        || request.enrollment_protocol_major != 1
        || request.enrollment_protocol_minor != 1
        || request
            .proof
            .as_ref()
            .is_none_or(|proof| proof.version != 1 || proof.signature.len() != 64)
    {
        return Err(protocol_error("endpoint identity mismatch"));
    }
    let Some(token) = request.token.strip_prefix("obt2_") else {
        return Err(protocol_error("enrollment credential or nonce is invalid"));
    };
    if token.len() != 43 || request.nonce.len() < 16 || request.nonce.len() > 64 {
        return Err(protocol_error("enrollment credential or nonce is invalid"));
    }
    if request.capabilities.is_empty()
        || request.capabilities.len() > 128
        || request
            .capabilities
            .iter()
            .any(|value| value.is_empty() || value.chars().count() > 128)
        || request.capabilities.iter().collect::<HashSet<_>>().len() != request.capabilities.len()
        || request.time.is_none()
        || [
            &request.agent_version,
            &request.os_release,
            &request.ocserv_version,
            &request.boot_id,
            &request.environment,
        ]
        .iter()
        .any(|value| value.is_empty() || value.chars().count() > 256)
    {
        return Err(protocol_error("enrollment field limit exceeded"));
    }
    Ok(())
}

pub(crate) fn validate_handshake(
    handshake: &SessionHandshake,
    remote: EndpointId,
) -> Result<(), AcceptError> {
    validate_uuid(&handshake.node_id, "node_id").map_err(|status| status_to_accept(&status))?;
    validate_uuid(&handshake.agent_instance_id, "agent_instance_id")
        .map_err(|status| status_to_accept(&status))?;
    if handshake.endpoint_id != remote.as_bytes() {
        return Err(protocol_error("endpoint identity mismatch"));
    }
    if handshake.max_message_size == 0 || handshake.max_message_size as usize > MAX_FRAME_BYTES {
        return Err(protocol_error("message size incompatible"));
    }
    if handshake.nonce.len() < 16 || handshake.nonce.len() > 64 {
        return Err(protocol_error("nonce size invalid"));
    }
    if handshake.capabilities.len() > 128
        || handshake
            .capabilities
            .iter()
            .any(|value| value.chars().count() > 128)
        || handshake.supported_compressions.len() > 16
        || [
            &handshake.agent_version,
            &handshake.controller_version,
            &handshake.ocserv_version,
            &handshake.os_release,
            &handshake.boot_id,
        ]
        .iter()
        .any(|value| value.chars().count() > 256)
    {
        return Err(protocol_error("handshake field limit exceeded"));
    }
    Ok(())
}

pub(crate) fn validate_protocol_version(handshake: &SessionHandshake) -> Result<(), AcceptError> {
    if handshake.protocol_major != PROTOCOL_MAJOR || handshake.protocol_minor != PROTOCOL_MINOR {
        return Err(protocol_error("protocol version incompatible"));
    }
    Ok(())
}

fn status_to_accept(status: &Status) -> AcceptError {
    protocol_error(status.message())
}

pub(crate) fn protocol_error(message: &str) -> AcceptError {
    AcceptError::from_err(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        message.to_owned(),
    ))
}

async fn metadata(session: &NegotiatedSession, connection: &Connection) -> NodeConnection {
    let handshake = &session.handshake;
    let path_metadata = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let next = metadata_path(connection);
            if next.0 != ConnectionPath::Unspecified {
                break next;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap_or_else(|_| metadata_path(connection));
    let (path, detail, rtt) = path_metadata;
    let now = now_timestamp();
    NodeConnection {
        node_id: handshake.node_id.clone(),
        endpoint_id: connection.remote_id().as_bytes().to_vec(),
        path: path.into(),
        round_trip_time_millis: rtt,
        connected_at: Some(now),
        agent_instance_id: handshake.agent_instance_id.clone(),
        path_detail: detail,
        last_seen: Some(now),
        negotiated_capabilities: {
            let mut capabilities = session
                .negotiated_capabilities
                .iter()
                .cloned()
                .collect::<Vec<_>>();
            capabilities.sort();
            capabilities
        },
        authorization_revision: session.authorization_revision,
        session_expires_at: Some(session.session_expires_at.into()),
        session_epoch: session.session_epoch,
    }
}

pub(crate) fn metadata_path(connection: &Connection) -> (ConnectionPath, String, u64) {
    let paths = connection.paths();
    let selected = paths
        .iter()
        .find(iroh::endpoint::Path::is_selected)
        .or_else(|| paths.iter().next());
    selected.map_or(
        (ConnectionPath::Unspecified, "unknown".to_owned(), 0),
        |path| {
            let kind = if path.is_ip() {
                ConnectionPath::Direct
            } else if path.is_relay() {
                ConnectionPath::Relay
            } else {
                ConnectionPath::Unspecified
            };
            let detail = format!("{:?}", path.remote_addr());
            let millis = u64::try_from(path.rtt().as_millis()).unwrap_or(u64::MAX);
            (kind, detail, millis)
        },
    )
}
