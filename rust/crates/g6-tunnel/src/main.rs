//! CLI entry point of the G6 harness tunnel.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

use ocservia_g6_tunnel::{
    TUNNEL_ALPN, load_or_create_key, node_id_hex, parse_node_id_hex, run_forward, serve,
    shutdown_signal,
};
use tokio::net::TcpStream;

const TARGET_READY_TIMEOUT: Duration = Duration::from_mins(5);
const TARGET_READY_POLL_INTERVAL: Duration = Duration::from_millis(250);

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();
    let args: Vec<String> = std::env::args().skip(1).collect();
    let command = args.first().map(String::as_str);
    match command {
        Some("node-id") => run_node_id(&args[1..]),
        Some("serve") => run_serve(&args[1..]).await,
        Some("forward") => run_forward_cli(&args[1..]).await,
        _ => Err(invalid_usage(
            "expected node-id, serve, or forward with --key-file, --peer-node, and address flags"
                .to_owned(),
        )
        .into()),
    }
}

fn invalid_usage(message: String) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::InvalidInput, message)
}

fn require_value(args: &[String], flag: &str) -> Result<String, std::io::Error> {
    let index = args
        .iter()
        .position(|arg| arg == flag)
        .ok_or_else(|| invalid_usage(format!("{flag} is required")))?;
    args.get(index + 1)
        .cloned()
        .filter(|value| !value.is_empty() && !value.starts_with("--"))
        .ok_or_else(|| invalid_usage(format!("{flag} requires a value")))
}

fn reject_unknown(args: &[String], known: &[&str]) -> Result<(), std::io::Error> {
    let mut index = 0;
    while index < args.len() {
        let arg = args[index].as_str();
        if !arg.starts_with("--") {
            return Err(invalid_usage(format!("unexpected argument: {arg}")));
        }
        if !known.contains(&arg) {
            return Err(invalid_usage(format!("unknown flag: {arg}")));
        }
        require_value(args, arg)?;
        index += 2;
    }
    Ok(())
}

fn key_from(args: &[String]) -> Result<iroh::SecretKey, std::io::Error> {
    let path = PathBuf::from(require_value(args, "--key-file")?);
    load_or_create_key(&path)
}

fn peer_from(args: &[String]) -> Result<iroh::EndpointId, std::io::Error> {
    parse_node_id_hex(&require_value(args, "--peer-node")?)
}

fn run_node_id(args: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    reject_unknown(args, &["--key-file"])?;
    let key = key_from(args)?;
    println!("{}", node_id_hex(key.public()));
    Ok(())
}

async fn wait_for_target(target: SocketAddr) -> Result<(), std::io::Error> {
    wait_for_target_with_timeout(target, TARGET_READY_TIMEOUT).await
}

async fn wait_for_target_with_timeout(
    target: SocketAddr,
    timeout: Duration,
) -> Result<(), std::io::Error> {
    let wait = async {
        loop {
            if let Ok(stream) = TcpStream::connect(target).await {
                drop(stream);
                return;
            }
            tokio::time::sleep(TARGET_READY_POLL_INTERVAL).await;
        }
    };
    tokio::time::timeout(timeout, wait).await.map_err(|_| {
        std::io::Error::new(
            std::io::ErrorKind::TimedOut,
            format!(
                "local tunnel target {target} was not reachable within {} seconds",
                timeout.as_secs()
            ),
        )
    })?;
    Ok(())
}

async fn run_serve(args: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    reject_unknown(args, &["--key-file", "--peer-node", "--forward"])?;
    let key = key_from(args)?;
    let peer = peer_from(args)?;
    let target: SocketAddr = require_value(args, "--forward")?
        .parse()
        .map_err(|_| invalid_usage("--forward must be an ip:port address".to_owned()))?;
    // A server endpoint must not become discoverable before its local target
    // exists. Publishing early creates a cross-VM race where a one-shot
    // enrollment reaches the Iroh tunnel but is rejected by the not-yet-live
    // PostgreSQL, API, or relay socket behind it.
    tracing::info!(
        %target,
        timeout_seconds = TARGET_READY_TIMEOUT.as_secs(),
        "waiting for local tunnel target"
    );
    wait_for_target(target).await?;
    let router = serve(key, iroh::endpoint::RelayMode::Default, peer, target).await?;
    tracing::info!(alpn = %String::from_utf8_lossy(TUNNEL_ALPN), %target, "g6 tunnel serving");
    shutdown_signal().await;
    let _ = router.shutdown().await;
    Ok(())
}

async fn run_forward_cli(args: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    reject_unknown(args, &["--key-file", "--peer-node", "--listen"])?;
    let key = key_from(args)?;
    let peer = peer_from(args)?;
    let listen: SocketAddr = require_value(args, "--listen")?
        .parse()
        .map_err(|_| invalid_usage("--listen must be an ip:port address".to_owned()))?;
    tracing::info!(%listen, "g6 tunnel forwarding");
    run_forward(
        key,
        iroh::endpoint::RelayMode::Default,
        iroh::EndpointAddr::new(peer),
        listen,
    )
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flags_require_values() {
        assert!(require_value(&[], "--key-file").is_err());
        let args = vec!["--key-file".to_string(), "/tmp/key".to_string()];
        assert_eq!(require_value(&args, "--key-file").unwrap(), "/tmp/key");
        let missing = vec!["--key-file".to_string()];
        assert!(require_value(&missing, "--key-file").is_err());
        let swallow = vec!["--key-file".to_string(), "--peer-node".to_string()];
        assert!(require_value(&swallow, "--key-file").is_err());
    }

    #[test]
    fn unknown_flags_are_rejected() {
        let args = vec!["--key-file".to_string(), "a".to_string()];
        assert!(reject_unknown(&args, &["--key-file"]).is_ok());
        assert!(reject_unknown(&args, &["--forward"]).is_err());
        let positional = vec!["serve".to_string()];
        assert!(reject_unknown(&positional, &["--key-file"]).is_err());
    }

    #[tokio::test]
    async fn serve_wait_accepts_a_live_local_target() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target = listener.local_addr().unwrap();
        wait_for_target_with_timeout(target, Duration::from_secs(1))
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn serve_wait_fails_closed_when_the_target_never_starts() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target = listener.local_addr().unwrap();
        drop(listener);
        let error = wait_for_target_with_timeout(target, Duration::from_millis(50))
            .await
            .unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::TimedOut);
    }
}
