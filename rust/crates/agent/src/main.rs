use std::env;
use std::io;
use std::path::PathBuf;
use std::time::{Duration, SystemTime};

use iroh::endpoint::{RelayMode, presets};
use iroh::{Endpoint, EndpointAddr, EndpointId};
use ocservia_agent::PrivdClient;
use ocservia_agent_protocol::privd_response;
use ocservia_command_journal::Journal;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    HandshakeResult, SessionHandshake, SessionHandshakeResponse,
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
    let _journal = Journal::open(&config.journal)?;
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
    let endpoint = Endpoint::builder(presets::Minimal)
        .secret_key(identity.secret_key().clone())
        .relay_mode(RelayMode::Default)
        .bind()
        .await?;
    let run = async {
        let mut attempt = 0_u32;
        let backoff = ocservia_agent::Backoff::default();
        loop {
            match connect_once(&endpoint, controller, node_id, identity.endpoint_id()).await {
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

async fn connect_once(
    endpoint: &Endpoint,
    controller: EndpointId,
    node_id: Uuid,
    endpoint_id: EndpointId,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let connection = endpoint
        .connect(EndpointAddr::new(controller), AGENT_ALPN)
        .await?;
    let handshake = SessionHandshake {
        protocol_major: 1,
        protocol_minor: 0,
        agent_version: env!("CARGO_PKG_VERSION").to_owned(),
        controller_version: String::new(),
        node_id: node_id.as_bytes().to_vec(),
        endpoint_id: endpoint_id.as_bytes().to_vec(),
        capabilities: vec![
            "ocserv.status.read".to_owned(),
            "ocserv.version.read".to_owned(),
            "ocserv.sessions.read".to_owned(),
            "ocserv.ip_bans.read".to_owned(),
            "ocserv.config_fingerprint.read".to_owned(),
        ],
        ocserv_version: "unknown".to_owned(),
        os_release: ocservia_agent::read_os_release().await?,
        boot_id: ocservia_agent::read_boot_id().await?,
        agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
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
    loop {
        tokio::select! {
            _ = connection.closed() => return Ok(()),
            _ = heartbeat.tick() => tracing::info!(node_id = %node_id, "agent heartbeat"),
        }
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
