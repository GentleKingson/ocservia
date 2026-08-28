use std::collections::HashSet;
use std::env;
use std::future::Future;
use std::io;
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::time::{Duration, SystemTime};

use iroh::endpoint::{QuicTransportConfig, RelayMode, VarInt, presets};
use iroh::{
    Endpoint, EndpointAddr, EndpointId, RelayMap, RelayUrl, SecretKey, TransportAddr, Watcher,
};
use ocservia_agent::{
    CommandContext, CommandError, CommandExecutor, ExternalEffectObservation, ExternalPreparation,
    MAX_COMMAND_BYTES, MAX_WRITE_QUEUE, PrivdClient,
};
use ocservia_agent_protocol::{
    ArtifactConsumeRequest, ArtifactReadRequest, DesiredEffectState, ErrorKind,
    MAX_MANAGED_RESOURCES, PrivdResponse, UpgradeOperationResult, privd_request, privd_response,
};
use ocservia_command_authorization::{
    ControllerCommandKeyring, FenceBindingClaimsV2, VerifiedConnectionFenceV2,
    VerifiedSessionGrant, load_verification_key,
};
use ocservia_command_journal::{
    CommandRecord, CommandState, Journal, OFFLINE_RECOVERY_RETENTION_SECONDS, TelemetryInsert,
};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, AgentUpgradeOutcomeState, AgentUpgradeResultReport, ArtifactChunk,
    ArtifactConsumeRequest as ArtifactConsumeFinalizeRequest,
    ArtifactConsumeResponse as ArtifactConsumeFinalizeResponse, ArtifactFetchRequest,
    CommandDeliveryMode, CommandEnvelope, CommandResult, CommandResultState, ConnectionFenceV2,
    EnrollRequest, EnrollResponse, FenceBindingV2, FenceOperationKind, GroupObservation,
    HandshakeResult, IpBanObservation, MetricSample, ObservedSnapshot, PrivdReceiptVersion,
    PrivilegedResultProof, SealedSecretPurpose, SealedSecretVersion, SealingKeyDescriptorV1,
    SemanticPayloadHashVersion, SessionHandshake, SessionHandshakeResponse, SessionObservation,
    TelemetryBatch, TelemetryDropCounters, TelemetryPriority, UserObservation, command_envelope,
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
const CONNECTION_FENCING_CAPABILITY: &str = "ocserv.fencing.v2";
const RELAY_FAILOVER_POLL_INTERVAL: Duration = Duration::from_secs(1);
const ARTIFACT_FRAME_MASK: u32 = 3 << 30;
const ARTIFACT_FETCH_FRAME: u32 = 1 << 31;
const ARTIFACT_CONSUME_FRAME: u32 = 3 << 30;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    // The packaging pipeline verifies the binary's embedded release
    // identity before it is shipped, so --version stays a read-only
    // query that works for any caller.
    if std::env::args().any(|argument| argument == "--version") {
        println!(
            "ocservia-agent {}",
            ocservia_contracts::agent_upgrade::release_version()
        );
        return Ok(());
    }
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
    let boot_id = ocservia_agent::read_boot_id().await?;
    let os_release = ocservia_agent::read_os_release().await?;
    let agent_instance_id = Uuid::now_v7();
    let bind_endpoint = agent_endpoint_factory(
        identity.secret_key().clone(),
        config.relay_mode.clone(),
        transport,
        relay_tls_roots.as_ref().clone(),
    );
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
    let run =
        supervise_controller_sessions(bind_endpoint, controller, &config.relay_mode, &mut session);
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
    let keep_relays_connected = keep_dedicated_relays_connected(&config.relay_mode);
    let endpoint = with_relay_tls_roots(
        Endpoint::builder(presets::N0),
        load_relay_tls_roots(config)?,
    )
    .secret_key(identity.secret_key().clone())
    .relay_mode(config.relay_mode.clone())
    .keep_relays_connected(keep_relays_connected)
    .bind()
    .await?;
    let connection = endpoint
        .connect(EndpointAddr::new(controller), ENROLL_ALPN)
        .await?;
    let mut request = EnrollRequest {
        token,
        endpoint_id: identity.endpoint_id().as_bytes().to_vec(),
        agent_version: ocservia_contracts::agent_upgrade::release_version().to_owned(),
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

fn keep_dedicated_relays_connected(relay_mode: &RelayMode) -> bool {
    matches!(relay_mode, RelayMode::Custom(relays) if relays.len() >= 2)
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

struct SessionGrantAuthority {
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    expires_at_unix_seconds: i64,
}

enum ActiveSessionAuthority {
    SignedReadOnlyV11(SessionGrantAuthority),
    FencedV11 {
        grant: SessionGrantAuthority,
        connection_fence: VerifiedConnectionFenceV2,
    },
}

impl ActiveSessionAuthority {
    fn grant(&self) -> &SessionGrantAuthority {
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
}

fn fenced_artifact_authority(
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
            Self::AuthorizedV11(ActiveSessionAuthority::SignedReadOnlyV11(_)) => {
                "signed_read_only_v1_1"
            }
            Self::AuthorizedV11(ActiveSessionAuthority::FencedV11 { .. }) => "fenced_v1_1",
        }
    }

    fn capability_count(&self) -> usize {
        match self {
            Self::ReadOnly {
                negotiated_capabilities,
            } => negotiated_capabilities.len(),
            Self::AuthorizedV11(authority) => authority.grant().negotiated_capabilities.len(),
        }
    }

    fn expires_at_unix_seconds(&self) -> Option<i64> {
        match self {
            Self::ReadOnly { .. } => None,
            Self::AuthorizedV11(authority) => Some(authority.grant().expires_at_unix_seconds),
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
            "ocserv.agent.upgrade.v2",
        ])
        .map(str::to_owned)
        .collect()
}

#[allow(clippy::too_many_lines)]
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
    let session_mode = negotiate_and_activate_session(&response, &supported_capabilities, session)?;
    tracing::info!(
        controller = %controller.id,
        session_mode = session_mode.name(),
        negotiated_capabilities = session_mode.capability_count(),
        "agent session accepted"
    );
    let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
    // Dedicated-relay health is rechecked while the session is idle so a dead
    // relay is failed over before the QUIC idle timeout notices the loss.
    let mut relay_watchdog = tokio::time::interval(RELAY_FAILOVER_POLL_INTERVAL);
    relay_watchdog.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let session_expiry = wait_for_session_expiry(&session_mode);
    tokio::pin!(session_expiry);
    let mut sequence = 0_u64;
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            _ = relay_watchdog.tick() => {
                let Some(failover) = relay_failover_plan(endpoint, &connection) else {
                    continue;
                };
                tracing::warn!(
                    failed_relay = %failover.failed_relay,
                    healthy_relays = ?failover.healthy_relays,
                    "controller session relay failed; redialing over the healthy standby"
                );
                connection.close(VarInt::from_u32(0x10b), b"relay failover redial");
                return Err(failover.into());
            },
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
                // Durable upgrade outcome evidence is best-effort: an older
                // privd without the fixed read simply yields an empty report.
                let upgrade_outcomes=session.privd.upgrade_results().await.unwrap_or_default();
                sequence=sequence.saturating_add(1);
                let drops=session.journal.telemetry_drop_counters()?;
                let batch=build_telemetry(session,sequence,&observations,upgrade_outcomes,&connection,drops)?;
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

fn negotiate_and_activate_session(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    session: &mut SessionContext<'_>,
) -> Result<AgentSessionMode, Box<dyn std::error::Error + Send + Sync>> {
    let session_mode = negotiate_session_mode(
        response,
        supported_capabilities,
        session.node_id,
        session.endpoint_id,
        session.command_keys,
    )?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    activate_session_connection_fence(
        &session_mode,
        session.journal,
        session.fence_epoch_floor,
        i64::try_from(now.as_secs())?,
        now.subsec_nanos(),
    )?;
    Ok(session_mode)
}

/// Builds the production Agent endpoint factory.
///
/// The factory is indirect so tests can pin relay-only transports while
/// production keeps the configured preset, dedicated relays, and relay CA
/// roots.
fn agent_endpoint_factory(
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
            let keep_relays_connected = keep_dedicated_relays_connected(&relay_mode);
            with_relay_tls_roots(Endpoint::builder(presets::N0), relay_tls_roots)
                .secret_key(secret_key)
                .relay_mode(relay_mode)
                .keep_relays_connected(keep_relays_connected)
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
/// The Iroh Endpoint outlives every controller session: `keep_relays_connected`
/// standbys are Endpoint state and must survive a session redial, so the
/// Endpoint is rebuilt only when it is actually closed, never between
/// sessions. A session whose dedicated relay failed while a standby stays
/// connected is redialed immediately over the healthy relay instead of waiting
/// for the QUIC idle timeout or a transport-level failure.
async fn supervise_controller_sessions(
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
        let target = controller_dial_target(controller, relay_mode, endpoint);
        let mut relay_failover = false;
        match connect_once(endpoint, target, session).await {
            Ok(()) => attempt = 0,
            Err(error) => {
                if error.downcast_ref::<RelayFailoverError>().is_some() {
                    relay_failover = true;
                    tracing::warn!(
                        error = %error,
                        "controller session moved to a healthy dedicated relay"
                    );
                } else {
                    tracing::warn!(error = %error, attempt, "controller connection ended");
                    attempt = attempt.saturating_add(1);
                }
            }
        }
        let delay = if relay_failover {
            // The healthy standby is already connected: redial without the
            // exponential backoff a genuine controller failure deserves.
            backoff.delay(0, &mut rand::rng())
        } else {
            backoff.delay(attempt, &mut rand::rng())
        };
        tokio::time::sleep(delay).await;
    }
}

/// Session-end instruction telling the supervisor to redial immediately over
/// a healthy dedicated standby relay.
#[derive(Debug)]
struct RelayFailoverError {
    failed_relay: RelayUrl,
    healthy_relays: Vec<RelayUrl>,
}

impl std::fmt::Display for RelayFailoverError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "controller session relay {} failed while a standby is connected",
            self.failed_relay
        )
    }
}

impl std::error::Error for RelayFailoverError {}

/// Detects an active session whose selected relay path no longer matches a
/// healthy home relay.
///
/// Only the home relay's state is publicly observable, but with dedicated
/// standbys kept hot the relay actor moves the home relay to a connected
/// standby as soon as the current one fails. A session still riding the
/// failed relay at that point must be redialed over the new home relay
/// instead of waiting for the QUIC idle timeout.
fn relay_failover_plan(
    endpoint: &Endpoint,
    connection: &iroh::endpoint::Connection,
) -> Option<RelayFailoverError> {
    let home = endpoint.home_relay_status().get().into_iter().next()?;
    if !home.is_connected() {
        return None;
    }
    let failed_relay = connection
        .paths()
        .iter()
        .find(iroh::endpoint::Path::is_selected)
        .filter(iroh::endpoint::Path::is_relay)
        .and_then(|path| match path.remote_addr() {
            TransportAddr::Relay(url) => Some(url.clone()),
            _ => None,
        })?;
    if home.url() == &failed_relay {
        return None;
    }
    Some(RelayFailoverError {
        failed_relay,
        healthy_relays: vec![home.url().clone()],
    })
}

/// Builds the controller dial target from the configured dedicated relays.
///
/// The `EndpointID` remains the authenticated controller identity. Dedicated
/// relay URLs are safe addressing hints and keep redial independent of public
/// address discovery during a relay or network-domain transition. While a
/// dedicated home relay is connected it becomes the only hint, so a redial
/// after a relay fault never spends its connect deadline on the failed relay.
fn controller_dial_target(
    controller: EndpointId,
    relay_mode: &RelayMode,
    endpoint: &Endpoint,
) -> EndpointAddr {
    let mut address = EndpointAddr::new(controller);
    let RelayMode::Custom(relays) = relay_mode else {
        return address;
    };
    let home = endpoint.home_relay_status().get().into_iter().next();
    if keep_dedicated_relays_connected(relay_mode)
        && home
            .as_ref()
            .is_some_and(iroh::endpoint::RelayStatus::is_connected)
    {
        return address.with_relay_url(
            home.expect("home relay presence was just checked")
                .url()
                .clone(),
        );
    }
    for relay in relays.urls::<Vec<_>>() {
        address = address.with_relay_url(relay);
    }
    address
}
fn negotiate_session_mode(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
) -> Result<AgentSessionMode, Box<dyn std::error::Error + Send + Sync>> {
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

#[cfg(test)]
fn negotiate_session_mode_at(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
    now_unix_seconds: i64,
) -> Result<AgentSessionMode, Box<dyn std::error::Error + Send + Sync>> {
    negotiate_session_mode_at_instant(
        response,
        supported_capabilities,
        node_id,
        endpoint_id,
        command_keys,
        now_unix_seconds,
        0,
    )
}

#[allow(clippy::too_many_arguments)]
fn negotiate_session_mode_at_instant(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
    now_unix_seconds: i64,
    now_unix_nanos: u32,
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
            || response.connection_fence.is_some()
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
        return Ok(AgentSessionMode::AuthorizedV11(
            ActiveSessionAuthority::SignedReadOnlyV11(grant),
        ));
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
    Ok(AgentSessionMode::AuthorizedV11(
        ActiveSessionAuthority::FencedV11 {
            grant,
            connection_fence: verified,
        },
    ))
}

/// Verifies and durably records the owner term before a fenced session can
/// become active. Every accepted handshake is a replacement connection, so
/// its epoch must be strictly newer than the Agent's retained high-water mark.
#[allow(clippy::too_many_arguments)]
fn activate_session_connection_fence(
    session_mode: &AgentSessionMode,
    journal: &mut Journal,
    fence_epoch_floor: &mut u64,
    now_unix_seconds: i64,
    now_unix_nanos: u32,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let verified = match session_mode {
        AgentSessionMode::ReadOnly { .. }
        | AgentSessionMode::AuthorizedV11(ActiveSessionAuthority::SignedReadOnlyV11(_)) => {
            return Ok(());
        }
        AgentSessionMode::AuthorizedV11(ActiveSessionAuthority::FencedV11 {
            connection_fence,
            ..
        }) => connection_fence,
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

fn refresh_fence_epoch_floor(
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

enum FenceDecision {
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
fn gate_signed_read_only_stale_diagnostic(
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
fn verify_fenced_operation(
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
        let (grant, active_fence) = fenced_artifact_authority(authority, framed_length)?;
        return handle_artifact_stream(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.endpoint_id,
            session.command_keys,
            session.privd,
            grant,
            active_fence,
            session.journal,
            session.fence_epoch_floor,
        )
        .await;
    }
    if framed_length & ARTIFACT_FRAME_MASK == ARTIFACT_CONSUME_FRAME {
        let (grant, active_fence) = fenced_artifact_authority(authority, framed_length)?;
        return handle_artifact_consume(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.endpoint_id,
            session.command_keys,
            session.privd,
            grant,
            active_fence,
            session.journal,
            session.fence_epoch_floor,
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
    refresh_fence_epoch_floor(session.journal, session.fence_epoch_floor)?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    let stale_owner = match authority {
        ActiveSessionAuthority::SignedReadOnlyV11(_) => {
            let fence = envelope
                .connection_fence
                .as_ref()
                .ok_or_else(|| invalid("signed read-only session received an unfenced command"))?;
            matches!(
                gate_signed_read_only_stale_diagnostic(
                    session.command_keys,
                    *session.fence_epoch_floor,
                    &session.node_id,
                    &session.endpoint_id,
                    fence,
                    &envelope.command_id,
                    now_unix_seconds,
                    now.subsec_nanos(),
                )?,
                FenceDecision::RejectedStaleOwnerEpoch
            )
        }
        ActiveSessionAuthority::FencedV11 {
            grant,
            connection_fence,
        } => {
            verify_fenced_operation(
                session.command_keys,
                grant,
                connection_fence,
                *session.fence_epoch_floor,
                &session.node_id,
                &session.endpoint_id,
                envelope.connection_fence.as_ref(),
                envelope.fence_binding.as_ref(),
                FenceOperationKind::Command,
                &envelope.command_id,
                &envelope.required_capability,
                now_unix_seconds,
                now.subsec_nanos(),
            )?;
            false
        }
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
    let grant = authority.grant();
    let context = CommandContext {
        node_id: *session.node_id.as_bytes(),
        authorization_revision: grant.authorization_revision,
        capabilities: grant.negotiated_capabilities.clone(),
        session_expires_at_unix_seconds: grant.expires_at_unix_seconds,
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
        wait_for_synthetic_barrier(
            session.synthetic_barrier_file,
            envelope.command_id.as_slice(),
        )
        .await?;
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
                | command_envelope::Payload::AgentUpgrade(_)
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

fn synthetic_barrier_target(raw: &str) -> Result<Option<Uuid>, io::Error> {
    if raw.is_empty() {
        return Ok(None);
    }
    let value = raw.strip_suffix('\n').unwrap_or(raw);
    if value.is_empty() || value.contains(['\n', '\r']) {
        return Err(invalid("synthetic barrier target is invalid"));
    }
    let target =
        Uuid::parse_str(value).map_err(|_| invalid("synthetic barrier target is not a UUID"))?;
    if target.to_string() != value {
        return Err(invalid("synthetic barrier target is not canonical"));
    }
    Ok(Some(target))
}

fn synthetic_receipt_path(barrier_path: &Path) -> PathBuf {
    let mut path = barrier_path.as_os_str().to_owned();
    path.push(".received");
    PathBuf::from(path)
}

async fn read_synthetic_barrier(path: &Path) -> Result<Option<Option<Uuid>>, io::Error> {
    match tokio::fs::read_to_string(path).await {
        Ok(raw) => Ok(Some(synthetic_barrier_target(&raw)?)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error),
    }
}

async fn wait_for_synthetic_barrier(
    path: Option<&Path>,
    command_id: &[u8],
) -> Result<(), io::Error> {
    let Some(path) = path else {
        return Ok(());
    };
    let command_id = Uuid::from_slice(command_id)
        .map_err(|_| invalid("synthetic barrier command ID is invalid"))?;
    let Some(target) = read_synthetic_barrier(path).await? else {
        return Ok(());
    };
    if let Some(target) = target {
        if target != command_id {
            return Ok(());
        }
        tokio::fs::write(
            synthetic_receipt_path(path),
            format!("{command_id}\n").as_bytes(),
        )
        .await?;
    }
    while let Some(target) = read_synthetic_barrier(path).await? {
        if target.is_some_and(|target| target != command_id) {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    Ok(())
}

#[allow(clippy::too_many_arguments, clippy::too_many_lines)]
async fn handle_artifact_stream(
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
async fn handle_artifact_consume(
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
                Some(privd_response::Result::AgentUpgradeScheduled(result)) => {
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
    upgrade_outcomes: Vec<UpgradeOperationResult>,
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
            agent_version: ocservia_contracts::agent_upgrade::release_version().to_owned(),
            ocserv_version: version,
            os_release: session.os_release.to_owned(),
            architecture: ocservia_contracts::agent_upgrade::runtime_architecture()
                .unwrap_or_default()
                .to_owned(),
            ocserv_json: serde_json::to_vec(&ocserv).unwrap_or_else(|_| b"{}".to_vec()),
            system_json: b"{}".to_vec(),
            path_json: serde_json::to_vec(&path).unwrap_or_else(|_| b"{}".to_vec()),
            upgrade_results: upgrade_outcomes
                .into_iter()
                .map(|outcome| AgentUpgradeResultReport {
                    operation_id: outcome.operation_id,
                    state: match outcome.state.as_str() {
                        "succeeded" => AgentUpgradeOutcomeState::Succeeded,
                        "failed" => AgentUpgradeOutcomeState::Failed,
                        "rolled_back" => AgentUpgradeOutcomeState::RolledBack,
                        _ => AgentUpgradeOutcomeState::Unspecified,
                    } as i32,
                    target_version: outcome.target_version,
                    completed_unix_ms: outcome.completed_unix_ms,
                    detail: outcome.detail,
                    privileged_result_proof: outcome.privileged_result_proof,
                })
                .collect(),
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
        canonical_fence_binding_v2, canonical_v1, claims_from_envelope_v1,
        semantic_payload_hash_v1, verification_key_id,
    };
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        ArtifactGrantV1, CommandAuthorizationProof, CommandAuthorizationVersion, ConfigApply,
        ConfigPlan, EnrollRequest, EnrollResponse, FenceSignatureVersion, SealedSecretV1,
        SessionGrantV1, SessionGrantVersion, SyntheticNoop, UserPasswordRotate,
    };
    use ocservia_contracts::generated::ocserv::platform::transport::v1::{
        AuthorizeSessionRequest, CheckEndpointRequest, CheckEndpointResponse,
        ConsumeArtifactRequest, FetchArtifactRequest, GetNodeConnectionRequest,
        GetOwnerFenceRequest, ListNodeTrustRequest, ListNodeTrustResponse, NodeTrustBinding,
        NodeTrustState, SendCommandRequest, TransportEventType, ValidateEnrollmentRequest,
        ValidateEnrollmentResponse, WatchEventsRequest,
        transport_service_server::TransportService,
        trust_service_server::{TrustService, TrustServiceServer},
    };
    use ocservia_transportd::{IdentityPolicy, IrohTransportService, TrustAuthority};
    use prost_types::Timestamp;
    use std::collections::HashMap;
    use std::net::SocketAddr;
    use std::os::unix::fs::PermissionsExt as _;
    use tokio::net::{TcpListener, TcpStream};
    use tokio::sync::oneshot;
    use tokio::task::JoinHandle;
    use tokio_stream::wrappers::TcpListenerStream;

    struct RestartableTcpProxy {
        addr: SocketAddr,
        stop: oneshot::Sender<()>,
        task: JoinHandle<()>,
    }

    impl RestartableTcpProxy {
        async fn start(addr: SocketAddr, target: SocketAddr) -> Self {
            let listener = TcpListener::bind(addr).await.expect("bind relay proxy");
            let addr = listener.local_addr().expect("relay proxy address");
            let (stop, mut stop_rx) = oneshot::channel();
            let task = tokio::spawn(async move {
                let mut children = tokio::task::JoinSet::new();
                loop {
                    tokio::select! {
                        _ = &mut stop_rx => break,
                        accepted = listener.accept() => {
                            let Ok((mut client, _)) = accepted else { break };
                            children.spawn(async move {
                                let Ok(mut upstream) = TcpStream::connect(target).await else {
                                    return;
                                };
                                tokio::io::copy_bidirectional(&mut client, &mut upstream)
                                    .await
                                    .ok();
                            });
                        }
                        Some(_) = children.join_next(), if !children.is_empty() => {}
                    }
                }
                children.abort_all();
                while children.join_next().await.is_some() {}
            });
            Self { addr, stop, task }
        }

        async fn stop(self) -> SocketAddr {
            self.stop.send(()).ok();
            self.task.await.expect("join relay proxy");
            self.addr
        }
    }

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

    #[test]
    fn agent_advertises_connection_owner_fencing_once() {
        assert_eq!(
            supported_capabilities()
                .iter()
                .filter(|capability| capability.as_str() == "ocserv.fencing.v2")
                .count(),
            1
        );
    }

    #[test]
    fn signed_v11_read_only_rejects_both_artifact_frames_before_handler() {
        let authority = ActiveSessionAuthority::SignedReadOnlyV11(SessionGrantAuthority {
            negotiated_capabilities: ["ocserv.status.read".to_owned()].into_iter().collect(),
            authorization_revision: 3,
            expires_at_unix_seconds: 1_700_000_300,
        });
        for framed_length in [ARTIFACT_FETCH_FRAME | 32, ARTIFACT_CONSUME_FRAME | 32] {
            assert!(
                fenced_artifact_authority(&authority, framed_length).is_err(),
                "signed read-only session must reject artifact frame {framed_length:#x} before its handler"
            );
        }
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

    #[derive(Debug)]
    struct FencedSessionTrust {
        signing: SigningKey,
        node_id: Uuid,
        endpoint_id: EndpointId,
        next_epoch: std::sync::atomic::AtomicU64,
    }

    #[tonic::async_trait]
    impl TrustService for FencedSessionTrust {
        async fn list_node_trust(
            &self,
            _request: tonic::Request<ListNodeTrustRequest>,
        ) -> Result<tonic::Response<ListNodeTrustResponse>, tonic::Status> {
            Ok(tonic::Response::new(ListNodeTrustResponse {
                bindings: vec![NodeTrustBinding {
                    node_id: self.node_id.as_bytes().to_vec(),
                    endpoint_id: self.endpoint_id.as_bytes().to_vec(),
                    state: NodeTrustState::Active.into(),
                    revision: 3,
                }],
            }))
        }

        async fn check_endpoint(
            &self,
            request: tonic::Request<CheckEndpointRequest>,
        ) -> Result<tonic::Response<CheckEndpointResponse>, tonic::Status> {
            Ok(tonic::Response::new(CheckEndpointResponse {
                permitted: request.into_inner().endpoint_id == self.endpoint_id.as_bytes(),
            }))
        }

        async fn validate_enrollment(
            &self,
            _request: tonic::Request<ValidateEnrollmentRequest>,
        ) -> Result<tonic::Response<ValidateEnrollmentResponse>, tonic::Status> {
            Err(tonic::Status::unimplemented(
                "enrollment is outside this fixture",
            ))
        }

        async fn enroll(
            &self,
            _request: tonic::Request<EnrollRequest>,
        ) -> Result<tonic::Response<EnrollResponse>, tonic::Status> {
            Err(tonic::Status::unimplemented(
                "enrollment is outside this fixture",
            ))
        }

        async fn authorize_session(
            &self,
            request: tonic::Request<AuthorizeSessionRequest>,
        ) -> Result<tonic::Response<SessionHandshakeResponse>, tonic::Status> {
            use std::sync::atomic::Ordering;

            let request = request.into_inner();
            if request.remote_endpoint_id != self.endpoint_id.as_bytes() {
                return Err(tonic::Status::permission_denied("unexpected endpoint"));
            }
            let handshake = request
                .handshake
                .ok_or_else(|| tonic::Status::invalid_argument("missing handshake"))?;
            if handshake.node_id != self.node_id.as_bytes() {
                return Err(tonic::Status::permission_denied("unexpected node"));
            }
            let epoch = self.next_epoch.fetch_add(1, Ordering::SeqCst);
            Ok(tonic::Response::new(live_signed_handshake_response(
                &self.signing,
                &self.node_id,
                &self.endpoint_id,
                epoch,
                handshake.capabilities,
            )))
        }
    }

    fn live_signed_handshake_response(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
        mut capabilities: Vec<String>,
    ) -> SessionHandshakeResponse {
        capabilities.sort();
        capabilities.dedup();
        let now = unix_seconds().expect("current time");
        let fence_id = Uuid::now_v7().into_bytes();
        let connection_id = Uuid::now_v7().into_bytes();
        let claims = ConnectionFenceClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&signing.verifying_key()),
            fence_id,
            node_id: *node_id.as_bytes(),
            endpoint_id: *endpoint_id.as_bytes(),
            owner_instance_id: [0x42; 16],
            owner_incarnation: 1,
            owner_epoch,
            connection_id,
            authorization_revision: 3,
            capabilities: capabilities.clone(),
            lease_until_seconds: now + 90,
            lease_until_nanos: 0,
            issued_at_seconds: now - 1,
            issued_at_nanos: 0,
            expires_at_seconds: now + 120,
            expires_at_nanos: 0,
        };
        let canonical = canonical_connection_fence_v2(&claims).expect("canonical live fence");
        let fence = ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: claims.key_id.clone(),
            fence_id: claims.fence_id.to_vec(),
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            owner_instance_id: claims.owner_instance_id.to_vec(),
            owner_incarnation: claims.owner_incarnation,
            owner_epoch: claims.owner_epoch,
            connection_id: claims.connection_id.to_vec(),
            authorization_revision: claims.authorization_revision,
            capabilities: claims.capabilities.clone(),
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
            signature: signing.sign(&canonical).to_bytes().to_vec(),
        };
        let mut response =
            signed_handshake_response(signing, node_id, endpoint_id, 3, capabilities, Some(fence));
        let grant = response.session_grant.as_mut().expect("session grant");
        grant.issued_at = Some(Timestamp {
            seconds: now - 1,
            nanos: 0,
        });
        grant.expires_at = Some(Timestamp {
            seconds: now + 120,
            nanos: 0,
        });
        let grant_claims = ocservia_command_authorization::session_grant_claims_v1(grant)
            .expect("live session grant claims");
        grant.signature = signing
            .sign(
                &ocservia_command_authorization::canonical_session_grant_v1(&grant_claims)
                    .expect("canonical live session grant"),
            )
            .to_bytes()
            .to_vec();
        response
    }

    #[allow(clippy::too_many_lines)]
    fn live_signed_noop(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        fence: &ConnectionFenceV2,
    ) -> CommandEnvelope {
        let now = unix_seconds().expect("current time");
        let command_id = Uuid::now_v7().into_bytes();
        let mut envelope = CommandEnvelope {
            protocol_version: "1.1".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: command_id.to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            node_id: node_id.as_bytes().to_vec(),
            sequence: 1,
            issued_at: Some(Timestamp {
                seconds: now,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: now + 60,
                nanos: 0,
            }),
            expected_revision: fence.authorization_revision,
            traceparent: format!(
                "00-{}-0000000000000001-01",
                hex::encode(Uuid::now_v7().as_bytes())
            ),
            actor_id: "relay-fencing-combination-test".to_owned(),
            reason: "verify relay failover preserves fencing".to_owned(),
            delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
            semantic_payload_hash_version: SemanticPayloadHashVersion::V1.into(),
            operation_id: Uuid::now_v7().as_bytes().to_vec(),
            action: "operation.create".to_owned(),
            required_capability: "synthetic.noop".to_owned(),
            authorization: Some(CommandAuthorizationProof {
                version: CommandAuthorizationVersion::V1.into(),
                key_id: verification_key_id(&signing.verifying_key()),
                signature: Vec::new(),
            }),
            payload: Some(command_envelope::Payload::SyntheticNoop(SyntheticNoop {})),
            ..CommandEnvelope::default()
        };
        envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
            .expect("semantic no-op hash")
            .to_vec();
        let claims = claims_from_envelope_v1(&envelope).expect("no-op command claims");
        envelope
            .authorization
            .as_mut()
            .expect("command authorization")
            .signature = signing
            .sign(&canonical_v1(&claims).expect("canonical no-op command"))
            .to_bytes()
            .to_vec();

        let fence_id: [u8; 16] = fence.fence_id.as_slice().try_into().expect("fence ID");
        let owner_instance_id: [u8; 16] = fence
            .owner_instance_id
            .as_slice()
            .try_into()
            .expect("owner instance ID");
        let connection_id: [u8; 16] = fence
            .connection_id
            .as_slice()
            .try_into()
            .expect("connection ID");
        let binding_claims = FenceBindingClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&signing.verifying_key()),
            operation_kind: FenceOperationKind::Command as u32,
            operation_id: command_id,
            fence_id,
            node_id: *node_id.as_bytes(),
            endpoint_id: *endpoint_id.as_bytes(),
            owner_instance_id,
            owner_incarnation: fence.owner_incarnation,
            owner_epoch: fence.owner_epoch,
            connection_id,
            authorization_revision: fence.authorization_revision,
            capability: "synthetic.noop".to_owned(),
            issued_at_seconds: now,
            issued_at_nanos: 0,
            expires_at_seconds: now + 60,
            expires_at_nanos: 0,
        };
        let binding_signature = signing
            .sign(
                &canonical_fence_binding_v2(&binding_claims)
                    .expect("canonical live command binding"),
            )
            .to_bytes();
        envelope.connection_fence = Some(fence.clone());
        envelope.fence_binding = Some(FenceBindingV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: binding_claims.key_id,
            operation_kind: FenceOperationKind::Command.into(),
            operation_id: binding_claims.operation_id.to_vec(),
            fence_id: binding_claims.fence_id.to_vec(),
            node_id: binding_claims.node_id.to_vec(),
            endpoint_id: binding_claims.endpoint_id.to_vec(),
            owner_instance_id: binding_claims.owner_instance_id.to_vec(),
            owner_incarnation: binding_claims.owner_incarnation,
            owner_epoch: binding_claims.owner_epoch,
            connection_id: binding_claims.connection_id.to_vec(),
            authorization_revision: binding_claims.authorization_revision,
            capability: binding_claims.capability,
            issued_at: Some(Timestamp {
                seconds: binding_claims.issued_at_seconds,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: binding_claims.expires_at_seconds,
                nanos: 0,
            }),
            signature: binding_signature.to_vec(),
        });
        envelope
    }

    #[tokio::test]
    async fn synthetic_barrier_blocks_until_removed() {
        let command_id = Uuid::now_v7();
        let path = PathBuf::from(format!(
            "/tmp/ocservia-agent-barrier-{}",
            Uuid::now_v7().simple()
        ));
        tokio::fs::write(&path, b"")
            .await
            .expect("arm synthetic barrier");
        let waiting_path = path.clone();
        let waiter = tokio::spawn(async move {
            wait_for_synthetic_barrier(Some(&waiting_path), command_id.as_bytes()).await
        });
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(!waiter.is_finished());
        tokio::fs::remove_file(&path)
            .await
            .expect("release synthetic barrier");
        tokio::time::timeout(Duration::from_secs(1), waiter)
            .await
            .expect("synthetic barrier release timed out")
            .expect("synthetic barrier task")
            .expect("synthetic barrier wait");
    }

    #[tokio::test]
    async fn exact_synthetic_barrier_signals_receipt_before_release() {
        let command_id = Uuid::now_v7();
        let path = PathBuf::from(format!(
            "/tmp/ocservia-agent-exact-barrier-{}",
            Uuid::now_v7().simple()
        ));
        let receipt = synthetic_receipt_path(&path);
        tokio::fs::write(&path, format!("{command_id}\n"))
            .await
            .expect("arm exact synthetic barrier");
        tokio::fs::write(&receipt, b"")
            .await
            .expect("pre-create synthetic receipt");
        let waiting_path = path.clone();
        let waiter = tokio::spawn(async move {
            wait_for_synthetic_barrier(Some(&waiting_path), command_id.as_bytes()).await
        });
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if tokio::fs::read_to_string(&receipt)
                    .await
                    .is_ok_and(|value| value == format!("{command_id}\n"))
                {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("exact synthetic receipt timed out");
        assert!(
            !waiter.is_finished(),
            "receipt must precede barrier release"
        );
        tokio::fs::remove_file(&path)
            .await
            .expect("release exact synthetic barrier");
        tokio::time::timeout(Duration::from_secs(1), waiter)
            .await
            .expect("exact synthetic barrier release timed out")
            .expect("exact synthetic barrier task")
            .expect("exact synthetic barrier wait");
        tokio::fs::remove_file(receipt)
            .await
            .expect("remove synthetic receipt");
    }

    fn signed_fence(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
    ) -> ConnectionFenceV2 {
        signed_fence_for_authority(
            signing,
            node_id,
            endpoint_id,
            owner_epoch,
            1,
            vec!["synthetic.noop".to_owned()],
            1_700_000_200,
            0,
            1_700_000_300,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn signed_fence_for_authority(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
        authorization_revision: u64,
        capabilities: Vec<String>,
        lease_until_seconds: i64,
        lease_until_nanos: u32,
        expires_at_seconds: i64,
    ) -> ConnectionFenceV2 {
        signed_fence_for_term(
            signing,
            node_id,
            endpoint_id,
            owner_epoch,
            authorization_revision,
            capabilities,
            lease_until_seconds,
            lease_until_nanos,
            expires_at_seconds,
            0,
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn signed_fence_for_term(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
        authorization_revision: u64,
        capabilities: Vec<String>,
        lease_until_seconds: i64,
        lease_until_nanos: u32,
        expires_at_seconds: i64,
        expires_at_nanos: u32,
        fence_id: [u8; 16],
        owner_instance_id: [u8; 16],
        owner_incarnation: u64,
        connection_id: [u8; 16],
    ) -> ConnectionFenceV2 {
        let claims = ConnectionFenceClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&signing.verifying_key()),
            fence_id,
            node_id: *node_id.as_bytes(),
            endpoint_id: *endpoint_id.as_bytes(),
            owner_instance_id,
            owner_incarnation,
            owner_epoch,
            connection_id,
            authorization_revision,
            capabilities,
            lease_until_seconds,
            lease_until_nanos,
            issued_at_seconds: 1_700_000_000,
            issued_at_nanos: 0,
            expires_at_seconds,
            expires_at_nanos,
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
                nanos: i32::try_from(claims.lease_until_nanos).expect("lease nanos"),
            }),
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: i32::try_from(claims.expires_at_nanos).expect("proof expiry nanos"),
            }),
            signature: signature.to_vec(),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn signed_fence_binding(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
        authorization_revision: u64,
        operation_kind: FenceOperationKind,
        operation_id: [u8; 16],
        capability: &str,
        fence_id: [u8; 16],
        owner_instance_id: [u8; 16],
        owner_incarnation: u64,
        connection_id: [u8; 16],
    ) -> FenceBindingV2 {
        signed_fence_binding_until(
            signing,
            node_id,
            endpoint_id,
            owner_epoch,
            authorization_revision,
            operation_kind,
            operation_id,
            capability,
            fence_id,
            owner_instance_id,
            owner_incarnation,
            connection_id,
            1_700_000_300,
            0,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn signed_fence_binding_until(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        owner_epoch: u64,
        authorization_revision: u64,
        operation_kind: FenceOperationKind,
        operation_id: [u8; 16],
        capability: &str,
        fence_id: [u8; 16],
        owner_instance_id: [u8; 16],
        owner_incarnation: u64,
        connection_id: [u8; 16],
        expires_at_seconds: i64,
        expires_at_nanos: u32,
    ) -> FenceBindingV2 {
        let claims = FenceBindingClaimsV2 {
            signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
            key_id: verification_key_id(&signing.verifying_key()),
            operation_kind: operation_kind as u32,
            operation_id,
            fence_id,
            node_id: *node_id.as_bytes(),
            endpoint_id: *endpoint_id.as_bytes(),
            owner_instance_id,
            owner_incarnation,
            owner_epoch,
            connection_id,
            authorization_revision,
            capability: capability.to_owned(),
            issued_at_seconds: 1_700_000_000,
            issued_at_nanos: 0,
            expires_at_seconds,
            expires_at_nanos,
        };
        let canonical = canonical_fence_binding_v2(&claims).expect("canonical fence binding");
        let signature = signing.sign(&canonical).to_bytes();
        FenceBindingV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: claims.key_id,
            operation_kind: operation_kind.into(),
            operation_id: claims.operation_id.to_vec(),
            fence_id: claims.fence_id.to_vec(),
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            owner_instance_id: claims.owner_instance_id.to_vec(),
            owner_incarnation: claims.owner_incarnation,
            owner_epoch: claims.owner_epoch,
            connection_id: claims.connection_id.to_vec(),
            authorization_revision: claims.authorization_revision,
            capability: claims.capability,
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: i32::try_from(claims.expires_at_nanos).expect("binding expiry nanos"),
            }),
            signature: signature.to_vec(),
        }
    }

    fn signed_handshake_response(
        signing: &SigningKey,
        node_id: &Uuid,
        endpoint_id: &EndpointId,
        authorization_revision: u64,
        capabilities: Vec<String>,
        connection_fence: Option<ConnectionFenceV2>,
    ) -> SessionHandshakeResponse {
        let mut grant = SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: verification_key_id(&signing.verifying_key()),
            protocol_major: 1,
            protocol_minor: 1,
            node_id: node_id.as_bytes().to_vec(),
            endpoint_id: endpoint_id.as_bytes().to_vec(),
            authorization_revision,
            negotiated_capabilities: capabilities.clone(),
            issued_at: Some(Timestamp {
                seconds: 1_700_000_000,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: 1_700_000_300,
                nanos: 0,
            }),
            signature: Vec::new(),
        };
        let claims = ocservia_command_authorization::session_grant_claims_v1(&grant)
            .expect("session grant claims");
        let canonical = ocservia_command_authorization::canonical_session_grant_v1(&claims)
            .expect("canonical session grant");
        grant.signature = signing.sign(&canonical).to_bytes().to_vec();
        SessionHandshakeResponse {
            result: HandshakeResult::Accepted.into(),
            protocol_major: 1,
            protocol_minor: 1,
            max_message_size: 1024 * 1024,
            controller_version: "test".to_owned(),
            negotiated_capabilities: capabilities,
            session_grant: Some(grant),
            connection_fence,
        }
    }

    #[test]
    fn signed_read_only_stale_diagnostic_never_advances_the_floor() {
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test command keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id: EndpointId = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let command_id = [5_u8; 16];
        let now = 1_700_000_050_i64;
        let stale = signed_fence(&signing, &node_id, &endpoint_id, 7);
        assert!(matches!(
            gate_signed_read_only_stale_diagnostic(
                &keyring,
                8,
                &node_id,
                &endpoint_id,
                &stale,
                &command_id,
                now,
                0,
            )
            .expect("signed stale diagnostic classified"),
            FenceDecision::RejectedStaleOwnerEpoch
        ));

        for epoch in [8, 9] {
            let non_stale = signed_fence(&signing, &node_id, &endpoint_id, epoch);
            assert!(
                gate_signed_read_only_stale_diagnostic(
                    &keyring,
                    8,
                    &node_id,
                    &endpoint_id,
                    &non_stale,
                    &command_id,
                    now,
                    0,
                )
                .is_err(),
                "signed read-only command at epoch {epoch} must fail closed"
            );
        }
        let foreign_node = signed_fence(&signing, &Uuid::now_v7(), &endpoint_id, 7);
        assert!(
            gate_signed_read_only_stale_diagnostic(
                &keyring,
                8,
                &node_id,
                &endpoint_id,
                &foreign_node,
                &command_id,
                now,
                0,
            )
            .is_err()
        );
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn fenced_operation_requires_the_active_term_and_never_advances_the_floor() {
        const NOW: i64 = 1_700_000_050;
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let capabilities = vec![
            CONNECTION_FENCING_CAPABILITY.to_owned(),
            "synthetic.noop".to_owned(),
        ];
        let grant = SessionGrantAuthority {
            negotiated_capabilities: capabilities.iter().cloned().collect(),
            authorization_revision: 3,
            expires_at_unix_seconds: 1_700_000_300,
        };
        let command_id = [5_u8; 16];
        let active = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            capabilities.clone(),
            NOW,
            100,
            1_700_000_300,
        );
        let active_verified = keyring
            .verify_connection_fence_v2(&active, node_id.as_bytes(), endpoint_id.as_bytes(), NOW)
            .expect("active fence");
        let binding = signed_fence_binding(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            FenceOperationKind::Command,
            command_id,
            "synthetic.noop",
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&active),
            Some(&binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            99,
        )
        .expect("active term accepted one nanosecond before lease deadline");

        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&active),
                Some(&binding),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                100,
            )
            .is_err(),
            "lease expires at its exact nanosecond deadline"
        );

        let proof_boundary = signed_fence_for_term(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            capabilities.clone(),
            NOW,
            100,
            NOW,
            100,
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&proof_boundary),
            Some(&binding),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            99,
        )
        .expect("fence proof remains live one nanosecond before expiry");
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&proof_boundary),
                Some(&binding),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                100,
            )
            .is_err(),
            "fence proof expires at its exact nanosecond deadline"
        );

        let live_refresh = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            capabilities.clone(),
            NOW + 1,
            0,
            1_700_000_300,
        );
        let binding_boundary = signed_fence_binding_until(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            FenceOperationKind::Command,
            command_id,
            "synthetic.noop",
            [1; 16],
            [2; 16],
            1,
            [3; 16],
            NOW,
            100,
        );
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&live_refresh),
            Some(&binding_boundary),
            FenceOperationKind::Command,
            &command_id,
            "synthetic.noop",
            NOW,
            99,
        )
        .expect("fence binding remains live one nanosecond before expiry");
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&live_refresh),
                Some(&binding_boundary),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                100,
            )
            .is_err(),
            "fence binding expires at its exact nanosecond deadline"
        );

        for (fence, binding) in [(None, Some(&binding)), (Some(&active), None)] {
            assert!(
                verify_fenced_operation(
                    &keyring,
                    &grant,
                    &active_verified,
                    8,
                    &node_id,
                    &endpoint_id,
                    fence,
                    binding,
                    FenceOperationKind::Command,
                    &command_id,
                    "synthetic.noop",
                    NOW,
                    99,
                )
                .is_err()
            );
        }

        let higher = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            9,
            3,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        );
        let higher_binding = signed_fence_binding(
            &signing,
            &node_id,
            &endpoint_id,
            9,
            3,
            FenceOperationKind::Command,
            command_id,
            "synthetic.noop",
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&higher),
                Some(&higher_binding),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                0,
            )
            .is_err(),
            "a command cannot activate a successor epoch"
        );

        let different = signed_fence_for_term(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            capabilities,
            1_700_000_200,
            0,
            1_700_000_300,
            0,
            [9; 16],
            [2; 16],
            1,
            [4; 16],
        );
        let different_binding = signed_fence_binding(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            FenceOperationKind::Command,
            command_id,
            "synthetic.noop",
            [9; 16],
            [2; 16],
            1,
            [4; 16],
        );
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&different),
                Some(&different_binding),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                0,
            )
            .is_err(),
            "same epoch under a different immutable term fails closed"
        );

        let artifact_id = [6_u8; 16];
        let artifact_binding = signed_fence_binding(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            FenceOperationKind::Artifact,
            artifact_id,
            CONNECTION_FENCING_CAPABILITY,
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        verify_fenced_operation(
            &keyring,
            &grant,
            &active_verified,
            8,
            &node_id,
            &endpoint_id,
            Some(&active),
            Some(&artifact_binding),
            FenceOperationKind::Artifact,
            &artifact_id,
            CONNECTION_FENCING_CAPABILITY,
            NOW,
            99,
        )
        .expect("artifact binding is grounded in the active session term");
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &active_verified,
                8,
                &node_id,
                &endpoint_id,
                Some(&active),
                Some(&artifact_binding),
                FenceOperationKind::Artifact,
                &[7; 16],
                CONNECTION_FENCING_CAPABILITY,
                NOW,
                99,
            )
            .is_err(),
            "an artifact binding cannot be replayed for another operation"
        );
    }

    #[test]
    fn overlapping_agent_floor_retires_an_old_active_session_before_dispatch() {
        const NOW: i64 = 1_700_000_050;
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let capabilities = vec![
            CONNECTION_FENCING_CAPABILITY.to_owned(),
            "synthetic.noop".to_owned(),
        ];
        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("agent-floor-overlap-{}", Uuid::now_v7().simple()));
        std::fs::create_dir_all(&directory).expect("test directory");
        let journal_path = directory.join("journal.db");
        let active_agent = Journal::open(&journal_path).expect("active Agent journal");
        let successor_agent = Journal::open(&journal_path).expect("successor Agent journal");
        assert_eq!(
            active_agent
                .raise_owner_fence_epoch_floor(5)
                .expect("activate old term"),
            5
        );
        let mut cached_floor = 5_u64;
        assert_eq!(
            successor_agent
                .raise_owner_fence_epoch_floor(10)
                .expect("activate successor term"),
            10
        );
        refresh_fence_epoch_floor(&active_agent, &mut cached_floor)
            .expect("refresh durable epoch floor before dispatch");
        assert_eq!(cached_floor, 10);

        let old_fence = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            5,
            3,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        );
        let old_verified = keyring
            .verify_connection_fence_v2(&old_fence, node_id.as_bytes(), endpoint_id.as_bytes(), NOW)
            .expect("old active fence");
        let command_id = [5_u8; 16];
        let binding = signed_fence_binding(
            &signing,
            &node_id,
            &endpoint_id,
            5,
            3,
            FenceOperationKind::Command,
            command_id,
            "synthetic.noop",
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        let grant = SessionGrantAuthority {
            negotiated_capabilities: capabilities.into_iter().collect(),
            authorization_revision: 3,
            expires_at_unix_seconds: 1_700_000_300,
        };
        assert!(
            verify_fenced_operation(
                &keyring,
                &grant,
                &old_verified,
                cached_floor,
                &node_id,
                &endpoint_id,
                Some(&old_fence),
                Some(&binding),
                FenceOperationKind::Command,
                &command_id,
                "synthetic.noop",
                NOW,
                0,
            )
            .is_err(),
            "the old active session must be retired before command execution"
        );
        assert_eq!(
            active_agent
                .owner_fence_epoch_floor()
                .expect("durable floor"),
            10
        );
        drop(successor_agent);
        drop(active_agent);
        std::fs::remove_dir_all(&directory).ok();
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn fenced_handshake_persists_new_epoch_before_activation_and_rejects_replay() {
        const NOW: i64 = 1_700_000_050;
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let capabilities = vec![
            CONNECTION_FENCING_CAPABILITY.to_owned(),
            "synthetic.noop".to_owned(),
        ];
        let fence = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            8,
            3,
            capabilities.clone(),
            NOW,
            100,
            1_700_000_300,
        );
        let response = signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            capabilities.clone(),
            Some(fence),
        );
        let mode = negotiate_session_mode_at(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
            NOW,
        )
        .expect("signed session grant");
        assert!(matches!(
            &mode,
            AgentSessionMode::AuthorizedV11(ActiveSessionAuthority::FencedV11 {
                connection_fence,
                ..
            }) if connection_fence.owner_epoch == 8
        ));
        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("agent-handshake-fence-{}", Uuid::now_v7().simple()));
        std::fs::create_dir_all(&directory).expect("test directory");
        let journal_path = directory.join("journal.db");
        let mut journal = Journal::open(&journal_path).expect("journal");
        journal
            .raise_owner_fence_epoch_floor(7)
            .expect("preseed floor");
        let mut floor = 7_u64;

        activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99)
            .expect("higher owner epoch activates");
        assert_eq!(floor, 8);
        assert_eq!(journal.owner_fence_epoch_floor().expect("durable floor"), 8);

        assert!(
            activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 99,).is_err(),
            "a second connection cannot reactivate an already observed epoch"
        );

        let _ = capabilities;
        drop(journal);
        let reopened = Journal::open(&journal_path).expect("reopen journal");
        assert_eq!(
            reopened.owner_fence_epoch_floor().expect("reopened floor"),
            8
        );
        std::fs::remove_dir_all(&directory).ok();
    }

    #[test]
    fn fenced_handshake_proof_uses_the_exact_nanosecond_boundary() {
        const NOW: i64 = 1_700_000_050;
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let capabilities = vec![CONNECTION_FENCING_CAPABILITY.to_owned()];
        let fence = signed_fence_for_term(
            &signing,
            &node_id,
            &endpoint_id,
            1,
            3,
            capabilities.clone(),
            NOW,
            100,
            NOW,
            100,
            [1; 16],
            [2; 16],
            1,
            [3; 16],
        );
        let response = signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            capabilities,
            Some(fence),
        );
        let mode = negotiate_session_mode_at_instant(
            &response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
            NOW,
            99,
        )
        .expect("fence proof remains live one nanosecond before expiry");
        assert!(
            negotiate_session_mode_at_instant(
                &response,
                &supported_capabilities(),
                node_id,
                endpoint_id,
                &keyring,
                NOW,
                100,
            )
            .is_err(),
            "handshake verification rejects the exact proof expiry deadline"
        );

        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("agent-proof-boundary-{}", Uuid::now_v7().simple()));
        std::fs::create_dir_all(&directory).expect("test directory");
        let journal_path = directory.join("journal.db");
        let mut journal = Journal::open(&journal_path).expect("journal");
        let mut floor = 0_u64;
        assert!(
            activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 100).is_err(),
            "activation rechecks the proof at its exact expiry deadline"
        );
        assert_eq!(floor, 0);
        assert_eq!(journal.owner_fence_epoch_floor().expect("durable floor"), 0);
        drop(journal);
        std::fs::remove_dir_all(&directory).ok();
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn fenced_handshake_failures_do_not_advance_the_durable_epoch() {
        const NOW: i64 = 1_700_000_050;
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keyring =
            ControllerCommandKeyring::new([signing.verifying_key()]).expect("test keyring");
        let node_id = Uuid::now_v7();
        let endpoint_id = iroh::SecretKey::from_bytes(&[9; 32]).public();
        let capabilities = vec![
            CONNECTION_FENCING_CAPABILITY.to_owned(),
            "synthetic.noop".to_owned(),
        ];
        let valid_fence = signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            11,
            3,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        );
        let valid_response = signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            capabilities.clone(),
            Some(valid_fence.clone()),
        );

        let mut cases = Vec::new();
        let mut missing = valid_response.clone();
        missing.connection_fence = None;
        cases.push(("missing fence", missing));

        let non_fencing_capabilities = vec!["synthetic.noop".to_owned()];
        cases.push((
            "mutation capability without fencing",
            signed_handshake_response(
                &signing,
                &node_id,
                &endpoint_id,
                3,
                non_fencing_capabilities.clone(),
                None,
            ),
        ));
        cases.push((
            "unexpected fence",
            signed_handshake_response(
                &signing,
                &node_id,
                &endpoint_id,
                3,
                non_fencing_capabilities.clone(),
                Some(signed_fence_for_authority(
                    &signing,
                    &node_id,
                    &endpoint_id,
                    11,
                    3,
                    non_fencing_capabilities,
                    1_700_000_200,
                    0,
                    1_700_000_300,
                )),
            ),
        ));

        for (name, epoch) in [("lower epoch", 9), ("equal epoch", 10)] {
            let mut response = valid_response.clone();
            response.connection_fence = Some(signed_fence_for_authority(
                &signing,
                &node_id,
                &endpoint_id,
                epoch,
                3,
                capabilities.clone(),
                1_700_000_200,
                0,
                1_700_000_300,
            ));
            cases.push((name, response));
        }

        let mut expired_proof = valid_response.clone();
        expired_proof.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            11,
            3,
            capabilities.clone(),
            NOW,
            0,
            NOW,
        ));
        cases.push(("expired proof", expired_proof));

        let mut expired_lease = valid_response.clone();
        expired_lease.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            11,
            3,
            capabilities.clone(),
            NOW,
            99,
            1_700_000_300,
        ));
        cases.push(("expired lease", expired_lease));

        let mut bad_signature = valid_response.clone();
        bad_signature
            .connection_fence
            .as_mut()
            .expect("fence")
            .signature[0] ^= 1;
        cases.push(("bad signature", bad_signature));

        let mut wrong_revision = valid_response.clone();
        wrong_revision.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            11,
            4,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        ));
        cases.push(("authorization revision mismatch", wrong_revision));

        let mut wrong_capabilities = valid_response.clone();
        wrong_capabilities.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &endpoint_id,
            11,
            3,
            vec![CONNECTION_FENCING_CAPABILITY.to_owned()],
            1_700_000_200,
            0,
            1_700_000_300,
        ));
        cases.push(("capability mismatch", wrong_capabilities));

        let other_node = Uuid::now_v7();
        let mut wrong_node = valid_response.clone();
        wrong_node.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &other_node,
            &endpoint_id,
            11,
            3,
            capabilities.clone(),
            1_700_000_200,
            0,
            1_700_000_300,
        ));
        cases.push(("node mismatch", wrong_node));

        let other_endpoint = iroh::SecretKey::from_bytes(&[8; 32]).public();
        let mut wrong_endpoint = valid_response;
        wrong_endpoint.connection_fence = Some(signed_fence_for_authority(
            &signing,
            &node_id,
            &other_endpoint,
            11,
            3,
            capabilities,
            1_700_000_200,
            0,
            1_700_000_300,
        ));
        cases.push(("endpoint mismatch", wrong_endpoint));

        let directory = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!(
                "agent-handshake-fence-negative-{}",
                Uuid::now_v7().simple()
            ));
        std::fs::create_dir_all(&directory).expect("test directory");
        let journal_path = directory.join("journal.db");
        let mut journal = Journal::open(&journal_path).expect("journal");
        journal
            .raise_owner_fence_epoch_floor(10)
            .expect("preseed floor");
        let mut floor = 10_u64;
        let read_only_response = signed_handshake_response(
            &signing,
            &node_id,
            &endpoint_id,
            3,
            vec!["ocserv.status.read".to_owned()],
            None,
        );
        let read_only_mode = negotiate_session_mode_at(
            &read_only_response,
            &supported_capabilities(),
            node_id,
            endpoint_id,
            &keyring,
            NOW,
        )
        .expect("signed read-only grant");
        assert!(matches!(
            &read_only_mode,
            AgentSessionMode::AuthorizedV11(ActiveSessionAuthority::SignedReadOnlyV11(_))
        ));
        activate_session_connection_fence(&read_only_mode, &mut journal, &mut floor, NOW, 100)
            .expect("authorized read-only compatibility session");
        assert_eq!(floor, 10);
        for (name, response) in cases {
            let negotiated = negotiate_session_mode_at(
                &response,
                &supported_capabilities(),
                node_id,
                endpoint_id,
                &keyring,
                NOW,
            );
            if let Ok(mode) = negotiated {
                assert!(
                    activate_session_connection_fence(&mode, &mut journal, &mut floor, NOW, 100,)
                        .is_err(),
                    "{name} must fail closed"
                );
            }
            assert_eq!(floor, 10, "{name} changed the in-memory floor");
            assert_eq!(
                journal.owner_fence_epoch_floor().expect("durable floor"),
                10,
                "{name} changed the durable floor"
            );
        }
        drop(journal);
        let reopened = Journal::open(&journal_path).expect("reopen journal");
        assert_eq!(
            reopened.owner_fence_epoch_floor().expect("reopened floor"),
            10
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

    async fn answer_snapshot(mut stream: tokio::net::UnixStream) {
        let request: PrivdRequest = read_frame(&mut stream).await.expect("snapshot request");
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
    }

    async fn serve_snapshot(listener: tokio::net::UnixListener) {
        let mut handlers = tokio::task::JoinSet::new();
        for _ in 0..7 {
            let (stream, _) = listener.accept().await.expect("accept snapshot request");
            handlers.spawn(answer_snapshot(stream));
        }
        while let Some(result) = handlers.join_next().await {
            result.expect("snapshot handler");
        }
    }

    /// Serves snapshot requests for the whole test, so every redialed
    /// controller session can keep taking privd heartbeats.
    async fn serve_snapshots(listener: tokio::net::UnixListener) {
        let mut handlers = tokio::task::JoinSet::new();
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                break;
            };
            handlers.spawn(answer_snapshot(stream));
        }
        while handlers.join_next().await.is_some() {}
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
    fn only_multi_member_custom_relays_enable_persistent_connections() {
        let one = RelayMap::try_from_iter(["https://relay-one.invalid"]).expect("single relay map");
        let two =
            RelayMap::try_from_iter(["https://relay-one.invalid", "https://relay-two.invalid"])
                .expect("two relay map");

        assert!(!keep_dedicated_relays_connected(&RelayMode::Disabled));
        assert!(!keep_dedicated_relays_connected(&RelayMode::Default));
        assert!(!keep_dedicated_relays_connected(&RelayMode::Staging));
        assert!(!keep_dedicated_relays_connected(&RelayMode::Custom(
            RelayMap::empty()
        )));
        assert!(!keep_dedicated_relays_connected(&RelayMode::Custom(one)));
        assert!(keep_dedicated_relays_connected(&RelayMode::Custom(two)));
    }

    #[tokio::test]
    #[allow(clippy::similar_names, clippy::too_many_lines)]
    async fn agent_hot_standby_recovers_authenticated_alpn_after_relay_fault() {
        use iroh::tls::CaTlsConfig;

        let (relay_a_map, relay_a_url, relay_a) = iroh::test_utils::run_relay_server_with(false)
            .await
            .expect("start relay A");
        let (_relay_b_map, _relay_b_url, relay_b) = iroh::test_utils::run_relay_server_with(false)
            .await
            .expect("start relay B");
        let relay_b_addr = relay_b.https_addr().expect("relay B HTTPS address");
        let relay_b_proxy =
            RestartableTcpProxy::start("127.0.0.1:0".parse().unwrap(), relay_b_addr).await;
        let relay_b_url: RelayUrl = format!("https://{}", relay_b_proxy.addr)
            .parse()
            .expect("proxy relay URL");
        let relay_b_config =
            std::sync::Arc::new(iroh_relay::RelayConfig::new(relay_b_url.clone(), None));
        let relay_map = RelayMap::from_iter([
            relay_a_map
                .get(&relay_a_url)
                .expect("relay A configuration"),
            relay_b_config,
        ]);
        assert!(keep_dedicated_relays_connected(&RelayMode::Custom(
            relay_map.clone()
        )));

        let controller = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Custom(relay_map.clone()))
            .keep_relays_connected(true)
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .alpns(vec![AGENT_ALPN.to_vec()])
            .bind()
            .await
            .expect("build controller endpoint");
        let agent = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Custom(relay_map))
            .keep_relays_connected(true)
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build Agent endpoint");

        tokio::time::timeout(Duration::from_secs(10), async {
            while relay_a.metrics().server.accepts.get() < 2
                || relay_b.metrics().server.accepts.get() < 2
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Agent and Controller did not preconnect both relays");

        let relay_b_proxy_addr = relay_b_proxy.stop().await;
        let refused = TcpStream::connect(relay_b_proxy_addr)
            .await
            .expect_err("stopped Relay-B proxy accepted a connection");
        assert_eq!(refused.kind(), io::ErrorKind::ConnectionRefused);

        let echo = tokio::spawn({
            let controller = controller.clone();
            async move {
                let mut connections = Vec::new();
                for _ in 0..2 {
                    let connection = controller
                        .accept()
                        .await
                        .expect("incoming Agent connection")
                        .await
                        .expect("authenticated ALPN handshake");
                    let (mut send, mut recv) = connection.accept_bi().await.expect("accept stream");
                    let bytes = recv.read_to_end(1024).await.expect("read Agent payload");
                    send.write_all(&bytes).await.expect("echo Agent payload");
                    send.finish().expect("finish Agent echo");
                    connections.push(connection);
                }
                connections
            }
        });

        let relay_a_target = EndpointAddr::new(controller.id()).with_relay_url(relay_a_url);
        let first = agent
            .connect(relay_a_target, AGENT_ALPN)
            .await
            .expect("connect Agent through relay A");
        let (mut send, mut recv) = first.open_bi().await.expect("open relay A stream");
        send.write_all(b"before-fault")
            .await
            .expect("write relay A");
        send.finish().expect("finish relay A");
        assert_eq!(recv.read_to_end(1024).await.unwrap(), b"before-fault");

        relay_a.shutdown().await.expect("stop relay A");
        first.close(0_u32.into(), b"relay A failed");
        let relay_b_proxy = RestartableTcpProxy::start(relay_b_proxy_addr, relay_b_addr).await;
        let relay_b_target = EndpointAddr::new(controller.id()).with_relay_url(relay_b_url);
        let second = tokio::time::timeout(
            Duration::from_secs(15),
            agent.connect(relay_b_target, AGENT_ALPN),
        )
        .await
        .expect("Agent Relay-B transition exceeded fifteen seconds")
        .expect("connect Agent through restored relay B");
        let (mut send, mut recv) = second.open_bi().await.expect("open relay B stream");
        send.write_all(b"after-fault").await.expect("write relay B");
        send.finish().expect("finish relay B");
        assert_eq!(recv.read_to_end(1024).await.unwrap(), b"after-fault");

        let connections = tokio::time::timeout(Duration::from_secs(3), echo)
            .await
            .expect("Agent echo task timeout")
            .expect("Agent echo task");
        assert_eq!(connections.len(), 2);
        second.close(0_u32.into(), b"test complete");
        agent.close().await;
        controller.close().await;
        drop(connections);
        relay_b_proxy.stop().await;
        relay_b.shutdown().await.expect("stop relay B");
    }

    /// Starts a relay whose self-signed certificate is returned so controllers
    /// can pin it as an extra relay TLS root.
    async fn start_private_relay() -> (
        iroh_relay::server::Server,
        std::net::SocketAddr,
        rustls_pki_types::CertificateDer<'static>,
    ) {
        let (certs, server_config) =
            iroh_relay::server::testing::self_signed_tls_certs_and_config();
        let localhost = (std::net::Ipv4Addr::LOCALHOST, 0);
        let mut relay = iroh_relay::server::RelayConfig::new(localhost);
        relay.tls = Some(iroh_relay::server::TlsConfig::new(
            localhost,
            iroh_relay::server::CertConfig::Manual { server_config },
        ));
        relay.key_cache_capacity = Some(1024);
        relay.access = std::sync::Arc::new(iroh_relay::server::AllowAll);
        let mut config = iroh_relay::server::ServerConfig::default();
        config.relay = Some(relay);
        let server = iroh_relay::server::Server::spawn(config)
            .await
            .expect("spawn private relay");
        let address = server.https_addr().expect("private relay https address");
        (
            server,
            address,
            certs.into_iter().next().expect("private relay certificate"),
        )
    }

    #[tokio::test]
    #[allow(clippy::similar_names, clippy::too_many_lines)]
    async fn fenced_agent_supervisor_redials_over_hot_standby_and_advances_epoch() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        let temp_root = if cfg!(target_os = "macos") {
            "/private/tmp"
        } else {
            "/tmp"
        };
        let directory = PathBuf::from(temp_root).join(format!("ocsa-{}", Uuid::now_v7().simple()));
        std::fs::create_dir(&directory).expect("create relay failover fixture");
        let privd_socket = PathBuf::from(format!("/tmp/ocsm-{}.sock", Uuid::now_v7().simple()));
        let privd_listener =
            tokio::net::UnixListener::bind(&privd_socket).expect("bind privd snapshot fixture");
        let privd_server = tokio::spawn(serve_snapshots(privd_listener));
        let privd =
            PrivdClient::new(privd_socket.clone(), Duration::from_secs(2)).expect("privd client");
        require_healthy_snapshot(privd.snapshot().await.expect("startup snapshot"))
            .expect("healthy startup snapshot");

        let (relay_a, relay_a_addr, relay_a_cert) = start_private_relay().await;
        let relay_a_url: RelayUrl = format!("https://{relay_a_addr}")
            .parse()
            .expect("relay A URL");
        let (relay_b, relay_b_addr, relay_b_cert) = start_private_relay().await;
        // Relay B stays disabled behind a stopped proxy until the fault phase,
        // so only relay A carries the initial controller session.
        let disabled_proxy =
            RestartableTcpProxy::start("127.0.0.1:0".parse().unwrap(), relay_b_addr).await;
        let relay_b_proxy_addr = disabled_proxy.stop().await;
        let refused = TcpStream::connect(relay_b_proxy_addr)
            .await
            .expect_err("disabled relay B accepted a connection");
        assert_eq!(refused.kind(), io::ErrorKind::ConnectionRefused);
        let relay_b_url: RelayUrl = format!("https://{relay_b_proxy_addr}")
            .parse()
            .expect("relay B URL");
        let relay_map = RelayMap::from_iter([
            iroh_relay::RelayConfig::new(relay_a_url.clone(), None),
            iroh_relay::RelayConfig::new(relay_b_url.clone(), None),
        ]);
        assert!(keep_dedicated_relays_connected(&RelayMode::Custom(
            relay_map.clone()
        )));

        let node_id = Uuid::now_v7();
        let agent_key = iroh::SecretKey::generate();
        let controller_key = iroh::SecretKey::generate();
        let signing = SigningKey::from_bytes(&[7; 32]);
        let trust = std::sync::Arc::new(FencedSessionTrust {
            signing: signing.clone(),
            node_id,
            endpoint_id: agent_key.public(),
            next_epoch: std::sync::atomic::AtomicU64::new(41),
        });
        let trust_listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind trust fixture");
        let trust_addr = trust_listener.local_addr().expect("trust fixture address");
        let (trust_stop, trust_stop_rx) = oneshot::channel::<()>();
        let trust_server = tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(TrustServiceServer::from_arc(trust.clone()))
                .serve_with_incoming_shutdown(TcpListenerStream::new(trust_listener), async move {
                    trust_stop_rx.await.ok();
                }),
        );
        let trust_channel = tonic::transport::Endpoint::from_shared(format!("http://{trust_addr}"))
            .expect("trust endpoint")
            .connect()
            .await
            .expect("connect trust fixture");
        let policy = IdentityPolicy::new(
            HashMap::from([(agent_key.public(), node_id.as_bytes().to_vec())]),
            HashSet::new(),
        );
        let service = IrohTransportService::new_with_fence_policy(
            16,
            policy.clone(),
            Some(std::sync::Arc::new(test_command_keyring())),
            true,
        );
        let router = ocservia_transportd::build_router_with_trust_tls_roots(
            controller_key,
            RelayMode::Custom(relay_map.clone()),
            policy,
            TrustAuthority::new(trust_channel),
            vec![relay_a_cert.clone(), relay_b_cert.clone()],
            &service,
        )
        .await
        .expect("build relay failover transportd");
        let controller_id = router.endpoint().id();

        let telemetry_batches = std::sync::Arc::new(AtomicUsize::new(0));
        let command_results = std::sync::Arc::new(AtomicUsize::new(0));
        let events_watcher = tokio::spawn({
            let service = service.clone();
            let telemetry_batches = telemetry_batches.clone();
            let command_results = command_results.clone();
            async move {
                let mut events = service
                    .watch_events(tonic::Request::new(WatchEventsRequest {
                        after_event_id: Vec::new(),
                    }))
                    .await
                    .expect("watch transport events")
                    .into_inner();
                while let Some(event) = events.next().await {
                    let event = event.expect("valid transport event");
                    if event.r#type == i32::from(TransportEventType::Telemetry) {
                        telemetry_batches.fetch_add(1, Ordering::SeqCst);
                    } else if event.r#type == i32::from(TransportEventType::CommandResult) {
                        command_results.fetch_add(1, Ordering::SeqCst);
                    }
                }
            }
        });

        let journal_path = directory.join("agent.db");
        let supervisor = tokio::spawn({
            let journal_path = journal_path.clone();
            let relay_map = relay_map.clone();
            let relay_tls_roots = vec![relay_a_cert.clone(), relay_b_cert.clone()];
            let agent_key = agent_key.clone();
            async move {
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
                    boot_id: "relay-failover-test-boot",
                    os_release: "relay-failover-test-os",
                    agent_instance_id: Uuid::now_v7(),
                    command_keys: &command_keys,
                    sealing_keys: &sealing_keys,
                    fence_epoch_floor: &mut fence_epoch_floor,
                    synthetic_barrier_file: None,
                };
                let relay_mode = RelayMode::Custom(relay_map);
                let dial_relay_mode = relay_mode.clone();
                let transport = QuicTransportConfig::builder()
                    .max_concurrent_bidi_streams(VarInt::from_u32(
                        u32::try_from(MAX_WRITE_QUEUE).expect("write queue fits u32"),
                    ))
                    .build();
                // Relay-only transports keep the localhost fixture honest:
                // the session must ride the dedicated relays, never a
                // same-host direct path.
                let bind_endpoint: EndpointFactory = Box::new(move || {
                    let agent_key = agent_key.clone();
                    let relay_mode = relay_mode.clone();
                    let transport = transport.clone();
                    let relay_tls_roots = relay_tls_roots.clone();
                    Box::pin(async move {
                        Endpoint::builder(presets::Minimal)
                            .secret_key(agent_key)
                            .relay_mode(relay_mode)
                            .keep_relays_connected(true)
                            .ca_tls_config(
                                iroh::tls::CaTlsConfig::default().with_extra_roots(relay_tls_roots),
                            )
                            .clear_address_lookup()
                            .clear_ip_transports()
                            .transport_config(transport)
                            .bind()
                            .await
                    })
                });
                supervise_controller_sessions(
                    bind_endpoint,
                    controller_id,
                    &dial_relay_mode,
                    &mut session,
                )
                .await;
            }
        });

        let node_connection = |relay_url: String| {
            let service = service.clone();
            async move {
                loop {
                    if let Ok(response) = service
                        .get_node_connection(tonic::Request::new(GetNodeConnectionRequest {
                            node_id: node_id.as_bytes().to_vec(),
                        }))
                        .await
                    {
                        let metadata = response.into_inner();
                        if metadata.path_detail.contains(&relay_url) {
                            return metadata;
                        }
                    }
                    tokio::time::sleep(Duration::from_millis(200)).await;
                }
            }
        };

        let before = tokio::time::timeout(Duration::from_secs(30), {
            let telemetry_batches = telemetry_batches.clone();
            async move {
                let metadata = node_connection(relay_a_url.to_string()).await;
                // The relay-A session must be functional, not merely present.
                while telemetry_batches.load(Ordering::SeqCst) == 0 {
                    tokio::time::sleep(Duration::from_millis(200)).await;
                }
                metadata
            }
        })
        .await
        .expect("Agent session through relay A did not become healthy");
        assert_eq!(before.endpoint_id, agent_key.public().as_bytes());
        assert_eq!(before.owner_epoch, 41);
        let first_fence = service
            .get_owner_fence(tonic::Request::new(GetOwnerFenceRequest {
                node_id: node_id.as_bytes().to_vec(),
            }))
            .await
            .expect("query first owner fence")
            .into_inner()
            .fence
            .expect("first owner fence");
        let first_command = live_signed_noop(&signing, &node_id, &agent_key.public(), &first_fence);
        assert!(
            service
                .send_command(tonic::Request::new(SendCommandRequest {
                    node_id: node_id.as_bytes().to_vec(),
                    command_envelope: first_command.encode_to_vec(),
                }))
                .await
                .expect("dispatch under first owner epoch")
                .into_inner()
                .accepted
        );
        tokio::time::timeout(Duration::from_secs(5), async {
            while command_results.load(Ordering::SeqCst) < 1 {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .expect("first owner epoch command did not complete");
        let before_connected_at = before.connected_at.expect("relay A connected_at");
        // Captured before the fault: session 1's next heartbeat is 30s away,
        // so any additional batch inside the failover window can only come
        // from the replacement session.
        let batches_at_fault = telemetry_batches.load(Ordering::SeqCst);

        relay_a.shutdown().await.expect("stop relay A");
        let relay_b_proxy = RestartableTcpProxy::start(relay_b_proxy_addr, relay_b_addr).await;

        let after = tokio::time::timeout(
            Duration::from_secs(15),
            node_connection(relay_b_url.to_string()),
        )
        .await
        .expect("Agent did not fail over to relay B within fifteen seconds");
        let after_connected_at = after.connected_at.expect("relay B connected_at");
        assert!(
            (after_connected_at.seconds, after_connected_at.nanos)
                > (before_connected_at.seconds, before_connected_at.nanos),
            "relay B must carry a replacement session, not a path migration"
        );
        assert_eq!(after.endpoint_id, agent_key.public().as_bytes());
        assert_eq!(after.owner_epoch, 42);
        assert_eq!(
            after.agent_instance_id, before.agent_instance_id,
            "the same Agent process must own the replacement session"
        );
        let stale_command = live_signed_noop(&signing, &node_id, &agent_key.public(), &first_fence);
        let stale_error = service
            .send_command(tonic::Request::new(SendCommandRequest {
                node_id: node_id.as_bytes().to_vec(),
                command_envelope: stale_command.encode_to_vec(),
            }))
            .await
            .expect_err("superseded owner epoch must be rejected");
        assert_eq!(stale_error.code(), tonic::Code::PermissionDenied);
        let second_fence = service
            .get_owner_fence(tonic::Request::new(GetOwnerFenceRequest {
                node_id: node_id.as_bytes().to_vec(),
            }))
            .await
            .expect("query replacement owner fence")
            .into_inner()
            .fence
            .expect("replacement owner fence");
        assert_eq!(second_fence.owner_epoch, 42);
        let second_command =
            live_signed_noop(&signing, &node_id, &agent_key.public(), &second_fence);
        assert!(
            service
                .send_command(tonic::Request::new(SendCommandRequest {
                    node_id: node_id.as_bytes().to_vec(),
                    command_envelope: second_command.encode_to_vec(),
                }))
                .await
                .expect("dispatch under replacement owner epoch")
                .into_inner()
                .accepted
        );
        tokio::time::timeout(Duration::from_secs(5), async {
            while command_results.load(Ordering::SeqCst) < 2 {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .expect("replacement owner epoch command did not complete");

        tokio::time::timeout(Duration::from_secs(20), async {
            while telemetry_batches.load(Ordering::SeqCst) <= batches_at_fault {
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        })
        .await
        .expect("replacement session did not deliver telemetry");
        assert!(
            !supervisor.is_finished(),
            "the session supervisor keeps running after the relay failover"
        );

        supervisor.abort();
        assert!(
            supervisor
                .await
                .expect_err("aborted Agent supervisor")
                .is_cancelled()
        );
        events_watcher.abort();
        privd_server.abort();
        ocservia_transportd::shutdown(&service, router)
            .await
            .expect("shutdown relay failover transportd");
        relay_b_proxy.stop().await;
        relay_b.shutdown().await.expect("stop relay B");
        trust_stop.send(()).ok();
        trust_server
            .await
            .expect("trust fixture task")
            .expect("trust fixture server");
        let reopened = Journal::open(&journal_path).expect("reopen Agent journal");
        assert_eq!(
            reopened
                .owner_fence_epoch_floor()
                .expect("durable owner floor"),
            42
        );
        std::fs::remove_file(&privd_socket).expect("remove privd snapshot socket");
        std::fs::remove_dir_all(directory).expect("remove relay failover fixture");
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
