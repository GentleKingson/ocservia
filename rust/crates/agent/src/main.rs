use std::collections::HashSet;
use std::env;
use std::io;
use std::path::PathBuf;
use std::time::{Duration, SystemTime};

use iroh::endpoint::{QuicTransportConfig, RelayMode, VarInt, presets};
use iroh::{Endpoint, EndpointAddr, EndpointId};
use ocservia_agent::{
    CommandContext, CommandError, CommandExecutor, ExternalPreparation, MAX_COMMAND_BYTES,
    MAX_WRITE_QUEUE, PrivdClient,
};
use ocservia_agent_protocol::{
    ErrorKind, IpBanRemoveRequest, PrivdResponse, ServiceReloadRequest, SessionMutationRequest,
    privd_request, privd_response,
};
use ocservia_command_journal::{CommandRecord, CommandState, Journal, TelemetryInsert};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandDeliveryMode, CommandEnvelope, CommandResult,
    CommandResultState, HandshakeResult, IpBanObservation, MetricSample, ObservedSnapshot,
    SemanticPayloadHashVersion, SessionHandshake, SessionHandshakeResponse, SessionObservation,
    TelemetryBatch, TelemetryDropCounters, TelemetryPriority, command_envelope,
};
use prost::Message;
use uuid::Uuid;

const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    ocservia_observability::init("ocservia-agent")?;
    ocservia_agent::ensure_unprivileged(rustix::process::geteuid().as_raw())?;
    let config = parse_args()?;
    let mut journal = Journal::open(&config.journal)?;
    let mut command_executor = CommandExecutor::new(Journal::open(&config.journal)?);
    let privd = PrivdClient::new(config.privd_socket, Duration::from_secs(5))?;
    let observations = privd.snapshot().await?;
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
    if config.probe_only {
        return Ok(());
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
    let endpoint = Endpoint::builder(presets::N0)
        .secret_key(identity.secret_key().clone())
        .relay_mode(RelayMode::Default)
        .transport_config(transport)
        .bind()
        .await?;
    let boot_id = ocservia_agent::read_boot_id().await?;
    let os_release = ocservia_agent::read_os_release().await?;
    let agent_instance_id = Uuid::now_v7();
    let run = async {
        let mut attempt = 0_u32;
        let backoff = ocservia_agent::Backoff::default();
        loop {
            let mut session = SessionContext {
                node_id,
                endpoint_id: identity.endpoint_id(),
                privd: &privd,
                journal: &mut journal,
                command_executor: &mut command_executor,
                boot_id: &boot_id,
                os_release: &os_release,
                agent_instance_id,
            };
            match connect_once(&endpoint, controller, &mut session).await {
                Ok(()) => attempt = 0,
                Err(error) => {
                    tracing::warn!(error = %error, attempt, "controller connection ended");
                    attempt = attempt.saturating_add(1);
                }
            }
            let delay = backoff.delay(attempt, &mut rand::rng());
            tokio::time::sleep(delay).await;
        }
    };
    tokio::select! {
        () = run => {}
        () = shutdown_signal() => tracing::info!("agent shutdown requested"),
    }
    endpoint.close().await;
    Ok(())
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
}

async fn connect_once(
    endpoint: &Endpoint,
    controller: EndpointId,
    session: &mut SessionContext<'_>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let connection = endpoint
        .connect(EndpointAddr::new(controller), AGENT_ALPN)
        .await?;
    let handshake = SessionHandshake {
        protocol_major: 1,
        protocol_minor: 0,
        agent_version: env!("CARGO_PKG_VERSION").to_owned(),
        controller_version: String::new(),
        node_id: session.node_id.as_bytes().to_vec(),
        endpoint_id: session.endpoint_id.as_bytes().to_vec(),
        capabilities: vec![
            "ocserv.status.read".to_owned(),
            "ocserv.version.read".to_owned(),
            "ocserv.sessions.read".to_owned(),
            "ocserv.ip_bans.read".to_owned(),
            "ocserv.config_fingerprint.read".to_owned(),
            "synthetic.noop".to_owned(),
            "synthetic.echo".to_owned(),
            "command.semantic-hash.v1".to_owned(),
            "command.strict-wire.v1".to_owned(),
            "ocserv.session.disconnect".to_owned(),
            "ocserv.session.terminate".to_owned(),
            "ocserv.ip_ban.remove".to_owned(),
            "ocserv.service.reload".to_owned(),
        ],
        ocserv_version: "unknown".to_owned(),
        os_release: session.os_release.to_owned(),
        boot_id: session.boot_id.to_owned(),
        agent_instance_id: session.agent_instance_id.as_bytes().to_vec(),
        supported_compressions: Vec::new(),
        max_message_size: 1024 * 1024,
        time: Some(SystemTime::now().into()),
        nonce: Uuid::now_v7().as_bytes().to_vec(),
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
    tracing::info!(controller = %controller, "agent session accepted");
    let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
    let mut sequence = 0_u64;
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            stream = connection.accept_bi() => {
                let (send,recv)=stream?;
                handle_command_stream(send,recv,session).await?;
            },
            _ = heartbeat.tick() => {
                let observations=session.privd.snapshot().await?;
                sequence=sequence.saturating_add(1);
                let drops=session.journal.telemetry_drop_counters()?;
                let batch=build_telemetry(session,sequence,&observations,&connection,drops);
                let payload=batch.encode_to_vec();
                let batch_id: [u8;16]=batch.batch_id.as_slice().try_into().map_err(|_| invalid("telemetry batch ID invalid"))?;
                let now=SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?.as_secs();
                session.journal.enqueue_telemetry(&TelemetryInsert { batch_id: &batch_id, priority: 2, observed_at: i64::try_from(now)?, expires_at: i64::try_from(now+24*60*60)?, payload: &payload, now: i64::try_from(now)?, max_bytes: 64*1024*1024 })?;
                for (pending_id,pending) in session.journal.telemetry_pending(32)? {
                    send_telemetry(&connection,&pending).await?;
                    session.journal.acknowledge_telemetry(&pending_id)?;
                }
                tracing::info!(node_id = %session.node_id, sequence, "agent telemetry delivered");
            },
        }
    }
}

async fn handle_command_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    session: &mut SessionContext<'_>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut length = [0_u8; 4];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| invalid("command length timed out"))??;
    let length = u32::from_be_bytes(length) as usize;
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
    let context = CommandContext {
        node_id: *session.node_id.as_bytes(),
        observed_revision: None,
        capabilities: HashSet::from([
            "synthetic.noop",
            "synthetic.echo",
            "ocserv.session.disconnect",
            "ocserv.session.terminate",
            "ocserv.ip_ban.remove",
            "ocserv.service.reload",
        ]),
        now_unix_seconds,
        cancelled: false,
    };
    let external = matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::SessionDisconnect(_)
                | command_envelope::Payload::SessionTerminate(_)
                | command_envelope::Payload::IpBanRemove(_)
                | command_envelope::Payload::ServiceReload(_)
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

async fn execute_external_command(
    session: &mut SessionContext<'_>,
    envelope: &CommandEnvelope,
    context: &CommandContext,
    now: i64,
) -> Result<ocservia_agent::CommandOutcome, CommandError> {
    let mode = CommandDeliveryMode::try_from(envelope.delivery_mode)
        .unwrap_or(CommandDeliveryMode::Unspecified);
    let command = match mode {
        CommandDeliveryMode::ExecuteOrReplay => {
            match session
                .command_executor
                .prepare_external(envelope, context)?
            {
                ExternalPreparation::Execute(command) => command,
                ExternalPreparation::Replay(outcome) => return Ok(outcome),
            }
        }
        CommandDeliveryMode::ReconcileOnly => {
            let observed = observe_external_effect(session.privd, envelope).await;
            return session
                .command_executor
                .reconcile_external(envelope, context, observed);
        }
        CommandDeliveryMode::RetryIfEffectAbsent => {
            session.command_executor.retry_external(envelope, context)?
        }
        CommandDeliveryMode::Unspecified => {
            return Err(CommandError::Rejected("delivery_mode_invalid"));
        }
    };
    let operation = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            privd_request::Operation::SessionDisconnect(SessionMutationRequest {
                session_id: payload.session_id.clone(),
                boot_id: payload.boot_id.clone(),
            })
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            privd_request::Operation::SessionTerminate(SessionMutationRequest {
                session_id: payload.session_id.clone(),
                boot_id: payload.boot_id.clone(),
            })
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            privd_request::Operation::IpBanRemove(IpBanRemoveRequest {
                ip: payload.ip.clone(),
            })
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            privd_request::Operation::ServiceReload(ServiceReloadRequest {})
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    };
    match session.privd.call(operation).await {
        Ok(response) => match response.result {
            Some(privd_response::Result::Mutation(result)) if result.applied => session
                .command_executor
                .complete_external(&command, Ok(b"applied"), now),
            Some(privd_response::Result::Error(error))
                if matches!(
                    ErrorKind::try_from(error.kind).unwrap_or(ErrorKind::Unspecified),
                    ErrorKind::InvalidRequest
                        | ErrorKind::PermissionDenied
                        | ErrorKind::MalformedOutput
                ) =>
            {
                session
                    .command_executor
                    .complete_external(&command, Err("privd_rejected"), now)
            }
            _ => session.command_executor.mark_external_unknown(
                &command,
                "privd_outcome_unknown",
                now,
            ),
        },
        Err(_) => {
            session
                .command_executor
                .mark_external_unknown(&command, "privd_transport_unknown", now)
        }
    }
}

async fn observe_external_effect(privd: &PrivdClient, envelope: &CommandEnvelope) -> Option<bool> {
    let target_boot = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(target)) => Some(&target.boot_id),
        Some(command_envelope::Payload::SessionTerminate(target)) => Some(&target.boot_id),
        _ => None,
    };
    if let Some(target_boot) = target_boot {
        let current_boot = tokio::fs::read_to_string("/proc/sys/kernel/random/boot_id")
            .await
            .ok()?;
        if current_boot.trim() != target_boot {
            return None;
        }
    }
    let operation = match envelope.payload.as_ref()? {
        command_envelope::Payload::SessionDisconnect(_)
        | command_envelope::Payload::SessionTerminate(_) => {
            privd_request::Operation::SessionList(ocservia_agent_protocol::ReadRequest {})
        }
        command_envelope::Payload::IpBanRemove(_) => {
            privd_request::Operation::IpBanList(ocservia_agent_protocol::ReadRequest {})
        }
        _ => return None,
    };
    let response = privd.call(operation).await.ok()?;
    match (envelope.payload.as_ref()?, response.result?) {
        (
            command_envelope::Payload::SessionDisconnect(target),
            privd_response::Result::SessionList(current),
        ) => Some(
            !current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id),
        ),
        (
            command_envelope::Payload::SessionTerminate(target),
            privd_response::Result::SessionList(current),
        ) => Some(
            !current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id),
        ),
        (
            command_envelope::Payload::IpBanRemove(target),
            privd_response::Result::IpBanList(current),
        ) => Some(!current.bans.iter().any(|ban| ban.ip == target.ip)),
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
    CommandResult {
        command_id: record.command_id.to_vec(),
        idempotency_key: record.idempotency_key.to_vec(),
        payload_sha256: record.payload_sha256.to_vec(),
        state: state.into(),
        result: record.result.clone().unwrap_or_default(),
        error_code: record.error_code.clone().unwrap_or_default(),
        accepted_at: Some(prost_types::Timestamp {
            seconds: record.accepted_at,
            nanos: 0,
        }),
        completed_at: Some(prost_types::Timestamp {
            seconds: record.updated_at,
            nanos: 0,
        }),
        replayed,
        semantic_payload_hash_version: record.payload_hash_version,
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

fn build_telemetry(
    session: &SessionContext<'_>,
    sequence: u64,
    observations: &[PrivdResponse],
    connection: &iroh::endpoint::Connection,
    drops: [u64; 4],
) -> TelemetryBatch {
    let now = SystemTime::now();
    let mut service = serde_json::json!({"active_state":"unknown","sub_state":"unknown"});
    let mut version = "unknown".to_owned();
    let mut sessions = Vec::new();
    let mut ip_bans = Vec::new();
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
            _ => {}
        }
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
    TelemetryBatch {
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
    }
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
}

fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
        journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
        privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
        controller: None,
        node_id: None,
        probe_only: false,
    };
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
            _ => return Err(invalid("unknown agent argument")),
        }
    }
    Ok(config)
}

fn required(args: &mut impl Iterator<Item = String>, name: &str) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

fn invalid(detail: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, detail)
}
