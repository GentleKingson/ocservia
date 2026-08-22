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
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use iroh::endpoint::{Connection, RelayMode, VarInt, presets};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, EndpointAddr, EndpointId, SecretKey};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::tcp::{OwnedReadHalf, OwnedWriteHalf};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore};
use zeroize::Zeroizing;

/// ALPN of the G6 harness tunnel.
pub const TUNNEL_ALPN: &[u8] = b"ocserv-platform/g6-tunnel/1";

// The cross-domain relay can still be draining every one-shot enrollment
// endpoint when the 25 runtime Agents and transportd synchronously establish
// relay-a paths. Each Iroh endpoint can overlap its persistent relay socket
// with three staggered HTTPS probes. One extra connection accounts for the
// harness readiness/control path. The resulting 205-connection minimum
// transition budget is rounded up to a fixed 256, leaving 51 connections for
// bounded retry churn. Excess work enters a 64-connection queue for at most
// five seconds; this is intentionally not an open-ended substitute for a
// production network.
const G6_FORMAL_AGENTS_PER_FAILURE_DOMAIN: usize = 25;
const G6_TRANSIENT_ENROLLMENT_ENDPOINTS_PER_FAILURE_DOMAIN: usize = 25;
const G6_RELAY_SUPPORT_ENDPOINTS_PER_FAILURE_DOMAIN: usize = 1;
const G6_RELAY_TCP_BURST_PER_ENDPOINT: usize = 4;
const G6_RELAY_TUNNEL_CONTROL_CONNECTIONS: usize = 1;
const REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS: usize =
    (G6_TRANSIENT_ENROLLMENT_ENDPOINTS_PER_FAILURE_DOMAIN
        + G6_FORMAL_AGENTS_PER_FAILURE_DOMAIN
        + G6_RELAY_SUPPORT_ENDPOINTS_PER_FAILURE_DOMAIN)
        * G6_RELAY_TCP_BURST_PER_ENDPOINT
        + G6_RELAY_TUNNEL_CONTROL_CONNECTIONS;
const MAX_TUNNEL_CONNECTIONS: usize = 256;
const MAX_TUNNEL_QUEUED_CONNECTIONS: usize = 64;
const _: () = assert!(MAX_TUNNEL_CONNECTIONS >= REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS);
const _: () = assert!(MAX_TUNNEL_QUEUED_CONNECTIONS <= MAX_TUNNEL_CONNECTIONS);
// A persistent outer QUIC connection carries every admitted local TCP flow as
// an independent bidirectional stream. Keep the negotiated stream budget
// large enough for both the active set and its explicitly bounded queue.
const MAX_TUNNEL_STREAMS: u32 = 320;
const _: () =
    assert!(MAX_TUNNEL_STREAMS as usize >= MAX_TUNNEL_CONNECTIONS + MAX_TUNNEL_QUEUED_CONNECTIONS);
// Surface capacity pressure independently of the longer Iroh connection and
// stream deadlines below.
const PERMIT_ACQUIRE_TIMEOUT: Duration = Duration::from_secs(5);
const STREAM_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(30);
const IROH_CONNECT_TIMEOUT: Duration = Duration::from_secs(90);
const IROH_RECONNECT_FAILURE_BACKOFF: Duration = Duration::from_secs(1);
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
    serve_with_components(
        secret_key,
        relay_mode,
        peer,
        target,
        ConnectionLimiter::formal(),
        OuterConnectionMetrics::default(),
    )
    .await
}

async fn serve_with_components(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    peer: EndpointId,
    target: SocketAddr,
    limiter: ConnectionLimiter,
    metrics: OuterConnectionMetrics,
) -> Result<Router, std::io::Error> {
    let endpoint = bind_endpoint(secret_key, relay_mode).await?;
    let handler = ForwardHandler {
        peer,
        target,
        limiter,
        metrics,
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
    limiter: ConnectionLimiter,
    metrics: OuterConnectionMetrics,
}

#[derive(Clone, Debug, Default)]
struct OuterConnectionMetrics {
    accepted: Arc<AtomicUsize>,
}

impl ProtocolHandler for ForwardHandler {
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        if connection.remote_id() != self.peer {
            connection.close(VarInt::from_u32(0x201), b"unpinned tunnel peer");
            return Err(protocol_error("tunnel connection from unpinned peer"));
        }

        let outer_connection_generation = self.metrics.accepted.fetch_add(1, Ordering::AcqRel) + 1;
        tracing::info!(
            direction = "serve",
            outer_connection_generation,
            outer_connection_id = connection.stable_id(),
            "tunnel outer connection accepted"
        );

        let mut streams = tokio::task::JoinSet::new();
        loop {
            tokio::select! {
                reason = connection.closed() => {
                    tracing::info!(
                        direction = "serve",
                        outer_connection_generation,
                        outer_connection_id = connection.stable_id(),
                        reason = %reason,
                        "tunnel outer connection closed"
                    );
                    break;
                }
                finished = streams.join_next(), if !streams.is_empty() => {
                    if let Some(Err(error)) = finished {
                        tracing::warn!(error = %error, "tunnel stream task failed");
                    }
                }
                stream = connection.accept_bi() => {
                    let (send, recv) = match stream {
                        Ok(stream) => stream,
                        Err(error) => {
                            tracing::debug!(
                                error = %error,
                                outer_connection_generation,
                                outer_connection_id = connection.stable_id(),
                                "tunnel outer connection stopped accepting streams"
                            );
                            break;
                        }
                    };
                    let admission = match self.limiter.admit("serve") {
                        Ok(admission) => admission,
                        Err(error) => {
                            tracing::warn!(error = %error, "tunnel stream rejected");
                            reject_stream(send, recv, 0x205);
                            continue;
                        }
                    };
                    let limiter = self.limiter.clone();
                    let target = self.target;
                    streams.spawn(async move {
                        serve_stream(send, recv, target, limiter, admission).await;
                    });
                }
            }
        }
        streams.shutdown().await;
        Ok(())
    }
}

async fn serve_stream(
    send: iroh::endpoint::SendStream,
    recv: iroh::endpoint::RecvStream,
    target: SocketAddr,
    limiter: ConnectionLimiter,
    admission: ConnectionAdmission,
) {
    let _permit = match limiter.acquire_admitted("serve", admission).await {
        Ok(permit) => permit,
        Err(error) => {
            tracing::warn!(error = %error, "tunnel stream rejected");
            reject_stream(send, recv, 0x205);
            return;
        }
    };
    let tcp = match tokio::time::timeout(TCP_CONNECT_TIMEOUT, TcpStream::connect(target)).await {
        Ok(Ok(stream)) => stream,
        Ok(Err(error)) => {
            tracing::warn!(error = %error, target = %target, "tunnel target unreachable");
            reject_stream(send, recv, 0x204);
            return;
        }
        Err(_) => {
            tracing::warn!(target = %target, "tunnel target connect timed out");
            reject_stream(send, recv, 0x204);
            return;
        }
    };
    if let Err(error) = copy_tunnel(send, recv, tcp).await {
        tracing::debug!(error = %error, "tunnel stream copy ended");
    }
}

fn reject_stream(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    code: u32,
) {
    let code = VarInt::from_u32(code);
    let _ = send.reset(code);
    let _ = recv.stop(code);
}

/// Runs the dialing side: a local TCP listener whose connections are carried
/// as independent streams over one persistent authenticated Iroh connection
/// to the pinned peer. The relay mode must match the serving side.
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
    let limiter = ConnectionLimiter::formal();
    let outer = SharedPeerConnection::new(endpoint, peer);
    run_forward_listener(listener, limiter, outer).await
}

async fn run_forward_listener(
    listener: TcpListener,
    limiter: ConnectionLimiter,
    outer: SharedPeerConnection,
) -> Result<(), std::io::Error> {
    loop {
        let (tcp, peer_addr) = listener.accept().await?;
        let admission = match limiter.admit("forward") {
            Ok(admission) => admission,
            Err(error) => {
                tracing::warn!(
                    error = %error,
                    peer = %peer_addr,
                    "tunnel forward connection rejected"
                );
                continue;
            }
        };
        let outer = outer.clone();
        let limiter = limiter.clone();
        tokio::spawn(async move {
            let _permit = match limiter.acquire_admitted("forward", admission).await {
                Ok(permit) => permit,
                Err(error) => {
                    tracing::warn!(
                        error = %error,
                        peer = %peer_addr,
                        "tunnel forward connection rejected"
                    );
                    return;
                }
            };
            if let Err(error) = forward_one(&outer, tcp).await {
                tracing::warn!(error = %error, peer = %peer_addr, "tunnel forward connection failed");
            }
        });
    }
}

#[derive(Clone, Debug)]
struct SharedPeerConnection {
    endpoint: Endpoint,
    peer: EndpointAddr,
    state: Arc<Mutex<OuterConnectionState>>,
    connect_gate: Arc<Mutex<()>>,
}

#[derive(Clone, Debug)]
struct OuterConnection {
    generation: usize,
    connection: Connection,
}

#[derive(Debug, Default)]
struct OuterConnectionState {
    connection: Option<OuterConnection>,
    attempts: usize,
    last_generation: usize,
    last_failure: Option<OuterConnectionFailure>,
}

#[derive(Debug)]
struct OuterConnectionFailure {
    observed_at: tokio::time::Instant,
    kind: std::io::ErrorKind,
    message: String,
}

impl SharedPeerConnection {
    fn new(endpoint: Endpoint, peer: EndpointAddr) -> Self {
        Self {
            endpoint,
            peer,
            state: Arc::new(Mutex::new(OuterConnectionState::default())),
            connect_gate: Arc::new(Mutex::new(())),
        }
    }

    async fn get(&self) -> Result<OuterConnection, std::io::Error> {
        let deadline = tokio::time::Instant::now() + IROH_CONNECT_TIMEOUT;
        {
            let mut state = tokio::time::timeout_at(deadline, self.state.lock())
                .await
                .map_err(|_| timed_out("tunnel outer connection state check timed out"))?;
            if let Some(existing) = Self::existing(&mut state) {
                return existing;
            }
        }

        // Only this dedicated gate spans the network operation. State and
        // generation invalidation remain immediately available to copy tasks.
        let _singleflight = tokio::time::timeout_at(deadline, self.connect_gate.lock())
            .await
            .map_err(|_| timed_out("tunnel outer connection single-flight timed out"))?;
        let (attempt, reconnect) = {
            let mut state = tokio::time::timeout_at(deadline, self.state.lock())
                .await
                .map_err(|_| timed_out("tunnel outer connection state check timed out"))?;
            if let Some(existing) = Self::existing(&mut state) {
                return existing;
            }
            state.attempts = state.attempts.saturating_add(1);
            (state.attempts, state.attempts > 1)
        };
        tracing::info!(
            direction = "forward",
            outer_connection_attempt = attempt,
            reconnect,
            "tunnel outer connection attempt started"
        );
        let connected = tokio::time::timeout_at(
            deadline,
            self.endpoint.connect(self.peer.clone(), TUNNEL_ALPN),
        )
        .await;
        let connection = match connected {
            Ok(Ok(connection)) => connection,
            Ok(Err(error)) => {
                let message = format!("tunnel outer connection attempt failed: {error}");
                self.record_failure(std::io::ErrorKind::Other, message.clone())
                    .await;
                tracing::warn!(
                    direction = "forward",
                    outer_connection_attempt = attempt,
                    reconnect,
                    error = %error,
                    "tunnel outer connection attempt failed"
                );
                return Err(std::io::Error::other(message));
            }
            Err(_) => {
                let message = "tunnel outer connection attempt timed out";
                self.record_failure(std::io::ErrorKind::TimedOut, message.to_owned())
                    .await;
                tracing::warn!(
                    direction = "forward",
                    outer_connection_attempt = attempt,
                    reconnect,
                    "tunnel outer connection attempt timed out"
                );
                return Err(timed_out(message));
            }
        };
        if connection.remote_id() != self.peer.id {
            connection.close(VarInt::from_u32(0x201), b"tunnel peer identity mismatch");
            self.record_failure(
                std::io::ErrorKind::InvalidInput,
                "tunnel peer identity mismatch".to_owned(),
            )
            .await;
            return Err(invalid("tunnel peer identity mismatch"));
        }

        let mut state = self.state.lock().await;
        state.last_failure = None;
        state.last_generation = state.last_generation.saturating_add(1);
        let established = OuterConnection {
            generation: state.last_generation,
            connection,
        };
        tracing::info!(
            direction = "forward",
            outer_connection_attempt = attempt,
            outer_connection_generation = established.generation,
            outer_connection_id = established.connection.stable_id(),
            reconnect,
            "tunnel outer connection established"
        );
        state.connection = Some(established.clone());
        Ok(established)
    }

    fn existing(
        state: &mut OuterConnectionState,
    ) -> Option<Result<OuterConnection, std::io::Error>> {
        if let Some(cached) = state.connection.as_ref()
            && cached.connection.close_reason().is_none()
        {
            tracing::debug!(
                direction = "forward",
                outer_connection_generation = cached.generation,
                outer_connection_id = cached.connection.stable_id(),
                "tunnel outer connection reused"
            );
            return Some(Ok(cached.clone()));
        }
        if let Some(stale) = state.connection.take() {
            tracing::info!(
                direction = "forward",
                outer_connection_generation = stale.generation,
                outer_connection_id = stale.connection.stable_id(),
                reason = ?stale.connection.close_reason(),
                "tunnel outer connection invalidated"
            );
        }
        let failure = state.last_failure.as_ref()?;
        let age = tokio::time::Instant::now().saturating_duration_since(failure.observed_at);
        if age >= IROH_RECONNECT_FAILURE_BACKOFF {
            return None;
        }
        tracing::warn!(
            direction = "forward",
            outer_connection_attempt = state.attempts,
            retry_after_ms = u64::try_from(
                IROH_RECONNECT_FAILURE_BACKOFF
                    .saturating_sub(age)
                    .as_millis()
            )
            .unwrap_or(u64::MAX),
            "tunnel outer connection retry coalesced"
        );
        Some(Err(std::io::Error::new(
            failure.kind,
            failure.message.clone(),
        )))
    }

    async fn record_failure(&self, kind: std::io::ErrorKind, message: String) {
        self.state.lock().await.last_failure = Some(OuterConnectionFailure {
            observed_at: tokio::time::Instant::now(),
            kind,
            message,
        });
    }

    async fn invalidate(&self, candidate: &OuterConnection, reason: &'static str) {
        let mut guard = self.state.lock().await;
        let is_current = guard.connection.as_ref().is_some_and(|current| {
            current.generation == candidate.generation
                && current.connection.stable_id() == candidate.connection.stable_id()
        });
        if is_current {
            guard.connection = None;
            tracing::info!(
                direction = "forward",
                outer_connection_generation = candidate.generation,
                outer_connection_id = candidate.connection.stable_id(),
                reason,
                "tunnel outer connection invalidated"
            );
        } else {
            tracing::debug!(
                direction = "forward",
                outer_connection_generation = candidate.generation,
                outer_connection_id = candidate.connection.stable_id(),
                reason,
                "stale tunnel outer connection invalidation ignored"
            );
        }
    }
}

fn timed_out(message: &str) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::TimedOut, message.to_owned())
}

#[derive(Clone, Debug)]
struct ConnectionLimiter {
    permits: Arc<Semaphore>,
    waiters: Arc<Semaphore>,
    high_water: Arc<AtomicUsize>,
    capacity: usize,
    max_queued: usize,
    permit_timeout: Duration,
}

#[derive(Debug)]
enum ConnectionAdmission {
    Acquired(OwnedSemaphorePermit),
    Queued(OwnedSemaphorePermit),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct CapacitySnapshot {
    capacity: usize,
    in_use: usize,
    available: usize,
    queued: usize,
    max_queued: usize,
    high_water: usize,
}

impl ConnectionLimiter {
    fn formal() -> Self {
        let limiter = Self::with_limits(
            MAX_TUNNEL_CONNECTIONS,
            MAX_TUNNEL_QUEUED_CONNECTIONS,
            PERMIT_ACQUIRE_TIMEOUT,
        );
        tracing::info!(
            capacity = MAX_TUNNEL_CONNECTIONS,
            required_transition_capacity = REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS,
            max_queued = MAX_TUNNEL_QUEUED_CONNECTIONS,
            permit_timeout_ms =
                u64::try_from(PERMIT_ACQUIRE_TIMEOUT.as_millis()).unwrap_or(u64::MAX),
            "tunnel connection capacity initialized"
        );
        limiter
    }

    fn with_limits(capacity: usize, max_queued: usize, permit_timeout: Duration) -> Self {
        Self {
            permits: Arc::new(Semaphore::new(capacity)),
            waiters: Arc::new(Semaphore::new(max_queued)),
            high_water: Arc::new(AtomicUsize::new(0)),
            capacity,
            max_queued,
            permit_timeout,
        }
    }

    fn snapshot(&self) -> CapacitySnapshot {
        let available = self.permits.available_permits();
        CapacitySnapshot {
            capacity: self.capacity,
            in_use: self.capacity.saturating_sub(available),
            available,
            queued: self
                .max_queued
                .saturating_sub(self.waiters.available_permits()),
            max_queued: self.max_queued,
            high_water: self.high_water.load(Ordering::Acquire),
        }
    }

    fn record_acquired(&self, direction: &'static str) {
        let snapshot = self.snapshot();
        let mut previous = snapshot.high_water;
        while snapshot.in_use > previous {
            match self.high_water.compare_exchange_weak(
                previous,
                snapshot.in_use,
                Ordering::AcqRel,
                Ordering::Acquire,
            ) {
                Ok(_) => {
                    tracing::info!(
                        direction,
                        capacity = snapshot.capacity,
                        in_use = snapshot.in_use,
                        available = snapshot.available,
                        queued = snapshot.queued,
                        max_queued = snapshot.max_queued,
                        high_water = snapshot.in_use,
                        "tunnel connection capacity high-water mark advanced"
                    );
                    return;
                }
                Err(current) => previous = current,
            }
        }
    }

    fn admit(&self, direction: &'static str) -> Result<ConnectionAdmission, std::io::Error> {
        if let Ok(permit) = Arc::clone(&self.permits).try_acquire_owned() {
            self.record_acquired(direction);
            return Ok(ConnectionAdmission::Acquired(permit));
        }

        let Ok(waiter) = Arc::clone(&self.waiters).try_acquire_owned() else {
            let snapshot = self.snapshot();
            tracing::warn!(
                direction,
                reason = "queue_full",
                capacity = snapshot.capacity,
                in_use = snapshot.in_use,
                available = snapshot.available,
                queued = snapshot.queued,
                max_queued = snapshot.max_queued,
                high_water = snapshot.high_water,
                "tunnel connection capacity queue is full"
            );
            return Err(std::io::Error::new(
                std::io::ErrorKind::WouldBlock,
                "tunnel connection capacity queue is full",
            ));
        };
        let snapshot = self.snapshot();
        tracing::warn!(
            direction,
            reason = "capacity_wait",
            capacity = snapshot.capacity,
            in_use = snapshot.in_use,
            available = snapshot.available,
            queued = snapshot.queued,
            max_queued = snapshot.max_queued,
            high_water = snapshot.high_water,
            permit_timeout_ms = u64::try_from(self.permit_timeout.as_millis()).unwrap_or(u64::MAX),
            "tunnel connection waiting for capacity"
        );
        Ok(ConnectionAdmission::Queued(waiter))
    }

    async fn acquire_admitted(
        &self,
        direction: &'static str,
        admission: ConnectionAdmission,
    ) -> Result<OwnedSemaphorePermit, std::io::Error> {
        let waiter = match admission {
            ConnectionAdmission::Acquired(permit) => return Ok(permit),
            ConnectionAdmission::Queued(waiter) => waiter,
        };

        let acquired = tokio::time::timeout(
            self.permit_timeout,
            Arc::clone(&self.permits).acquire_owned(),
        )
        .await;
        drop(waiter);
        match acquired {
            Ok(Ok(permit)) => {
                self.record_acquired(direction);
                Ok(permit)
            }
            Ok(Err(_)) => {
                let snapshot = self.snapshot();
                tracing::warn!(
                    direction,
                    reason = "capacity_closed",
                    capacity = snapshot.capacity,
                    in_use = snapshot.in_use,
                    available = snapshot.available,
                    queued = snapshot.queued,
                    max_queued = snapshot.max_queued,
                    high_water = snapshot.high_water,
                    "tunnel connection capacity closed"
                );
                Err(std::io::Error::new(
                    std::io::ErrorKind::BrokenPipe,
                    "tunnel connection capacity closed",
                ))
            }
            Err(_) => {
                let snapshot = self.snapshot();
                tracing::warn!(
                    direction,
                    reason = "permit_timeout",
                    capacity = snapshot.capacity,
                    in_use = snapshot.in_use,
                    available = snapshot.available,
                    queued = snapshot.queued,
                    max_queued = snapshot.max_queued,
                    high_water = snapshot.high_water,
                    permit_timeout_ms =
                        u64::try_from(self.permit_timeout.as_millis()).unwrap_or(u64::MAX),
                    "tunnel connection capacity wait timed out"
                );
                Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    "tunnel connection capacity wait timed out",
                ))
            }
        }
    }

    #[cfg(test)]
    async fn acquire(
        &self,
        direction: &'static str,
    ) -> Result<OwnedSemaphorePermit, std::io::Error> {
        let admission = self.admit(direction)?;
        self.acquire_admitted(direction, admission).await
    }
}

async fn forward_one(outer: &SharedPeerConnection, tcp: TcpStream) -> Result<(), std::io::Error> {
    let established = outer.get().await?;
    let stream =
        tokio::time::timeout(STREAM_HANDSHAKE_TIMEOUT, established.connection.open_bi()).await;
    let (send, recv) = match stream {
        Ok(Ok(stream)) => stream,
        Ok(Err(error)) => {
            outer
                .invalidate(&established, "tunnel stream open failed")
                .await;
            return Err(std::io::Error::other(format!(
                "tunnel stream open failed: {error}"
            )));
        }
        Err(_) => {
            return Err(timed_out("tunnel stream open timed out"));
        }
    };
    let copied = copy_tunnel(send, recv, tcp).await;
    if established.connection.close_reason().is_some() {
        outer
            .invalidate(&established, "tunnel outer connection closed during copy")
            .await;
    }
    copied
}

async fn copy_tunnel(
    mut send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    tcp: TcpStream,
) -> Result<(), std::io::Error> {
    let (mut tcp_read, mut tcp_write) = tcp.into_split();
    let upstream = pump_tcp_to_stream(&mut tcp_read, &mut send);
    let downstream = pump_stream_to_tcp(&mut recv, &mut tcp_write);
    tokio::pin!(upstream);
    tokio::pin!(downstream);
    tokio::select! {
        result = &mut upstream => match result {
            Ok(()) => downstream.await,
            Err(error) => {
                tracing::debug!(error = %error, "tunnel upstream copy failed");
                Err(error)
            }
        },
        result = &mut downstream => match result {
            Ok(()) => upstream.await,
            Err(error) => {
                tracing::debug!(error = %error, "tunnel downstream copy failed");
                Err(error)
            }
        },
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

    #[test]
    fn formal_relay_burst_fits_bounded_tunnel_capacity() {
        assert_eq!(G6_TRANSIENT_ENROLLMENT_ENDPOINTS_PER_FAILURE_DOMAIN, 25);
        assert_eq!(REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS, 205);
        assert_eq!(MAX_TUNNEL_CONNECTIONS, 256);
        assert_eq!(MAX_TUNNEL_QUEUED_CONNECTIONS, 64);
    }

    #[tokio::test]
    async fn capacity_wait_is_bounded_and_queue_is_capped() {
        const TEST_TIMEOUT: Duration = Duration::from_millis(500);
        let limiter = ConnectionLimiter::with_limits(2, 1, TEST_TIMEOUT);
        let first = limiter.acquire("test").await.unwrap();
        let second = limiter.acquire("test").await.unwrap();
        assert_eq!(
            limiter.snapshot(),
            CapacitySnapshot {
                capacity: 2,
                in_use: 2,
                available: 0,
                queued: 0,
                max_queued: 1,
                high_water: 2,
            }
        );

        let waiting_limiter = limiter.clone();
        let waiting = tokio::spawn(async move { waiting_limiter.acquire("test").await });
        tokio::time::timeout(Duration::from_secs(1), async {
            while limiter.snapshot().queued != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("capacity waiter did not enter the bounded queue");

        let queue_full = limiter.acquire("test").await.unwrap_err();
        assert_eq!(queue_full.kind(), std::io::ErrorKind::WouldBlock);
        let timed_out = waiting.await.unwrap().unwrap_err();
        assert_eq!(timed_out.kind(), std::io::ErrorKind::TimedOut);
        assert_eq!(limiter.snapshot().queued, 0);

        drop(first);
        let replacement = limiter.acquire("test").await.unwrap();
        assert_eq!(limiter.snapshot().in_use, 2);
        drop(replacement);
        drop(second);
        assert_eq!(limiter.snapshot().in_use, 0);
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

        // A clean local half-close must still allow the reverse pump to
        // deliver the echo before the tunnel stream reaches EOF.
        let mut half_closed = tokio::time::timeout(STAGE_TIMEOUT, TcpStream::connect(forward_addr))
            .await
            .expect("half-close tunnel listener stage timed out")
            .unwrap();
        half_closed.write_all(ping).await.unwrap();
        half_closed.shutdown().await.unwrap();
        let mut half_close_response = Vec::new();
        tokio::time::timeout(
            STAGE_TIMEOUT,
            half_closed.read_to_end(&mut half_close_response),
        )
        .await
        .expect("clean tunnel half-close did not finish")
        .unwrap();
        assert_eq!(half_close_response, ping);

        // The pinned server side must refuse a different peer: either the
        // connection is rejected outright or the first stream read fails.
        assert!(
            stranger_is_rejected(&server_addr).await,
            "unpinned peer must not receive tunnel data"
        );

        forward.abort();
        let _ = tokio::time::timeout(STAGE_TIMEOUT, router.shutdown()).await;
    }

    async fn open_tunnel_clients(
        forward_addr: SocketAddr,
        indexes: std::ops::Range<usize>,
    ) -> Vec<TcpStream> {
        let expected = indexes.len();
        let mut connects = tokio::task::JoinSet::new();
        for index in indexes {
            connects.spawn(async move {
                let mut stream = TcpStream::connect(forward_addr).await.unwrap();
                let ping = u64::try_from(index).unwrap().to_be_bytes();
                stream.write_all(&ping).await.unwrap();
                let mut received = [0_u8; 8];
                stream.read_exact(&mut received).await.unwrap();
                assert_eq!(received, ping);
                stream
            });
        }
        let mut clients = Vec::with_capacity(expected);
        while let Some(client) = connects.join_next().await {
            clients.push(client.expect("tunnel client task failed"));
        }
        clients
    }

    async fn wait_for_no_flows(limiter: &ConnectionLimiter) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let snapshot = limiter.snapshot();
                if snapshot.in_use == 0 && snapshot.queued == 0 {
                    return;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("tunnel flow permits did not release");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    #[allow(clippy::too_many_lines)]
    async fn carries_the_formal_transition_connection_budget() {
        const STAGE_TIMEOUT: Duration = Duration::from_secs(45);
        const ENROLLMENT_CONNECTIONS: usize =
            G6_TRANSIENT_ENROLLMENT_ENDPOINTS_PER_FAILURE_DOMAIN * G6_RELAY_TCP_BURST_PER_ENDPOINT;
        const RUNTIME_CONNECTIONS: usize = (G6_FORMAL_AGENTS_PER_FAILURE_DOMAIN
            + G6_RELAY_SUPPORT_ENDPOINTS_PER_FAILURE_DOMAIN)
            * G6_RELAY_TCP_BURST_PER_ENDPOINT
            + G6_RELAY_TUNNEL_CONTROL_CONNECTIONS;
        const _: () = assert!(
            ENROLLMENT_CONNECTIONS + RUNTIME_CONNECTIONS == REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS
        );

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

        let (server_key, client_key) = (SecretKey::generate(), SecretKey::generate());
        let server_limiter = ConnectionLimiter::formal();
        let metrics = OuterConnectionMetrics::default();
        let router = tokio::time::timeout(
            STAGE_TIMEOUT,
            serve_with_components(
                server_key,
                RelayMode::Disabled,
                client_key.public(),
                echo_addr,
                server_limiter,
                metrics.clone(),
            ),
        )
        .await
        .expect("tunnel serve stage timed out")
        .unwrap();
        let server_addr = router.endpoint().addr();

        let endpoint = tokio::time::timeout(
            STAGE_TIMEOUT,
            bind_endpoint(client_key, RelayMode::Disabled),
        )
        .await
        .expect("tunnel forward endpoint stage timed out")
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let forward_addr = listener.local_addr().unwrap();
        let outer = SharedPeerConnection::new(endpoint, server_addr);
        let forward = tokio::spawn(run_forward_listener(
            listener,
            ConnectionLimiter::formal(),
            outer.clone(),
        ));

        // Hold the entire enrollment generation first, then prove the later
        // runtime/transportd/control generation can still enter and echo. This
        // stages the starvation boundary rather than letting all 205 clients
        // race; the second generation also pushes the live total past 128.
        let enrollment_clients = tokio::time::timeout(
            STAGE_TIMEOUT,
            open_tunnel_clients(forward_addr, 0..ENROLLMENT_CONNECTIONS),
        )
        .await
        .expect("enrollment-generation connections timed out");
        assert_eq!(enrollment_clients.len(), ENROLLMENT_CONNECTIONS);
        let runtime_clients = tokio::time::timeout(
            STAGE_TIMEOUT,
            open_tunnel_clients(
                forward_addr,
                ENROLLMENT_CONNECTIONS..REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS,
            ),
        )
        .await
        .expect("runtime-generation connections timed out");
        assert_eq!(runtime_clients.len(), RUNTIME_CONNECTIONS);
        {
            let state = outer.state.lock().await;
            assert_eq!(
                state.attempts, 1,
                "205 flows must dial one outer QUIC connection"
            );
            assert_eq!(state.last_generation, 1);
        }
        assert_eq!(
            metrics.accepted.load(Ordering::Acquire),
            1,
            "the server must accept one outer QUIC connection for 205 flows"
        );

        // A fully closed enrollment generation must drain while the runtime
        // generation stays live. Opening an immediate replacement generation
        // consumes all 51 spare permits and therefore fails unless the old
        // forwarding tasks release at least 49 permits inside the five-second
        // admission bound.
        drop(enrollment_clients);
        let replacement_clients = tokio::time::timeout(
            STAGE_TIMEOUT,
            open_tunnel_clients(
                forward_addr,
                REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS
                    ..REQUIRED_G6_RELAY_TUNNEL_CONNECTIONS + ENROLLMENT_CONNECTIONS,
            ),
        )
        .await
        .expect("closed enrollment generation did not drain before replacement");
        assert_eq!(replacement_clients.len(), ENROLLMENT_CONNECTIONS);
        {
            let state = outer.state.lock().await;
            assert_eq!(
                state.attempts, 1,
                "replacement flows must reuse the outer connection"
            );
            assert_eq!(state.last_generation, 1);
        }
        assert_eq!(metrics.accepted.load(Ordering::Acquire), 1);

        drop(replacement_clients);
        drop(runtime_clients);
        forward.abort();
        let _ = tokio::time::timeout(STAGE_TIMEOUT, router.shutdown()).await;
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    #[allow(clippy::too_many_lines)]
    async fn outer_failure_reconnects_once_and_copy_error_releases() {
        const STAGE_TIMEOUT: Duration = Duration::from_secs(20);

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

        let (server_key, client_key) = (SecretKey::generate(), SecretKey::generate());
        let server_limiter = ConnectionLimiter::with_limits(32, 8, Duration::from_secs(1));
        let metrics = OuterConnectionMetrics::default();
        let router = tokio::time::timeout(
            STAGE_TIMEOUT,
            serve_with_components(
                server_key,
                RelayMode::Disabled,
                client_key.public(),
                echo_addr,
                server_limiter.clone(),
                metrics.clone(),
            ),
        )
        .await
        .expect("tunnel serve stage timed out")
        .unwrap();

        let endpoint = tokio::time::timeout(
            STAGE_TIMEOUT,
            bind_endpoint(client_key, RelayMode::Disabled),
        )
        .await
        .expect("tunnel forward endpoint stage timed out")
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let forward_addr = listener.local_addr().unwrap();
        let outer = SharedPeerConnection::new(endpoint, router.endpoint().addr());
        let forward_limiter = ConnectionLimiter::with_limits(32, 8, Duration::from_secs(1));
        let forward = tokio::spawn(run_forward_listener(
            listener,
            forward_limiter.clone(),
            outer.clone(),
        ));

        let mut original =
            tokio::time::timeout(STAGE_TIMEOUT, open_tunnel_clients(forward_addr, 0..1))
                .await
                .expect("initial tunnel flow timed out")
                .pop()
                .unwrap();
        let first_generation = {
            let state = outer.state.lock().await;
            assert_eq!(state.attempts, 1);
            assert_eq!(state.last_generation, 1);
            state.connection.clone().unwrap()
        };
        assert_eq!(metrics.accepted.load(Ordering::Acquire), 1);

        first_generation
            .connection
            .close(VarInt::from_u32(0x2ff), b"test outer failure");
        let mut byte = [0_u8; 1];
        let closed = tokio::time::timeout(STAGE_TIMEOUT, original.read(&mut byte))
            .await
            .expect("local flow did not close after the outer connection failed");
        assert!(
            matches!(closed, Ok(0) | Err(_)),
            "closed outer connection unexpectedly delivered application data"
        );
        drop(original);
        wait_for_no_flows(&forward_limiter).await;
        wait_for_no_flows(&server_limiter).await;

        // A concurrent replacement burst must share one reconnect attempt and
        // establish exactly one new generation.
        let replacements =
            tokio::time::timeout(STAGE_TIMEOUT, open_tunnel_clients(forward_addr, 1..17))
                .await
                .expect("replacement flows did not reconnect");
        {
            let state = outer.state.lock().await;
            assert_eq!(state.attempts, 2, "reconnect must be single-flight");
            assert_eq!(state.last_generation, 2);
            assert_eq!(state.connection.as_ref().unwrap().generation, 2);
        }
        assert_eq!(metrics.accepted.load(Ordering::Acquire), 2);

        // A late failure from generation one must not evict generation two.
        outer
            .invalidate(&first_generation, "test stale generation")
            .await;
        let extra = tokio::time::timeout(STAGE_TIMEOUT, open_tunnel_clients(forward_addr, 17..18))
            .await
            .expect("post-invalidation tunnel flow timed out");
        {
            let state = outer.state.lock().await;
            assert_eq!(state.attempts, 2);
            assert_eq!(state.connection.as_ref().unwrap().generation, 2);
        }
        assert_eq!(metrics.accepted.load(Ordering::Acquire), 2);

        drop(extra);
        drop(replacements);
        forward.abort();
        let _ = tokio::time::timeout(STAGE_TIMEOUT, router.shutdown()).await;
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn target_failure_rejects_only_its_stream() {
        const STAGE_TIMEOUT: Duration = Duration::from_secs(20);

        let reservation = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target = reservation.local_addr().unwrap();
        drop(reservation);
        let (server_key, client_key) = (SecretKey::generate(), SecretKey::generate());
        let server_limiter = ConnectionLimiter::with_limits(4, 2, Duration::from_secs(1));
        let metrics = OuterConnectionMetrics::default();
        let router = tokio::time::timeout(
            STAGE_TIMEOUT,
            serve_with_components(
                server_key,
                RelayMode::Disabled,
                client_key.public(),
                target,
                server_limiter.clone(),
                metrics.clone(),
            ),
        )
        .await
        .expect("tunnel serve stage timed out")
        .unwrap();

        let endpoint = tokio::time::timeout(
            STAGE_TIMEOUT,
            bind_endpoint(client_key, RelayMode::Disabled),
        )
        .await
        .expect("tunnel forward endpoint stage timed out")
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let forward_addr = listener.local_addr().unwrap();
        let outer = SharedPeerConnection::new(endpoint, router.endpoint().addr());
        let forward = tokio::spawn(run_forward_listener(
            listener,
            ConnectionLimiter::with_limits(4, 2, Duration::from_secs(1)),
            outer.clone(),
        ));

        let mut rejected = TcpStream::connect(forward_addr).await.unwrap();
        rejected.write_all(b"activate stream").await.unwrap();
        let mut byte = [0_u8; 1];
        let rejected_read = tokio::time::timeout(STAGE_TIMEOUT, rejected.read(&mut byte))
            .await
            .expect("target failure did not terminate its tunnel stream");
        assert!(matches!(rejected_read, Ok(0) | Err(_)));
        drop(rejected);
        wait_for_no_flows(&server_limiter).await;

        let echo = TcpListener::bind(target).await.unwrap();
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
        let healthy = tokio::time::timeout(STAGE_TIMEOUT, open_tunnel_clients(forward_addr, 0..1))
            .await
            .expect("healthy replacement tunnel flow timed out");
        {
            let state = outer.state.lock().await;
            assert_eq!(state.attempts, 1);
            assert_eq!(state.last_generation, 1);
        }
        assert_eq!(
            metrics.accepted.load(Ordering::Acquire),
            1,
            "a target failure must not close the healthy outer connection"
        );

        drop(healthy);
        forward.abort();
        let _ = tokio::time::timeout(STAGE_TIMEOUT, router.shutdown()).await;
    }
}
