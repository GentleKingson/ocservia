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
    CertificateCsrRequest, CertificateP12Request, CertificateRevokeRequest, ConfigApplyRequest,
    ConfigPlanRequest, DesiredEffectObserveRequest, DesiredEffectState, ErrorKind,
    GroupApplyRequest, IpBanRemoveRequest, MAX_MANAGED_RESOURCES, PrivdResponse,
    ServiceReloadRequest, SessionMutationRequest, UserDisableRequest, UserEnableRequest,
    UserSecretRequest, privd_request, privd_response,
};
use ocservia_command_authorization::{
    ControllerCommandKeyring, VerifiedSessionGrant, load_verification_key,
};
use ocservia_command_journal::{
    CommandRecord, CommandState, Journal, OFFLINE_RECOVERY_RETENTION_SECONDS, TelemetryInsert,
};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, ArtifactChunk, ArtifactFetchRequest, CommandDeliveryMode,
    CommandEnvelope, CommandResult, CommandResultState, GroupObservation, HandshakeResult,
    IpBanObservation, MetricSample, ObservedSnapshot, SemanticPayloadHashVersion, SessionHandshake,
    SessionHandshakeResponse, SessionObservation, TelemetryBatch, TelemetryDropCounters,
    TelemetryPriority, UserObservation, command_envelope,
};
use prost::Message;
use sha2::Digest;
use uuid::Uuid;

use zeroize::Zeroizing;

const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    ocservia_observability::init("ocservia-agent")?;
    ocservia_agent::ensure_unprivileged(rustix::process::geteuid().as_raw())?;
    let config = parse_args()?;
    let command_keys = load_controller_command_keys(&config)?;
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

    let command_keys = command_keys.expect("non-probe Agent requires command keys");

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
            let endpoint = match Endpoint::builder(presets::N0)
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
                artifact_dir: &config.artifact_dir,
                command_keys: &command_keys,
            };
            match connect_once(&endpoint, controller, &mut session).await {
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
    artifact_dir: &'a PathBuf,
    command_keys: &'a ControllerCommandKeyring,
}

struct ActiveSessionAuthority {
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    expires_at_unix_seconds: i64,
}

fn supported_capabilities() -> Vec<String> {
    [
        "ocserv.status.read",
        "ocserv.version.read",
        "ocserv.sessions.read",
        "ocserv.ip_bans.read",
        "ocserv.config_fingerprint.read",
        "synthetic.noop",
        "synthetic.echo",
        "command.semantic-hash.v1",
        "command.strict-wire.v1",
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
    ]
    .into_iter()
    .map(str::to_owned)
    .collect()
}

async fn connect_once(
    endpoint: &Endpoint,
    controller: EndpointId,
    session: &mut SessionContext<'_>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let connection = endpoint
        .connect(EndpointAddr::new(controller), AGENT_ALPN)
        .await?;
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
    let authority = verify_session_authority(
        &response,
        &supported_capabilities,
        session.node_id,
        session.endpoint_id,
        session.command_keys,
    )?;
    tracing::info!(controller = %controller, "agent session accepted");
    let mut heartbeat = tokio::time::interval(Duration::from_secs(30));
    let session_lifetime = u64::try_from(
        authority
            .expires_at_unix_seconds
            .saturating_sub(unix_seconds()?),
    )?;
    let session_expiry = tokio::time::sleep(Duration::from_secs(session_lifetime));
    tokio::pin!(session_expiry);
    let mut sequence = 0_u64;
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            () = &mut session_expiry => return Err(invalid("Controller session grant expired").into()),
            stream = connection.accept_bi() => {
                let (send,recv)=stream?;
                handle_command_stream(send,recv,session,&authority).await?;
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

fn verify_session_authority(
    response: &SessionHandshakeResponse,
    supported_capabilities: &[String],
    node_id: Uuid,
    endpoint_id: EndpointId,
    command_keys: &ControllerCommandKeyring,
) -> Result<ActiveSessionAuthority, Box<dyn std::error::Error + Send + Sync>> {
    if response.protocol_major != 1 || response.protocol_minor != 1 {
        return Err(invalid("Controller session protocol is not mutation-capable").into());
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
    Ok(ActiveSessionAuthority {
        negotiated_capabilities: negotiated_capabilities.into_iter().collect(),
        authorization_revision,
        expires_at_unix_seconds: expires_at_seconds,
    })
}

fn unix_seconds() -> Result<i64, Box<dyn std::error::Error + Send + Sync>> {
    Ok(i64::try_from(
        SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)?
            .as_secs(),
    )?)
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
    if framed_length & (1 << 31) != 0 {
        return handle_artifact_stream(
            send,
            recv,
            framed_length & !(1 << 31),
            session.artifact_dir,
        )
        .await;
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
    let context = CommandContext {
        node_id: *session.node_id.as_bytes(),
        authorization_revision: authority.authorization_revision,
        capabilities: authority.negotiated_capabilities.clone(),
        session_expires_at_unix_seconds: authority.expires_at_unix_seconds,
        command_keys: session.command_keys.clone(),
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

async fn handle_artifact_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    length: u32,
    artifact_dir: &Path,
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
    if artifact.get_version_num() != 7
        || request.purpose != "certificate_p12"
        || request.max_bytes == 0
        || request.max_bytes > 64 * 1024 * 1024
        || !artifact_dir.is_absolute()
    {
        return Err(invalid("artifact request invalid").into());
    }
    let path = artifact_dir.join(format!("{artifact}.p12"));
    let metadata = tokio::fs::symlink_metadata(&path).await?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.len() == 0
        || metadata.len() > request.max_bytes
    {
        return Err(invalid("artifact resource invalid").into());
    }
    let data = tokio::fs::read(&path).await?;
    let digest = sha2::Sha256::digest(&data).to_vec();
    let mut offset = 0_usize;
    while offset < data.len() {
        let end = (offset + 256 * 1024).min(data.len());
        let eof = end == data.len();
        let chunk = ArtifactChunk {
            artifact_id: request.artifact_id.clone(),
            offset: u64::try_from(offset)?,
            data: data[offset..end].to_vec(),
            eof,
            sha256: if eof { digest.clone() } else { Vec::new() },
        }
        .encode_to_vec();
        send.write_all(&u32::try_from(chunk.len())?.to_be_bytes())
            .await?;
        send.write_all(&chunk).await?;
        offset = end;
    }
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
        Some(command_envelope::Payload::UserCreate(payload)) => {
            privd_request::Operation::UserCreate(UserSecretRequest {
                username: payload.username.clone(),
                sealed_password: payload.sealed_password.clone(),
                secret_key_id: payload.secret_key_id.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            privd_request::Operation::UserDisable(UserDisableRequest {
                username: payload.username.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            privd_request::Operation::UserEnable(UserEnableRequest {
                username: payload.username.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            privd_request::Operation::UserPasswordRotate(UserSecretRequest {
                username: payload.username.clone(),
                sealed_password: payload.sealed_password.clone(),
                secret_key_id: payload.secret_key_id.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            privd_request::Operation::GroupApply(GroupApplyRequest {
                group_name: payload.group_name.clone(),
                members: payload.members.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::ConfigPlan(payload)) => {
            privd_request::Operation::ConfigPlan(ConfigPlanRequest {
                candidate: payload.candidate.clone(),
                candidate_hash: payload.candidate_hash.clone(),
            })
        }
        Some(command_envelope::Payload::ConfigApply(payload)) => {
            privd_request::Operation::ConfigApply(ConfigApplyRequest {
                candidate: payload.candidate.clone(),
                candidate_hash: payload.candidate_hash.clone(),
                expected_current_hash: payload.expected_current_hash.clone(),
                desired_revision: payload.desired_revision,
            })
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            privd_request::Operation::CertificateCsr(CertificateCsrRequest {
                certificate_id: payload.certificate_id.clone(),
                common_name: payload.common_name.clone(),
                dns_names: payload.dns_names.clone(),
                key_bits: payload.key_bits,
            })
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            privd_request::Operation::CertificateRevoke(CertificateRevokeRequest {
                certificate_id: payload.certificate_id.clone(),
            })
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            privd_request::Operation::CertificateP12(CertificateP12Request {
                certificate_id: payload.certificate_id.clone(),
                artifact_id: payload.artifact_id.clone(),
                certificate_chain_pem: payload.certificate_chain_pem.clone(),
                sealed_password: payload.sealed_password.clone(),
                secret_key_id: payload.secret_key_id.clone(),
            })
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    };
    let expires_at = envelope
        .expires_at
        .as_ref()
        .ok_or(CommandError::Rejected("expires_at_missing"))?
        .seconds;
    let response = if desired_resource(envelope).is_some() {
        session
            .privd
            .call_desired(
                operation,
                &envelope.command_id,
                &envelope.idempotency_key,
                &envelope.semantic_payload_sha256,
                expires_at,
            )
            .await
    } else {
        session.privd.call(operation).await
    };
    match response {
        Ok(response) => match response.result {
            Some(privd_response::Result::Mutation(result)) if result.applied => {
                if let Some((resource_type, resource_key, revision)) = desired_resource(envelope) {
                    let result = result.encode_to_vec();
                    session.command_executor.complete_external_applied(
                        &command,
                        resource_type,
                        &resource_key,
                        revision,
                        &result,
                        now,
                    )
                } else {
                    session
                        .command_executor
                        .complete_external(&command, Ok(b"applied"), now)
                }
            }
            Some(privd_response::Result::ConfigPlan(result)) => session
                .command_executor
                .complete_external(&command, Ok(&result.encode_to_vec()), now),
            Some(privd_response::Result::ConfigApply(result)) => {
                let bytes = result.encode_to_vec();
                let Some((resource_type, resource_key, revision)) = desired_resource(envelope)
                else {
                    return Err(CommandError::Rejected("config_apply_invalid"));
                };
                session.command_executor.complete_external_applied(
                    &command,
                    resource_type,
                    &resource_key,
                    revision,
                    &bytes,
                    now,
                )
            }
            Some(privd_response::Result::CertificateCsr(result)) => session
                .command_executor
                .complete_external(&command, Ok(&result.encode_to_vec()), now),
            Some(privd_response::Result::CertificateRevoke(result)) => session
                .command_executor
                .complete_external(&command, Ok(&result.encode_to_vec()), now),
            Some(privd_response::Result::CertificateP12(result)) => session
                .command_executor
                .complete_external(&command, Ok(&result.encode_to_vec()), now),
            Some(privd_response::Result::Error(error)) if terminal_privd_error(error.kind) => {
                let code = terminal_privd_error_code(error.kind).unwrap_or("privd_rejected");
                session
                    .command_executor
                    .complete_external(&command, Err(code), now)
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
    let operation = match payload {
        command_envelope::Payload::SessionDisconnect(_)
        | command_envelope::Payload::SessionTerminate(_) => {
            privd_request::Operation::SessionList(ocservia_agent_protocol::ReadRequest {})
        }
        command_envelope::Payload::IpBanRemove(_) => {
            privd_request::Operation::IpBanList(ocservia_agent_protocol::ReadRequest {})
        }
        payload if desired_effect_identity(payload).is_some() => {
            let Some((mutation_kind, resource_key, desired_revision)) =
                desired_effect_identity(payload)
            else {
                return ExternalEffectObservation::Unknown;
            };
            privd_request::Operation::DesiredEffectObserve(DesiredEffectObserveRequest {
                mutation_kind: mutation_kind.to_owned(),
                resource_key: resource_key.to_owned(),
                desired_revision,
            })
        }
        _ => return ExternalEffectObservation::Unknown,
    };
    let response = if desired_effect_identity(payload).is_some() {
        let Some(expires_at) = envelope.expires_at.as_ref() else {
            return ExternalEffectObservation::Unknown;
        };
        privd
            .call_desired(
                operation,
                &envelope.command_id,
                &envelope.idempotency_key,
                &envelope.semantic_payload_sha256,
                expires_at.seconds,
            )
            .await
    } else {
        privd.call(operation).await
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
    artifact_dir: PathBuf,
    relay_mode: RelayMode,
    command_verification_key_files: Vec<PathBuf>,
}

fn load_controller_command_keys(
    config: &Config,
) -> Result<Option<ControllerCommandKeyring>, Box<dyn std::error::Error + Send + Sync>> {
    if config.probe_only {
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

fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
        journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
        privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
        controller: None,
        node_id: None,
        probe_only: false,
        artifact_dir: PathBuf::from("/var/lib/ocservia-privd/certificates/artifacts"),
        relay_mode: RelayMode::Default,
        command_verification_key_files: Vec::new(),
    };
    let mut relay_mode = String::from("default");
    let mut relay_urls = Vec::new();
    let mut relay_token_file = None;
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
            "--artifact-dir" => {
                config.artifact_dir = PathBuf::from(required(&mut args, "--artifact-dir")?);
            }
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
            "--relay-mode" => relay_mode = required(&mut args, "--relay-mode")?,
            "--relay-url" => relay_urls.push(required(&mut args, "--relay-url")?),
            "--relay-token-file" => {
                relay_token_file = Some(PathBuf::from(required(&mut args, "--relay-token-file")?));
            }
            _ => return Err(invalid("unknown agent argument")),
        }
    }
    if !config.artifact_dir.is_absolute()
        || config
            .command_verification_key_files
            .iter()
            .any(|path| !path.is_absolute())
        || relay_token_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(invalid(
            "artifact, Controller command key, and relay token paths must be absolute",
        ));
    }
    config.relay_mode = build_relay_mode(&relay_mode, relay_urls, relay_token_file.as_deref())?;
    Ok(config)
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
    use ocservia_agent_protocol::{
        DesiredEffectObservation, PrivdRequest, read_frame, write_frame,
    };
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        ConfigApply, ConfigPlan, UserPasswordRotate,
    };
    use std::os::unix::fs::PermissionsExt as _;

    fn command_key_config(probe_only: bool) -> Config {
        Config {
            identity_dir: PathBuf::from("/var/lib/ocservia-agent/identity"),
            journal: PathBuf::from("/var/lib/ocservia-agent/agent.db"),
            privd_socket: PathBuf::from("/run/ocserv-platform/privd.sock"),
            controller: None,
            node_id: None,
            probe_only,
            artifact_dir: PathBuf::from("/var/lib/ocservia-privd/certificates/artifacts"),
            relay_mode: RelayMode::Default,
            command_verification_key_files: Vec::new(),
        }
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
            sealed_password: vec![0xa5; 64],
            secret_key_id: "node-key-1".to_owned(),
            desired_revision: 7,
        });
        let result = privd_response::Result::Mutation(ocservia_agent_protocol::MutationResult {
            applied: true,
        });

        assert_eq!(
            map_external_effect(&payload, result),
            ExternalEffectObservation::Unknown
        );
    }

    #[tokio::test]
    async fn password_reconcile_uses_non_secret_authoritative_effect_store() {
        let socket = PathBuf::from(format!("/tmp/ocsm-{}.sock", Uuid::now_v7().simple()));
        let listener = tokio::net::UnixListener::bind(&socket).expect("bind effect fixture");
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            let request: PrivdRequest = read_frame(&mut stream).await.expect("request");
            let Some(privd_request::Operation::DesiredEffectObserve(observe)) = request.operation
            else {
                panic!("desired effect observation required")
            };
            assert_eq!(observe.mutation_kind, "user_password_rotate");
            assert_eq!(observe.resource_key, "alice");
            assert_eq!(observe.desired_revision, 7);
            write_frame(
                &mut stream,
                &PrivdResponse {
                    request_id: request.request_id,
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
                    sealed_password: vec![0xa5; 64],
                    secret_key_id: "node-key-1".to_owned(),
                    desired_revision: 7,
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
                let Some(privd_request::Operation::DesiredEffectObserve(observe)) =
                    request.operation
                else {
                    panic!("config desired-effect observation required")
                };
                assert_eq!(observe.mutation_kind, "config_apply");
                assert_eq!(observe.resource_key, "ocserv.conf");
                assert_eq!(observe.desired_revision, 2);
                write_frame(
                    &mut stream,
                    &PrivdResponse {
                        request_id: request.request_id,
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
