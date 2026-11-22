mod artifacts;
mod commands;
mod session;
mod startup;
mod telemetry;
#[cfg(test)]
mod tests;
mod vpn_audit;
use crate::session::{SessionContext, agent_endpoint_factory, supervise_controller_sessions};
use crate::startup::{
    invalid, load_controller_command_keys, load_relay_tls_roots, parse_args,
    prepare_enrollment_if_requested, print_version_query, require_healthy_snapshot,
    run_one_shot_mode,
};
use iroh::endpoint::{QuicTransportConfig, VarInt};
use ocservia_agent::{CommandExecutor, MAX_WRITE_QUEUE, PrivdClient};
use ocservia_command_journal::Journal;
use std::time::Duration;
use uuid::Uuid;

const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";
const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const ARTIFACT_FRAME_MASK: u32 = 3 << 30;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    if print_version_query() {
        return Ok(());
    }
    ocservia_agent::ensure_unprivileged(rustix::process::geteuid().as_raw())?;
    let config = parse_args()?;
    if prepare_enrollment_if_requested(&config)? {
        return Ok(());
    }
    ocservia_observability::init(
        "ocservia-agent",
        ocservia_contracts::agent_upgrade::release_version(),
    )?;
    if run_one_shot_mode(&config).await? {
        return Ok(());
    }
    let command_keys = load_controller_command_keys(&config)?;
    let relay_tls_roots = std::sync::Arc::new(load_relay_tls_roots(&config)?);
    let mut command_executor = CommandExecutor::new(Journal::open(&config.journal)?);
    let privd = PrivdClient::new(config.privd_socket, Duration::from_secs(5))?;
    // The startup snapshot is a health gate: it fails fast when privd or the
    // fixed-path fixtures are unhealthy, before any command dispatch begins.
    require_healthy_snapshot(privd.snapshot().await?)?;
    if config.probe_only {
        return Ok(());
    }

    let command_keys = command_keys.expect("non-probe Agent requires command keys");
    let mut fence_epoch_floor = command_executor.journal().owner_fence_epoch_floor()?;
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
    let identity =
        ocservia_agent_protocol::identity::Identity::provision(&config.identity_dir, controller)?;
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
        command_executor: &mut command_executor,
        session_cache: telemetry::SessionCache::default(),
        boot_id: &boot_id,
        os_release: &os_release,
        agent_instance_id,
        command_keys: &command_keys,
        sealing_keys: &config.sealing_keys,
        fence_epoch_floor: &mut fence_epoch_floor,
    };
    let run =
        supervise_controller_sessions(bind_endpoint, controller, &config.relay_mode, &mut session);
    tokio::select! {
        () = run => {}
        () = shutdown_signal() => tracing::info!("agent shutdown requested"),
    }
    Ok(())
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
