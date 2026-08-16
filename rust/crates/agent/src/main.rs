use std::collections::HashSet;
use std::env;
use std::io;
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use iroh::endpoint::{QuicTransportConfig, RelayMode, VarInt, presets};
use iroh::{Endpoint, EndpointAddr, EndpointId, RelayMap, RelayUrl, Watcher as _};
use ocservia_agent::{
    CommandContext, CommandError, CommandExecutor, ExternalEffectObservation, ExternalPreparation,
    MAX_COMMAND_BYTES, MAX_WRITE_QUEUE, PrivdClient,
};
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, DesiredEffectState, ErrorKind,
    MAX_MANAGED_RESOURCES, PrivdResponse, privd_request, privd_response,
};
use ocservia_command_authorization::{
    ControllerCommandKeyring, VerifiedSessionGrant, load_verification_key,
};
use ocservia_command_journal::{
    CommandRecord, CommandState, Journal, OFFLINE_RECOVERY_RETENTION_SECONDS, TelemetryInsert,
};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, ArtifactChunk,
    ArtifactConsumeRequest as ArtifactConsumeFinalizeRequest,
    ArtifactConsumeResponse as ArtifactConsumeFinalizeResponse, ArtifactFetchRequest,
    CommandDeliveryMode, CommandEnvelope, CommandResult, CommandResultState, ConnectionFenceV2,
    EnrollRequest, EnrollResponse, GroupObservation, HandshakeResult, IpBanObservation,
    MetricSample, ObservedSnapshot, PrivdReceiptVersion, PrivilegedResultProof,
    SealedSecretPurpose, SealedSecretVersion, SealingKeyDescriptorV1, SemanticPayloadHashVersion,
    SessionHandshake, SessionHandshakeResponse, SessionObservation, TelemetryBatch,
    TelemetryDropCounters, TelemetryPriority, UserObservation, command_envelope,
};
use ocservia_contracts::session::{
    READ_ONLY_SESSION_CAPABILITIES, is_read_only_session_capability,
};
use prost::Message;
use rustls_pki_types::pem::PemObject as _;
use sha2::Digest;
use uuid::Uuid;

use zeroize::Zeroizing;

const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";
const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const ARTIFACT_FRAME_MASK: u32 = 3 << 30;
const ARTIFACT_FETCH_FRAME: u32 = 1 << 31;
const ARTIFACT_CONSUME_FRAME: u32 = 3 << 30;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    ocservia_agent::ensure_unprivileged(rustix::process::geteuid().as_raw())?;
    let config = parse_args()?;
    if prepare_enrollment_if_requested(&config)? {
        return Ok(());
    }
    ocservia_observability::init("ocservia-agent")?;
    if run_one_shot_mode(&config).await? {
        return Ok(());
    }
    let command_keys = load_controller_command_keys(&config)?;
    let relay_tls_roots = std::sync::Arc::new(load_relay_tls_roots(&config)?);
    let mut journal = Journal::open(&config.journal)?;
    let mut command_executor = CommandExecutor::new(Journal::open(&config.journal)?);
    let privd = PrivdClient::new(config.privd_socket, Duration::from_secs(5))?;
    // The startup snapshot is a health gate: it fails fast when privd or the
    // fixed-path fixtures are unhealthy, before any command dispatch begins.
    require_healthy_snapshot(privd.snapshot().await?)?;
    if config.probe_only {
        return Ok(());
    }

    let command_keys = command_keys.expect("non-probe Agent requires command keys");
    let mut fence_epoch_floor = journal.owner_fence_epoch_floor()?;
    tracing::info!(
        fence_epoch_floor,
        "loaded connection-owner fencing epoch floor"
    );
    if let Some(stats_file) = config.stats_file.clone() {
        ocservia_observability::spawn_runtime_stats_writer(stats_file)?;
    }

    let controller = config
        .controller
        .ok_or_else(|| invalid("--controller is required"))?;
    let node_id = config
        .node_id
        .ok_or_else(|| invalid("--node-id is required"))?;
    let identity = ocservia_agent_identity::Identity::provision(&config.identity_dir, controller)?;
    let transport = QuicTransportConfig::builder()
        .max_concurrent_bidi_streams(VarInt::from_u32(u32::try_from(MAX_WRITE_QUEUE)?))
        .build();
    // The CLI receives the controller's stable endpoint ID, so the production
    // endpoint must retain N0 address discovery instead of requiring an
    // out-of-band direct or relay address.
    let boot_id = ocservia_agent::read_boot_id().await?;
    let os_release = ocservia_agent::read_os_release().await?;
    let agent_instance_id = Uuid::now_v7();
    let run = async {
        let mut attempt = 0_u32;
        let backoff = ocservia_agent::Backoff::default();
        loop {
            let endpoint = match with_relay_tls_roots(
                Endpoint::builder(presets::N0),
                relay_tls_roots.as_ref().clone(),
            )
            .secret_key(identity.secret_key().clone())
            .relay_mode(config.relay_mode.clone())
            .transport_config(transport.clone())
            .bind()
            .await
            {
                Ok(endpoint) => endpoint,
                Err(error) => {
                    tracing::warn!(error = %error, attempt, "agent endpoint creation failed");
                    attempt = attempt.saturating_add(1);
                    let delay = backoff.delay(attempt, &mut rand::rng());
                    tokio::time::sleep(delay).await;
                    continue;
                }
            };
            spawn_dedicated_relay_failover(&endpoint, &config.relay_mode);
            let mut session = SessionContext {
                node_id,
                endpoint_id: identity.endpoint_id(),
                privd: &privd,
                journal: &mut journal,
                command_executor: &mut command_executor,
                boot_id: &boot_id,
                os_release: &os_release,
                agent_instance_id,
                command_keys: &command_keys,
                sealing_keys: &config.sealing_keys,
                fence_epoch_floor: &mut fence_epoch_floor,
                synthetic_barrier_file: config.synthetic_barrier_file.as_deref(),
            };
            match connect_once(&endpoint, EndpointAddr::new(controller), &mut session).await {
                Ok(()) => attempt = 0,
                Err(error) => {
                    tracing::warn!(error = %error, attempt, "controller connection ended");
                    attempt = attempt.saturating_add(1);
                }
            }
            endpoint.close().await;
            let delay = backoff.delay(attempt, &mut rand::rng());
            tokio::time::sleep(delay).await;
        }
    };
    tokio::select! {
        () = run => {}
        () = shutdown_signal() => tracing::info!("agent shutdown requested"),
    }
    Ok(())
}

/// Fails closed unless every read-only privd snapshot probe succeeded, so an
/// Agent never reports a healthy session against a broken supervisor.
fn require_healthy_snapshot(
    observations: Vec<PrivdResponse>,
) -> Result<Vec<PrivdResponse>, Box<dyn std::error::Error + Send + Sync>> {
    let failures = observations
        .iter()
        .filter_map(|response| match &response.result {
            Some(privd_response::Result::Error(error)) => Some(error.detail.as_str()),
            None => Some("missing privd response result"),
            _ => None,
        })
        .collect::<Vec<_>>();
    if !failures.is_empty() {
        return Err(invalid(&format!(
            "privd read-only snapshot failed: {}",
            failures.join("; ")
        ))
        .into());
    }
    tracing::info!(
        observations = observations.len(),
        "initial read-only snapshot collected"
    );
    Ok(observations)
}

fn prepare_enrollment_if_requested(config: &Config) -> Result<bool, io::Error> {
    if !config.prepare_enrollment {
        return Ok(false);
    }
    println!("{}", prepare_enrollment(config)?);
    Ok(true)
}

async fn run_one_shot_mode(
    config: &Config,
) -> Result<bool, Box<dyn std::error::Error + Send + Sync>> {
    if let Some(token_file) = config.enrollment_token_file.as_deref() {
        enroll_agent(config, token_file).await?;
        return Ok(true);
    }
    Ok(false)
}

fn prepare_enrollment(config: &Config) -> Result<String, io::Error> {
    let controller = config
        .controller
        .ok_or_else(|| invalid("--controller is required for enrollment preparation"))?;
    let identity = ocservia_agent_identity::Identity::provision(&config.identity_dir, controller)?;
    Ok(hex::encode(identity.endpoint_id().as_bytes()))
}

async fn enroll_agent(
    config: &Config,
    token_file: &Path,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let controller = config
        .controller
        .ok_or_else(|| invalid("--controller is required for enrollment"))?;
    let environment = config
        .enrollment_environment
        .as_deref()
        .ok_or_else(|| invalid("--enrollment-environment is required"))?;
    let token = read_enrollment_token(token_file)?;
    let identity = ocservia_agent_identity::Identity::provision(&config.identity_dir, controller)?;
    let endpoint = with_relay_tls_roots(
        Endpoint::builder(presets::N0),
        load_relay_tls_roots(config)?,
    )
    .secret_key(identity.secret_key().clone())
    .relay_mode(config.relay_mode.clone())
    .bind()
    .await?;
    spawn_dedicated_relay_failover(&endpoint, &config.relay_mode);
    let connection = endpoint
        .connect(EndpointAddr::new(controller), ENROLL_ALPN)
        .await?;
    let mut request = EnrollRequest {
        token,
        endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
        agent_version: env!("CARGO_PKG_VERSION").to_owned(),
        os_release: ocservia_agent::read_os_release().await?,
        ocserv_version: "unknown".to_owned(),
        boot_id: ocservia_agent::read_boot_id().await?,
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
        capabilities: supported_capabilities(),
        environment: environment.to_owned(),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
        time: Some(SystemTime::now().into()),
        enrollment_protocol_major: 0,
        enrollment_protocol_minor: 0,
        proof: None,
        sealing_keys: config.sealing_keys.clone(),
    };
    identity.authorize_enrollment(&mut request)?;
    let response: EnrollResponse = exchange_message(&connection, &request, "enrollment").await?;
    let node_id = Uuid::from_slice(&response.node_id)
        .map_err(|_| invalid("Controller returned an invalid enrollment node ID"))?;
    if node_id.get_version_num() != 7
        || !matches!(
            HandshakeResult::try_from(response.result),
            Ok(HandshakeResult::PendingApproval | HandshakeResult::Accepted)
        )
    {
        return Err(invalid("Controller rejected enrollment").into());
    }
    println!("{node_id}");
    connection.close(VarInt::from_u32(0), b"enrollment complete");
    endpoint.close().await;
    Ok(())
}

async fn exchange_message<M: Message, R: Message + Default>(
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

fn spawn_dedicated_relay_failover(endpoint: &Endpoint, mode: &RelayMode) {
    let RelayMode::Custom(configured) = mode else {
        return;
    };
    if configured.len() < 2 {
        return;
    }
    let endpoint = endpoint.clone();
    let configured = configured.clone();
    tokio::spawn(async move {
        let mut watcher = endpoint.home_relay_status();
        loop {
            let failed = watcher
                .get()
                .into_iter()
                .find(|status| !status.is_connected() && status.last_error().is_some())
                .map(|status| status.url().clone());
            if let Some(failed) = failed
                && let Some(config) = configured.get(&failed)
                && endpoint.remove_relay(&failed).await.is_some()
            {
                tracing::warn!(relay = %failed, "temporarily removed failed dedicated relay");
                tokio::time::sleep(Duration::from_mins(1)).await;
                let _ = endpoint.insert_relay(failed, config).await;
            }
            if watcher.updated().await.is_err() {
                return;
            }
        }
    });
}

struct SessionContext<'a> {
    node_id: Uuid,
    endpoint_id: EndpointId,
    privd: &'a PrivdClient,
    journal: &'a mut Journal,
    command_executor: &'a mut CommandExecutor,
    boot_id: &'a str,
    os_release: &'a str,
    agent_instance_id: Uuid,
    command_keys: &'a ControllerCommandKeyring,
    sealing_keys: &'a [SealingKeyDescriptorV1],
    fence_epoch_floor: &'a mut u64,
    synthetic_barrier_file: Option<&'a Path>,
}

struct ActiveSessionAuthority {
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    expires_at_unix_seconds: i64,
}

enum AgentSessionMode {
    ReadOnly {
        negotiated_capabilities: HashSet<String>,
    },
    AuthorizedV11(ActiveSessionAuthority),
}

impl AgentSessionMode {
    fn name(&self) -> &'static str {
        match self {
            Self::ReadOnly { .. } => "read_only_v1_0",
            Self::AuthorizedV11(_) => "authorized_v1_1",
        }
    }

    fn capability_count(&self) -> usize {
        match self {
            Self::ReadOnly {
                negotiated_capabilities,
            } => negotiated_capabilities.len(),
            Self::AuthorizedV11(authority) => authority.negotiated_capabilities.len(),
        }
    }

    fn expires_at_unix_seconds(&self) -> Option<i64> {
        match self {
            Self::ReadOnly { .. } => None,
            Self::AuthorizedV11(authority) => Some(authority.expires_at_unix_seconds),
        }
    }
}

async fn wait_for_session_expiry(
    session_mode: &AgentSessionMode,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    if let Some(expires_at) = session_mode.expires_at_unix_seconds() {
        let lifetime = u64::try_from(expires_at.saturating_sub(unix_seconds()?))?;
        tokio::time::sleep(Duration::from_secs(lifetime)).await;
    } else {
        std::future::pending::<()>().await;
    }
    Ok(())
}

fn supported_capabilities() -> Vec<String> {
    READ_ONLY_SESSION_CAPABILITIES
        .iter()
        .copied()
        .chain([
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
            "ocserv.config.plan",
            "ocserv.config.apply",
            "config.auth",
            "config.tls",
            "config.sessions",
            "config.network",
            "config.limits",
            "config.runtime",
            "ocserv.certificate.issue",
            "ocserv.certificate.revoke",
        ])
        .map(str::to_owned)
        .collect()
}

async fn connect_once(
    endpoint: &Endpoint,
    controller: EndpointAddr,
    session: &mut SessionContext<'_>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let connection = endpoint.connect(controller.clone(), AGENT_ALPN).await?;
    let supported_capabilities = supported_capabilities();
    let handshake = SessionHandshake {
        protocol_major: 1,
        protocol_minor: 1,
        agent_version: env!("CARGO_PKG_VERSION").to_owned(),
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
    let session_mode = negotiate_session_mode(
        &response,
        &supported_capabilities,
        session.node_id,
        session.endpoint_id,
        session.command_keys,
    )?;
    tracing::info!(
        controller = %controller.id,
        session_mode = session_mode.name(),
        negotiated_capabilities = session_mode.capability_count(),
        "agent session accepted"
    );
    let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
    let session_expiry = wait_for_session_expiry(&session_mode);
    tokio::pin!(session_expiry);
    let mut sequence = 0_u64;
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            expiry = &mut session_expiry => {
                expiry?;
                return Err(invalid("Controller session grant expired").into());
            },
            stream = connection.accept_bi() => {
                let (send,recv)=stream?;
                let AgentSessionMode::AuthorizedV11(authority) = &session_mode else {
                    connection.close(VarInt::from_u32(0x107), b"read-only session stream denied");
                    return Err(invalid("read-only session received a command or artifact stream").into());
                };
                handle_command_stream(send,recv,session,authority).await?;
            },
            _ = heartbeat.tick() => {
                let observations=session.privd.snapshot().await?;
                sequence=sequence.saturating_add(1);
                let drops=session.journal.telemetry_drop_counters()?;
                let batch=build_telemetry(session,sequence,&observations,&connection,drops)?;
                let payload=batch.encode_to_vec();
                let batch_id: [u8;16]=batch.batch_id.as_slice().try_into().map_err(|_| invalid("telemetry batch ID invalid"))?;
                let now=SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?.as_secs();
                session.journal.enqueue_telemetry(&TelemetryInsert { batch_id: &batch_id, priority: 2, observed_at: i64::try_from(now)?, expires_at: i64::try_from(now+OFFLINE_RECOVERY_RETENTION_SECONDS)?, payload: &payload, now: i64::try_from(now)?, max_bytes: 64*1024*1024 })?;
                for (pending_id,pending) in session.journal.telemetry_pending(32, i64::try_from(now)?)? {
                    send_telemetry(&connection,&pending).await?;
                    session.journal.acknowledge_telemetry(&pending_id)?;
                }
                tracing::info!(node_id = %session.node_id, sequence, "agent telemetry delivered");
            },
        }
    }
}

fn negotiate_session_mode(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
) -> Result<AgentSessionMode, Box<dyn std::error::Error + Send + Sync>> {
    if response.protocol_major != 1 || response.protocol_minor > 1 {
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
    if response.protocol_minor == 0 {
        if response.session_grant.is_some()
            || response
                .negotiated_capabilities
                .iter()
                .any(|capability| !is_read_only_session_capability(capability))
        {
            return Err(invalid("Controller read-only session is invalid").into());
        }
        return Ok(AgentSessionMode::ReadOnly {
            negotiated_capabilities: response.negotiated_capabilities.iter().cloned().collect(),
        });
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
    let now = unix_seconds()?;
    let VerifiedSessionGrant {
        authorization_revision,
        negotiated_capabilities,
        expires_at_seconds,
    } = command_keys.verify_session_grant(
        grant,
        node_id.as_bytes(),
        endpoint_id.as_bytes(),
        now,
    )?;
    if negotiated_capabilities != response.negotiated_capabilities {
        return Err(invalid("verified session capabilities do not match response").into());
    }
    Ok(AgentSessionMode::AuthorizedV11(ActiveSessionAuthority {
        negotiated_capabilities: negotiated_capabilities.into_iter().collect(),
        authorization_revision,
        expires_at_unix_seconds: expires_at_seconds,
    }))
}

fn unix_seconds() -> Result<i64, Box<dyn std::error::Error + Send + Sync>> {
    Ok(i64::try_from(
        SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)?
            .as_secs(),
    )?)
}

enum FenceDecision {
    Accepted,
    RejectedStaleOwnerEpoch,
}

/// Verifies a fenced command's connection-owner fence against the durable
/// per-Agent epoch floor. A fence carrying an epoch below the highest epoch
/// this Agent already accepted belongs to a superseded owner term: the
/// command is rejected without side effects, and the floor is never lowered.
#[allow(clippy::too_many_arguments)]
fn gate_connection_fence(
    command_keys: &ControllerCommandKeyring,
    journal: &mut Journal,
    fence_epoch_floor: &mut u64,
    node_id: &Uuid,
    endpoint_id: &EndpointId,
    fence: &ConnectionFenceV2,
    command_id: &[u8],
    now_unix_seconds: i64,
) -> Result<FenceDecision, Box<dyn std::error::Error + Send + Sync>> {
    let verified = command_keys
        .verify_connection_fence_v2(
            fence,
            node_id.as_bytes(),
            endpoint_id.as_bytes(),
            now_unix_seconds,
        )
        .map_err(|error| {
            tracing::warn!(
                command_id = %hex::encode(command_id),
                error = %error,
                "command connection fence is invalid"
            );
            invalid("command connection fence is invalid")
        })?;
    if verified.owner_epoch < *fence_epoch_floor {
        tracing::warn!(
            command_id = %hex::encode(command_id),
            owner_epoch = verified.owner_epoch,
            floor = *fence_epoch_floor,
            "command from a stale connection-owner epoch rejected"
        );
        return Ok(FenceDecision::RejectedStaleOwnerEpoch);
    }
    if verified.owner_epoch > *fence_epoch_floor {
        journal.raise_owner_fence_epoch_floor(verified.owner_epoch)?;
        *fence_epoch_floor = verified.owner_epoch;
        tracing::info!(
            owner_epoch = verified.owner_epoch,
            "connection-owner fencing epoch floor raised"
        );
    }
    Ok(FenceDecision::Accepted)
}

#[allow(clippy::too_many_lines)]
async fn handle_command_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    session: &mut SessionContext<'_>,
    authority: &ActiveSessionAuthority,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut length = [0_u8; 4];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| invalid("command length timed out"))??;
    let framed_length = u32::from_be_bytes(length);
    if framed_length & ARTIFACT_FRAME_MASK == ARTIFACT_FETCH_FRAME {
        return handle_artifact_stream(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.command_keys,
            session.privd,
        )
        .await;
    }
    if framed_length & ARTIFACT_FRAME_MASK == ARTIFACT_CONSUME_FRAME {
        return handle_artifact_consume(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.command_keys,
            session.privd,
        )
        .await;
    }
    if framed_length & ARTIFACT_FRAME_MASK != 0 {
        return Err(invalid("command frame kind invalid").into());
    }
    let length = framed_length as usize;
    if length == 0 || length > MAX_COMMAND_BYTES {
        return Err(invalid("command size invalid").into());
    }
    let mut bytes = vec![0_u8; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| invalid("command body timed out"))??;
    let envelope = decode_strict_command_envelope(bytes.as_slice())?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    let stale_owner = match envelope.connection_fence.as_ref() {
        Some(fence) => matches!(
            gate_connection_fence(
                session.command_keys,
                session.journal,
                session.fence_epoch_floor,
                &session.node_id,
                &session.endpoint_id,
                fence,
                &envelope.command_id,
                now_unix_seconds,
            )?,
            FenceDecision::RejectedStaleOwnerEpoch
        ),
        None => false,
    };
    if stale_owner {
        let result = rejected_result(&envelope, "stale_owner_epoch", now_unix_seconds);
        let event = AgentEvent {
            r#type: AgentEventType::CommandResult.into(),
            payload: result.encode_to_vec(),
        };
        let encoded = event.encode_to_vec();
        send.write_all(&u32::try_from(encoded.len())?.to_be_bytes())
            .await?;
        send.write_all(&encoded).await?;
        send.finish()?;
        return Ok(());
    }
    let context = CommandContext {
        node_id: *session.node_id.as_bytes(),
        authorization_revision: authority.authorization_revision,
        capabilities: authority.negotiated_capabilities.clone(),
        session_expires_at_unix_seconds: authority.expires_at_unix_seconds,
        command_keys: session.command_keys.clone(),
        now_unix_seconds,
        cancelled: false,
    };
    if matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::SyntheticNoop(_)
                | command_envelope::Payload::SyntheticEcho(_)
        )
    ) {
        wait_for_synthetic_barrier(session.synthetic_barrier_file).await;
    }
    let external = matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::SessionDisconnect(_)
                | command_envelope::Payload::SessionTerminate(_)
                | command_envelope::Payload::IpBanRemove(_)
                | command_envelope::Payload::ServiceReload(_)
                | command_envelope::Payload::UserCreate(_)
                | command_envelope::Payload::UserDisable(_)
                | command_envelope::Payload::UserEnable(_)
                | command_envelope::Payload::UserPasswordRotate(_)
                | command_envelope::Payload::GroupApply(_)
                | command_envelope::Payload::ConfigPlan(_)
                | command_envelope::Payload::ConfigApply(_)
                | command_envelope::Payload::CertificateCsr(_)
                | command_envelope::Payload::CertificateRevoke(_)
                | command_envelope::Payload::CertificateP12(_)
        )
    );
    let execution = if external {
        execute_external_command(session, &envelope, &context, now_unix_seconds).await
    } else {
        session.command_executor.deliver(&envelope, &context)
    };
    let result = match execution {
        Ok(outcome) => command_result(&outcome.record, outcome.replayed),
        Err(CommandError::Rejected(code)) => rejected_result(&envelope, code, now_unix_seconds),
        Err(CommandError::IdentityConflict) => {
            rejected_result(&envelope, "command_identity_conflict", now_unix_seconds)
        }
        Err(CommandError::PayloadConflict) => {
            rejected_result(&envelope, "idempotency_payload_conflict", now_unix_seconds)
        }
        Err(CommandError::PreEffectJournalFailure(error)) => {
            tracing::error!(command_id = %hex::encode(&envelope.command_id), error = %error, "command journal failure");
            rejected_result(&envelope, "journal_unavailable", now_unix_seconds)
        }
        Err(CommandError::OutcomeUnknown {
            code,
            record,
            source,
        }) => {
            tracing::error!(command_id = %hex::encode(&envelope.command_id), error = %source, "command outcome requires reconciliation");
            unknown_result(record.as_ref(), code)
        }
        Err(CommandError::InjectedCrash(_)) => unreachable!("crash injection is test-only"),
    };
    let event = AgentEvent {
        r#type: AgentEventType::CommandResult.into(),
        payload: result.encode_to_vec(),
    };
    let encoded = event.encode_to_vec();
    send.write_all(&u32::try_from(encoded.len())?.to_be_bytes())
        .await?;
    send.write_all(&encoded).await?;
    send.finish()?;
    Ok(())
}

async fn wait_for_synthetic_barrier(path: Option<&Path>) {
    let Some(path) = path else {
        return;
    };
    while tokio::fs::try_exists(path).await.unwrap_or(true) {
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn handle_artifact_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
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
    command_keys.verify_artifact_grant(
        grant,
        node_id.as_bytes(),
        artifact.as_bytes(),
        &request.purpose,
        request.max_bytes,
        unix_seconds()?,
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

async fn handle_artifact_consume(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    node_id: Uuid,
    command_keys: &ControllerCommandKeyring,
    privd: &PrivdClient,
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
    if artifact.get_version_num() != 7
        || request.sha256.len() != 32
        || request.size == 0
        || request.size != grant.max_bytes
        || grant.purpose != "certificate_p12"
    {
        return Err(invalid("artifact finalize request invalid").into());
    }
    if request.confirm_only {
        command_keys.verify_artifact_grant_for_confirmation(
            grant,
            node_id.as_bytes(),
            artifact.as_bytes(),
            "certificate_p12",
            request.size,
            unix_seconds()?,
        )?;
    } else {
        command_keys.verify_artifact_grant(
            grant,
            node_id.as_bytes(),
            artifact.as_bytes(),
            "certificate_p12",
            request.size,
            unix_seconds()?,
        )?;
    }
    let response = privd
        .call(privd_request::Operation::ArtifactConsume(
            ArtifactConsumeRequest {
                grant: Some(grant.clone()),
                sha256: request.sha256,
                size: request.size,
                confirm_only: request.confirm_only,
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

#[allow(clippy::too_many_lines)]
async fn execute_external_command(
    session: &mut SessionContext<'_>,
    envelope: &CommandEnvelope,
    context: &CommandContext,
    now: i64,
) -> Result<ocservia_agent::CommandOutcome, CommandError> {
    let mode = CommandDeliveryMode::try_from(envelope.delivery_mode)
        .unwrap_or(CommandDeliveryMode::Unspecified);
    let (command, reconciled_response) = match mode {
        CommandDeliveryMode::ExecuteOrReplay => {
            match session
                .command_executor
                .prepare_external(envelope, context)?
            {
                ExternalPreparation::Execute(command) => (command, None),
                ExternalPreparation::Replay(outcome) => return Ok(outcome),
            }
        }
        CommandDeliveryMode::ReconcileOnly => {
            let response = session.privd.call_reconcile(envelope).await;
            if response.as_ref().ok().and_then(response_proof).is_some() {
                let command = session
                    .command_executor
                    .begin_attested_recovery(envelope, context)?;
                (command, Some(response))
            } else {
                let observed = observe_external_effect(session.privd, envelope).await;
                return session
                    .command_executor
                    .reconcile_external(envelope, context, observed);
            }
        }
        CommandDeliveryMode::RetryIfEffectAbsent => (
            session.command_executor.retry_external(envelope, context)?,
            None,
        ),
        CommandDeliveryMode::Unspecified => {
            return Err(CommandError::Rejected("delivery_mode_invalid"));
        }
    };
    let response = match reconciled_response {
        Some(response) => response,
        None => {
            session
                .privd
                .call_command(envelope, command.accepted_at())
                .await
        }
    };
    match response {
        Ok(response) => {
            let Some((proof, completed_at)) = response_proof(&response) else {
                return session.command_executor.mark_external_unknown(
                    &command,
                    "privd_receipt_missing_or_malformed",
                    now,
                );
            };
            match response.result {
                Some(privd_response::Result::Mutation(result)) if result.applied => {
                    let result = result.encode_to_vec();
                    if let Some((resource_type, resource_key, revision)) =
                        desired_resource(envelope)
                    {
                        session.command_executor.complete_external_applied_attested(
                            &command,
                            resource_type,
                            &resource_key,
                            revision,
                            &result,
                            &proof,
                            completed_at,
                        )
                    } else {
                        session.command_executor.complete_external_attested(
                            &command,
                            Ok(&result),
                            &proof,
                            completed_at,
                        )
                    }
                }
                Some(privd_response::Result::ConfigPlan(result)) => {
                    session.command_executor.complete_external_attested(
                        &command,
                        Ok(&result.encode_to_vec()),
                        &proof,
                        completed_at,
                    )
                }
                Some(privd_response::Result::ConfigApply(result)) => {
                    let bytes = result.encode_to_vec();
                    let Some((resource_type, resource_key, revision)) = desired_resource(envelope)
                    else {
                        return Err(CommandError::Rejected("config_apply_invalid"));
                    };
                    session.command_executor.complete_external_applied_attested(
                        &command,
                        resource_type,
                        &resource_key,
                        revision,
                        &bytes,
                        &proof,
                        completed_at,
                    )
                }
                Some(privd_response::Result::CertificateCsr(result)) => {
                    session.command_executor.complete_external_attested(
                        &command,
                        Ok(&result.encode_to_vec()),
                        &proof,
                        completed_at,
                    )
                }
                Some(privd_response::Result::CertificateRevoke(result)) => {
                    session.command_executor.complete_external_attested(
                        &command,
                        Ok(&result.encode_to_vec()),
                        &proof,
                        completed_at,
                    )
                }
                Some(privd_response::Result::CertificateP12(result)) => {
                    session.command_executor.complete_external_attested(
                        &command,
                        Ok(&result.encode_to_vec()),
                        &proof,
                        completed_at,
                    )
                }
                Some(privd_response::Result::Error(error)) if terminal_privd_error(error.kind) => {
                    let code = terminal_privd_error_code(error.kind).unwrap_or("privd_rejected");
                    session.command_executor.complete_external_attested(
                        &command,
                        Err(code),
                        &proof,
                        completed_at,
                    )
                }
                _ => session.command_executor.mark_external_unknown(
                    &command,
                    "privd_outcome_unknown",
                    now,
                ),
            }
        }
        Err(_) => {
            session
                .command_executor
                .mark_external_unknown(&command, "privd_transport_unknown", now)
        }
    }
}

fn response_proof(response: &PrivdResponse) -> Option<(Vec<u8>, i64)> {
    let proof = response.privileged_result_proof.as_ref()?;
    if PrivdReceiptVersion::try_from(proof.version).ok()? != PrivdReceiptVersion::V1
        || proof.signature.len() != 64
    {
        return None;
    }
    let receipt = proof.receipt_v1.as_ref()?;
    if PrivdReceiptVersion::try_from(receipt.receipt_version).ok()? != PrivdReceiptVersion::V1
        || receipt.completed_at.as_ref()?.seconds < 0
    {
        return None;
    }
    let encoded = proof.encode_to_vec();
    (encoded.len() <= 65_536).then_some((encoded, receipt.completed_at.as_ref()?.seconds))
}

fn terminal_privd_error(kind: i32) -> bool {
    terminal_privd_error_code(kind).is_some()
}

fn terminal_privd_error_code(kind: i32) -> Option<&'static str> {
    match ErrorKind::try_from(kind).unwrap_or(ErrorKind::Unspecified) {
        ErrorKind::CapacityExceeded => Some("capacity_exceeded"),
        ErrorKind::InvalidRequest | ErrorKind::PermissionDenied | ErrorKind::MalformedOutput => {
            Some("privd_rejected")
        }
        _ => None,
    }
}

fn desired_resource(envelope: &CommandEnvelope) -> Option<(&'static str, String, u64)> {
    match envelope.payload.as_ref()? {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::GroupApply(value) => {
            Some(("group", value.group_name.clone(), value.desired_revision))
        }
        command_envelope::Payload::ConfigApply(value) => {
            Some(("config", "ocserv.conf".to_owned(), value.desired_revision))
        }
        command_envelope::Payload::CertificateCsr(value) => {
            Uuid::from_slice(&value.certificate_id).ok().map(|id| {
                (
                    "certificate_key",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        command_envelope::Payload::CertificateRevoke(value) => {
            Uuid::from_slice(&value.certificate_id).ok().map(|id| {
                (
                    "certificate_revoke",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        command_envelope::Payload::CertificateP12(value) => {
            Uuid::from_slice(&value.artifact_id).ok().map(|id| {
                (
                    "certificate_artifact",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        _ => None,
    }
}

async fn observe_external_effect(
    privd: &PrivdClient,
    envelope: &CommandEnvelope,
) -> ExternalEffectObservation {
    // Planning is side-effect free. After privd startup cleanup, an interrupted
    // validation can always be retried from the immutable candidate.
    if matches!(
        envelope.payload,
        Some(command_envelope::Payload::ConfigPlan(_))
    ) {
        return ExternalEffectObservation::Absent;
    }
    // Certificate key creation and deletion are both idempotent for their
    // controller-issued UUID. Retrying is safer than guessing a terminal state.
    if matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::CertificateCsr(_)
                | command_envelope::Payload::CertificateRevoke(_)
                | command_envelope::Payload::CertificateP12(_)
        )
    ) {
        return ExternalEffectObservation::Absent;
    }
    let target_boot = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(target)) => Some(&target.boot_id),
        Some(command_envelope::Payload::SessionTerminate(target)) => Some(&target.boot_id),
        _ => None,
    };
    if let Some(target_boot) = target_boot {
        let Ok(current_boot) = tokio::fs::read_to_string("/proc/sys/kernel/random/boot_id").await
        else {
            return ExternalEffectObservation::Unknown;
        };
        if current_boot.trim() != target_boot {
            return ExternalEffectObservation::Unknown;
        }
    }
    let Some(payload) = envelope.payload.as_ref() else {
        return ExternalEffectObservation::Unknown;
    };
    let response = match payload {
        command_envelope::Payload::SessionDisconnect(_)
        | command_envelope::Payload::SessionTerminate(_) => {
            privd
                .call(privd_request::Operation::SessionList(
                    ocservia_agent_protocol::ReadRequest {},
                ))
                .await
        }
        command_envelope::Payload::IpBanRemove(_) => {
            privd
                .call(privd_request::Operation::IpBanList(
                    ocservia_agent_protocol::ReadRequest {},
                ))
                .await
        }
        payload if desired_effect_identity(payload).is_some() => {
            privd.call_reconcile(envelope).await
        }
        _ => return ExternalEffectObservation::Unknown,
    };
    let Ok(response) = response else {
        return ExternalEffectObservation::Unknown;
    };
    let Some(result) = response.result else {
        return ExternalEffectObservation::Unknown;
    };
    map_external_effect(payload, result)
}

fn map_external_effect(
    payload: &command_envelope::Payload,
    result: privd_response::Result,
) -> ExternalEffectObservation {
    match (payload, result) {
        (
            command_envelope::Payload::SessionDisconnect(target),
            privd_response::Result::SessionList(current),
        ) => {
            if current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id)
            {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (
            command_envelope::Payload::SessionTerminate(target),
            privd_response::Result::SessionList(current),
        ) => {
            if current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id)
            {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (
            command_envelope::Payload::IpBanRemove(target),
            privd_response::Result::IpBanList(current),
        ) => {
            if current.bans.iter().any(|ban| ban.ip == target.ip) {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (payload, privd_response::Result::DesiredEffectObservation(observation))
            if desired_effect_identity(payload).is_some() =>
        {
            match DesiredEffectState::try_from(observation.state)
                .unwrap_or(DesiredEffectState::Unspecified)
            {
                DesiredEffectState::AppliedExact => ExternalEffectObservation::AppliedExact,
                DesiredEffectState::SupersededByNewerRevision => {
                    ExternalEffectObservation::SupersededByNewerRevision
                }
                DesiredEffectState::Absent => ExternalEffectObservation::Absent,
                DesiredEffectState::Unknown | DesiredEffectState::Unspecified => {
                    ExternalEffectObservation::Unknown
                }
            }
        }
        _ => ExternalEffectObservation::Unknown,
    }
}

fn desired_effect_identity(
    payload: &command_envelope::Payload,
) -> Option<(&'static str, &str, u64)> {
    match payload {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user_create", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user_disable", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user_enable", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => Some((
            "user_password_rotate",
            &value.username,
            value.desired_revision,
        )),
        command_envelope::Payload::GroupApply(value) => {
            Some(("group_apply", &value.group_name, value.desired_revision))
        }
        command_envelope::Payload::ConfigApply(value) => {
            Some(("config_apply", "ocserv.conf", value.desired_revision))
        }
        _ => None,
    }
}

fn unknown_result(record: &CommandRecord, code: &str) -> CommandResult {
    let mut result = command_result(record, false);
    result.state = CommandResultState::Unknown.into();
    result.result.clear();
    code.clone_into(&mut result.error_code);
    result
}

fn command_result(record: &CommandRecord, replayed: bool) -> CommandResult {
    let state = match record.state {
        CommandState::Succeeded => CommandResultState::Succeeded,
        CommandState::Failed => CommandResultState::Failed,
        CommandState::Unknown | CommandState::Accepted | CommandState::Running => {
            CommandResultState::Unknown
        }
    };
    let proof = record
        .privileged_result_proof
        .as_deref()
        .and_then(|encoded| PrivilegedResultProof::decode(encoded).ok());
    let receipt = proof.as_ref().and_then(|value| value.receipt_v1.as_ref());
    CommandResult {
        command_id: record.command_id.to_vec(),
        idempotency_key: record.idempotency_key.to_vec(),
        payload_sha256: record.payload_sha256.to_vec(),
        state: state.into(),
        result: record.result.clone().unwrap_or_default(),
        error_code: record.error_code.clone().unwrap_or_default(),
        accepted_at: receipt
            .and_then(|value| value.accepted_at)
            .or(Some(prost_types::Timestamp {
                seconds: record.accepted_at,
                nanos: 0,
            })),
        completed_at: receipt.and_then(|value| value.completed_at).or(Some(
            prost_types::Timestamp {
                seconds: record.updated_at,
                nanos: 0,
            },
        )),
        replayed: receipt.is_some_and(|value| value.replayed) || proof.is_none() && replayed,
        semantic_payload_hash_version: record.payload_hash_version,
        privileged_result_proof: proof,
    }
}

fn rejected_result(envelope: &CommandEnvelope, code: &str, now: i64) -> CommandResult {
    CommandResult {
        command_id: envelope.command_id.clone(),
        idempotency_key: envelope.idempotency_key.clone(),
        payload_sha256: Vec::new(),
        state: CommandResultState::Rejected.into(),
        result: Vec::new(),
        error_code: code.to_owned(),
        accepted_at: None,
        completed_at: Some(prost_types::Timestamp {
            seconds: now,
            nanos: 0,
        }),
        replayed: false,
        semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
        privileged_result_proof: None,
    }
}

async fn send_telemetry(
    connection: &iroh::endpoint::Connection,
    payload: &[u8],
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    if payload.is_empty() || payload.len() > 512 * 1024 {
        return Err(invalid("telemetry payload size invalid").into());
    }
    let mut send = tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.open_uni()).await??;
    send.write_all(&u32::try_from(payload.len())?.to_be_bytes())
        .await?;
    send.write_all(payload).await?;
    send.finish()?;
    Ok(())
}

#[allow(clippy::too_many_lines)]
fn build_telemetry(
    session: &SessionContext<'_>,
    sequence: u64,
    observations: &[PrivdResponse],
    connection: &iroh::endpoint::Connection,
    drops: [u64; 4],
) -> Result<TelemetryBatch, io::Error> {
    let now = SystemTime::now();
    let mut service = serde_json::json!({"active_state":"unknown","sub_state":"unknown"});
    let mut version = "unknown".to_owned();
    let mut sessions = Vec::new();
    let mut ip_bans = Vec::new();
    let mut users = Vec::new();
    let mut groups = Vec::new();
    let mut user_snapshot_complete = false;
    let mut group_snapshot_complete = false;
    let mut fingerprint = serde_json::json!({});
    for observation in observations {
        match &observation.result {
            Some(privd_response::Result::ServiceStatus(value)) => {
                service = serde_json::json!({"load_state":value.load_state,"active_state":value.active_state,"sub_state":value.sub_state});
            }
            Some(privd_response::Result::OcservVersion(value)) => {
                version.clone_from(&value.version);
            }
            Some(privd_response::Result::ConfigFingerprint(value)) => {
                fingerprint = serde_json::json!({"config_sha256":value.sha256,"config_size_bytes":value.size_bytes});
            }
            Some(privd_response::Result::SessionList(value)) => {
                sessions = value
                    .sessions
                    .iter()
                    .map(|session| SessionObservation {
                        session_id: session.id.clone(),
                        username: session.username.clone(),
                        client_ip: session.remote_ip.clone(),
                        connected_at: Some(now.into()),
                        bytes_in: 0,
                        bytes_out: 0,
                    })
                    .collect();
            }
            Some(privd_response::Result::IpBanList(value)) => {
                ip_bans = value
                    .bans
                    .iter()
                    .map(|ban| IpBanObservation {
                        ip: ban.ip.clone(),
                        seconds_remaining: ban.seconds_remaining,
                    })
                    .collect();
            }
            Some(privd_response::Result::UserList(value)) => {
                user_snapshot_complete = true;
                users = value
                    .users
                    .iter()
                    .map(|user| {
                        let canonical = format!(
                            "{{\"name\":\"{}\",\"enabled\":{}}}",
                            user.username, user.enabled
                        );
                        UserObservation {
                            username: user.username.clone(),
                            enabled: user.enabled,
                            revision: session
                                .journal
                                .applied_revision("user", &user.username)
                                .ok()
                                .flatten()
                                .unwrap_or(0),
                            fingerprint_sha256: sha2::Sha256::digest(canonical.as_bytes()).to_vec(),
                        }
                    })
                    .collect();
            }
            Some(privd_response::Result::GroupList(value)) => {
                group_snapshot_complete = true;
                groups = value
                    .groups
                    .iter()
                    .map(|group| {
                        let encoded_members = group
                            .members
                            .iter()
                            .map(|member| format!("\"{member}\""))
                            .collect::<Vec<_>>()
                            .join(",");
                        let canonical = format!(
                            "{{\"name\":\"{}\",\"members\":[{}]}}",
                            group.group_name, encoded_members
                        );
                        GroupObservation {
                            group_name: group.group_name.clone(),
                            members: group.members.clone(),
                            revision: session
                                .journal
                                .applied_revision("group", &group.group_name)
                                .ok()
                                .flatten()
                                .unwrap_or(0),
                            fingerprint_sha256: sha2::Sha256::digest(canonical.as_bytes()).to_vec(),
                        }
                    })
                    .collect();
            }
            _ => {}
        }
    }
    if !user_snapshot_complete || !group_snapshot_complete {
        return Err(invalid("privd user/group snapshot incomplete"));
    }
    if let Ok(applied_groups) = session.journal.applied_revisions("group") {
        for (group_name, revision) in applied_groups {
            if groups.iter().any(|group| group.group_name == group_name) {
                continue;
            }
            let canonical = format!("{{\"name\":\"{group_name}\",\"members\":[]}}");
            groups.push(GroupObservation {
                group_name,
                members: Vec::new(),
                revision,
                fingerprint_sha256: sha2::Sha256::digest(canonical.as_bytes()).to_vec(),
            });
        }
        groups.sort_by(|left, right| left.group_name.cmp(&right.group_name));
    }
    if users.len() > MAX_MANAGED_RESOURCES || groups.len() > MAX_MANAGED_RESOURCES.saturating_mul(2)
    {
        return Err(invalid("user/group telemetry capacity exceeded"));
    }
    let paths = connection.paths();
    let selected = paths.iter().find(iroh::endpoint::Path::is_selected);
    let (mode, rtt) = selected.map_or(("unknown", 0), |path| {
        (
            if path.is_ip() {
                "direct"
            } else if path.is_relay() {
                "relay"
            } else {
                "unknown"
            },
            u64::try_from(path.rtt().as_millis()).unwrap_or(u64::MAX),
        )
    });
    let ocserv = serde_json::json!({"service":service,"configuration":fingerprint});
    let path = serde_json::json!({"mode":mode,"rtt_ms":rtt});
    let session_count = f64::from(u32::try_from(sessions.len()).unwrap_or(u32::MAX));
    let batch = TelemetryBatch {
        batch_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: session.node_id.as_bytes().to_vec(),
        sequence,
        priority: i32::from(TelemetryPriority::CurrentHealth),
        snapshot: Some(ObservedSnapshot {
            observed_at: Some(now.into()),
            boot_id: session.boot_id.to_owned(),
            agent_instance_id: session.agent_instance_id.as_bytes().to_vec(),
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            ocserv_version: version,
            os_release: session.os_release.to_owned(),
            ocserv_json: serde_json::to_vec(&ocserv).unwrap_or_else(|_| b"{}".to_vec()),
            system_json: b"{}".to_vec(),
            path_json: serde_json::to_vec(&path).unwrap_or_else(|_| b"{}".to_vec()),
            dropped: Some(TelemetryDropCounters {
                security: drops[0],
                health: drops[1],
                aggregate: drops[2],
                raw: drops[3],
            }),
        }),
        sessions,
        samples: vec![MetricSample {
            sampled_at: Some(now.into()),
            metric: "session_count".to_owned(),
            value: session_count,
        }],
        security_events: Vec::new(),
        ip_bans,
        users,
        groups,
    };
    if batch.encoded_len() > 512 * 1024 {
        return Err(invalid("telemetry payload size invalid"));
    }
    Ok(batch)
}

async fn shutdown_signal() {
    let Ok(mut terminate) =
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
    else {
        let _ = tokio::signal::ctrl_c().await;
        return;
    };
    tokio::select! {
        _ = terminate.recv() => {}
        _ = tokio::signal::ctrl_c() => {}
    }
}

#[derive(Debug)]
struct Config {
    identity_dir: PathBuf,
    journal: PathBuf,
    privd_socket: PathBuf,
    controller: Option<EndpointId>,
    node_id: Option<Uuid>,
    probe_only: bool,
    relay_mode: RelayMode,
    command_verification_key_files: Vec<PathBuf>,
    prepare_enrollment: bool,
    enrollment_token_file: Option<PathBuf>,
    enrollment_environment: Option<String>,
    sealing_keys: Vec<SealingKeyDescriptorV1>,
    stats_file: Option<PathBuf>,
    relay_ca_file: Option<PathBuf>,
    synthetic_barrier_file: Option<PathBuf>,
}

/// Loads the PEM relay certificate authority as additional relay TLS roots.
/// Deployments whose relays chain to public roots leave this unset.
fn load_relay_tls_roots(
    config: &Config,
) -> Result<Vec<rustls_pki_types::CertificateDer<'static>>, io::Error> {
    let Some(path) = config.relay_ca_file.as_ref() else {
        return Ok(Vec::new());
    };
    let roots: Vec<_> = rustls_pki_types::CertificateDer::pem_file_iter(path)
        .map_err(|error| invalid(&format!("relay CA file is unreadable: {error}")))?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| invalid(&format!("relay CA PEM is invalid: {error}")))?;
    if roots.is_empty() {
        return Err(invalid("relay CA file contains no certificates"));
    }
    Ok(roots)
}

/// Applies the optional private relay CA to one endpoint builder.
fn with_relay_tls_roots(
    builder: iroh::endpoint::Builder,
    roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> iroh::endpoint::Builder {
    if roots.is_empty() {
        builder
    } else {
        builder.ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(roots))
    }
}

fn load_controller_command_keys(
    config: &Config,
) -> Result<Option<ControllerCommandKeyring>, Box<dyn std::error::Error + Send + Sync>> {
    if config.probe_only || config.prepare_enrollment || config.enrollment_token_file.is_some() {
        return Ok(None);
    }
    if config.command_verification_key_files.is_empty() {
        return Err(invalid("--controller-command-key-file is required").into());
    }
    let expected_owner = rustix::process::geteuid().as_raw();
    let expected_group = rustix::process::getegid().as_raw();
    let keys = config
        .command_verification_key_files
        .iter()
        .map(|path| load_verification_key(path, expected_owner, expected_group))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(Some(ControllerCommandKeyring::new(keys)?))
}

#[allow(clippy::too_many_lines)]
fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
        journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
        privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
        controller: None,
        node_id: None,
        probe_only: false,
        relay_mode: RelayMode::Default,
        command_verification_key_files: Vec::new(),
        prepare_enrollment: false,
        enrollment_token_file: None,
        enrollment_environment: None,
        sealing_keys: Vec::new(),
        stats_file: None,
        relay_ca_file: None,
        synthetic_barrier_file: None,
    };
    let mut relay_mode = String::from("default");
    let mut relay_urls = Vec::new();
    let mut relay_token_file = None;
    let mut user_seal_key_id = None;
    let mut user_seal_key_sha256 = None;
    let mut p12_seal_key_id = None;
    let mut p12_seal_key_sha256 = None;
    let mut args = env::args().skip(1);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--identity-dir" => {
                config.identity_dir = PathBuf::from(required(&mut args, "--identity-dir")?);
            }
            "--journal" => config.journal = PathBuf::from(required(&mut args, "--journal")?),
            "--privd-socket" => {
                config.privd_socket = PathBuf::from(required(&mut args, "--privd-socket")?);
            }
            "--controller" => {
                config.controller = Some(
                    required(&mut args, "--controller")?
                        .parse()
                        .map_err(|_| invalid("controller EndpointID invalid"))?,
                );
            }
            "--node-id" => {
                config.node_id = Some(
                    required(&mut args, "--node-id")?
                        .parse()
                        .map_err(|_| invalid("node ID invalid"))?,
                );
            }
            "--probe-privd-only" => config.probe_only = true,
            "--controller-command-key-file" => {
                if config.command_verification_key_files.len() == 8 {
                    return Err(invalid(
                        "at most eight Controller command verification keys are allowed",
                    ));
                }
                config
                    .command_verification_key_files
                    .push(PathBuf::from(required(
                        &mut args,
                        "--controller-command-key-file",
                    )?));
            }
            "--prepare-enrollment" => config.prepare_enrollment = true,
            "--enrollment-token-file" => {
                config.enrollment_token_file = Some(PathBuf::from(required(
                    &mut args,
                    "--enrollment-token-file",
                )?));
            }
            "--enrollment-environment" => {
                config.enrollment_environment =
                    Some(required(&mut args, "--enrollment-environment")?);
            }
            "--user-password-seal-key-id" => {
                user_seal_key_id = Some(required(&mut args, "--user-password-seal-key-id")?);
            }
            "--user-password-seal-public-key-sha256" => {
                user_seal_key_sha256 = Some(required(
                    &mut args,
                    "--user-password-seal-public-key-sha256",
                )?);
            }
            "--p12-password-seal-key-id" => {
                p12_seal_key_id = Some(required(&mut args, "--p12-password-seal-key-id")?);
            }
            "--p12-password-seal-public-key-sha256" => {
                p12_seal_key_sha256 = Some(required(
                    &mut args,
                    "--p12-password-seal-public-key-sha256",
                )?);
            }
            "--relay-mode" => relay_mode = required(&mut args, "--relay-mode")?,
            "--relay-url" => relay_urls.push(required(&mut args, "--relay-url")?),
            "--relay-token-file" => {
                relay_token_file = Some(PathBuf::from(required(&mut args, "--relay-token-file")?));
            }
            "--stats-file" => {
                config.stats_file = Some(PathBuf::from(required(&mut args, "--stats-file")?));
            }
            "--relay-ca-file" => {
                let path = PathBuf::from(required(&mut args, "--relay-ca-file")?);
                if !path.is_absolute() {
                    return Err(invalid("--relay-ca-file must be an absolute path"));
                }
                config.relay_ca_file = Some(path);
            }
            "--synthetic-barrier-file" => {
                config.synthetic_barrier_file = Some(PathBuf::from(required(
                    &mut args,
                    "--synthetic-barrier-file",
                )?));
            }
            _ => return Err(invalid("unknown agent argument")),
        }
    }
    validate_absolute_paths(&config, relay_token_file.as_deref())?;
    if config.prepare_enrollment
        && (relay_mode != "default" || !relay_urls.is_empty() || relay_token_file.is_some())
    {
        return Err(invalid(
            "enrollment preparation does not accept relay configuration",
        ));
    }
    validate_enrollment_mode(&config)?;
    config.sealing_keys = build_sealing_keys(
        &config,
        user_seal_key_id,
        user_seal_key_sha256,
        p12_seal_key_id,
        p12_seal_key_sha256,
    )?;
    config.relay_mode = build_relay_mode(&relay_mode, relay_urls, relay_token_file.as_deref())?;
    Ok(config)
}

fn build_sealing_keys(
    config: &Config,
    user_key_id: Option<String>,
    user_sha256: Option<String>,
    p12_key_id: Option<String>,
    p12_sha256: Option<String>,
) -> io::Result<Vec<SealingKeyDescriptorV1>> {
    if config.probe_only || config.prepare_enrollment {
        if user_key_id.is_some()
            || user_sha256.is_some()
            || p12_key_id.is_some()
            || p12_sha256.is_some()
        {
            return Err(invalid(
                "probe and preparation modes do not accept sealing keys",
            ));
        }
        return Ok(Vec::new());
    }
    let keys = [
        (SealedSecretPurpose::UserPassword, user_key_id, user_sha256),
        (
            SealedSecretPurpose::CertificateP12Password,
            p12_key_id,
            p12_sha256,
        ),
    ]
    .into_iter()
    .map(|(purpose, key_id, digest)| {
        let key_id = key_id.ok_or_else(|| invalid("both password sealing keys are required"))?;
        let digest =
            digest.ok_or_else(|| invalid("both password sealing key fingerprints are required"))?;
        if key_id.is_empty()
            || key_id.len() > 128
            || !key_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(invalid("password sealing key descriptor invalid"));
        }
        Ok(SealingKeyDescriptorV1 {
            version: SealedSecretVersion::V1.into(),
            purpose: purpose.into(),
            key_id,
            public_key_sha256: hex::decode(digest)
                .map_err(|_| invalid("password sealing key fingerprint invalid"))?,
        })
    })
    .collect::<io::Result<Vec<_>>>()?;
    if keys[0].key_id == keys[1].key_id || keys[0].public_key_sha256 == keys[1].public_key_sha256 {
        return Err(invalid("password sealing keys must be distinct"));
    }
    Ok(keys)
}

fn validate_absolute_paths(config: &Config, relay_token_file: Option<&Path>) -> io::Result<()> {
    if !config.identity_dir.is_absolute()
        || config
            .command_verification_key_files
            .iter()
            .any(|path| !path.is_absolute())
        || relay_token_file.is_some_and(|path| !path.is_absolute())
        || config
            .enrollment_token_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
        || config
            .synthetic_barrier_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(invalid(
            "identity, Controller command key, token, and synthetic barrier paths must be absolute",
        ));
    }
    Ok(())
}

fn validate_enrollment_mode(config: &Config) -> Result<(), io::Error> {
    if config.prepare_enrollment
        && (config.controller.is_none()
            || config.enrollment_token_file.is_some()
            || config.enrollment_environment.is_some()
            || config.node_id.is_some()
            || config.probe_only
            || !config.command_verification_key_files.is_empty())
        || config.enrollment_token_file.is_some() != config.enrollment_environment.is_some()
        || config.enrollment_token_file.is_some() && (config.node_id.is_some() || config.probe_only)
        || config.enrollment_environment.as_ref().is_some_and(|value| {
            value.is_empty() || value.len() > 64 || value.chars().any(char::is_whitespace)
        })
    {
        return Err(invalid("enrollment mode arguments are invalid"));
    }
    Ok(())
}

fn read_enrollment_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    let metadata = file.metadata()?;
    let owner_only = metadata.file_type().is_file()
        && metadata.uid() == rustix::process::geteuid().as_raw()
        && metadata.mode().trailing_zeros() >= 6;
    let protected_group = metadata.file_type().is_file()
        && metadata.uid() == 0
        && metadata.gid() == rustix::process::getegid().as_raw()
        && metadata.mode() & 0o027 == 0
        && metadata.mode() & 0o040 != 0;
    if !owner_only && !protected_group {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "enrollment token must be process-owned mode 0600 or root:agent mode 0640",
        ));
    }
    let mut raw = Vec::with_capacity(44);
    std::io::Read::read_to_end(&mut std::io::Read::take(&mut file, 45), &mut raw)?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid("enrollment token must be UTF-8"))?
        .trim_end_matches(['\n', '\r']);
    if token.len() != 43
        || !token
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(invalid("enrollment token is invalid"));
    }
    Ok(token.to_owned())
}

fn build_relay_mode(
    mode: &str,
    raw_urls: Vec<String>,
    token_file: Option<&Path>,
) -> Result<RelayMode, io::Error> {
    match mode {
        "default" if raw_urls.is_empty() && token_file.is_none() => Ok(RelayMode::Default),
        "disabled" if raw_urls.is_empty() && token_file.is_none() => Ok(RelayMode::Disabled),
        "default" | "disabled" => Err(invalid(
            "relay URLs and token are accepted only with custom relay mode",
        )),
        "custom" => {
            if !(2..=8).contains(&raw_urls.len()) {
                return Err(invalid("custom relay mode requires 2..8 relay URLs"));
            }
            let token_file = token_file
                .ok_or_else(|| invalid("custom relay mode requires --relay-token-file"))?;
            let mut urls = Vec::with_capacity(raw_urls.len());
            for raw in raw_urls {
                let url: RelayUrl = raw.parse().map_err(|_| invalid("relay URL is invalid"))?;
                if url.scheme() != "https"
                    || !url.username().is_empty()
                    || url.password().is_some()
                    || url.query().is_some()
                    || url.fragment().is_some()
                    || url.host_str().is_none()
                {
                    return Err(invalid(
                        "relay URL must be credential-free HTTPS without query or fragment",
                    ));
                }
                if urls.contains(&url) {
                    return Err(invalid("relay URLs must be unique"));
                }
                urls.push(url);
            }
            let token = read_relay_token(token_file)?;
            Ok(RelayMode::Custom(
                RelayMap::from_iter(urls).with_auth_token(token),
            ))
        }
        _ => Err(invalid("relay mode must be default, disabled, or custom")),
    }
}

#[allow(clippy::verbose_bit_mask)]
fn read_relay_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.file_type().is_file() {
        return Err(invalid("relay token path must be a regular file"));
    }
    let mode = metadata.mode();
    let owner_only = metadata.uid() == rustix::process::geteuid().as_raw() && mode & 0o077 == 0;
    let protected_group = metadata.uid() == 0
        && metadata.gid() == rustix::process::getegid().as_raw()
        && mode & 0o027 == 0
        && mode & 0o040 != 0;
    if !owner_only && !protected_group {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "relay token must be process-owned mode 0600 or root:agent mode 0640",
        ));
    }
    let mut raw = Vec::with_capacity(129);
    std::io::Read::read_to_end(&mut std::io::Read::take(&mut file, 513), &mut raw)?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid("relay token must be UTF-8"))?
        .trim_end_matches(['\n', '\r']);
    if !(32..=512).contains(&token.len()) || token.chars().any(char::is_whitespace) {
        return Err(invalid(
            "relay token must be 32..512 non-whitespace UTF-8 bytes",
        ));
    }
    Ok(token.to_owned())
}

#[cfg(target_os = "linux")]
const fn libc_o_nofollow() -> i32 {
    0x20_000
}

#[cfg(target_os = "macos")]
const fn libc_o_nofollow() -> i32 {
    0x100
}

fn required(args: &mut impl Iterator<Item = String>, name: &str) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

fn invalid(detail: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, detail)
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer as _, SigningKey};
    use futures_util::StreamExt as _;
    use ocservia_agent_protocol::{
        ConfigFingerprint, DesiredEffectObservation, GroupList, IpBanList, OcservVersion,
        PrivdRequest, ServiceStatus, SessionList, UserList, read_frame, write_frame,
    };
    use ocservia_command_authorization::{
        ConnectionFenceClaimsV2, FENCE_SIGNATURE_VERSION_ED25519_V1, canonical_connection_fence_v2,
        verification_key_id,
    };
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        ArtifactGrantV1, ConfigApply, ConfigPlan, FenceSignatureVersion, SealedSecretV1,
        SessionGrantV1, SessionGrantVersion, UserPasswordRotate,
    };
    use ocservia_contracts::generated::ocserv::platform::transport::v1::{
        ConsumeArtifactRequest, FetchArtifactRequest, GetNodeConnectionRequest, SendCommandRequest,
        TransportEventType, WatchEventsRequest, transport_service_server::TransportService,
    };
    use ocservia_transportd::{IdentityPolicy, IrohTransportService};
    use prost_types::Timestamp;
    use std::collections::HashMap;
    use std::os::unix::fs::PermissionsExt as _;

    fn test_sealing_keys() -> Vec<SealingKeyDescriptorV1> {
        vec![
            SealingKeyDescriptorV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::UserPassword as i32,
                key_id: "node-key-1".to_owned(),
                public_key_sha256: vec![0x11; 32],
            },
            SealingKeyDescriptorV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::CertificateP12Password as i32,
                key_id: "node-p12-key-1".to_owned(),
                public_key_sha256: vec![0x22; 32],
            },
        ]
    }

    fn test_user_password(ciphertext: Vec<u8>) -> SealedSecretV1 {
        SealedSecretV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "node-key-1".to_owned(),
            ciphertext,
        }
    }

    fn command_key_config(probe_only: bool) -> Config {
        Config {
            identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
            journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
            privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
            controller: None,
            node_id: None,
            probe_only,
            relay_mode: RelayMode::Default,
            command_verification_key_files: Vec::new(),
            prepare_enrollment: false,
            enrollment_token_file: None,
            enrollment_environment: None,
            sealing_keys: test_sealing_keys(),
            stats_file: None,
            relay_ca_file: None,
            synthetic_barrier_file: None,
        }
    }

    fn test_command_keyring() -> ControllerCommandKeyring {
        ControllerCommandKeyring::new([SigningKey::from_bytes(&[7; 32]).verifying_key()])
            .expect("test command keyring")
    }

    #[tokio::test]
    async fn synthetic_barrier_blocks_until_removed() {
        let path = PathBuf::from(format!(
            "/tmp/ocservia-agent-barrier-{}",
            Uuid::now_v7().simple()
        ));
        tokio::fs::write(&path, b"armed")
            .await
            .expect("arm synthetic barrier");
        let waiting_path = path.clone();
        let waiter = tokio::spawn(async move {
            wait_for_synthetic_barrier(Some(&waiting_path)).await;
        });
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(!waiter.is_finished());
        tokio::fs::remove_file(&path)
            .await
            .expect("release synthetic barrier");
        tokio::time::timeout(Duration::from_secs(1), waiter)
            .await
            .expect("synthetic barrier release timed out")
            .expect("synthetic barrier task");
    }

    fn signed_fence(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
    ) -> ConnectionFenceV2 {
        let claims = ConnectionFenceClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&signing.verifying_key()),
            fence_id: [1; 16],
            node_id: *node_id.as_bytes(),
            endpoint_id: *endpoint_id.as_bytes(),
            owner_instance_id: [2; 16],
            owner_incarnation: 1,
            owner_epoch,
            connection_id: [3; 16],
            authorization_revision: 1,
            capabilities: vec!["synthetic.noop".to_owned()],
            lease_until_seconds: 1_700_000_200,
            lease_until_nanos: 0,
            issued_at_seconds: 1_700_000_000,
            issued_at_nanos: 0,
            expires_at_seconds: 1_700_000_300,
            expires_at_nanos: 0,
        };
        let canonical = canonical_connection_fence_v2(&claims).expect("canonical connection fence");
        let signature = signing.sign(&canonical).to_bytes();
        ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: claims.key_id,
            fence_id: claims.fence_id.to_vec(),
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            owner_instance_id: claims.owner_instance_id.to_vec(),
            owner_incarnation: claims.owner_incarnation,
            owner_epoch: claims.owner_epoch,
            connection_id: claims.connection_id.to_vec(),
            authorization_revision: claims.authorization_revision,
            capabilities: claims.capabilities,
            lease_until: Some(Timestamp {
                seconds: claims.lease_until_seconds,
                nanos: 0,
            }),
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: 0,
            }),
            signature: signature.to_vec(),
        }
    }

    #[test]
    fn gate_rejects_stale_owner_epoch_and_persists_the_floor() {
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test command keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id: EndpointId = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let other_node = Uuid::now_v7();
        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("agent-fence-gate-{}", Uuid::now_v7().simple()));
        std::fs::create_dir_all(&directory).expect("test directory");
        let journal_path = directory.join("journal.db");
        let mut journal = Journal::open(&journal_path).expect("journal");
        let mut floor = 0_u64;
        let command_id = [5_u8; 16];
        let now = 1_700_000_050_i64;

        let first = signed_fence(&signing, &node_id, &endpoint_id, 1);
        let stale_first = signed_fence(&signing, &node_id, &endpoint_id, 1);
        let second = signed_fence(&signing, &node_id, &endpoint_id, 2);
        let foreign_node = signed_fence(&signing, &other_node, &endpoint_id, 3);

        assert!(matches!(
            gate_connection_fence(
                &keyring,
                &mut journal,
                &mut floor,
                &node_id,
                &endpoint_id,
                &first,
                &command_id,
                now
            )
            .expect("first epoch accepted"),
            FenceDecision::Accepted
        ));
        assert_eq!(floor, 1);
        // The same epoch remains the current owner's term: legitimate
        // re-dispatches under the standing fence stay acceptable.
        assert!(matches!(
            gate_connection_fence(
                &keyring,
                &mut journal,
                &mut floor,
                &node_id,
                &endpoint_id,
                &stale_first,
                &command_id,
                now
            )
            .expect("current epoch re-dispatch accepted"),
            FenceDecision::Accepted
        ));
        assert!(matches!(
            gate_connection_fence(
                &keyring,
                &mut journal,
                &mut floor,
                &node_id,
                &endpoint_id,
                &second,
                &command_id,
                now
            )
            .expect("successor epoch accepted"),
            FenceDecision::Accepted
        ));
        assert_eq!(floor, 2);
        assert!(matches!(
            gate_connection_fence(
                &keyring,
                &mut journal,
                &mut floor,
                &node_id,
                &endpoint_id,
                &stale_first,
                &command_id,
                now
            )
            .expect("superseded epoch classified"),
            FenceDecision::RejectedStaleOwnerEpoch
        ));
        assert!(
            gate_connection_fence(
                &keyring,
                &mut journal,
                &mut floor,
                &node_id,
                &endpoint_id,
                &foreign_node,
                &command_id,
                now
            )
            .is_err()
        );
        drop(journal);
        let reopened = Journal::open(&journal_path).expect("reopen journal");
        assert_eq!(
            reopened.owner_fence_epoch_floor().expect("durable floor"),
            2
        );
        std::fs::remove_dir_all(&directory).ok();
    }

    #[test]
    fn agent_accepts_only_explicit_grantless_read_only_sessions() {
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::generate().public();
        let response = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: 0,
            max_message_size: 1024 * 1024,
            controller_version: "test".to_owned(),
            negotiated_capabilities: READ_ONLY_SESSION_CAPABILITIES
                .iter()
                .map(|capability| (*capability).to_owned())
                .collect(),
            session_grant: None,
            connection_fence: None,
        };
        let mode = negotiate_session_mode(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &test_command_keyring(),
        )
        .expect("grantless read-only mode accepted");
        assert!(matches!(mode, AgentSessionMode::ReadOnly { .. }));

        for capability in ["synthetic.noop", "future.feature.read"] {
            let mut invalid = response.clone();
            invalid.negotiated_capabilities = vec![capability.to_owned()];
            assert!(
                negotiate_session_mode(
                    &invalid,
                    &supported_capabilities(),
                    node_id,
                    endpoint_id,
                    &test_command_keyring(),
                )
                .is_err()
            );
        }
    }

    #[test]
    fn agent_rejects_grantless_protocol_v1_1() {
        let response = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: 1,
            max_message_size: 1024 * 1024,
            controller_version: "test".to_owned(),
            negotiated_capabilities: vec!["ocserv.status.read".to_owned()],
            session_grant: None,
            connection_fence: None,
        };
        assert!(
            negotiate_session_mode(
                &response,
                &supported_capabilities(),
                Uuid::now_v7(),
                iroh::SecretKey::generate().public(),
                &test_command_keyring(),
            )
            .is_err()
        );
    }

    #[test]
    fn agent_rejects_invalid_and_accepts_valid_protocol_v1_1_grant_signatures() {
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::generate().public();
        let signing_key = SigningKey::from_bytes(&[7; 32]);
        let capabilities = vec!["ocserv.status.read".to_owned()];
        let now = SystemTime::now();
        let mut response = SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: 1,
            max_message_size: 1024 * 1024,
            controller_version: "test".to_owned(),
            negotiated_capabilities: capabilities.clone(),
            session_grant: Some(SessionGrantV1 {
                version: SessionGrantVersion::V1.into(),
                key_id: ocservia_command_authorization::verification_key_id(
                    &signing_key.verifying_key(),
                ),
                protocol_major: 1,
                protocol_minor: 1,
                node_id: node_id.as_bytes().to_vec(),
                endpoint_id: endpoint_id.as_bytes().to_vec(),
                authorization_revision: 1,
                negotiated_capabilities: capabilities,
                issued_at: Some((now - Duration::from_secs(1)).into()),
                expires_at: Some((now + Duration::from_secs(30)).into()),
                signature: vec![0; 64],
            }),
            connection_fence: None,
        };
        let keyring = ControllerCommandKeyring::new([signing_key.verifying_key()])
            .expect("test command keyring");
        assert!(
            negotiate_session_mode(
                &response,
                &supported_capabilities(),
                node_id,
                endpoint_id,
                &keyring,
            )
            .is_err()
        );
        let grant = response.session_grant.as_mut().expect("session grant");
        let claims = ocservia_command_authorization::session_grant_claims_v1(grant)
            .expect("session grant claims");
        let canonical = ocservia_command_authorization::canonical_session_grant_v1(&claims)
            .expect("canonical session grant");
        grant.signature = signing_key.sign(&canonical).to_bytes().to_vec();
        assert!(matches!(
            negotiate_session_mode(
                &response,
                &supported_capabilities(),
                node_id,
                endpoint_id,
                &keyring,
            )
            .expect("valid signed protocol 1.1 session"),
            AgentSessionMode::AuthorizedV11(_)
        ));
    }

    async fn serve_snapshot(listener: tokio::net::UnixListener) {
        let mut handlers = tokio::task::JoinSet::new();
        for _ in 0..7 {
            let (mut stream, _) = listener.accept().await.expect("accept snapshot request");
            handlers.spawn(async move {
                let request: PrivdRequest =
                    read_frame(&mut stream).await.expect("snapshot request");
                let result = match request.operation {
                    Some(privd_request::Operation::ServiceStatus(_)) => {
                        privd_response::Result::ServiceStatus(ServiceStatus::default())
                    }
                    Some(privd_request::Operation::OcservVersion(_)) => {
                        privd_response::Result::OcservVersion(OcservVersion::default())
                    }
                    Some(privd_request::Operation::SessionList(_)) => {
                        privd_response::Result::SessionList(SessionList::default())
                    }
                    Some(privd_request::Operation::IpBanList(_)) => {
                        privd_response::Result::IpBanList(IpBanList::default())
                    }
                    Some(privd_request::Operation::ConfigFingerprint(_)) => {
                        privd_response::Result::ConfigFingerprint(ConfigFingerprint::default())
                    }
                    Some(privd_request::Operation::UserList(_)) => {
                        privd_response::Result::UserList(UserList::default())
                    }
                    Some(privd_request::Operation::GroupList(_)) => {
                        privd_response::Result::GroupList(GroupList::default())
                    }
                    _ => panic!("unexpected snapshot operation"),
                };
                write_frame(
                    &mut stream,
                    &PrivdResponse {
                        request_id: request.request_id,
                        privileged_result_proof: None,
                        result: Some(result),
                    },
                )
                .await
                .expect("snapshot response");
            });
        }
        while let Some(result) = handlers.join_next().await {
            result.expect("snapshot handler");
        }
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)]
    async fn real_agent_static_session_delivers_telemetry_and_denies_control_streams() {
        let temp_root = if cfg!(target_os = "macos") {
            "/private/tmp"
        } else {
            "/tmp"
        };
        let directory = PathBuf::from(temp_root).join(format!("ocsa-{}", Uuid::now_v7().simple()));
        std::fs::create_dir(&directory).expect("create static session fixture");
        let privd_socket = PathBuf::from(format!("/tmp/ocsm-{}.sock", Uuid::now_v7().simple()));
        let privd_listener =
            tokio::net::UnixListener::bind(&privd_socket).expect("bind privd snapshot fixture");
        let privd_server = tokio::spawn(serve_snapshot(privd_listener));
        let node_id = Uuid::now_v7();
        let agent_key = iroh::SecretKey::generate();
        let controller_key = iroh::SecretKey::generate();
        let policy = IdentityPolicy::new(
            HashMap::from([(agent_key.public(), node_id.as_bytes().to_vec())]),
            HashSet::new(),
        );
        let service = IrohTransportService::new_with_policy(16, policy.clone());
        let router = ocservia_transportd::build_router(
            controller_key,
            RelayMode::Disabled,
            policy,
            &service,
        )
        .await
        .expect("build static transportd");
        let controller = router.endpoint().addr();
        let mut events = service
            .watch_events(tonic::Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("watch transport events")
            .into_inner();
        let journal_path = directory.join("agent.db");
        let endpoint = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build real Agent endpoint");
        let agent_endpoint = endpoint.clone();
        let mut run = tokio::spawn({
            let journal_path = journal_path.clone();
            let agent_privd_socket = privd_socket.clone();
            async move {
                let privd = PrivdClient::new(agent_privd_socket, Duration::from_secs(2))
                    .expect("privd client");
                let mut journal = Journal::open(&journal_path).expect("Agent journal");
                let mut command_executor =
                    CommandExecutor::new(Journal::open(&journal_path).expect("executor journal"));
                let command_keys = test_command_keyring();
                let sealing_keys = test_sealing_keys();
                let mut fence_epoch_floor = 0_u64;
                let mut session = SessionContext {
                    node_id,
                    endpoint_id: agent_key.public(),
                    privd: &privd,
                    journal: &mut journal,
                    command_executor: &mut command_executor,
                    boot_id: "static-test-boot",
                    os_release: "static-test-os",
                    agent_instance_id: Uuid::now_v7(),
                    command_keys: &command_keys,
                    sealing_keys: &sealing_keys,
                    fence_epoch_floor: &mut fence_epoch_floor,
                    synthetic_barrier_file: None,
                };
                let result = connect_once(&endpoint, controller, &mut session).await;
                endpoint.close().await;
                result
            }
        });

        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                tokio::select! {
                    result = &mut run => panic!("Agent ended before telemetry: {result:?}"),
                    event = events.next() => {
                        let event = event.expect("event stream remains open").expect("valid event");
                        if event.r#type == i32::from(TransportEventType::Telemetry) {
                            return;
                        }
                    }
                }
            }
        })
        .await
        .expect("real Agent registered and delivered telemetry");
        let metadata = service
            .get_node_connection(tonic::Request::new(GetNodeConnectionRequest {
                node_id: node_id.as_bytes().to_vec(),
            }))
            .await
            .expect("query static Agent session")
            .into_inner();
        assert_eq!(metadata.authorization_revision, 0);
        assert!(metadata.session_expires_at.is_none());
        assert_eq!(
            metadata.negotiated_capabilities,
            READ_ONLY_SESSION_CAPABILITIES
                .iter()
                .map(|capability| (*capability).to_owned())
                .collect::<Vec<_>>()
        );

        let command = CommandEnvelope {
            node_id: node_id.as_bytes().to_vec(),
            traceparent: "00-11111111111111111111111111111111-2222222222222222-01".to_owned(),
            required_capability: "ocserv.status.read".to_owned(),
            expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
            ..CommandEnvelope::default()
        };
        let command_error = service
            .send_command(tonic::Request::new(SendCommandRequest {
                node_id: node_id.as_bytes().to_vec(),
                command_envelope: command.encode_to_vec(),
            }))
            .await
            .expect_err("read-only command dispatch denied");
        assert_eq!(command_error.code(), tonic::Code::PermissionDenied);

        let artifact_id = Uuid::now_v7();
        let grant = ArtifactGrantV1 {
            node_id: node_id.as_bytes().to_vec(),
            artifact_id: artifact_id.as_bytes().to_vec(),
            purpose: "certificate_p12".to_owned(),
            max_bytes: 32,
            grant_id: Uuid::now_v7().as_bytes().to_vec(),
            ..ArtifactGrantV1::default()
        };
        let Err(fetch_error) = service
            .fetch_artifact(tonic::Request::new(FetchArtifactRequest {
                node_id: node_id.as_bytes().to_vec(),
                artifact_id: artifact_id.as_bytes().to_vec(),
                purpose: "certificate_p12".to_owned(),
                max_bytes: 32,
                grant: Some(grant.clone()),
                fence_binding: None,
            }))
            .await
        else {
            panic!("read-only artifact fetch denied");
        };
        assert_eq!(fetch_error.code(), tonic::Code::PermissionDenied);
        let consume_error = service
            .consume_artifact(tonic::Request::new(ConsumeArtifactRequest {
                node_id: node_id.as_bytes().to_vec(),
                grant: Some(grant),
                sha256: vec![0; 32],
                size: 32,
                confirm_only: false,
                fence_binding: None,
            }))
            .await
            .expect_err("read-only artifact consumption denied");
        assert_eq!(consume_error.code(), tonic::Code::PermissionDenied);

        assert!(
            !run.is_finished(),
            "Agent session remains active until the test closes its endpoint"
        );
        agent_endpoint.close().await;
        let session_result = tokio::time::timeout(Duration::from_secs(5), run)
            .await
            .expect("Agent stops after endpoint shutdown")
            .expect("Agent task");
        if let Err(error) = session_result {
            assert!(
                matches!(
                    error.downcast_ref::<iroh::endpoint::ConnectionError>(),
                    Some(iroh::endpoint::ConnectionError::LocallyClosed)
                ),
                "Agent endpoint shutdown returned an unexpected error: {error}"
            );
        }
        ocservia_transportd::shutdown(&service, router)
            .await
            .expect("shutdown static transportd");
        privd_server.await.expect("privd server");
        std::fs::remove_file(privd_socket).expect("remove privd snapshot socket");
        std::fs::remove_dir_all(directory).expect("remove static session fixture");
    }

    #[test]
    fn network_agent_requires_a_controller_command_verification_key() {
        assert!(load_controller_command_keys(&command_key_config(false)).is_err());
        assert!(
            load_controller_command_keys(&command_key_config(true))
                .expect("read-only probe exception")
                .is_none()
        );
    }

    #[test]
    fn network_agent_requires_two_distinct_password_sealing_keys() {
        let config = command_key_config(false);
        assert!(build_sealing_keys(&config, None, None, None, None).is_err());
        assert!(
            build_sealing_keys(
                &config,
                Some("shared".to_owned()),
                Some("11".repeat(32)),
                Some("shared".to_owned()),
                Some("22".repeat(32)),
            )
            .is_err()
        );
        assert!(
            build_sealing_keys(
                &config,
                Some("user".to_owned()),
                Some("11".repeat(32)),
                Some("p12".to_owned()),
                Some("11".repeat(32)),
            )
            .is_err()
        );
    }

    #[test]
    fn enrollment_mode_uses_endpoint_identity_without_command_keys() {
        let mut config = command_key_config(false);
        config.enrollment_token_file = Some(PathBuf::from("/run/secrets/enrollment-token"));
        config.enrollment_environment = Some("production".to_owned());
        assert!(
            load_controller_command_keys(&config)
                .expect("enrollment has no mutation-capable command session")
                .is_none()
        );
    }

    #[test]
    fn fresh_enrollment_preparation_reuses_the_endpoint_key_for_proof() {
        use ed25519_dalek::{Signature, Verifier as _, VerifyingKey};

        let directory =
            std::env::temp_dir().join(format!("ocservia-agent-prepare-{}", Uuid::now_v7()));
        let controller = iroh::SecretKey::generate().public();
        let mut config = command_key_config(false);
        config.identity_dir.clone_from(&directory);
        config.controller = Some(controller);
        config.prepare_enrollment = true;

        validate_enrollment_mode(&config).expect("standalone preparation mode");
        assert!(
            load_controller_command_keys(&config)
                .expect("preparation has no command session")
                .is_none()
        );
        let endpoint = prepare_enrollment(&config).expect("prepare fresh endpoint identity");
        assert_eq!(endpoint.len(), 64);
        assert!(
            endpoint
                .bytes()
                .all(|byte| byte.is_ascii_digit() || matches!(byte, b'a'..=b'f'))
        );

        let identity = ocservia_agent_identity::Identity::provision(&directory, controller)
            .expect("reload prepared identity");
        assert_eq!(endpoint, hex::encode(identity.endpoint_id().as_bytes()));
        let mut request = EnrollRequest {
            token: "a".repeat(43),
            endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
            agent_version: "test".to_owned(),
            os_release: "test".to_owned(),
            ocserv_version: "test".to_owned(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            capabilities: supported_capabilities(),
            environment: "production".to_owned(),
            nonce: Uuid::now_v7().as_bytes().to_vec(),
            time: Some(SystemTime::now().into()),
            enrollment_protocol_major: 0,
            enrollment_protocol_minor: 0,
            proof: None,
            sealing_keys: config.sealing_keys.clone(),
        };
        identity
            .authorize_enrollment(&mut request)
            .expect("authorize enrollment with prepared key");
        let canonical = ocservia_agent_identity::enrollment_canonical_v1(&request)
            .expect("canonical enrollment claims");
        let proof = request.proof.expect("signed proof");
        let verification_key =
            VerifyingKey::from_bytes(identity.endpoint_id().as_bytes()).expect("endpoint key");
        let signature = Signature::from_slice(&proof.signature).expect("Ed25519 signature");
        verification_key
            .verify(&canonical, &signature)
            .expect("prepared endpoint proves possession");

        std::fs::remove_dir_all(directory).expect("remove enrollment preparation fixture");
    }

    #[test]
    fn enrollment_token_file_requires_strict_metadata_and_format() {
        let directory =
            std::env::temp_dir().join(format!("ocservia-agent-enrollment-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create test directory");
        let token = directory.join("enrollment.token");
        let valid_token = "0".repeat(43);
        std::fs::write(&token, format!("{valid_token}\n")).expect("write token");
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600))
            .expect("protect token");
        assert_eq!(
            read_enrollment_token(&token).expect("valid token"),
            valid_token
        );

        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644))
            .expect("make token insecure");
        assert!(read_enrollment_token(&token).is_err());
        std::fs::remove_dir_all(directory).expect("remove test directory");
    }

    #[test]
    fn production_relay_selection_is_closed_and_redundant() {
        let directory =
            std::env::temp_dir().join(format!("ocservia-agent-relay-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create test directory");
        let token = directory.join("relay.token");
        std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("write token");
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600))
            .expect("protect token");

        let mode = build_relay_mode(
            "custom",
            vec![
                "https://relay-a.example.test".to_owned(),
                "https://relay-b.example.test".to_owned(),
            ],
            Some(&token),
        )
        .expect("valid custom relay mode");
        assert!(matches!(mode, RelayMode::Custom(_)));
        assert!(build_relay_mode("default", Vec::new(), None).is_ok());
        assert!(
            build_relay_mode(
                "custom",
                vec!["https://relay-a.example.test".into()],
                Some(&token)
            )
            .is_err()
        );
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644))
            .expect("make token insecure");
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "https://relay-a.example.test".into(),
                    "https://relay-b.example.test".into(),
                ],
                Some(&token),
            )
            .is_err()
        );
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "https://user@relay-a.example.test".into(),
                    "https://relay-b.example.test".into(),
                ],
                Some(&token),
            )
            .is_err()
        );
        std::fs::remove_dir_all(directory).expect("remove test directory");
    }

    #[test]
    fn authoritative_precondition_rejection_is_terminal() {
        assert!(terminal_privd_error(ErrorKind::InvalidRequest.into()));
        assert!(terminal_privd_error(ErrorKind::CapacityExceeded.into()));
        assert_eq!(
            terminal_privd_error_code(ErrorKind::CapacityExceeded.into()),
            Some("capacity_exceeded")
        );
        assert!(!terminal_privd_error(ErrorKind::OutputLimit.into()));
        assert!(!terminal_privd_error(ErrorKind::Unavailable.into()));
        assert!(!terminal_privd_error(ErrorKind::CommandFailed.into()));
    }

    #[test]
    fn unexpected_privd_result_cannot_prove_a_password_effect() {
        let payload = command_envelope::Payload::UserPasswordRotate(UserPasswordRotate {
            username: "alice".to_owned(),
            sealed_password: Vec::new(),
            secret_key_id: String::new(),
            desired_revision: 7,
            sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
        });
        let result = privd_response::Result::Mutation(ocservia_agent_protocol::MutationResult {
            applied: true,
        });

        assert_eq!(
            map_external_effect(&payload, result),
            ExternalEffectObservation::Unknown
        );
    }

    #[test]
    fn privileged_success_without_root_receipt_is_never_terminal() {
        let response = PrivdResponse {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            privileged_result_proof: None,
            result: Some(privd_response::Result::Mutation(
                ocservia_agent_protocol::MutationResult { applied: true },
            )),
        };
        assert!(response_proof(&response).is_none());
    }

    #[tokio::test]
    async fn password_reconcile_uses_non_secret_authoritative_effect_store() {
        let socket = PathBuf::from(format!("/tmp/ocsm-{}.sock", Uuid::now_v7().simple()));
        let listener = tokio::net::UnixListener::bind(&socket).expect("bind effect fixture");
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            let request: PrivdRequest = read_frame(&mut stream).await.expect("request");
            assert!(request.operation.is_none());
            assert_eq!(
                ocservia_agent_protocol::PrivilegedRequestMode::try_from(request.privileged_mode)
                    .expect("privileged mode"),
                ocservia_agent_protocol::PrivilegedRequestMode::Reconcile
            );
            let Some(CommandEnvelope {
                payload: Some(command_envelope::Payload::UserPasswordRotate(observe)),
                ..
            }) = request.authorization_command
            else {
                panic!("desired effect observation required")
            };
            assert_eq!(observe.username, "alice");
            assert_eq!(observe.desired_revision, 7);
            write_frame(
                &mut stream,
                &PrivdResponse {
                    request_id: request.request_id,
                    privileged_result_proof: None,
                    result: Some(privd_response::Result::DesiredEffectObservation(
                        DesiredEffectObservation {
                            state: DesiredEffectState::AppliedExact.into(),
                            observed_revision: observe.desired_revision,
                        },
                    )),
                },
            )
            .await
            .expect("response");
        });
        let client = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
        let envelope = CommandEnvelope {
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            semantic_payload_sha256: vec![0x5a; 32],
            expires_at: Some(prost_types::Timestamp {
                seconds: i64::MAX,
                nanos: 0,
            }),
            payload: Some(command_envelope::Payload::UserPasswordRotate(
                UserPasswordRotate {
                    username: "alice".to_owned(),
                    sealed_password: Vec::new(),
                    secret_key_id: String::new(),
                    desired_revision: 7,
                    sealed_password_v1: Some(test_user_password(vec![0xa5; 64])),
                },
            )),
            ..CommandEnvelope::default()
        };
        assert_eq!(
            observe_external_effect(&client, &envelope).await,
            ExternalEffectObservation::AppliedExact
        );
        server.await.expect("server");
        std::fs::remove_file(socket).expect("remove socket");
    }

    #[tokio::test]
    async fn interrupted_config_plan_is_safe_to_retry() {
        let socket = std::env::temp_dir().join(format!("missing-{}.sock", Uuid::now_v7()));
        let client = PrivdClient::new(socket, Duration::from_millis(20)).expect("client");
        let envelope = CommandEnvelope {
            payload: Some(command_envelope::Payload::ConfigPlan(ConfigPlan {
                candidate: b"tcp-port = 443\n".to_vec(),
                candidate_hash: vec![0x5a; 32],
                expected_revision: 0,
            })),
            ..CommandEnvelope::default()
        };
        assert_eq!(
            observe_external_effect(&client, &envelope).await,
            ExternalEffectObservation::Absent
        );
    }

    #[tokio::test]
    async fn interrupted_config_apply_uses_durable_revision_and_effect_identity() {
        async fn observe(
            state: DesiredEffectState,
            observed_revision: u64,
        ) -> ExternalEffectObservation {
            let socket = std::env::temp_dir().join(format!("apply-{}.sock", Uuid::now_v7()));
            let listener =
                tokio::net::UnixListener::bind(&socket).expect("bind fingerprint fixture");
            let server = tokio::spawn(async move {
                let (mut stream, _) = listener.accept().await.expect("accept");
                let request: PrivdRequest = read_frame(&mut stream).await.expect("request");
                assert!(request.operation.is_none());
                let Some(CommandEnvelope {
                    payload: Some(command_envelope::Payload::ConfigApply(observe)),
                    ..
                }) = request.authorization_command
                else {
                    panic!("config desired-effect observation required")
                };
                assert_eq!(observe.desired_revision, 2);
                write_frame(
                    &mut stream,
                    &PrivdResponse {
                        request_id: request.request_id,
                        privileged_result_proof: None,
                        result: Some(privd_response::Result::DesiredEffectObservation(
                            DesiredEffectObservation {
                                state: state.into(),
                                observed_revision,
                            },
                        )),
                    },
                )
                .await
                .expect("response");
            });
            let client = PrivdClient::new(socket.clone(), Duration::from_secs(2)).expect("client");
            let envelope = CommandEnvelope {
                command_id: Uuid::now_v7().as_bytes().to_vec(),
                idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
                semantic_payload_sha256: vec![0x6a; 32],
                expires_at: Some(prost_types::Timestamp {
                    seconds: i64::MAX,
                    nanos: 0,
                }),
                payload: Some(command_envelope::Payload::ConfigApply(ConfigApply {
                    candidate: b"tcp-port = 443\n".to_vec(),
                    candidate_hash: vec![0x5a; 32],
                    expected_current_hash: vec![0x4b; 32],
                    desired_revision: 2,
                })),
                ..CommandEnvelope::default()
            };
            let result = observe_external_effect(&client, &envelope).await;
            server.await.expect("server");
            std::fs::remove_file(socket).expect("remove socket");
            result
        }

        assert_eq!(
            observe(DesiredEffectState::AppliedExact, 2).await,
            ExternalEffectObservation::AppliedExact
        );
        assert_eq!(
            observe(DesiredEffectState::Absent, 2).await,
            ExternalEffectObservation::Absent
        );
        assert_eq!(
            observe(DesiredEffectState::SupersededByNewerRevision, 3).await,
            ExternalEffectObservation::SupersededByNewerRevision
        );
        assert_eq!(
            observe(DesiredEffectState::Unknown, 0).await,
            ExternalEffectObservation::Unknown
        );
    }
}
