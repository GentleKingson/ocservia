use crate::commands::handle_command_stream;
use crate::startup::{invalid, system_dns_resolver, with_relay_tls_roots};
use crate::telemetry::{build_telemetry, send_telemetry};
use crate::{AGENT_ALPN, ARTIFACT_FRAME_MASK, CONNECTION_FENCING_CAPABILITY, HANDSHAKE_TIMEOUT};
use iroh::endpoint::{QuicTransportConfig, RelayMode, presets};
use iroh::{Endpoint, EndpointAddr, EndpointId, SecretKey};
use ocservia_agent::{CommandExecutor, PrivdClient};
use ocservia_agent_protocol::{PrivdResponse, ReadRequest, privd_request, privd_response};
use ocservia_command_authorization::{
    ControllerCommandKeyring, FenceBindingClaimsV2, VerifiedConnectionFenceV2, VerifiedSessionGrant,
};
use ocservia_command_journal::{Journal, OFFLINE_RECOVERY_RETENTION_SECONDS, TelemetryInsert};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ConnectionFenceV2, FenceBindingV2, FenceOperationKind, HandshakeResult, SealingKeyDescriptorV1,
    SessionHandshake, SessionHandshakeResponse,
};
use ocservia_contracts::session::{
    ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME, READ_ONLY_SESSION_CAPABILITIES,
    is_read_only_session_capability,
};
use prost::Message;
use std::collections::HashSet;
use std::future::Future;
use std::io;
use std::pin::Pin;
use std::time::{Duration, SystemTime};
use uuid::Uuid;

pub(super) async fn exchange_message<M: Message, R: Message + Default>(
    connection: &iroh::endpoint::Connection,
    request: &M,
    kind: &str,
) -> Result<R, Box<dyn std::error::Error + Send + Sync>> {
    let bytes = request.encode_to_vec();
    if bytes.is_empty() || bytes.len() > 64 * 1024 {
        return Err(invalid(&format!("{kind} request size is invalid")).into());
    }
    let response = tokio::time::timeout(HANDSHAKE_TIMEOUT, async {
        let (mut send, mut recv) = connection.open_bi().await?;
        send.write_all(&u32::try_from(bytes.len())?.to_be_bytes())
            .await?;
        send.write_all(&bytes).await?;
        send.finish()?;
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length).await?;
        let length = u32::from_be_bytes(length) as usize;
        if length == 0 || length > 64 * 1024 {
            return Err(invalid(&format!("{kind} response size is invalid")).into());
        }
        let mut response = vec![0_u8; length];
        recv.read_exact(&mut response).await?;
        Ok::<R, Box<dyn std::error::Error + Send + Sync>>(R::decode(response.as_slice())?)
    })
    .await
    .map_err(|_| invalid(&format!("{kind} timed out")))??;
    Ok(response)
}

pub(super) struct SessionContext<'a> {
    pub(super) node_id: Uuid,
    pub(super) endpoint_id: EndpointId,
    pub(super) privd: &'a PrivdClient,
    pub(super) command_executor: &'a mut CommandExecutor,
    pub(super) session_cache: crate::telemetry::SessionCache,
    pub(super) boot_id: &'a str,
    pub(super) os_release: &'a str,
    pub(super) agent_instance_id: Uuid,
    pub(super) command_keys: &'a ControllerCommandKeyring,
    pub(super) sealing_keys: &'a [SealingKeyDescriptorV1],
    pub(super) fence_epoch_floor: &'a mut u64,
}

pub(super) struct SessionGrantAuthority {
    pub(super) negotiated_capabilities: HashSet<String>,
    pub(super) authorization_revision: u64,
    pub(super) expires_at_unix_seconds: i64,
}

pub(super) enum ActiveSessionAuthority {
    SignedReadOnlyV11(SessionGrantAuthority),
    FencedV11 {
        grant: SessionGrantAuthority,
        connection_fence: VerifiedConnectionFenceV2,
    },
}

impl ActiveSessionAuthority {
    pub(super) fn grant(&self) -> &SessionGrantAuthority {
        match self {
            Self::SignedReadOnlyV11(grant) | Self::FencedV11 { grant, .. } => grant,
        }
    }

    fn fenced(&self) -> Option<(&SessionGrantAuthority, &VerifiedConnectionFenceV2)> {
        match self {
            Self::SignedReadOnlyV11(_) => None,
            Self::FencedV11 {
                grant,
                connection_fence,
            } => Some((grant, connection_fence)),
        }
    }
    fn name(&self) -> &'static str {
        match self {
            Self::SignedReadOnlyV11(_) => "signed_read_only_v1_1",
            Self::FencedV11 { .. } => "fenced_v1_1",
        }
    }

    fn expires_at_unix_seconds(&self) -> i64 {
        self.grant().expires_at_unix_seconds
    }
}

pub(super) fn fenced_artifact_authority(
    authority: &ActiveSessionAuthority,
    framed_length: u32,
) -> Result<(&SessionGrantAuthority, &VerifiedConnectionFenceV2), io::Error> {
    let frame_kind = framed_length & ARTIFACT_FRAME_MASK;
    if frame_kind != ARTIFACT_FETCH_FRAME && frame_kind != ARTIFACT_CONSUME_FRAME {
        return Err(invalid("artifact frame kind invalid"));
    }
    authority
        .fenced()
        .ok_or_else(|| invalid("signed read-only session received an artifact stream"))
}

async fn wait_for_session_expiry(
    authority: &ActiveSessionAuthority,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let lifetime = u64::try_from(
        authority
            .expires_at_unix_seconds()
            .saturating_sub(unix_seconds()?),
    )?;
    tokio::time::sleep(Duration::from_secs(lifetime)).await;
    Ok(())
}

pub(super) fn supported_capabilities() -> Vec<String> {
    READ_ONLY_SESSION_CAPABILITIES
        .iter()
        .copied()
        .chain([
            CONNECTION_FENCING_CAPABILITY,
            "synthetic.noop",
            "synthetic.echo",
            "command.semantic-hash.v1",
            "command.strict-wire.v1",
            "privd_result_attestation_v1",
            "ocserv.session.disconnect",
            "ocserv.session.terminate",
            "ocserv.ip_ban.remove",
            "ocserv.service.reload",
            "ocserv.users.write",
            "ocserv.groups.write",
            "ocserv.config.complete.plan",
            "ocserv.config.complete.apply",
            "config.auth",
            "config.tls",
            "config.sessions",
            "config.network",
            "config.limits",
            "config.runtime",
            "ocserv.certificate.issue",
            "ocserv.certificate.revoke",
            "ocserv.agent.upgrade.v2",
            "ocserv.agent.upgrade.v1",
        ])
        .map(str::to_owned)
        .collect()
}

/// Session capabilities this node can honor now. `occtl terminate` exists
/// only from Ocserv 1.4.2, so terminate is withheld unless privd reports such
/// a version. Enrollment keeps the static build set, so approval and
/// re-enrollment matching stay stable.
pub(super) async fn session_capabilities(privd: &PrivdClient) -> Vec<String> {
    let terminate = matches!(
        privd
            .call(privd_request::Operation::OcservVersion(ReadRequest {}))
            .await,
        Ok(PrivdResponse { result: Some(privd_response::Result::OcservVersion(version)), .. })
            if ocservia_agent_protocol::supports_session_terminate(&version.version)
    );
    supported_capabilities()
        .into_iter()
        .filter(|capability| terminate || capability != "ocserv.session.terminate")
        .collect()
}

#[allow(clippy::too_many_lines)]
async fn connect_once(
    endpoint: &Endpoint,
    controller: EndpointAddr,
    session: &mut SessionContext<'_>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let connection = endpoint.connect(controller.clone(), AGENT_ALPN).await?;
    let supported_capabilities = session_capabilities(session.privd).await;
    let handshake = SessionHandshake {
        protocol_major: 1,
        protocol_minor: 1,
        agent_version: ocservia_contracts::agent_upgrade::release_version().to_owned(),
        controller_version: String::new(),
        node_id: session.node_id.as_bytes().to_vec(),
        endpoint_id: session.endpoint_id.as_bytes().to_vec(),
        capabilities: supported_capabilities.clone(),
        ocserv_version: "unknown".to_owned(),
        os_release: session.os_release.to_owned(),
        boot_id: session.boot_id.to_owned(),
        agent_instance_id: session.agent_instance_id.as_bytes().to_vec(),
        supported_compressions: Vec::new(),
        max_message_size: 1024 * 1024,
        time: Some(SystemTime::now().into()),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
        sealing_keys: session.sealing_keys.to_vec(),
    };
    let bytes = handshake.encode_to_vec();
    let response = tokio::time::timeout(HANDSHAKE_TIMEOUT, async {
        let (mut send, mut recv) = connection.open_bi().await?;
        let length = u32::try_from(bytes.len()).map_err(|_| invalid("handshake too large"))?;
        send.write_all(&length.to_be_bytes()).await?;
        send.write_all(&bytes).await?;
        send.finish()?;
        let mut response_length = [0_u8; 4];
        recv.read_exact(&mut response_length).await?;
        let response_length = u32::from_be_bytes(response_length) as usize;
        if response_length == 0 || response_length > 64 * 1024 {
            return Err::<_, Box<dyn std::error::Error + Send + Sync>>(
                invalid("handshake response size invalid").into(),
            );
        }
        let mut response = vec![0_u8; response_length];
        recv.read_exact(&mut response).await?;
        Ok(SessionHandshakeResponse::decode(response.as_slice())?)
    })
    .await
    .map_err(|_| invalid("handshake timed out"))??;
    if response.result != i32::from(HandshakeResult::Accepted) {
        return Err(invalid("controller refused agent handshake").into());
    }
    let session_authority =
        negotiate_and_activate_session(&response, &supported_capabilities, session)?;
    tracing::info!(
        controller = %controller.id,
        session_mode = session_authority.name(),
        negotiated_capabilities = session_authority.grant().negotiated_capabilities.len(),
        "agent session accepted"
    );
    let audit_upload = crate::vpn_audit::upload_loop(connection.clone());
    tokio::pin!(audit_upload);
    let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
    let session_expiry = wait_for_session_expiry(&session_authority);
    tokio::pin!(session_expiry);
    let mut sequence = 0_u64;
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            expiry = &mut session_expiry => {
                expiry?;
                return Err(invalid("Controller session grant expired").into());
            },
            () = &mut audit_upload => return Err(invalid("audit transport closed").into()),
            stream = connection.accept_bi() => {
                let (send, recv) = stream?;
                handle_command_stream(send, recv, session, &session_authority).await?;
            },
            _ = heartbeat.tick() => {
                let sampled=SystemTime::now();
                let observations=session.privd.snapshot().await?;
                // Durable upgrade outcome evidence is best-effort: an older
                // privd without the fixed read simply yields an empty report.
                let upgrade_outcomes = match session.privd.upgrade_results().await {
                    Ok(outcomes) => outcomes,
                    Err(error) => {
                        tracing::warn!(error = %error, "failed to read privd upgrade results");
                        Vec::new()
                    }
                };
                sequence=sequence.saturating_add(1);
                let drops = session.command_executor.journal().telemetry_drop_counters()?;
                let batch = build_telemetry(session, sequence, &observations, upgrade_outcomes, &connection, drops)?;
                crate::vpn_audit::reconcile(session.boot_id.to_owned(), sampled, batch.sessions.clone()).await;
                let payload = batch.encode_to_vec();
                let batch_id: [u8; 16] = batch.batch_id.as_slice().try_into().map_err(|_| invalid("telemetry batch ID invalid"))?;
                let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?.as_secs();
                session.command_executor.journal_mut().enqueue_telemetry(&TelemetryInsert {
                    batch_id: &batch_id,
                    priority: 2,
                    observed_at: i64::try_from(now)?,
                    expires_at: i64::try_from(now + OFFLINE_RECOVERY_RETENTION_SECONDS)?,
                    payload: &payload,
                    now: i64::try_from(now)?,
                    max_bytes: 64 * 1024 * 1024,
                })?;
                let pending_batches = session
                    .command_executor
                    .journal_mut()
                    .telemetry_pending(32, i64::try_from(now)?)?;
                let mut delivered = Vec::with_capacity(pending_batches.len());
                for (pending_id, pending) in pending_batches {
                    if let Err(error) = send_telemetry(&connection, &pending).await {
                        if !delivered.is_empty() {
                            let _ = session
                                .command_executor
                                .journal_mut()
                                .acknowledge_telemetry_batch(&delivered);
                        }
                        return Err(error);
                    }
                    delivered.push(pending_id);
                }
                session
                    .command_executor
                    .journal_mut()
                    .acknowledge_telemetry_batch(&delivered)?;
                tracing::info!(node_id = %session.node_id, sequence, "agent telemetry delivered");
            },
        }
    }
}

fn negotiate_and_activate_session(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    session: &mut SessionContext<'_>,
) -> Result<ActiveSessionAuthority, Box<dyn std::error::Error + Send + Sync>> {
    let authority = negotiate_session_mode(
        response,
        supported_capabilities,
        session.node_id,
        session.endpoint_id,
        session.command_keys,
    )?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    activate_session_connection_fence(
        &authority,
        session.command_executor.journal_mut(),
        session.fence_epoch_floor,
        i64::try_from(now.as_secs())?,
        now.subsec_nanos(),
    )?;
    Ok(authority)
}

/// Builds the production Agent endpoint factory.
///
/// The factory is indirect so tests can pin relay-only transports while
/// production keeps the configured preset, dedicated relay, and relay CA
/// roots.
pub(super) fn agent_endpoint_factory(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    transport: QuicTransportConfig,
    relay_tls_roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> EndpointFactory {
    Box::new(move || {
        let secret_key = secret_key.clone();
        let relay_mode = relay_mode.clone();
        let transport = transport.clone();
        let relay_tls_roots = relay_tls_roots.clone();
        Box::pin(async move {
            with_relay_tls_roots(Endpoint::builder(presets::N0), relay_tls_roots)
                .secret_key(secret_key)
                .relay_mode(relay_mode)
                .clear_address_lookup()
                .dns_resolver(system_dns_resolver())
                .transport_config(transport)
                .bind()
                .await
        })
    })
}

/// The process-lifetime Agent endpoint factory used by the session supervisor.
type EndpointFactory = Box<
    dyn FnMut() -> Pin<Box<dyn Future<Output = Result<Endpoint, iroh::endpoint::BindError>> + Send>>
        + Send,
>;

/// Supervises controller sessions for one Agent process.
///
/// The Iroh Endpoint outlives every controller session and is rebuilt only when
/// it is actually closed, never between sessions.
pub(super) async fn supervise_controller_sessions(
    mut bind_endpoint: EndpointFactory,
    controller: EndpointId,
    relay_mode: &RelayMode,
    session: &mut SessionContext<'_>,
) {
    let mut attempt = 0_u32;
    let backoff = ocservia_agent::Backoff::default();
    let mut endpoint: Option<Endpoint> = None;
    loop {
        if endpoint.as_ref().is_none_or(Endpoint::is_closed) {
            match bind_endpoint().await {
                Ok(bound) => endpoint = Some(bound),
                Err(error) => {
                    tracing::warn!(error = %error, attempt, "agent endpoint creation failed");
                    attempt = attempt.saturating_add(1);
                    let delay = backoff.delay(attempt, &mut rand::rng());
                    tokio::time::sleep(delay).await;
                    continue;
                }
            }
        }
        let endpoint = endpoint.as_ref().expect("agent endpoint is bound");
        let target = controller_dial_target(controller, relay_mode);
        match connect_once(endpoint, target, session).await {
            Ok(()) => attempt = 0,
            Err(error) => {
                tracing::warn!(error = %error, attempt, "controller connection ended");
                attempt = attempt.saturating_add(1);
            }
        }
        let delay = backoff.delay(attempt, &mut rand::rng());
        tokio::time::sleep(delay).await;
    }
}

/// Builds the controller dial target from the configured dedicated relay.
///
/// The `EndpointID` remains the authenticated controller identity. A dedicated
/// relay URL is a safe addressing hint and keeps redial independent of public
/// address discovery.
pub(super) fn controller_dial_target(
    controller: EndpointId,
    relay_mode: &RelayMode,
) -> EndpointAddr {
    let mut address = EndpointAddr::new(controller);
    if let RelayMode::Custom(relays) = relay_mode {
        for relay in relays.urls::<Vec<_>>() {
            address = address.with_relay_url(relay);
        }
    }
    address
}
pub(super) fn negotiate_session_mode(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
) -> Result<ActiveSessionAuthority, Box<dyn std::error::Error + Send + Sync>> {
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    negotiate_session_mode_at_instant(
        response,
        supported_capabilities,
        node_id,
        endpoint_id,
        command_keys,
        i64::try_from(now.as_secs())?,
        now.subsec_nanos(),
    )
}

pub(super) fn negotiate_session_mode_at_instant(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
    now_unix_seconds: i64,
    now_unix_nanos: u32,
) -> Result<ActiveSessionAuthority, Box<dyn std::error::Error + Send + Sync>> {
    if response.protocol_major != 1 || response.protocol_minor != 1 {
        return Err(invalid("Controller session protocol is unsupported").into());
    }
    let supported = supported_capabilities.iter().collect::<HashSet<_>>();
    let mut previous: Option<&str> = None;
    for capability in &response.negotiated_capabilities {
        if !supported.contains(capability)
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(invalid("Controller negotiated capability set is invalid").into());
        }
        previous = Some(capability);
    }
    let grant = response
        .session_grant
        .as_ref()
        .ok_or_else(|| invalid("Controller session grant is required"))?;
    if grant.protocol_major != response.protocol_major
        || grant.protocol_minor != response.protocol_minor
        || grant.negotiated_capabilities != response.negotiated_capabilities
    {
        return Err(invalid("Controller session grant response mismatch").into());
    }
    let VerifiedSessionGrant {
        authorization_revision,
        negotiated_capabilities,
        expires_at_seconds,
    } = command_keys.verify_session_grant(
        grant,
        node_id.as_bytes(),
        endpoint_id.as_bytes(),
        now_unix_seconds,
    )?;
    if negotiated_capabilities != response.negotiated_capabilities {
        return Err(invalid("verified session capabilities do not match response").into());
    }
    let grant = SessionGrantAuthority {
        negotiated_capabilities: negotiated_capabilities.into_iter().collect(),
        authorization_revision,
        expires_at_unix_seconds: expires_at_seconds,
    };
    if !grant
        .negotiated_capabilities
        .contains(CONNECTION_FENCING_CAPABILITY)
    {
        if response.connection_fence.is_some() {
            return Err(invalid("Controller returned an unexpected connection fence").into());
        }
        if grant
            .negotiated_capabilities
            .iter()
            .any(|capability| !is_read_only_session_capability(capability))
        {
            return Err(invalid("Controller mutation session omitted connection fencing").into());
        }
        return Ok(ActiveSessionAuthority::SignedReadOnlyV11(grant));
    }
    let fence = response
        .connection_fence
        .as_ref()
        .ok_or_else(|| invalid("Controller fencing session omitted its connection fence"))?;
    let verified = command_keys
        .verify_connection_fence_v2_at(
            fence,
            node_id.as_bytes(),
            endpoint_id.as_bytes(),
            now_unix_seconds,
            now_unix_nanos,
        )
        .map_err(|_| invalid("Controller connection fence is invalid"))?;
    if verified.authorization_revision != grant.authorization_revision
        || !same_capability_set(&verified.capabilities, &grant.negotiated_capabilities)
    {
        return Err(invalid("Controller connection fence authority mismatch").into());
    }
    Ok(ActiveSessionAuthority::FencedV11 {
        grant,
        connection_fence: verified,
    })
}

/// Verifies and durably records the owner term before a fenced session can
/// become active. Every accepted handshake is a replacement connection, so
/// its epoch must be strictly newer than the Agent's retained high-water mark.
pub(super) fn activate_session_connection_fence(
    authority: &ActiveSessionAuthority,
    journal: &mut Journal,
    fence_epoch_floor: &mut u64,
    now_unix_seconds: i64,
    now_unix_nanos: u32,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let verified = match authority {
        ActiveSessionAuthority::SignedReadOnlyV11(_) => return Ok(()),
        ActiveSessionAuthority::FencedV11 {
            connection_fence, ..
        } => connection_fence,
    };
    if verified.lease_expired(now_unix_seconds, now_unix_nanos) {
        return Err(invalid("Controller connection fence lease has expired").into());
    }
    if (verified.expires_at_seconds, verified.expires_at_nanos)
        <= (now_unix_seconds, now_unix_nanos)
    {
        return Err(invalid("Controller connection fence proof has expired").into());
    }
    refresh_fence_epoch_floor(journal, fence_epoch_floor)?;
    if verified.owner_epoch <= *fence_epoch_floor {
        return Err(invalid("Controller connection fence epoch is not newer").into());
    }
    let durable_floor = journal.raise_owner_fence_epoch_floor(verified.owner_epoch)?;
    if durable_floor != verified.owner_epoch {
        *fence_epoch_floor = durable_floor;
        return Err(invalid("Controller connection fence epoch is not newer").into());
    }
    *fence_epoch_floor = durable_floor;
    tracing::info!(
        owner_epoch = verified.owner_epoch,
        "connection-owner fencing epoch floor raised before session activation"
    );
    Ok(())
}

fn same_capability_set(capabilities: &[String], expected: &HashSet<String>) -> bool {
    capabilities.len() == expected.len()
        && capabilities
            .iter()
            .all(|capability| expected.contains(capability))
}

pub(super) fn refresh_fence_epoch_floor(
    journal: &Journal,
    fence_epoch_floor: &mut u64,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let durable_floor = journal.owner_fence_epoch_floor()?;
    *fence_epoch_floor = (*fence_epoch_floor).max(durable_floor);
    Ok(())
}

fn unix_seconds() -> Result<i64, Box<dyn std::error::Error + Send + Sync>> {
    Ok(i64::try_from(
        SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)?
            .as_secs(),
    )?)
}

pub(super) enum FenceDecision {
    RejectedStaleOwnerEpoch,
}

fn same_fence_term(left: &VerifiedConnectionFenceV2, right: &VerifiedConnectionFenceV2) -> bool {
    left.fence_id == right.fence_id
        && left.owner_instance_id == right.owner_instance_id
        && left.owner_incarnation == right.owner_incarnation
        && left.owner_epoch == right.owner_epoch
        && left.connection_id == right.connection_id
        && left.authorization_revision == right.authorization_revision
}

/// Classifies a deliberately signed, read-only stale-owner diagnostic.
/// This path can report only a valid fence below the durable floor; it never
/// activates an owner term or authorizes a non-stale command.
#[allow(clippy::too_many_arguments)]
pub(super) fn gate_signed_read_only_stale_diagnostic(
    command_keys: &ControllerCommandKeyring,
    fence_epoch_floor: u64,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    fence: &ConnectionFenceV2,
    command_id: &[u8],
    now_unix_seconds: i64,
    now_unix_nanos: u32,
) -> Result<FenceDecision, Box<dyn std::error::Error + Send + Sync>> {
    let verified = command_keys
        .verify_connection_fence_v2_at(
            fence,
            node_id.as_bytes(),
            endpoint_id.as_bytes(),
            now_unix_seconds,
            now_unix_nanos,
        )
        .map_err(|error| {
            tracing::warn!(
                command_id = %hex::encode(command_id),
                error = %error,
                "command connection fence is invalid"
            );
            invalid("command connection fence is invalid")
        })?;
    if verified.lease_expired(now_unix_seconds, now_unix_nanos) {
        tracing::warn!(
            command_id = %hex::encode(command_id),
            owner_epoch = verified.owner_epoch,
            "command connection-owner lease has expired"
        );
        return Err(invalid("command connection-owner lease has expired").into());
    }
    if verified.owner_epoch < fence_epoch_floor {
        tracing::warn!(
            command_id = %hex::encode(command_id),
            owner_epoch = verified.owner_epoch,
            floor = fence_epoch_floor,
            "command from a stale connection-owner epoch rejected"
        );
        return Ok(FenceDecision::RejectedStaleOwnerEpoch);
    }
    Err(invalid("signed read-only session cannot authorize a non-stale command").into())
}

#[allow(clippy::too_many_arguments)]
pub(super) fn verify_fenced_operation(
    command_keys: &ControllerCommandKeyring,
    grant: &SessionGrantAuthority,
    active_fence: &VerifiedConnectionFenceV2,
    fence_epoch_floor: u64,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    fence: Option<&ConnectionFenceV2>,
    binding: Option<&FenceBindingV2>,
    operation_kind: FenceOperationKind,
    operation_id: &[u8],
    capability: &str,
    now_unix_seconds: i64,
    now_unix_nanos: u32,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let fence = fence.ok_or_else(|| invalid("fenced operation omitted its connection fence"))?;
    let binding = binding.ok_or_else(|| invalid("fenced operation omitted its fence binding"))?;
    let verified = command_keys
        .verify_connection_fence_v2_at(
            fence,
            node_id.as_bytes(),
            endpoint_id.as_bytes(),
            now_unix_seconds,
            now_unix_nanos,
        )
        .map_err(|_| invalid("operation connection fence is invalid"))?;
    if verified.lease_expired(now_unix_seconds, now_unix_nanos) {
        return Err(invalid("operation connection-owner lease has expired").into());
    }
    if active_fence.owner_epoch != fence_epoch_floor
        || verified.owner_epoch != fence_epoch_floor
        || !same_fence_term(&verified, active_fence)
        || verified.authorization_revision != grant.authorization_revision
        || !same_capability_set(&verified.capabilities, &grant.negotiated_capabilities)
    {
        return Err(invalid("operation fence does not match the active session term").into());
    }
    let claims: FenceBindingClaimsV2 = command_keys
        .verify_fence_binding_v2_at(
            binding,
            node_id.as_bytes(),
            endpoint_id.as_bytes(),
            now_unix_seconds,
            now_unix_nanos,
        )
        .map_err(|_| invalid("operation fence binding is invalid"))?;
    if claims.operation_kind != operation_kind as u32
        || claims.operation_id.as_slice() != operation_id
        || claims.capability != capability
        || !grant.negotiated_capabilities.contains(capability)
        || !claims.matches_fence(&verified)
    {
        return Err(invalid("operation fence binding does not match its carrier").into());
    }
    Ok(())
}
