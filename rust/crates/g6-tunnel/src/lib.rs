//! G6 harness-only Iroh TCP tunnel between the two validation failure domains.
//!
//! The GitHub-hosted G6 topology places the `PostgreSQL` primary and standby on
//! two different runner VMs. Hosted runners cannot accept inbound network
//! connections, so plain cross-host streaming replication is unreachable. This
//! crate forwards one TCP endpoint over an authenticated Iroh connection whose
//! peer node ID is pinned in both directions: the serving side rejects any
//! connection whose remote node ID is not the configured peer, and the dialing
//! side aborts when the established connection does not match. This component
//! never runs in production; production failure domains use real network
//! reachability instead.

#![forbid(unsafe_code)]

use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use iroh::endpoint::{Connection, RelayMode, VarInt, presets};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, EndpointAddr, EndpointId, SecretKey};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::tcp::{OwnedReadHalf, OwnedWriteHalf};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Semaphore;
use zeroize::Zeroizing;

/// ALPN of the G6 harness tunnel.
pub const TUNNEL_ALPN: &[u8] = b"ocserv-platform/g6-tunnel/1";

const MAX_TUNNEL_CONNECTIONS: usize = 32;
const MAX_TUNNEL_STREAMS: u32 = 8;
const STREAM_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(30);
const IROH_CONNECT_TIMEOUT: Duration = Duration::from_secs(90);
const TCP_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const COPY_BUFFER_BYTES: usize = 64 * 1024;

/// Loads a key file that holds 32 raw bytes or 64 lowercase hex characters,
/// creating it with fresh random material when it does not exist yet.
///
/// # Errors
///
/// Returns an I/O error when the file cannot be created or does not hold a
/// valid 32-byte secret in either accepted encoding.
pub fn load_or_create_key(path: &Path) -> Result<SecretKey, std::io::Error> {
    if path.exists() {
        return load_key(path);
    }
    let secret = Zeroizing::new(SecretKey::generate().to_bytes());
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    std::io::Write::write_all(&mut file, secret.as_slice())?;
    drop(file);
    load_key(path)
}

fn load_key(path: &Path) -> Result<SecretKey, std::io::Error> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.is_file() {
        return Err(invalid("tunnel key file must be a regular file"));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err(invalid(
                "tunnel key file must not be group or world accessible",
            ));
        }
    }
    let raw = std::fs::read(path)?;
    let bytes = Zeroizing::new(raw);
    let mut secret = Zeroizing::new([0_u8; 32]);
    if bytes.len() == 32 {
        secret.copy_from_slice(bytes.as_slice());
    } else {
        let text = std::str::from_utf8(bytes.as_slice())
            .map_err(|_| invalid("key file must contain 32 raw bytes or 64 lowercase hex"))?
            .trim_end_matches(['\n', '\r']);
        if text.len() != 64 || text.bytes().any(|byte| byte.is_ascii_uppercase()) {
            return Err(invalid(
                "key file must contain 32 raw bytes or 64 lowercase hex",
            ));
        }
        let decoded = Zeroizing::new(
            hex::decode(text).map_err(|_| invalid("key file contains invalid hex"))?,
        );
        if decoded.len() != secret.len() {
            return Err(invalid("key file must encode exactly 32 bytes"));
        }
        secret.copy_from_slice(decoded.as_slice());
    }
    Ok(SecretKey::from_bytes(&secret))
}

fn invalid(message: &str) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::InvalidInput, message.to_owned())
}

fn protocol_error(message: &str) -> AcceptError {
    AcceptError::from_err(std::io::Error::other(message.to_owned()))
}

/// Hex encoding of a tunnel node ID, used for harness rendezvous artifacts.
#[must_use]
pub fn node_id_hex(node: EndpointId) -> String {
    hex::encode(node.as_bytes())
}

/// Parses a 64-character lowercase hex node ID pinned as the tunnel peer.
///
/// # Errors
///
/// Returns an invalid-input error for anything but a 64 lowercase hex string
/// that encodes exactly 32 bytes.
pub fn parse_node_id_hex(value: &str) -> Result<EndpointId, std::io::Error> {
    let lowercase_hex = value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte));
    if !lowercase_hex {
        return Err(invalid("node ID must be 64 lowercase hex characters"));
    }
    let decoded =
        Zeroizing::new(hex::decode(value).map_err(|_| invalid("node ID must be lowercase hex"))?);
    let bytes: [u8; 32] = decoded
        .as_slice()
        .try_into()
        .map_err(|_| invalid("node ID must encode exactly 32 bytes"))?;
    EndpointId::from_bytes(&bytes).map_err(|_| invalid("node ID is not a valid key"))
}

fn tunnel_transport_config() -> iroh::endpoint::QuicTransportConfig {
    iroh::endpoint::QuicTransportConfig::builder()
        .max_concurrent_bidi_streams(VarInt::from_u32(MAX_TUNNEL_STREAMS))
        .max_concurrent_uni_streams(VarInt::from_u32(1))
        .max_idle_timeout(Some(VarInt::from_u32(300_000).into()))
        .stream_receive_window(VarInt::from_u32(1024 * 1024))
        .receive_window(VarInt::from_u32(4 * 1024 * 1024))
        .build()
}

async fn bind_endpoint(
    secret_key: SecretKey,
    relay_mode: RelayMode,
) -> Result<Endpoint, std::io::Error> {
    let endpoint = Endpoint::builder(presets::N0)
        .secret_key(secret_key)
        .relay_mode(relay_mode)
        .transport_config(tunnel_transport_config())
        .bind();
    endpoint.await.map_err(std::io::Error::other)
}

/// Serves the tunnel ALPN and forwards every accepted connection to a local
/// TCP target, rejecting any peer that is not the pinned node ID.
///
/// The relay mode must be [`RelayMode::Default`] for the cross-VM harness
/// topology (hosted runners have no inbound reachability) and
/// [`RelayMode::Disabled`] for direct-path deployments and loopback tests.
///
/// # Errors
///
/// Returns an error when the Iroh endpoint cannot bind.
pub async fn serve(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    peer: EndpointId,
    target: SocketAddr,
) -> Result<Router, std::io::Error> {
    let endpoint = bind_endpoint(secret_key, relay_mode).await?;
    let handler = ForwardHandler {
        peer,
        target,
        permits: Arc::new(Semaphore::new(MAX_TUNNEL_CONNECTIONS)),
    };
    let router = Router::builder(endpoint)
        .accept(TUNNEL_ALPN, handler)
        .spawn();
    Ok(router)
}

#[derive(Debug)]
struct ForwardHandler {
    peer: EndpointId,
    target: SocketAddr,
    permits: Arc<Semaphore>,
}

impl ProtocolHandler for ForwardHandler {
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        if connection.remote_id() != self.peer {
            connection.close(VarInt::from_u32(0x201), b"unpinned tunnel peer");
            return Err(protocol_error("tunnel connection from unpinned peer"));
        }
        let _permit = Arc::clone(&self.permits)
            .acquire_owned()
            .await
            .map_err(|_| protocol_error("tunnel capacity closed"))?;
        let stream = tokio::time::timeout(STREAM_HANDSHAKE_TIMEOUT, connection.accept_bi()).await;
        let (send, recv) = match stream {
            Ok(Ok(stream)) => stream,
            Ok(Err(_)) => {
                connection.close(VarInt::from_u32(0x203), b"tunnel stream failed");
                return Err(protocol_error("tunnel stream handshake failed"));
            }
            Err(_) => {
                connection.close(VarInt::from_u32(0x202), b"tunnel stream timeout");
                return Err(protocol_error("tunnel stream handshake timed out"));
            }
        };
        let tcp = match tokio::time::timeout(TCP_CONNECT_TIMEOUT, TcpStream::connect(self.target))
            .await
        {
            Ok(Ok(stream)) => stream,
            Ok(Err(error)) => {
                tracing::warn!(error = %error, target = %self.target, "tunnel target unreachable");
                connection.close(VarInt::from_u32(0x204), b"tunnel target unreachable");
                return Err(protocol_error("tunnel target unreachable"));
            }
            Err(_) => {
                connection.close(VarInt::from_u32(0x204), b"tunnel target unreachable");
                return Err(protocol_error("tunnel target connect timed out"));
            }
        };
        copy_tunnel(send, recv, tcp).await;
        Ok(())
    }
}

/// Runs the dialing side: a local TCP listener whose connections are each
/// carried over a fresh Iroh connection to the pinned peer. The relay mode
/// must match the serving side.
///
/// # Errors
///
/// Returns an error when the listener or the Iroh endpoint cannot bind, or
/// when the accept loop fails.
pub async fn run_forward(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    peer: EndpointAddr,
    listen: SocketAddr,
) -> Result<(), std::io::Error> {
    let endpoint = bind_endpoint(secret_key, relay_mode).await?;
    let listener = TcpListener::bind(listen).await?;
    let permits = Arc::new(Semaphore::new(MAX_TUNNEL_CONNECTIONS));
    loop {
        let (tcp, peer_addr) = listener.accept().await?;
        let permit = Arc::clone(&permits)
            .acquire_owned()
            .await
            .map_err(|_| invalid("tunnel capacity closed"))?;
        let endpoint = endpoint.clone();
        let peer = peer.clone();
        tokio::spawn(async move {
            let _permit = permit;
            if let Err(error) = forward_one(&endpoint, peer, tcp).await {
                tracing::warn!(error = %error, peer = %peer_addr, "tunnel forward connection failed");
            }
        });
    }
}

async fn forward_one(
    endpoint: &Endpoint,
    peer: EndpointAddr,
    tcp: TcpStream,
) -> Result<(), std::io::Error> {
    let connect = tokio::time::timeout(
        IROH_CONNECT_TIMEOUT,
        endpoint.connect(peer.clone(), TUNNEL_ALPN),
    );
    let connection = connect
        .await
        .map_err(|_| invalid("tunnel peer connect timed out"))?
        .map_err(|error| std::io::Error::other(format!("tunnel peer connect failed: {error}")))?;
    if connection.remote_id() != peer.id {
        return Err(invalid("tunnel peer identity mismatch"));
    }
    let stream = tokio::time::timeout(STREAM_HANDSHAKE_TIMEOUT, connection.open_bi()).await;
    let (send, recv) = match stream {
        Ok(Ok(stream)) => stream,
        Ok(Err(error)) => {
            return Err(std::io::Error::other(format!(
                "tunnel stream open failed: {error}"
            )));
        }
        Err(_) => return Err(invalid("tunnel stream open timed out")),
    };
    copy_tunnel(send, recv, tcp).await;
    Ok(())
}

async fn copy_tunnel(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    tcp: TcpStream,
) {
    let (mut tcp_read, mut tcp_write) = tcp.into_split();
    let upstream = pump_tcp_to_stream(&mut tcp_read, &mut send);
    let downstream = pump_stream_to_tcp(&mut recv, &mut tcp_write);
    let (upstream, downstream) = tokio::join!(upstream, downstream);
    if let Err(error) = upstream {
        tracing::debug!(error = %error, "tunnel upstream copy ended");
    }
    if let Err(error) = downstream {
        tracing::debug!(error = %error, "tunnel downstream copy ended");
    }
}

async fn pump_tcp_to_stream(
    tcp: &mut OwnedReadHalf,
    send: &mut iroh::endpoint::SendStream,
) -> Result<(), std::io::Error> {
    let mut buffer = vec![0_u8; COPY_BUFFER_BYTES];
    loop {
        let read = tcp.read(buffer.as_mut_slice()).await?;
        if read == 0 {
            send.finish().map_err(|_| invalid("tunnel stream closed"))?;
            return Ok(());
        }
        send.write_all(&buffer[..read])
            .await
            .map_err(|_| invalid("tunnel stream write failed"))?;
    }
}

async fn pump_stream_to_tcp(
    recv: &mut iroh::endpoint::RecvStream,
    tcp: &mut OwnedWriteHalf,
) -> Result<(), std::io::Error> {
    let mut buffer = vec![0_u8; COPY_BUFFER_BYTES];
    loop {
        let read = recv.read(buffer.as_mut_slice()).await;
        match read {
            Ok(Some(read)) => {
                tcp.write_all(&buffer[..read]).await?;
            }
            Ok(None) => {
                tcp.shutdown().await?;
                return Ok(());
            }
            Err(_) => return Err(invalid("tunnel stream read failed")),
        }
    }
}

/// Resolves when SIGINT or SIGTERM arrives so harness scripts can stop the
/// tunnel process.
pub async fn shutdown_signal() {
    let terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate());
    let mut terminate = match terminate {
        Ok(signals) => signals,
        Err(error) => {
            tracing::warn!(error = %error, "SIGTERM handler unavailable, waiting for ctrl-c");
            let _ = tokio::signal::ctrl_c().await;
            return;
        }
    };
    tokio::select! {
        _ = terminate.recv() => {},
        _ = tokio::signal::ctrl_c() => {},
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_round_trip_hex_and_raw() {
        let dir = std::env::temp_dir().join(format!(
            "g6-tunnel-key-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let path = dir.join("tunnel.key");
        let key = load_or_create_key(&path).unwrap();
        let mode = {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt as _;
                std::fs::metadata(&path).unwrap().permissions().mode()
            }
            #[cfg(not(unix))]
            {
                0o600
            }
        };
        assert!(path.is_file());
        assert_eq!(mode & 0o077, 0, "created key file must be owner-only");
        let reloaded = load_or_create_key(&path).unwrap();
        assert_eq!(node_id_hex(key.public()), node_id_hex(reloaded.public()));

        let hex_path = dir.join("tunnel-hex.key");
        {
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt as _;
                let mut options = std::fs::OpenOptions::new();
                options.write(true).create(true).truncate(true).mode(0o600);
                let mut file = options.open(&hex_path).unwrap();
                std::io::Write::write_all(&mut file, hex::encode(key.to_bytes()).as_bytes())
                    .unwrap();
            }
            #[cfg(not(unix))]
            {
                std::fs::write(&hex_path, hex::encode(key.to_bytes())).unwrap();
            }
        }
        assert_eq!(
            node_id_hex(load_key(&hex_path).unwrap().public()),
            node_id_hex(key.public())
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn parse_node_id_rejects_malformed_values() {
        let key = SecretKey::generate();
        let encoded = node_id_hex(key.public());
        assert_eq!(parse_node_id_hex(&encoded).unwrap(), key.public());
        assert!(parse_node_id_hex("abc").is_err());
        assert!(parse_node_id_hex(&encoded.to_uppercase()).is_err());
        assert!(parse_node_id_hex(&format!("{encoded}00")).is_err());
    }

    /// Dials the pinned server with a stranger key and reports whether any
    /// tunnel application data could be received: a stranger that cannot even
    /// connect never receives tunnel data.
    async fn stranger_is_rejected(server_addr: &EndpointAddr) -> bool {
        const STAGE_TIMEOUT: Duration = Duration::from_secs(20);
        let stranger = tokio::time::timeout(STAGE_TIMEOUT, async {
            Endpoint::builder(presets::Minimal)
                .secret_key(SecretKey::generate())
                .relay_mode(RelayMode::Disabled)
                .bind()
                .await
                .unwrap()
        })
        .await
        .expect("stranger endpoint stage timed out");
        let outcome = tokio::time::timeout(
            STAGE_TIMEOUT,
            stranger.connect(server_addr.clone(), TUNNEL_ALPN),
        )
        .await;
        let received = match outcome {
            Err(_) | Ok(Err(_)) => false,
            Ok(Ok(connection)) => {
                let stream = tokio::time::timeout(STAGE_TIMEOUT, connection.accept_bi()).await;
                match stream {
                    Err(_) | Ok(Err(_)) => false,
                    Ok(Ok((_, mut recv))) => {
                        let mut scratch = vec![0_u8; 8];
                        tokio::time::timeout(STAGE_TIMEOUT, recv.read_exact(scratch.as_mut_slice()))
                            .await
                            .is_ok()
                    }
                }
            }
        };
        let _ = tokio::time::timeout(STAGE_TIMEOUT, stranger.close()).await;
        !received
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn tunnels_tcp_between_pinned_endpoints() {
        // Every network stage is bounded so a stalled Iroh path fails the test
        // instead of hanging the harness run.
        const STAGE_TIMEOUT: Duration = Duration::from_secs(20);

        let stage = tokio::time::timeout(STAGE_TIMEOUT, async {
            let echo = TcpListener::bind("127.0.0.1:0").await.unwrap();
            let echo_addr = echo.local_addr().unwrap();
            tokio::spawn(async move {
                loop {
                    let Ok((mut stream, _)) = echo.accept().await else {
                        return;
                    };
                    tokio::spawn(async move {
                        let (mut read, mut write) = stream.split();
                        let _ = tokio::io::copy(&mut read, &mut write).await;
                    });
                }
            });
            echo_addr
        })
        .await
        .expect("echo server stage timed out");

        let (server_key, client_key) = (SecretKey::generate(), SecretKey::generate());
        // Relay-disabled endpoints on both sides keep the test hermetic: no
        // external relay must be reachable for the loopback path. The cross-VM
        // harness dials with the default relay mode instead.
        let router = tokio::time::timeout(
            STAGE_TIMEOUT,
            serve(server_key, RelayMode::Disabled, client_key.public(), stage),
        )
        .await
        .expect("tunnel serve stage timed out")
        .unwrap();
        let server_addr = router.endpoint().addr();

        let reserved = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let forward_addr = reserved.local_addr().unwrap();
        drop(reserved);
        let forward = tokio::spawn(run_forward(
            client_key,
            RelayMode::Disabled,
            server_addr.clone(),
            forward_addr,
        ));
        tokio::time::sleep(Duration::from_millis(300)).await;

        let mut tunnel_tcp = tokio::time::timeout(STAGE_TIMEOUT, TcpStream::connect(forward_addr))
            .await
            .expect("tunnel listener stage timed out")
            .unwrap();
        let ping = b"g6-tunnel-ping";
        tunnel_tcp.write_all(ping).await.unwrap();
        let mut received = vec![0_u8; ping.len()];
        tokio::time::timeout(
            STAGE_TIMEOUT,
            tunnel_tcp.read_exact(received.as_mut_slice()),
        )
        .await
        .expect("tunnel echo stage timed out")
        .unwrap();
        assert_eq!(received, ping);
        drop(tunnel_tcp);

        // The pinned server side must refuse a different peer: either the
        // connection is rejected outright or the first stream read fails.
        assert!(
            stranger_is_rejected(&server_addr).await,
            "unpinned peer must not receive tunnel data"
        );

        forward.abort();
        let _ = tokio::time::timeout(STAGE_TIMEOUT, router.shutdown()).await;
    }
}
