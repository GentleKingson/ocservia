//! Iroh endpoint ownership, protocol routing, and the typed UDS transport service.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock, Weak};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

mod admission;
mod artifacts;
mod events;
mod registry;
#[cfg(test)]
use admission::record_attempt;
use admission::{AttemptRejection, SecurityHook, record_global_attempt, trust_rejection_error};
use artifacts::{artifact_stream_deadline, finalize_artifact_consumption, relay_artifact_stream};
use futures_util::StreamExt as _;
use iroh::endpoint::{
    AfterHandshakeOutcome, Connection, EndpointHooks, PathEvent, QuicTransportConfig, RelayMode,
    VarInt, presets,
};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, EndpointId, SecretKey};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, ArtifactChunk,
    ArtifactConsumeRequest as AgentArtifactConsumeRequest,
    ArtifactConsumeResponse as AgentArtifactConsumeResponse, ArtifactFetchRequest, ArtifactGrantV1,
    CommandDeliveryMode, CommandEnvelope, CommandResult, CommandResultState, ConnectionFenceV2,
    EnrollRequest, EnrollResponse, FenceBindingV2, FenceOperationKind, HandshakeResult,
    SessionHandshake, SessionHandshakeResponse, TelemetryBatch, TelemetryPriority,
    command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, CloseNodeRequest, CloseNodeResponse,
    ConnectionPath, ConsumeArtifactRequest, ConsumeArtifactResponse, FetchArtifactRequest,
    GetNodeConnectionRequest, GetOwnerFenceRequest, GetOwnerFenceResponse, HealthRequest,
    HealthResponse, HealthStatus, NodeConnection, NodeTrustState, OwnerFenceDisposition,
    RegisterOwnerFenceRequest, RegisterOwnerFenceResponse, SendCommandRequest, SendCommandResponse,
    TransportEvent, TransportEventType, TrustUpdateDisposition, UpdateNodeTrustRequest,
    UpdateNodeTrustResponse, ValidateEnrollmentRequest, WatchEventsRequest,
    transport_service_server::TransportService, trust_service_client::TrustServiceClient,
};
use ocservia_contracts::session::{ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME};
use prost::Message;
use registry::finish_session;
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore, mpsc, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use uuid::Uuid;

/// ALPN for one-time enrollment connections.
pub const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
/// ALPN for approved agent sessions.
pub const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";

const PROTOCOL_MAJOR: u32 = 1;
const PROTOCOL_MINOR: u32 = 1;
const MAX_HANDSHAKE_BYTES: usize = 64 * 1024;
const MAX_FRAME_BYTES: usize = 1024 * 1024;
const MAX_FRAME_BYTES_U32: u32 = 1024 * 1024;
const MAX_STREAMS: u32 = 8;
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const STREAM_TIMEOUT: Duration = Duration::from_secs(15);
const MAX_CONNECTIONS: usize = 4096;
const MAX_ENROLLMENT_CONNECTIONS: usize = 64;
const MAX_AGENT_CONNECTIONS: usize = MAX_CONNECTIONS - MAX_ENROLLMENT_CONNECTIONS;
const MAX_ATTEMPTS_PER_MINUTE: usize = 30;
const MAX_TRUST_SNAPSHOT_BINDINGS: usize = 8192;
// Per-node telemetry admission. The Agent sends one batch every 30 seconds and
// drains an offline backlog at most 32 frames per tick, each at most 512 KiB.
// The bucket therefore holds that 32-frame burst plus 8 frames of slack and
// refills at 2 frames/s and 1 MiB/s (about twice the fastest legitimate drain,
// 32 frames per 30 s), so only a misbehaving node is limited.
const TELEMETRY_BURST_EVENTS: u64 = 40;
const TELEMETRY_REFILL_EVENTS_PER_SECOND: u64 = 2;
const TELEMETRY_BURST_BYTES: u64 = 32 * 512 * 1024;
const TELEMETRY_REFILL_BYTES_PER_MILLI: u64 = 1024;
// Security priority is claimed by the Agent itself and lets a frame use the
// subscriber capacity kept for control events (overflow then evicts the slow
// subscriber, which recovers by replaying retained events). Each node may claim
// it for a burst of 4 frames, refilled at one frame per minute; further claims
// are published as routine telemetry, so a forged priority cannot keep forcing
// subscribers to reconnect.
const SECURITY_TELEMETRY_FRAME_MILLIS: u64 = 60_000;
const SECURITY_TELEMETRY_BURST_MILLIS: u64 = 4 * SECURITY_TELEMETRY_FRAME_MILLIS;
/// Telemetry retained for replay never exceeds this many bytes (64 maximum
/// frames), and never more than half of the retained event slots, so
/// command, result and connection events keep their own capacity.
const MAX_RETAINED_TELEMETRY_BYTES: usize = 32 * 1024 * 1024;
/// QUIC application error code for an artifact stream aborted by this side.
const ARTIFACT_STREAM_ABORT: VarInt = VarInt::from_u32(0x108);

/// One security domain with capacity that cannot be consumed by another domain.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum TrustClass {
    /// Local admission for an `EndpointID` already present in the synchronized snapshot.
    KnownAgentHandshake,
    /// Live Controller authorization of a complete Agent session handshake.
    AgentAuthorization,
    /// High-priority live recheck immediately before connection registration.
    AgentRegistrationRecheck,
    /// Small local-only admission for an unknown enrollment `EndpointID`.
    UnknownEnrollmentPreAuth,
    /// Transactional completion after the bounded first frame's authority check.
    EnrollmentCompletion,
}

impl TrustClass {
    const COUNT: usize = 5;

    fn index(self) -> usize {
        match self {
            Self::KnownAgentHandshake => 0,
            Self::AgentAuthorization => 1,
            Self::AgentRegistrationRecheck => 2,
            Self::UnknownEnrollmentPreAuth => 3,
            Self::EnrollmentCompletion => 4,
        }
    }

    /// Stable bounded metric label.
    #[must_use]
    pub const fn label(self) -> &'static str {
        match self {
            Self::KnownAgentHandshake => "known_agent_handshake",
            Self::AgentAuthorization => "agent_authorization",
            Self::AgentRegistrationRecheck => "agent_registration_recheck",
            Self::UnknownEnrollmentPreAuth => "unknown_enrollment_pre_auth",
            Self::EnrollmentCompletion => "enrollment_completion",
        }
    }
}

/// Per-class rolling-window and concurrency limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TrustClassLimit {
    /// Attempts accepted during one rolling minute.
    pub attempts_per_minute: usize,
    /// Checks that may be in flight in this class.
    pub concurrency: usize,
}

/// Security defaults reserve substantially more capacity for approved Agents
/// than for unauthenticated enrollment traffic.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustBudgetConfig {
    limits: [TrustClassLimit; TrustClass::COUNT],
}

impl Default for TrustBudgetConfig {
    fn default() -> Self {
        Self {
            limits: [
                TrustClassLimit {
                    attempts_per_minute: 2400,
                    concurrency: 256,
                },
                TrustClassLimit {
                    attempts_per_minute: 1200,
                    concurrency: 48,
                },
                TrustClassLimit {
                    attempts_per_minute: 1200,
                    concurrency: 48,
                },
                TrustClassLimit {
                    attempts_per_minute: 120,
                    concurrency: 16,
                },
                TrustClassLimit {
                    attempts_per_minute: 120,
                    concurrency: 8,
                },
            ],
        }
    }
}

impl TrustBudgetConfig {
    /// Replaces one class limit after validating the complete safe range.
    ///
    /// # Errors
    ///
    /// Returns an error when either limit is outside the enforced safe range.
    pub fn with_limit(
        mut self,
        class: TrustClass,
        limit: TrustClassLimit,
    ) -> Result<Self, &'static str> {
        if !(1..=10_000).contains(&limit.attempts_per_minute)
            || !(1..=512).contains(&limit.concurrency)
        {
            return Err("trust class limits are outside the safe range");
        }
        self.limits[class.index()] = limit;
        Ok(self)
    }
}

#[derive(Debug)]
struct TrustClassBudget {
    attempts: Mutex<VecDeque<Instant>>,
    checks: Arc<Semaphore>,
    accepted: AtomicU64,
    rate_rejected: AtomicU64,
    capacity_rejected: AtomicU64,
    in_flight: AtomicU64,
    limit: TrustClassLimit,
}

#[derive(Debug)]
struct TrustBudgetSet {
    classes: [TrustClassBudget; TrustClass::COUNT],
}

impl TrustBudgetSet {
    fn new(config: &TrustBudgetConfig) -> Self {
        Self {
            classes: std::array::from_fn(|index| {
                let limit = config.limits[index];
                TrustClassBudget {
                    attempts: Mutex::new(VecDeque::with_capacity(limit.attempts_per_minute)),
                    checks: Arc::new(Semaphore::new(limit.concurrency)),
                    accepted: AtomicU64::new(0),
                    rate_rejected: AtomicU64::new(0),
                    capacity_rejected: AtomicU64::new(0),
                    in_flight: AtomicU64::new(0),
                    limit,
                }
            }),
        }
    }

    async fn acquire(
        self: &Arc<Self>,
        class: TrustClass,
    ) -> Result<TrustBudgetPermit, AttemptRejection> {
        self.acquire_at(class, Instant::now()).await
    }

    async fn acquire_at(
        self: &Arc<Self>,
        class: TrustClass,
        now: Instant,
    ) -> Result<TrustBudgetPermit, AttemptRejection> {
        let budget = &self.classes[class.index()];
        let Ok(permit) = budget.checks.clone().try_acquire_owned() else {
            budget.capacity_rejected.fetch_add(1, Ordering::Relaxed);
            return Err(AttemptRejection::Capacity);
        };
        let mut attempts = budget.attempts.lock().await;
        if let Err(rejection) =
            record_global_attempt(&mut attempts, now, budget.limit.attempts_per_minute)
        {
            budget.rate_rejected.fetch_add(1, Ordering::Relaxed);
            return Err(rejection);
        }
        drop(attempts);
        budget.accepted.fetch_add(1, Ordering::Relaxed);
        budget.in_flight.fetch_add(1, Ordering::Relaxed);
        Ok(TrustBudgetPermit {
            budgets: Arc::clone(self),
            class,
            _permit: permit,
        })
    }

    fn acquire_capacity(
        self: &Arc<Self>,
        class: TrustClass,
    ) -> Result<TrustBudgetPermit, AttemptRejection> {
        let budget = &self.classes[class.index()];
        let permit = budget.checks.clone().try_acquire_owned().map_err(|_| {
            budget.capacity_rejected.fetch_add(1, Ordering::Relaxed);
            AttemptRejection::Capacity
        })?;
        budget.in_flight.fetch_add(1, Ordering::Relaxed);
        Ok(TrustBudgetPermit {
            budgets: Arc::clone(self),
            class,
            _permit: permit,
        })
    }

    fn snapshot(&self, class: TrustClass) -> TrustMetricsSnapshot {
        let budget = &self.classes[class.index()];
        TrustMetricsSnapshot {
            accepted: budget.accepted.load(Ordering::Relaxed),
            rate_rejected: budget.rate_rejected.load(Ordering::Relaxed),
            capacity_rejected: budget.capacity_rejected.load(Ordering::Relaxed),
            in_flight: budget.in_flight.load(Ordering::Relaxed),
        }
    }
}

struct TrustBudgetPermit {
    budgets: Arc<TrustBudgetSet>,
    class: TrustClass,
    _permit: OwnedSemaphorePermit,
}

impl Drop for TrustBudgetPermit {
    fn drop(&mut self) {
        self.budgets.classes[self.class.index()]
            .in_flight
            .fetch_sub(1, Ordering::Relaxed);
    }
}

/// Bounded counters backing trust metrics. No `EndpointID` is recorded.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct TrustMetricsSnapshot {
    /// Accepted attempts.
    pub accepted: u64,
    /// Rolling-window refusals.
    pub rate_rejected: u64,
    /// Concurrency refusals.
    pub capacity_rejected: u64,
    /// Currently retained permits.
    pub in_flight: u64,
}

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;
type ArtifactStream = Pin<Box<dyn Stream<Item = Result<ArtifactChunk, Status>> + Send>>;

/// A bounded, transport-only identity policy supplied at process startup.
#[derive(Clone, Debug)]
pub struct IdentityPolicy {
    state: Arc<RwLock<IdentityState>>,
}

impl Default for IdentityPolicy {
    fn default() -> Self {
        Self::new(HashMap::new(), HashSet::new())
    }
}

#[derive(Debug, Default)]
struct IdentityState {
    approved: HashMap<EndpointId, Vec<u8>>,
    revoked: HashMap<EndpointId, Vec<u8>>,
    revisions: HashMap<EndpointId, u64>,
    synchronized: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum PolicyUpdateDisposition {
    Applied,
    Stale,
    Rejected,
}

impl IdentityPolicy {
    /// Builds a policy from endpoint-to-node bindings and revoked identifiers.
    #[must_use]
    pub fn new(approved: HashMap<EndpointId, Vec<u8>>, revoked: HashSet<EndpointId>) -> Self {
        let revisions = revoked
            .iter()
            .copied()
            .map(|endpoint| (endpoint, u64::MAX))
            .collect();
        Self {
            state: Arc::new(RwLock::new(IdentityState {
                approved,
                revoked: revoked
                    .into_iter()
                    .map(|endpoint| (endpoint, Vec::new()))
                    .collect(),
                revisions,
                synchronized: true,
            })),
        }
    }

    fn replace_snapshot(
        &self,
        bindings: impl IntoIterator<Item = (EndpointId, Vec<u8>, NodeTrustState, u64)>,
    ) -> Result<(), &'static str> {
        let mut approved = HashMap::new();
        let mut revoked = HashMap::new();
        let mut revisions = HashMap::new();
        for (endpoint, node_id, state, revision) in bindings {
            if node_id.len() != 16
                || revision == 0
                || revisions.len() >= MAX_TRUST_SNAPSHOT_BINDINGS
                || revisions.insert(endpoint, revision).is_some()
            {
                return Err("trust snapshot is invalid or exceeds capacity");
            }
            match state {
                NodeTrustState::Active => {
                    if approved.values().any(|bound| bound == &node_id) {
                        return Err("trust snapshot contains a duplicate node binding");
                    }
                    approved.insert(endpoint, node_id);
                }
                NodeTrustState::Revoked => {
                    revoked.insert(endpoint, node_id);
                }
                NodeTrustState::Unspecified => return Err("trust snapshot state is invalid"),
            }
        }
        let mut state = self
            .state
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *state = IdentityState {
            approved,
            revoked,
            revisions,
            synchronized: true,
        };
        Ok(())
    }

    fn permits(&self, endpoint: EndpointId, alpn: &[u8]) -> bool {
        let state = self
            .state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if !state.synchronized || state.revoked.contains_key(&endpoint) {
            return false;
        }
        alpn == ENROLL_ALPN || (alpn == AGENT_ALPN && state.approved.contains_key(&endpoint))
    }

    fn revoked(&self, endpoint: EndpointId) -> bool {
        self.state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .revoked
            .contains_key(&endpoint)
    }

    fn revision(&self, endpoint: EndpointId) -> u64 {
        self.state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .revisions
            .get(&endpoint)
            .copied()
            .unwrap_or(0)
    }

    fn update(
        &self,
        endpoint: EndpointId,
        node_id: Vec<u8>,
        state: NodeTrustState,
        revision: u64,
    ) -> PolicyUpdateDisposition {
        let mut identities = self
            .state
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if !identities.revisions.contains_key(&endpoint)
            && identities.revisions.len() >= MAX_TRUST_SNAPSHOT_BINDINGS
        {
            return PolicyUpdateDisposition::Rejected;
        }
        let current_revision = identities.revisions.get(&endpoint).copied().unwrap_or(0);
        if revision < current_revision {
            return PolicyUpdateDisposition::Stale;
        }
        if revision == current_revision && current_revision != 0 {
            let matches = match state {
                NodeTrustState::Active => identities
                    .approved
                    .get(&endpoint)
                    .is_some_and(|bound_node| bound_node == &node_id),
                NodeTrustState::Revoked => identities
                    .revoked
                    .get(&endpoint)
                    .is_some_and(|bound_node| bound_node.is_empty() || bound_node == &node_id),
                NodeTrustState::Unspecified => false,
            };
            return if matches {
                PolicyUpdateDisposition::Applied
            } else {
                PolicyUpdateDisposition::Rejected
            };
        }
        match state {
            NodeTrustState::Active => {
                if identities.revoked.contains_key(&endpoint) {
                    return PolicyUpdateDisposition::Rejected;
                }
                if identities
                    .approved
                    .iter()
                    .any(|(bound_endpoint, bound_node)| {
                        (*bound_endpoint == endpoint && bound_node != &node_id)
                            || (*bound_endpoint != endpoint && bound_node == &node_id)
                    })
                {
                    return PolicyUpdateDisposition::Rejected;
                }
                identities.approved.insert(endpoint, node_id);
            }
            NodeTrustState::Revoked => {
                if identities
                    .approved
                    .get(&endpoint)
                    .is_some_and(|bound_node| bound_node != &node_id)
                {
                    return PolicyUpdateDisposition::Rejected;
                }
                if identities
                    .revoked
                    .get(&endpoint)
                    .is_some_and(|bound_node| !bound_node.is_empty() && bound_node != &node_id)
                {
                    return PolicyUpdateDisposition::Rejected;
                }
                identities.approved.remove(&endpoint);
                identities.revoked.insert(endpoint, node_id);
            }
            NodeTrustState::Unspecified => return PolicyUpdateDisposition::Rejected,
        }
        identities.revisions.insert(endpoint, revision);
        PolicyUpdateDisposition::Applied
    }

    fn retained_state(&self, endpoint: EndpointId) -> NodeTrustState {
        let identities = self
            .state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if identities.revoked.contains_key(&endpoint) {
            NodeTrustState::Revoked
        } else if identities.approved.contains_key(&endpoint) {
            NodeTrustState::Active
        } else {
            NodeTrustState::Unspecified
        }
    }
}

#[derive(Clone, Debug)]
pub struct TrustAuthority {
    client: TrustServiceClient<tonic::transport::Channel>,
    budgets: Arc<TrustBudgetSet>,
}

impl TrustAuthority {
    #[must_use]
    pub fn new(channel: tonic::transport::Channel) -> Self {
        Self::new_with_config(channel, &TrustBudgetConfig::default())
    }

    /// Builds a trust client with independently validated class limits.
    #[must_use]
    pub fn new_with_config(channel: tonic::transport::Channel, config: &TrustBudgetConfig) -> Self {
        Self {
            client: TrustServiceClient::new(channel),
            budgets: Arc::new(TrustBudgetSet::new(config)),
        }
    }

    /// Returns a bounded metric snapshot for one fixed class label.
    #[must_use]
    pub fn metrics(&self, class: TrustClass) -> TrustMetricsSnapshot {
        self.budgets.snapshot(class)
    }

    async fn load_snapshot(&self, policy: &IdentityPolicy) -> Result<(), AcceptError> {
        let mut client = self.client.clone();
        let response = tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            client.list_node_trust(
                ocservia_contracts::generated::ocserv::platform::transport::v1::ListNodeTrustRequest {},
            ),
        )
        .await
        .map_err(|_| protocol_error("initial trust snapshot timed out"))?
        .map_err(|_| protocol_error("initial trust snapshot unavailable"))?
        .into_inner();
        let mut bindings = Vec::with_capacity(response.bindings.len());
        for binding in response.bindings {
            let endpoint = EndpointId::from_bytes(
                binding
                    .endpoint_id
                    .as_slice()
                    .try_into()
                    .map_err(|_| protocol_error("initial trust snapshot invalid"))?,
            )
            .map_err(|_| protocol_error("initial trust snapshot invalid"))?;
            let state = NodeTrustState::try_from(binding.state)
                .map_err(|_| protocol_error("initial trust snapshot invalid"))?;
            bindings.push((endpoint, binding.node_id, state, binding.revision));
        }
        policy
            .replace_snapshot(bindings)
            .map_err(|_| protocol_error("initial trust snapshot invalid"))
    }

    async fn recheck_agent(&self, endpoint: EndpointId) -> Result<bool, AttemptRejection> {
        let _permit = self
            .budgets
            .acquire(TrustClass::AgentRegistrationRecheck)
            .await?;
        let mut client = self.client.clone();
        Ok(tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            client.check_endpoint(CheckEndpointRequest {
                endpoint_id: endpoint.as_bytes().to_vec(),
                alpn: String::from_utf8_lossy(AGENT_ALPN).into_owned(),
            }),
        )
        .await
        .ok()
        .and_then(Result::ok)
        .is_some_and(|response| response.into_inner().permitted))
    }

    async fn enroll(&self, request: EnrollRequest) -> Result<EnrollResponse, AcceptError> {
        let validation_permit = self
            .budgets
            .acquire_capacity(TrustClass::UnknownEnrollmentPreAuth)
            .map_err(trust_rejection_error)?;
        let mut validation_client = self.client.clone();
        let validated = tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            validation_client.validate_enrollment(ValidateEnrollmentRequest {
                enrollment: Some(request.clone()),
            }),
        )
        .await
        .map_err(|_| protocol_error("enrollment credential validation timed out"))?
        .map_err(|_| protocol_error("enrollment rejected"))?
        .into_inner()
        .permitted;
        if !validated {
            return Err(protocol_error("enrollment rejected"));
        }
        drop(validation_permit);
        // Only a request carrying independently authenticated, currently
        // unconsumed authority may enter completion capacity. Enroll locks and
        // atomically consumes that same authority with the durable node write.
        let _permit = self
            .budgets
            .acquire(TrustClass::EnrollmentCompletion)
            .await
            .map_err(trust_rejection_error)?;
        let mut client = self.client.clone();
        tokio::time::timeout(HANDSHAKE_TIMEOUT, client.enroll(request))
            .await
            .map_err(|_| protocol_error("enrollment authority timed out"))?
            .map(Response::into_inner)
            .map_err(|_| protocol_error("enrollment rejected"))
    }

    async fn authorize(
        &self,
        endpoint: EndpointId,
        handshake: SessionHandshake,
    ) -> Result<SessionHandshakeResponse, AcceptError> {
        let _permit = self
            .budgets
            .acquire(TrustClass::AgentAuthorization)
            .await
            .map_err(trust_rejection_error)?;
        let mut client = self.client.clone();
        tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            client.authorize_session(AuthorizeSessionRequest {
                remote_endpoint_id: endpoint.as_bytes().to_vec(),
                handshake: Some(handshake),
            }),
        )
        .await
        .map_err(|_| protocol_error("trust authority timed out"))?
        .map(Response::into_inner)
        .map_err(|_| protocol_error("session authorization failed"))
    }
}

#[derive(Clone)]
struct Shared {
    inner: Arc<Inner>,
}

/// One signature-verified connection-owner fence term retained per node.
/// `verified` is the authorization truth; `fence` is the verified wire copy
/// returned to Controller readers through `GetOwnerFence`.
#[derive(Clone)]
struct RegisteredFence {
    verified: ocservia_command_authorization::VerifiedConnectionFenceV2,
    fence: ConnectionFenceV2,
    endpoint_id: [u8; 32],
}

struct Inner {
    connections: Mutex<HashMap<Vec<u8>, RegisteredConnection>>,
    registration_tokens: Mutex<HashMap<Vec<u8>, Weak<()>>>,
    fences: Mutex<HashMap<Vec<u8>, RegisteredFence>>,
    keyring: Arc<ControllerCommandKeyring>,
    events: Mutex<EventState>,
    telemetry_budgets: Mutex<HashMap<Vec<u8>, TelemetryBudget>>,
    event_capacity: usize,
    agent_connection_permits: Arc<Semaphore>,
    enrollment_connection_permits: Arc<Semaphore>,
    shutdown: watch::Sender<bool>,
}

struct RegisteredConnection {
    metadata: NodeConnection,
    connection: Connection,
    max_message_size: usize,
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    session_expires_at: Option<SystemTime>,
    fence: RegisteredFence,
}

struct NegotiatedSession {
    handshake: SessionHandshake,
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    session_expires_at: Option<SystemTime>,
    fence: RegisteredFence,
}

struct EventState {
    retained: VecDeque<TransportEvent>,
    subscribers: Vec<mpsc::Sender<Result<TransportEvent, Status>>>,
    telemetry_retained: usize,
    telemetry_retained_bytes: usize,
}

fn is_telemetry(event: &TransportEvent) -> bool {
    event.r#type == i32::from(TransportEventType::Telemetry)
}

impl EventState {
    /// Drops the oldest retained telemetry event; control events are never
    /// chosen. Returns whether one was removed.
    fn evict_oldest_telemetry(&mut self) -> bool {
        let Some(index) = self.retained.iter().position(is_telemetry) else {
            return false;
        };
        if let Some(evicted) = self.retained.remove(index) {
            self.telemetry_retained -= 1;
            self.telemetry_retained_bytes -= evicted.payload.len();
        }
        true
    }

    fn pop_oldest(&mut self) {
        if let Some(evicted) = self.retained.pop_front()
            && is_telemetry(&evicted)
        {
            self.telemetry_retained -= 1;
            self.telemetry_retained_bytes -= evicted.payload.len();
        }
    }
}

/// Token bucket for one node's telemetry; tokens are milli-events and bytes.
struct TelemetryBudget {
    milli_events: u64,
    bytes: u64,
    security_millis: u64,
    updated: tokio::time::Instant,
}

impl Shared {
    fn new(event_capacity: usize, keyring: Arc<ControllerCommandKeyring>) -> Self {
        let capacity = event_capacity.clamp(1, MAX_CONNECTIONS);
        let (shutdown, _) = watch::channel(false);
        Self {
            inner: Arc::new(Inner {
                connections: Mutex::new(HashMap::new()),
                registration_tokens: Mutex::new(HashMap::new()),
                fences: Mutex::new(HashMap::new()),
                keyring,
                events: Mutex::new(EventState {
                    retained: VecDeque::with_capacity(capacity),
                    subscribers: Vec::new(),
                    telemetry_retained: 0,
                    telemetry_retained_bytes: 0,
                }),
                telemetry_budgets: Mutex::new(HashMap::new()),
                event_capacity: capacity,
                agent_connection_permits: Arc::new(Semaphore::new(MAX_AGENT_CONNECTIONS)),
                enrollment_connection_permits: Arc::new(Semaphore::new(MAX_ENROLLMENT_CONNECTIONS)),
                shutdown,
            }),
        }
    }

    /// Charges one telemetry frame to the node's bucket. `false` means the
    /// node exceeded its budget and the frame must not be published.
    async fn admit_telemetry(&self, node_id: &[u8], bytes: usize) -> bool {
        let now = tokio::time::Instant::now();
        let bytes = u64::try_from(bytes).unwrap_or(u64::MAX);
        let mut budgets = self.inner.telemetry_budgets.lock().await;
        if budgets.len() >= MAX_CONNECTIONS {
            // An idle bucket is full again, so forgetting it changes nothing.
            budgets.retain(|_, budget| now.duration_since(budget.updated) < Duration::from_mins(1));
        }
        let budget = budgets
            .entry(node_id.to_vec())
            .or_insert_with(|| TelemetryBudget {
                milli_events: TELEMETRY_BURST_EVENTS * 1000,
                bytes: TELEMETRY_BURST_BYTES,
                security_millis: SECURITY_TELEMETRY_BURST_MILLIS,
                updated: now,
            });
        let elapsed =
            u64::try_from(now.duration_since(budget.updated).as_millis()).unwrap_or(u64::MAX);
        budget.updated = now;
        budget.milli_events = budget
            .milli_events
            .saturating_add(elapsed.saturating_mul(TELEMETRY_REFILL_EVENTS_PER_SECOND))
            .min(TELEMETRY_BURST_EVENTS * 1000);
        budget.bytes = budget
            .bytes
            .saturating_add(elapsed.saturating_mul(TELEMETRY_REFILL_BYTES_PER_MILLI))
            .min(TELEMETRY_BURST_BYTES);
        budget.security_millis = budget
            .security_millis
            .saturating_add(elapsed)
            .min(SECURITY_TELEMETRY_BURST_MILLIS);
        if budget.milli_events < 1000 || budget.bytes < bytes {
            return false;
        }
        budget.milli_events -= 1000;
        budget.bytes -= bytes;
        true
    }

    /// Spends the node's security-priority allowance for one admitted frame.
    /// `false` means the frame is published as routine telemetry.
    async fn claim_security_priority(&self, node_id: &[u8]) -> bool {
        let mut budgets = self.inner.telemetry_budgets.lock().await;
        let Some(budget) = budgets.get_mut(node_id) else {
            return false;
        };
        if budget.security_millis < SECURITY_TELEMETRY_FRAME_MILLIS {
            return false;
        }
        budget.security_millis -= SECURITY_TELEMETRY_FRAME_MILLIS;
        true
    }

    /// Verifies one Controller-signed operation binding against the node's
    /// registered fence term. The steps are fail-closed and ordered: both
    /// signatures and their node/endpoint pinning first, then the carrier
    /// identity, capability membership, and lease validity, and only then the
    /// exact-term match against the registered dispatch fence.
    async fn verify_operation_binding(
        &self,
        node_id: &[u8],
        endpoint_hint: Option<&[u8; 32]>,
        session_fence: Option<&RegisteredFence>,
        binding: Option<&FenceBindingV2>,
        kind: FenceOperationKind,
        operation_id: &[u8],
    ) -> Result<Option<RegisteredFence>, Status> {
        let node_id = fixed16(node_id)?;
        let registered = self.registered_fence(&node_id).await;
        let Some(registered) = registered else {
            return Err(Status::permission_denied(
                "no owner fence is registered for this node",
            ));
        };
        let Some(binding) = binding else {
            return Err(Status::permission_denied(
                "an owner fence binding is required for this operation",
            ));
        };
        let keyring = self.keyring();
        let endpoint = endpoint_hint.copied().unwrap_or(registered.endpoint_id);
        let (now_seconds, now_nanos) = unix_now_parts();
        let claims = keyring
            .verify_fence_binding_v2_at(binding, &node_id, &endpoint, now_seconds, now_nanos)
            .map_err(|_| Status::permission_denied("fence binding is invalid"))?;
        if claims.operation_kind != kind as u32 || claims.operation_id.as_slice() != operation_id {
            return Err(Status::permission_denied(
                "fence binding does not match the requested operation",
            ));
        }
        if !registered
            .verified
            .capabilities
            .iter()
            .any(|capability| capability == &claims.capability)
        {
            return Err(Status::permission_denied(
                "fence binding capability is not held by the registered owner",
            ));
        }
        if lease_expired_at(
            registered.verified.lease_until_seconds,
            registered.verified.lease_until_nanos,
        ) {
            return Err(Status::failed_precondition(
                "the registered owner lease has expired",
            ));
        }
        if !claims.matches_fence(&registered.verified) {
            return Err(Status::permission_denied(
                "fence binding does not match the registered owner term",
            ));
        }
        match session_fence {
            None if kind == FenceOperationKind::Artifact => {
                return Err(Status::permission_denied(
                    "artifact operation requires a fenced active agent session",
                ));
            }
            Some(session_fence)
                if session_fence.endpoint_id != endpoint
                    || !same_fence_term(&session_fence.verified, &registered.verified) =>
            {
                return Err(Status::permission_denied(
                    "operation fence does not match the active agent session",
                ));
            }
            None | Some(_) => {}
        }
        Ok(Some(registered))
    }

    fn keyring(&self) -> &Arc<ControllerCommandKeyring> {
        &self.inner.keyring
    }

    async fn shutdown(&self) {
        let _ = self.inner.shutdown.send(true);
        self.inner.events.lock().await.subscribers.clear();
        let connections = {
            let mut registry = self.inner.connections.lock().await;
            registry.drain().map(|(_, value)| value).collect::<Vec<_>>()
        };
        for registered in connections {
            registered
                .connection
                .close(VarInt::from_u32(0x105), b"transport shutdown");
        }
    }
}

/// The typed UDS service backed by active Iroh connections.
#[derive(Clone)]
pub struct IrohTransportService {
    shared: Shared,
    policy: IdentityPolicy,
}

impl IrohTransportService {
    /// Creates the typed transport service with its endpoint policy and
    /// Controller fence verification keyring.
    #[must_use]
    pub fn new(
        event_capacity: usize,
        policy: IdentityPolicy,
        keyring: Arc<ControllerCommandKeyring>,
    ) -> Self {
        Self {
            shared: Shared::new(event_capacity, keyring),
            policy,
        }
    }

    /// Stops active connections and terminates all long-lived event streams.
    pub async fn begin_shutdown(&self) {
        self.shared.shutdown().await;
    }

    /// Validates one command against the node's registered session before
    /// any Agent stream is opened: negotiated capability, authorization
    /// revision, Controller grant expiry, negotiated message size, and the
    /// connection-owner fence term.
    async fn validate_command_dispatch(
        &self,
        node_id: &[u8],
        command: &CommandEnvelope,
        request_len: usize,
    ) -> Result<(Connection, usize), Status> {
        let (
            connection,
            max_message_size,
            negotiated_capabilities,
            authorization_revision,
            session_expires_at,
            session_fence,
        ) = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get_mut(node_id)
            .map(|entry| {
                (
                    entry.connection.clone(),
                    entry.max_message_size,
                    entry.negotiated_capabilities.clone(),
                    entry.authorization_revision,
                    entry.session_expires_at,
                    entry.fence.clone(),
                )
            })
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if !negotiated_capabilities.contains(&command.required_capability) {
            return Err(Status::permission_denied(
                "command capability was not negotiated for this session",
            ));
        }
        if authorization_revision != 0 && command.expected_revision != authorization_revision {
            return Err(Status::failed_precondition(
                "command authorization revision does not match the active session",
            ));
        }
        if session_expires_at.is_some_and(|expires_at| expires_at <= SystemTime::now()) {
            return Err(Status::failed_precondition(
                "Controller session grant has expired",
            ));
        }
        if request_len > max_message_size {
            return Err(Status::resource_exhausted(
                "command exceeds the agent's negotiated message size",
            ));
        }
        self.verify_command_fence(node_id, command, &session_fence)
            .await?;
        Ok((connection, max_message_size))
    }

    /// Enforces the connection-owner fence before one command stream is
    /// opened. The predicate is fail-closed and ordered: both Controller
    /// signatures with their node/endpoint pinning first, then capability
    /// membership and carrier identity, then lease validity, and only then
    /// the exact-term match against the registered session and owner fences.
    /// A dispatch whose term predates the registered epoch is rejected, so a
    /// resumed old owner cannot reach the Agent stream.
    async fn verify_command_fence(
        &self,
        node_id: &[u8],
        command: &CommandEnvelope,
        session_fence: &RegisteredFence,
    ) -> Result<(), Status> {
        let keyring = self.shared.keyring();
        let node_id = fixed16(node_id)?;
        let (now_seconds, now_nanos) = unix_now_parts();
        let (verified, claims) = verify_command_carriers(
            keyring,
            &node_id,
            &session_fence.endpoint_id,
            command,
            now_seconds,
            now_nanos,
        )?;
        let registered = self
            .shared
            .registered_fence(&node_id)
            .await
            .ok_or_else(|| {
                Status::permission_denied("no owner fence is registered for this node")
            })?;
        if !same_fence_term(&registered.verified, &verified)
            || !same_fence_term(&session_fence.verified, &verified)
        {
            return Err(Status::permission_denied(
                "command fence does not match the registered owner term",
            ));
        }
        if !claims.matches_fence(&verified) {
            return Err(Status::permission_denied(
                "command fence binding does not match the command fence term",
            ));
        }
        Ok(())
    }
}

#[tonic::async_trait]
impl TransportService for IrohTransportService {
    type WatchEventsStream = EventStream;
    type FetchArtifactStream = ArtifactStream;

    async fn health(
        &self,
        _request: Request<HealthRequest>,
    ) -> Result<Response<HealthResponse>, Status> {
        Ok(Response::new(HealthResponse {
            status: HealthStatus::Serving.into(),
            version: env!("CARGO_PKG_VERSION").to_owned(),
        }))
    }

    async fn get_node_connection(
        &self,
        request: Request<GetNodeConnectionRequest>,
    ) -> Result<Response<NodeConnection>, Status> {
        let node_id = validate_uuid(&request.into_inner().node_id, "node_id")?;
        let metadata = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id)
            .map(|entry| entry.metadata.clone())
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        Ok(Response::new(metadata))
    }

    async fn send_command(
        &self,
        request: Request<SendCommandRequest>,
    ) -> Result<Response<SendCommandResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.command_envelope.len() > MAX_FRAME_BYTES {
            return Err(Status::resource_exhausted("command exceeds 1 MiB"));
        }
        let command =
            decode_strict_command_envelope(request.command_envelope.as_slice()).map_err(|_| {
                Status::invalid_argument(
                    "command envelope protobuf is invalid or contains unknown fields",
                )
            })?;
        if command.node_id != node_id {
            return Err(Status::invalid_argument(
                "command node_id does not match request",
            ));
        }
        validate_traceparent(&command.traceparent)?;
        let response_deadline = command_response_deadline(&command)?;
        let (connection, max_message_size) = self
            .validate_command_dispatch(&node_id, &command, request.command_envelope.len())
            .await?;
        let frame = request.command_envelope;
        let response_connection = connection.clone();
        let recv = tokio::time::timeout(STREAM_TIMEOUT, async move {
            let (mut send, recv) = connection
                .open_bi()
                .await
                .map_err(|_| Status::unavailable("failed to open command stream"))?;
            let frame_len = u32::try_from(frame.len())
                .map_err(|_| Status::resource_exhausted("command length exceeds u32"))?;
            send.write_all(&frame_len.to_be_bytes())
                .await
                .map_err(|_| Status::unavailable("failed to write command length"))?;
            send.write_all(&frame)
                .await
                .map_err(|_| Status::unavailable("failed to write command"))?;
            send.finish()
                .map_err(|_| Status::unavailable("failed to finish command stream"))?;
            Ok::<_, Status>(recv)
        })
        .await
        .map_err(|_| Status::deadline_exceeded("command stream timed out"))??;
        let (dispatch_path, dispatch_path_detail, _) = metadata_path(&response_connection);
        let dispatch_path = match dispatch_path {
            ConnectionPath::Direct => "direct",
            ConnectionPath::Relay => "relay",
            ConnectionPath::Unspecified => "unspecified",
        };
        let fence = command.connection_fence.as_ref();
        tracing::info!(
            event_type = "command_frame_written",
            message_id = %hex::encode(&command.message_id),
            command_id = %hex::encode(&command.command_id),
            operation_id = %hex::encode(&command.operation_id),
            idempotency_key = %hex::encode(&command.idempotency_key),
            delivery_mode = command.delivery_mode,
            sequence = command.sequence,
            expected_revision = command.expected_revision,
            required_capability = %command.required_capability,
            semantic_payload_sha256 = %hex::encode(&command.semantic_payload_sha256),
            semantic_payload_hash_version = command.semantic_payload_hash_version,
            node_id = %hex::encode(&node_id),
            owner_fence_id = %fence.map_or_else(String::new, |value| hex::encode(&value.fence_id)),
            connection_id = %fence.map_or_else(String::new, |value| hex::encode(&value.connection_id)),
            owner_epoch = fence.map_or(0, |value| value.owner_epoch),
            path = dispatch_path,
            path_detail = %dispatch_path_detail,
            "command frame written to authenticated Agent session"
        );
        tokio::spawn(read_agent_events(
            recv,
            self.shared.clone(),
            node_id,
            max_message_size,
            response_connection,
            response_deadline,
            command,
        ));
        Ok(Response::new(SendCommandResponse { accepted: true }))
    }

    async fn fetch_artifact(
        &self,
        request: Request<FetchArtifactRequest>,
    ) -> Result<Response<Self::FetchArtifactStream>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        let artifact_id = validate_uuid(&request.artifact_id, "artifact_id")?;
        let grant = request
            .grant
            .as_ref()
            .ok_or_else(|| Status::permission_denied("artifact grant is required"))?;
        if request.purpose != "certificate_p12"
            || request.max_bytes == 0
            || request.max_bytes > 64 * 1024 * 1024
            || grant.node_id != request.node_id
            || grant.artifact_id != request.artifact_id
            || grant.purpose != request.purpose
            || grant.max_bytes != request.max_bytes
        {
            return Err(Status::invalid_argument("artifact request is invalid"));
        }
        // Acceptance boundary contract: no response headers and no artifact
        // frames may leave this handler before verify_operation_binding has
        // accepted the fence binding. The controller holds its ownership
        // guard until the first validated frame of this stream, so deferring
        // or front-loading any output past this point would reopen the
        // stale-owner fetch window the guard exists to close.
        let (connection, connection_fence) = self
            .shared
            .artifact_connection(&node_id, request.fence_binding.as_ref(), &artifact_id)
            .await?;
        let stream_deadline = artifact_stream_deadline(grant)?;
        let payload = ArtifactFetchRequest {
            artifact_id,
            purpose: request.purpose,
            max_bytes: request.max_bytes,
            grant: request.grant,
            connection_fence,
            fence_binding: request.fence_binding,
        }
        .encode_to_vec();
        let (sender, receiver) = mpsc::channel::<Result<ArtifactChunk, Status>>(8);
        tokio::spawn(relay_artifact_stream(
            connection,
            payload,
            request.artifact_id,
            request.max_bytes,
            sender,
            stream_deadline,
        ));
        Ok(Response::new(Box::pin(ReceiverStream::new(receiver))))
    }

    async fn consume_artifact(
        &self,
        request: Request<ConsumeArtifactRequest>,
    ) -> Result<Response<ConsumeArtifactResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        let grant = request
            .grant
            .as_ref()
            .ok_or_else(|| Status::permission_denied("artifact grant is required"))?;
        if grant.node_id != request.node_id
            || grant.artifact_id.len() != 16
            || grant.grant_id.len() != 16
            || grant.purpose != "certificate_p12"
            || request.sha256.len() != 32
            || request.size == 0
            || request.size != grant.max_bytes
        {
            return Err(Status::invalid_argument(
                "artifact consumption request is invalid",
            ));
        }
        let expected_artifact_id = grant.artifact_id.clone();
        let expected_grant_id = grant.grant_id.clone();
        let confirm_only = request.confirm_only;
        let (connection, connection_fence) = self
            .shared
            .artifact_connection(&node_id, request.fence_binding.as_ref(), &expected_grant_id)
            .await?;
        let payload = AgentArtifactConsumeRequest {
            grant: request.grant,
            sha256: request.sha256,
            size: request.size,
            confirm_only,
            connection_fence,
            fence_binding: request.fence_binding,
        }
        .encode_to_vec();
        let response = tokio::time::timeout(
            STREAM_TIMEOUT,
            finalize_artifact_consumption(
                connection,
                payload,
                expected_artifact_id,
                expected_grant_id,
                confirm_only,
            ),
        )
        .await
        .map_err(|_| Status::deadline_exceeded("artifact finalize stream timed out"))??;
        Ok(Response::new(ConsumeArtifactResponse {
            consumed: response.consumed,
        }))
    }

    async fn close_node(
        &self,
        request: Request<CloseNodeRequest>,
    ) -> Result<Response<CloseNodeResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.reason.chars().count() > 1024 {
            return Err(Status::invalid_argument(
                "close reason exceeds 1024 characters",
            ));
        }
        self.shared
            .verify_operation_binding(
                &node_id,
                None,
                None,
                request.fence_binding.as_ref(),
                FenceOperationKind::ConnectionClose,
                &node_id,
            )
            .await
            .map(|_| ())?;
        self.shared
            .remove(&node_id, request.reason.as_bytes())
            .await
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        Ok(Response::new(CloseNodeResponse {}))
    }

    async fn watch_events(
        &self,
        request: Request<WatchEventsRequest>,
    ) -> Result<Response<Self::WatchEventsStream>, Status> {
        let after = request.into_inner().after_event_id;
        if !after.is_empty() {
            validate_uuid(&after, "after_event_id")?;
        }
        let mut shutdown = self.shared.inner.shutdown.subscribe();
        if *shutdown.borrow() {
            return Err(Status::unavailable("transport is shutting down"));
        }
        let receiver = self.shared.subscribe_events(&after).await?;
        let shutdown_signal =
            async move { while !*shutdown.borrow() && shutdown.changed().await.is_ok() {} };
        Ok(Response::new(Box::pin(
            ReceiverStream::new(receiver).take_until(shutdown_signal),
        )))
    }

    async fn update_node_trust(
        &self,
        request: Request<UpdateNodeTrustRequest>,
    ) -> Result<Response<UpdateNodeTrustResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.endpoint_id.len() != 32 {
            return Err(Status::invalid_argument("endpoint_id must be 32 bytes"));
        }
        if request.reason.is_empty() || request.reason.chars().count() > 1024 {
            return Err(Status::invalid_argument("trust update reason is invalid"));
        }
        if request.revision == 0 {
            return Err(Status::invalid_argument("trust update revision is invalid"));
        }
        let endpoint = EndpointId::from_bytes(
            &request
                .endpoint_id
                .clone()
                .try_into()
                .map_err(|_| Status::invalid_argument("endpoint_id is invalid"))?,
        )
        .map_err(|_| Status::invalid_argument("endpoint_id is invalid"))?;
        let state = NodeTrustState::try_from(request.state)
            .map_err(|_| Status::invalid_argument("trust state is invalid"))?;
        if state == NodeTrustState::Unspecified {
            return Err(Status::invalid_argument("trust state is unspecified"));
        }
        // The operation identity is derived from the exact carrier, so the
        // binding must be replayed with the identical endpoint, state,
        // revision, and reason to pass; the declared identity must also match
        // the derivation, which makes the carrier explicit.
        let node_key = fixed16(&node_id)?;
        let endpoint_key: [u8; 32] = request
            .endpoint_id
            .clone()
            .try_into()
            .map_err(|_| Status::invalid_argument("endpoint_id is invalid"))?;
        let derived_operation_id = ocservia_command_authorization::state_update_operation_id(
            &node_key,
            &endpoint_key,
            u32::try_from(request.state)
                .map_err(|_| Status::invalid_argument("trust state is invalid"))?,
            request.revision,
            &request.reason,
        );
        if !request.operation_id.is_empty()
            && request.operation_id.as_slice() != derived_operation_id.as_slice()
        {
            return Err(Status::invalid_argument(
                "trust update operation identity does not match its carrier",
            ));
        }
        self.shared
            .verify_operation_binding(
                &node_id,
                Some(&endpoint_key),
                None,
                request.fence_binding.as_ref(),
                FenceOperationKind::StateUpdate,
                &request.operation_id,
            )
            .await
            .map(|_| ())?;
        let previous_revision = self.policy.revision(endpoint);
        let disposition = self
            .policy
            .update(endpoint, node_id.clone(), state, request.revision);
        let retained_revision = self.policy.revision(endpoint);
        let retained_state = self.policy.retained_state(endpoint);
        if disposition == PolicyUpdateDisposition::Applied
            && retained_revision == request.revision
            && retained_state == state
            && retained_revision > previous_revision
        {
            self.shared
                .retire_prior_authorization(&node_id, retained_revision, state)
                .await;
        }
        let disposition = match disposition {
            PolicyUpdateDisposition::Applied => TrustUpdateDisposition::Applied,
            PolicyUpdateDisposition::Stale => TrustUpdateDisposition::Stale,
            PolicyUpdateDisposition::Rejected => TrustUpdateDisposition::Rejected,
        };
        Ok(Response::new(UpdateNodeTrustResponse {
            disposition: disposition.into(),
            retained_revision,
            retained_state: retained_state.into(),
        }))
    }

    async fn register_owner_fence(
        &self,
        request: Request<RegisterOwnerFenceRequest>,
    ) -> Result<Response<RegisterOwnerFenceResponse>, Status> {
        let request = request.into_inner();
        let fence = request
            .fence
            .ok_or_else(|| Status::invalid_argument("owner fence is required"))?;
        self.shared.register_fence(fence).await.map(Response::new)
    }

    async fn get_owner_fence(
        &self,
        request: Request<GetOwnerFenceRequest>,
    ) -> Result<Response<GetOwnerFenceResponse>, Status> {
        let node_id = validate_uuid(&request.into_inner().node_id, "node_id")?;
        let fence = self
            .shared
            .registered_fence(&fixed16(&node_id)?)
            .await
            .map(|registered| registered.fence)
            .ok_or_else(|| Status::not_found("owner fence is not registered"))?;
        Ok(Response::new(GetOwnerFenceResponse { fence: Some(fence) }))
    }
}

#[derive(Clone, Copy, Debug)]
enum ProtocolKind {
    Enroll,
    Agent,
}

#[derive(Clone)]
struct SessionHandler {
    shared: Shared,
    policy: IdentityPolicy,
    kind: ProtocolKind,
    trust: TrustAuthority,
    /// One absolute budget for the whole pre-registration handshake.
    handshake_timeout: Duration,
}

impl std::fmt::Debug for SessionHandler {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("SessionHandler")
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

impl SessionHandler {
    #[allow(clippy::too_many_lines)]
    async fn negotiate(
        &self,
        connection: &Connection,
        send: &mut iroh::endpoint::SendStream,
        recv: &mut iroh::endpoint::RecvStream,
    ) -> Result<Option<NegotiatedSession>, AcceptError> {
        if matches!(self.kind, ProtocolKind::Enroll) {
            let request = read_enrollment(recv).await?;
            validate_enrollment(&request, connection.remote_id())?;
            let response = self.trust.enroll(request).await?;
            write_message(send, &response).await?;
            wait_for_delivery(send).await?;
            let reason = if response.result == i32::from(HandshakeResult::Accepted) {
                b"enrollment recovered".as_slice()
            } else {
                b"enrollment pending".as_slice()
            };
            connection.close(VarInt::from_u32(0), reason);
            return Ok(None);
        }
        let handshake = read_handshake(recv).await?;
        validate_handshake(&handshake, connection.remote_id())?;
        validate_protocol_version(&handshake)?;
        let response = self
            .trust
            .authorize(connection.remote_id(), handshake.clone())
            .await?;
        write_message(send, &response).await?;
        let result =
            HandshakeResult::try_from(response.result).unwrap_or(HandshakeResult::Unspecified);
        if result != HandshakeResult::Accepted {
            wait_for_delivery(send).await?;
            connection.close(VarInt::from_u32(0x101), b"session rejected");
            return Ok(None);
        }
        if response.max_message_size == 0
            || response.max_message_size > handshake.max_message_size
            || response.max_message_size as usize > MAX_FRAME_BYTES
        {
            return Err(protocol_error("negotiated message size is invalid"));
        }
        let negotiated_capabilities =
            validate_negotiated_session(&handshake, connection.remote_id(), &response)?;
        let (authorization_revision, session_expires_at) = response
            .session_grant
            .as_ref()
            .map(|grant| {
                let expires_at = grant
                    .expires_at
                    .as_ref()
                    .ok_or_else(|| protocol_error("session grant expiry is missing"))?;
                let expires_at = SystemTime::try_from(*expires_at)
                    .map_err(|_| protocol_error("session grant expiry is invalid"))?;
                Ok::<_, AcceptError>((grant.authorization_revision, Some(expires_at)))
            })
            .transpose()?
            .unwrap_or((0, None));
        let fence = self.verify_session_fence(connection, &handshake, &response)?;
        let mut handshake = handshake;
        handshake.max_message_size = response.max_message_size;
        Ok(Some(NegotiatedSession {
            handshake,
            negotiated_capabilities,
            authorization_revision,
            session_expires_at,
            fence,
        }))
    }

    /// Verifies the Controller-signed connection-owner fence carried by one
    /// accepted session handshake. Fails closed when the fence is missing or
    /// cannot be verified, when its lease deadline has passed (it authorizes
    /// current mutations, unlike its later audit expiry), or when it is not
    /// pinned to the claiming node and the connection's endpoint.
    fn verify_session_fence(
        &self,
        connection: &Connection,
        handshake: &SessionHandshake,
        response: &SessionHandshakeResponse,
    ) -> Result<RegisteredFence, AcceptError> {
        let fence = response
            .connection_fence
            .as_ref()
            .ok_or_else(|| protocol_error("session requires a verified owner fence"))?;
        let keyring = self.shared.keyring();
        let node_id: [u8; 16] = handshake
            .node_id
            .clone()
            .try_into()
            .map_err(|_| protocol_error("handshake node id is invalid"))?;
        let endpoint_id: [u8; 32] = *connection.remote_id().as_bytes();
        let (now_seconds, now_nanos) = unix_now_parts();
        let verified = keyring
            .verify_connection_fence_v2_at(fence, &node_id, &endpoint_id, now_seconds, now_nanos)
            .map_err(|_| protocol_error("controller connection fence is invalid"))?;
        if lease_expired_at(verified.lease_until_seconds, verified.lease_until_nanos) {
            return Err(protocol_error("connection owner lease has expired"));
        }
        Ok(RegisteredFence {
            verified,
            fence: fence.clone(),
            endpoint_id,
        })
    }
}

impl ProtocolHandler for SessionHandler {
    #[allow(clippy::too_many_lines)]
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        let permits = match self.kind {
            ProtocolKind::Enroll => &self.shared.inner.enrollment_connection_permits,
            ProtocolKind::Agent => &self.shared.inner.agent_connection_permits,
        };
        let permit = permits
            .clone()
            .try_acquire_owned()
            .map_err(|_| protocol_error("connection capacity reached"))?;
        // One deadline covers stream acceptance, request read, trust
        // authorization, response write and delivery acknowledgement, so a peer
        // that stalls any step cannot hold this connection slot.
        // The streams stay owned here so an accepted session keeps them open.
        let mut streams = None;
        let negotiated = tokio::time::timeout(self.handshake_timeout, async {
            let (send, recv) = streams.insert(
                connection
                    .accept_bi()
                    .await
                    .map_err(|_| protocol_error("handshake stream failed"))?,
            );
            self.negotiate(&connection, send, recv).await
        })
        .await;
        let Ok(negotiated) = negotiated else {
            connection.close(VarInt::from_u32(0x10c), b"handshake timed out");
            return Err(protocol_error("handshake timed out"));
        };
        let Some(session) = negotiated? else {
            drop(permit);
            return Ok(());
        };
        let max_message_size = session.handshake.max_message_size as usize;

        connection.set_max_concurrent_bi_streams(VarInt::from_u32(MAX_STREAMS));
        connection.set_max_concurrent_uni_streams(VarInt::from_u32(2));
        let metadata = metadata(&session, &connection).await;
        let node_id = metadata.node_id.clone();
        let registration_token = self.shared.registration_token(&node_id).await;
        match self.trust.recheck_agent(connection.remote_id()).await {
            Ok(true) => {}
            Ok(false) => {
                connection.close(VarInt::from_u32(0x101), b"session trust changed");
                return Err(protocol_error(
                    "endpoint trust changed during session registration",
                ));
            }
            Err(rejection) => {
                connection.close(VarInt::from_u32(0x102), b"trust authority unavailable");
                return Err(trust_rejection_error(rejection));
            }
        }
        self.shared
            .register_session(
                &self.policy,
                &connection,
                metadata,
                session,
                &registration_token,
            )
            .await?;
        let monitors = (
            monitor_paths(self.shared.clone(), node_id.clone(), connection.clone()),
            monitor_telemetry(
                self.shared.clone(),
                node_id.clone(),
                connection.clone(),
                max_message_size,
            ),
            monitor_owner_lease(self.shared.clone(), node_id.clone(), connection.clone()),
        );
        let _closed = connection.closed().await;
        finish_session(&self.shared, &node_id, &connection, monitors, permit).await;
        Ok(())
    }
}

// A Controller restart stops renewing its owner leases while the QUIC
// connection can remain alive. Close that exact session on expiry so the
// Agent negotiates a new authoritative term without restarting transportd.
fn monitor_owner_lease(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut poll = tokio::time::interval_at(
            tokio::time::Instant::now() + Duration::from_secs(1),
            Duration::from_secs(1),
        );
        loop {
            poll.tick().await;
            let registry = shared.inner.connections.lock().await;
            let Some(entry) = registry.get(&node_id) else {
                break;
            };
            if entry.connection.stable_id() != connection.stable_id() {
                break;
            }
            let session_fence = &entry.fence;
            // Renewal replaces the registered fence, not the handshake's
            // snapshot. Read it under the same lock used by registration.
            let fences = shared.inner.fences.lock().await;
            let live = fences.get(&node_id).is_some_and(|registered| {
                same_fence_term(&session_fence.verified, &registered.verified)
                    && !lease_expired_at(
                        registered.verified.lease_until_seconds,
                        registered.verified.lease_until_nanos,
                    )
            });
            if !live {
                connection.close(VarInt::from_u32(0x109), b"owner lease expired");
                break;
            }
        }
    })
}

fn monitor_telemetry(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
    max_message_size: usize,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        loop {
            let Ok(accepted) = tokio::time::timeout(STREAM_TIMEOUT, connection.accept_uni()).await
            else {
                continue;
            };
            let Ok(mut recv) = accepted else { break };
            let mut length = [0_u8; 4];
            if !matches!(
                tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut length)).await,
                Ok(Ok(()))
            ) {
                continue;
            }
            let length = u32::from_be_bytes(length) as usize;
            if length == 0 || length > 512 * 1024 || length > max_message_size {
                connection.close(VarInt::from_u32(0x107), b"telemetry frame size invalid");
                break;
            }
            // Charge the node before reading the body so a flood never reaches
            // the shared publish path or the retention buffer.
            if !shared.admit_telemetry(&node_id, length).await {
                tracing::warn!(
                    event_type = "telemetry_budget_exceeded",
                    node_id = %hex::encode(&node_id),
                    "closing connection that exceeded its telemetry budget"
                );
                connection.close(VarInt::from_u32(0x10d), b"telemetry budget exceeded");
                break;
            }
            let mut payload = vec![0_u8; length];
            if !matches!(
                tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut payload)).await,
                Ok(Ok(()))
            ) {
                continue;
            }
            let Ok(batch) = TelemetryBatch::decode(payload.as_slice()) else {
                connection.close(VarInt::from_u32(0x107), b"telemetry protobuf invalid");
                break;
            };
            if batch.node_id != node_id
                || validate_uuid(&batch.batch_id, "telemetry batch_id").is_err()
            {
                connection.close(VarInt::from_u32(0x107), b"telemetry identity invalid");
                break;
            }
            let security = (batch.priority == i32::from(TelemetryPriority::Security)
                || !batch.security_events.is_empty())
                && shared.claim_security_priority(&node_id).await;
            shared
                .publish_telemetry(
                    event(
                        &node_id,
                        connection.remote_id().as_bytes(),
                        TransportEventType::Telemetry,
                        payload,
                    ),
                    security,
                )
                .await;
        }
    })
}

fn monitor_paths(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut events = connection.path_events();
        while let Some(path_event) = events.next().await {
            if !matches!(
                path_event,
                PathEvent::Selected { .. } | PathEvent::Lagged { .. }
            ) {
                continue;
            }
            let next = metadata_path(&connection);
            let mut registry = shared.inner.connections.lock().await;
            let Some(entry) = registry.get_mut(&node_id) else {
                break;
            };
            if entry.connection.stable_id() != connection.stable_id() {
                break;
            }
            entry.metadata.path = next.0.into();
            entry.metadata.path_detail.clone_from(&next.1);
            entry.metadata.round_trip_time_millis = next.2;
            entry.metadata.last_seen = Some(now_timestamp());
            shared
                .publish(event(
                    &node_id,
                    connection.remote_id().as_bytes(),
                    TransportEventType::PathChanged,
                    next.1.into_bytes(),
                ))
                .await;
            drop(registry);
        }
    })
}

#[allow(clippy::too_many_lines)]
async fn read_agent_events(
    mut recv: iroh::endpoint::RecvStream,
    shared: Shared,
    node_id: Vec<u8>,
    max_message_size: usize,
    connection: Connection,
    response_deadline: tokio::time::Instant,
    command: CommandEnvelope,
) {
    let traceparent = command.traceparent.clone();
    let event_context = (
        &shared,
        node_id.as_slice(),
        traceparent.as_str(),
        &connection,
    );
    // A command stream carries exactly one terminal frame. Heartbeats belong to
    // the telemetry path, whose queue quota would otherwise be bypassed here.
    let mut length = [0_u8; 4];
    match tokio::time::timeout_at(response_deadline, recv.read_exact(&mut length)).await {
        Ok(Ok(())) => {}
        Ok(Err(_)) => {
            publish_command_unknown(
                event_context,
                &command,
                "agent response ended before a terminal event",
            )
            .await;
            return;
        }
        Err(_) => {
            publish_command_unknown(event_context, &command, "agent response timed out").await;
            return;
        }
    }
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > max_message_size || length > MAX_FRAME_BYTES {
        publish_command_unknown(event_context, &command, "agent response size invalid").await;
        return;
    }
    let mut bytes = vec![0_u8; length];
    if !matches!(
        tokio::time::timeout_at(response_deadline, recv.read_exact(&mut bytes)).await,
        Ok(Ok(()))
    ) {
        publish_command_unknown(event_context, &command, "agent response body invalid").await;
        return;
    }
    let Ok(agent_event) = AgentEvent::decode(bytes.as_slice()) else {
        publish_command_unknown(event_context, &command, "agent response protobuf invalid").await;
        return;
    };
    let event_type = match AgentEventType::try_from(agent_event.r#type) {
        Ok(AgentEventType::CommandResult) => TransportEventType::CommandResult,
        Ok(AgentEventType::Error) => TransportEventType::Error,
        _ => {
            publish_command_unknown(event_context, &command, "agent response type invalid").await;
            return;
        }
    };
    if event_type == TransportEventType::CommandResult {
        let Ok(result) = CommandResult::decode(agent_event.payload.as_slice()) else {
            publish_command_unknown(
                event_context,
                &command,
                "Agent command result protobuf invalid",
            )
            .await;
            return;
        };
        // A result is only evidence for the command dispatched on this
        // stream; the Controller still re-checks node and command ownership.
        if result.command_id != command.command_id {
            publish_command_unknown(
                event_context,
                &command,
                "Agent command result names a different command",
            )
            .await;
            return;
        }
        if !valid_privileged_result_proof(&command, &result) {
            publish_command_unknown(
                event_context,
                &command,
                "privd result receipt missing or malformed",
            )
            .await;
            return;
        }
        tracing::info!(
            event_type = "command_response_received",
            message_id = %hex::encode(&command.message_id),
            command_id = %hex::encode(&result.command_id),
            node_id = %hex::encode(&node_id),
            delivery_mode = command.delivery_mode,
            state = result.state,
            replayed = result.replayed,
            "Agent command result received"
        );
    }
    publish_agent_event(
        event_context,
        event_with_traceparent(
            &node_id,
            connection.remote_id().as_bytes(),
            event_type,
            agent_event.payload,
            &traceparent,
        ),
    )
    .await;
}

async fn publish_command_unknown(
    (shared, node_id, traceparent, connection): (&Shared, &[u8], &str, &Connection),
    command: &CommandEnvelope,
    message: &'static str,
) {
    tracing::warn!(
        event_type = "command_response_unknown",
        response_reason = message,
        message_id = %hex::encode(&command.message_id),
        command_id = %hex::encode(&command.command_id),
        node_id = %hex::encode(node_id),
        delivery_mode = command.delivery_mode,
        "command response requires reconciliation"
    );
    let now = now_timestamp();
    let result = CommandResult {
        command_id: command.command_id.clone(),
        idempotency_key: command.idempotency_key.clone(),
        payload_sha256: command.semantic_payload_sha256.clone(),
        state: CommandResultState::Unknown.into(),
        result: Vec::new(),
        error_code: "outcome_requires_reconciliation".to_owned(),
        accepted_at: Some(now),
        completed_at: Some(now),
        replayed: false,
        semantic_payload_hash_version: command.semantic_payload_hash_version,
        privileged_result_proof: None,
    };
    publish_agent_event(
        (shared, node_id, traceparent, connection),
        event_with_traceparent(
            node_id,
            connection.remote_id().as_bytes(),
            TransportEventType::CommandResult,
            result.encode_to_vec(),
            traceparent,
        ),
    )
    .await;
}

fn valid_privileged_result_proof(command: &CommandEnvelope, result: &CommandResult) -> bool {
    let terminal = matches!(
        CommandResultState::try_from(result.state),
        Ok(CommandResultState::Succeeded
            | CommandResultState::Failed
            | CommandResultState::Rejected)
    );
    let privileged = !matches!(
        command.payload,
        Some(
            command_envelope::Payload::SimulationProbe(_)
                | command_envelope::Payload::SyntheticNoop(_)
                | command_envelope::Payload::SyntheticEcho(_)
        ) | None
    );
    if !terminal || !privileged {
        return true;
    }
    let Some(proof) = result.privileged_result_proof.as_ref() else {
        return false;
    };
    let Some(receipt) = proof.receipt_v1.as_ref() else {
        return false;
    };
    proof.version == 1
        && receipt.receipt_version == 1
        && proof.signature.len() == 64
        && proof.encoded_len() <= 65_536
        && receipt.node_id.len() == 16
        && receipt.command_id == command.command_id
        && receipt.operation_id.len() == 16
        && receipt.idempotency_key.len() == 16
        && receipt.semantic_payload_sha256.len() == 32
        && receipt.result_bytes_sha256.len() == 32
        && receipt.error_code_sha256.len() == 32
        && (16..=32).contains(&receipt.effect_record_id.len())
        && receipt.effect_sequence > 0
        && receipt.completed_at.is_some()
        && receipt.accepted_at.is_some()
}

fn command_response_deadline(command: &CommandEnvelope) -> Result<tokio::time::Instant, Status> {
    let expires_at = command
        .expires_at
        .as_ref()
        .ok_or_else(|| Status::invalid_argument("command expires_at is required"))?;
    let expires_at = SystemTime::try_from(*expires_at)
        .map_err(|_| Status::invalid_argument("command expires_at is invalid"))?;
    if command.delivery_mode == i32::from(CommandDeliveryMode::ReconcileOnly) {
        return Ok(tokio::time::Instant::now() + STREAM_TIMEOUT);
    }
    let remaining = expires_at
        .duration_since(SystemTime::now())
        .map_err(|_| Status::deadline_exceeded("command has expired"))?;
    Ok(tokio::time::Instant::now() + remaining)
}

async fn publish_agent_event(
    (shared, node_id, _, connection): (&Shared, &[u8], &str, &Connection),
    event: TransportEvent,
) {
    let mut registry = shared.inner.connections.lock().await;
    if let Some(entry) = registry
        .get_mut(node_id)
        .filter(|entry| entry.connection.stable_id() == connection.stable_id())
    {
        entry.metadata.last_seen = Some(now_timestamp());
        shared.publish(event).await;
    }
    drop(registry);
}

async fn read_handshake(
    recv: &mut iroh::endpoint::RecvStream,
) -> Result<SessionHandshake, AcceptError> {
    read_message(recv, "handshake").await
}

async fn read_enrollment(
    recv: &mut iroh::endpoint::RecvStream,
) -> Result<EnrollRequest, AcceptError> {
    read_message(recv, "enrollment").await
}

async fn read_message<M: Message + Default>(
    recv: &mut iroh::endpoint::RecvStream,
    kind: &str,
) -> Result<M, AcceptError> {
    let mut length = [0_u8; 4];
    recv.read_exact(&mut length)
        .await
        .map_err(|_| protocol_error(&format!("{kind} length invalid")))?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_HANDSHAKE_BYTES {
        return Err(protocol_error(&format!("{kind} size invalid")));
    }
    let mut bytes = vec![0_u8; length];
    recv.read_exact(&mut bytes)
        .await
        .map_err(|_| protocol_error(&format!("{kind} body invalid")))?;
    M::decode(bytes.as_slice()).map_err(|_| protocol_error(&format!("{kind} protobuf invalid")))
}

async fn write_message(
    send: &mut iroh::endpoint::SendStream,
    response: &impl Message,
) -> Result<(), AcceptError> {
    let response = response.encode_to_vec();
    let response_len = u32::try_from(response.len())
        .map_err(|_| protocol_error("handshake response length exceeds u32"))?;
    send.write_all(&response_len.to_be_bytes())
        .await
        .map_err(|_| protocol_error("handshake response length failed"))?;
    send.write_all(&response)
        .await
        .map_err(|_| protocol_error("handshake response failed"))?;
    send.finish()
        .map_err(|_| protocol_error("handshake response finish failed"))?;
    Ok(())
}

async fn wait_for_delivery(send: &iroh::endpoint::SendStream) -> Result<(), AcceptError> {
    let stopped = send
        .stopped()
        .await
        .map_err(|_| protocol_error("handshake response acknowledgement failed"))?;
    if stopped.is_some() {
        return Err(protocol_error("peer rejected handshake response"));
    }
    Ok(())
}

fn validate_negotiated_session(
    handshake: &SessionHandshake,
    endpoint: EndpointId,
    response: &SessionHandshakeResponse,
) -> Result<HashSet<String>, AcceptError> {
    if response.protocol_major != PROTOCOL_MAJOR
        || response.protocol_minor != PROTOCOL_MINOR
        || response.protocol_minor > handshake.protocol_minor
    {
        return Err(protocol_error("negotiated protocol version is invalid"));
    }
    let supported = handshake.capabilities.iter().collect::<HashSet<_>>();
    let mut previous: Option<&str> = None;
    for capability in &response.negotiated_capabilities {
        if capability.is_empty()
            || capability.len() > 128
            || !supported.contains(capability)
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(protocol_error("negotiated capability set is invalid"));
        }
        previous = Some(capability);
    }
    let grant = response
        .session_grant
        .as_ref()
        .ok_or_else(|| protocol_error("Controller session grant is required"))?;
    if grant.protocol_major != response.protocol_major
        || grant.protocol_minor != response.protocol_minor
        || grant.node_id != handshake.node_id
        || grant.endpoint_id != endpoint.as_bytes()
        || grant.authorization_revision == 0
        || grant.key_id.is_empty()
        || grant.signature.len() != 64
        || grant.negotiated_capabilities != response.negotiated_capabilities
        || grant.issued_at.is_none()
        || grant.expires_at.is_none()
    {
        return Err(protocol_error(
            "Controller session grant claims are inconsistent",
        ));
    }
    Ok(response.negotiated_capabilities.iter().cloned().collect())
}

fn validate_enrollment(request: &EnrollRequest, remote: EndpointId) -> Result<(), AcceptError> {
    validate_uuid(&request.agent_instance_id, "agent_instance_id")
        .map_err(|status| status_to_accept(&status))?;
    if request.endpoint_id != remote.as_bytes()
        || request.enrollment_protocol_major != 1
        || request.enrollment_protocol_minor != 1
        || request
            .proof
            .as_ref()
            .is_none_or(|proof| proof.version != 1 || proof.signature.len() != 64)
    {
        return Err(protocol_error("endpoint identity mismatch"));
    }
    let token = request
        .token
        .strip_prefix("obt2_")
        .unwrap_or(&request.token);
    if token.len() != 43 || request.nonce.len() < 16 || request.nonce.len() > 64 {
        return Err(protocol_error("enrollment credential or nonce is invalid"));
    }
    if request.capabilities.is_empty()
        || request.capabilities.len() > 128
        || request
            .capabilities
            .iter()
            .any(|value| value.is_empty() || value.chars().count() > 128)
        || request.capabilities.iter().collect::<HashSet<_>>().len() != request.capabilities.len()
        || request.time.is_none()
        || [
            &request.agent_version,
            &request.os_release,
            &request.ocserv_version,
            &request.boot_id,
            &request.environment,
        ]
        .iter()
        .any(|value| value.is_empty() || value.chars().count() > 256)
    {
        return Err(protocol_error("enrollment field limit exceeded"));
    }
    Ok(())
}

fn validate_handshake(handshake: &SessionHandshake, remote: EndpointId) -> Result<(), AcceptError> {
    validate_uuid(&handshake.node_id, "node_id").map_err(|status| status_to_accept(&status))?;
    validate_uuid(&handshake.agent_instance_id, "agent_instance_id")
        .map_err(|status| status_to_accept(&status))?;
    if handshake.endpoint_id != remote.as_bytes() {
        return Err(protocol_error("endpoint identity mismatch"));
    }
    if handshake.max_message_size == 0 || handshake.max_message_size as usize > MAX_FRAME_BYTES {
        return Err(protocol_error("message size incompatible"));
    }
    if handshake.nonce.len() < 16 || handshake.nonce.len() > 64 {
        return Err(protocol_error("nonce size invalid"));
    }
    if handshake.capabilities.len() > 128
        || handshake
            .capabilities
            .iter()
            .any(|value| value.chars().count() > 128)
        || handshake.supported_compressions.len() > 16
        || [
            &handshake.agent_version,
            &handshake.controller_version,
            &handshake.ocserv_version,
            &handshake.os_release,
            &handshake.boot_id,
        ]
        .iter()
        .any(|value| value.chars().count() > 256)
    {
        return Err(protocol_error("handshake field limit exceeded"));
    }
    Ok(())
}

fn validate_protocol_version(handshake: &SessionHandshake) -> Result<(), AcceptError> {
    if handshake.protocol_major != PROTOCOL_MAJOR || handshake.protocol_minor != PROTOCOL_MINOR {
        return Err(protocol_error("protocol version incompatible"));
    }
    Ok(())
}

fn status_to_accept(status: &Status) -> AcceptError {
    protocol_error(status.message())
}

fn protocol_error(message: &str) -> AcceptError {
    AcceptError::from_err(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        message.to_owned(),
    ))
}

async fn metadata(session: &NegotiatedSession, connection: &Connection) -> NodeConnection {
    let handshake = &session.handshake;
    let path_metadata = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let next = metadata_path(connection);
            if next.0 != ConnectionPath::Unspecified {
                break next;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap_or_else(|_| metadata_path(connection));
    let (path, detail, rtt) = path_metadata;
    let now = now_timestamp();
    NodeConnection {
        node_id: handshake.node_id.clone(),
        endpoint_id: connection.remote_id().as_bytes().to_vec(),
        path: path.into(),
        round_trip_time_millis: rtt,
        connected_at: Some(now),
        agent_instance_id: handshake.agent_instance_id.clone(),
        path_detail: detail,
        last_seen: Some(now),
        negotiated_capabilities: {
            let mut capabilities = session
                .negotiated_capabilities
                .iter()
                .cloned()
                .collect::<Vec<_>>();
            capabilities.sort();
            capabilities
        },
        authorization_revision: session.authorization_revision,
        session_expires_at: session.session_expires_at.map(Into::into),
        owner_epoch: session.fence.verified.owner_epoch,
    }
}

fn metadata_path(connection: &Connection) -> (ConnectionPath, String, u64) {
    let paths = connection.paths();
    let selected = paths
        .iter()
        .find(iroh::endpoint::Path::is_selected)
        .or_else(|| paths.iter().next());
    selected.map_or(
        (ConnectionPath::Unspecified, "unknown".to_owned(), 0),
        |path| {
            let kind = if path.is_ip() {
                ConnectionPath::Direct
            } else if path.is_relay() {
                ConnectionPath::Relay
            } else {
                ConnectionPath::Unspecified
            };
            let detail = format!("{:?}", path.remote_addr());
            let millis = u64::try_from(path.rtt().as_millis()).unwrap_or(u64::MAX);
            (kind, detail, millis)
        },
    )
}

#[cfg(test)]
fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(i64::MAX, |elapsed| {
            i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX)
        })
}

/// Returns the current instant with its nanosecond part. Lease deadlines are
/// compared at this precision: truncating them to whole seconds would reject
/// leases that remain valid for up to one second.
fn unix_now_parts() -> (i64, u32) {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or((i64::MAX, 0), |elapsed| {
            (
                i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX),
                elapsed.subsec_nanos(),
            )
        })
}

/// Reports whether an ownership lease deadline has passed at the current
/// nanosecond-precise instant.
fn lease_expired_at(lease_until_seconds: i64, lease_until_nanos: u32) -> bool {
    let (now_seconds, now_nanos) = unix_now_parts();
    (lease_until_seconds, lease_until_nanos) <= (now_seconds, now_nanos)
}

fn fixed16(value: &[u8]) -> Result<[u8; 16], Status> {
    value
        .try_into()
        .map_err(|_| Status::invalid_argument("identifier must be 16 bytes"))
}

fn fixed32(value: &[u8]) -> Result<[u8; 32], Status> {
    value
        .try_into()
        .map_err(|_| Status::invalid_argument("identifier must be 32 bytes"))
}

/// The registration decision for one incoming fence against the retained
/// term of the same node.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum FenceRegistration {
    /// A higher epoch (or the first fence) was recorded.
    Apply,
    /// The same term's lease deadline was refreshed.
    Refresh,
    /// The incoming epoch predates the retained epoch and is rejected.
    Stale,
    /// Two distinct fences claim one epoch, which is never legitimate.
    EpochCollision,
}

fn fence_registration_decision(
    existing: Option<&RegisteredFence>,
    incoming: &RegisteredFence,
) -> FenceRegistration {
    match existing {
        Some(existing) if existing.verified.owner_epoch > incoming.verified.owner_epoch => {
            FenceRegistration::Stale
        }
        Some(existing)
            if existing.verified.owner_epoch == incoming.verified.owner_epoch
                && existing.verified.fence_id != incoming.verified.fence_id =>
        {
            FenceRegistration::EpochCollision
        }
        Some(existing) if existing.verified.fence_id == incoming.verified.fence_id => {
            FenceRegistration::Refresh
        }
        _ => FenceRegistration::Apply,
    }
}

/// Verifies the envelope's fence and binding carriers: both Controller
/// signatures with their node/endpoint pinning, capability membership and
/// exact command capability, carrier identity, and lease validity. The
/// exact-term match against the registered fences is left to the caller so
/// it always runs after every independent check.
fn verify_command_carriers(
    keyring: &ControllerCommandKeyring,
    node_id: &[u8; 16],
    endpoint: &[u8; 32],
    command: &CommandEnvelope,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<
    (
        ocservia_command_authorization::VerifiedConnectionFenceV2,
        ocservia_command_authorization::FenceBindingClaimsV2,
    ),
    Status,
> {
    let (Some(envelope_fence), Some(envelope_binding)) = (
        command.connection_fence.as_ref(),
        command.fence_binding.as_ref(),
    ) else {
        return Err(Status::permission_denied(
            "fenced session commands must carry the owner fence and binding",
        ));
    };
    let verified = keyring
        .verify_connection_fence_v2_at(envelope_fence, node_id, endpoint, now_seconds, now_nanos)
        .map_err(|_| Status::permission_denied("command connection fence is invalid"))?;
    let claims = keyring
        .verify_fence_binding_v2_at(envelope_binding, node_id, endpoint, now_seconds, now_nanos)
        .map_err(|_| Status::permission_denied("command fence binding is invalid"))?;
    if claims.capability != command.required_capability
        || !verified
            .capabilities
            .iter()
            .any(|capability| capability == &claims.capability)
    {
        return Err(Status::permission_denied(
            "fence binding capability does not match the command capability",
        ));
    }
    if claims.operation_kind != FenceOperationKind::Command as u32
        || claims.operation_id.as_slice() != command.command_id.as_slice()
    {
        return Err(Status::permission_denied(
            "fence binding does not match the dispatched command",
        ));
    }
    if lease_expired_at(verified.lease_until_seconds, verified.lease_until_nanos) {
        return Err(Status::failed_precondition(
            "command owner lease has expired",
        ));
    }
    Ok((verified, claims))
}

/// Compares the fence identity tuple only: fence, owner instance,
/// incarnation, epoch, connection, and authorization revision. Lease and
/// expiry fields deliberately differ across lease refreshes of one term.
fn same_fence_term(
    left: &ocservia_command_authorization::VerifiedConnectionFenceV2,
    right: &ocservia_command_authorization::VerifiedConnectionFenceV2,
) -> bool {
    left.fence_id == right.fence_id
        && left.owner_instance_id == right.owner_instance_id
        && left.owner_incarnation == right.owner_incarnation
        && left.owner_epoch == right.owner_epoch
        && left.connection_id == right.connection_id
        && left.authorization_revision == right.authorization_revision
}

fn validate_uuid(value: &[u8], name: &'static str) -> Result<Vec<u8>, Status> {
    let id = Uuid::from_slice(value)
        .map_err(|_| Status::invalid_argument(format!("{name} must be a 16-byte UUID")))?;
    if id.get_version_num() != 7 {
        return Err(Status::invalid_argument(format!("{name} must be UUIDv7")));
    }
    Ok(value.to_vec())
}

fn event(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
) -> TransportEvent {
    event_with_traceparent(node_id, endpoint_id, kind, payload, &new_traceparent())
}

/// Builds an event that also names the connection-owner term of the
/// connection it is about, so the owner can end exactly its own term when
/// the connection goes away.
fn event_with_term(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
    fence: &RegisteredFence,
) -> TransportEvent {
    let mut transport_event = event(node_id, endpoint_id, kind, payload);
    transport_event.connection_id = fence.verified.connection_id.to_vec();
    transport_event.owner_epoch = fence.verified.owner_epoch;
    transport_event
}

fn event_with_traceparent(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
    traceparent: &str,
) -> TransportEvent {
    TransportEvent {
        event_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        r#type: kind.into(),
        occurred_at: Some(now_timestamp()),
        payload,
        traceparent: traceparent.to_owned(),
        endpoint_id: endpoint_id.to_vec(),
        connection_id: Vec::new(),
        owner_epoch: 0,
    }
}

fn validate_traceparent(value: &str) -> Result<(), Status> {
    let parts = value.split('-').collect::<Vec<_>>();
    if parts.len() != 4
        || parts[0] != "00"
        || parts[1].len() != 32
        || parts[2].len() != 16
        || parts[3].len() != 2
        || !parts.iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        })
        || parts[1] == "00000000000000000000000000000000"
        || parts[2] == "0000000000000000"
    {
        return Err(Status::invalid_argument("invalid traceparent"));
    }
    Ok(())
}

fn new_traceparent() -> String {
    let trace_id = Uuid::now_v7();
    let span_id = Uuid::now_v7();
    format!(
        "00-{}-{}-01",
        hex::encode(trace_id.as_bytes()),
        hex::encode(&span_id.as_bytes()[..8])
    )
}

fn now_timestamp() -> prost_types::Timestamp {
    SystemTime::now().into()
}

/// Builds the controller endpoint with a live Go trust authority.
///
/// # Errors
///
/// Returns an Iroh bind error when endpoint or transport initialization fails.
pub async fn build_router_with_trust(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    service: &IrohTransportService,
) -> Result<Router, AcceptError> {
    build_router_with_trust_tls_roots(secret_key, relay_mode, policy, trust, Vec::new(), service)
        .await
}

/// Builds the controller endpoint with a live Go trust authority and
/// additional relay TLS roots for privately certificated relays.
///
/// # Errors
///
/// Returns an Iroh bind error when endpoint or transport initialization fails.
pub async fn build_router_with_trust_tls_roots(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    relay_tls_roots: Vec<rustls_pki_types::CertificateDer<'static>>,
    service: &IrohTransportService,
) -> Result<Router, AcceptError> {
    trust.load_snapshot(&policy).await?;
    build_router_with_direct(
        secret_key,
        relay_mode,
        policy,
        trust,
        service,
        true,
        relay_tls_roots,
    )
    .await
    .map_err(|_| protocol_error("iroh transport bind failed"))
}

async fn build_router_with_direct(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    service: &IrohTransportService,
    direct_enabled: bool,
    relay_tls_roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> Result<Router, iroh::endpoint::BindError> {
    let transport = QuicTransportConfig::builder()
        .max_concurrent_bidi_streams(VarInt::from_u32(MAX_STREAMS))
        .max_concurrent_uni_streams(VarInt::from_u32(2))
        .max_idle_timeout(Some(VarInt::from_u32(90_000).into()))
        .stream_receive_window(VarInt::from_u32(MAX_FRAME_BYTES_U32))
        .receive_window(VarInt::from_u32(MAX_FRAME_BYTES_U32 * 4))
        .datagram_receive_buffer_size(None)
        .build();
    let mut endpoint_builder = Endpoint::builder(presets::N0)
        .secret_key(secret_key)
        .relay_mode(relay_mode)
        .clear_address_lookup()
        // System DNS and hosts only: fail visibly instead of querying iroh's
        // public fallback resolvers.
        .dns_resolver(
            iroh::dns::DnsResolver::builder()
                .with_system_defaults()
                .disable_fallback()
                .build(),
        )
        .keep_relays_connected(false)
        .transport_config(transport)
        .hooks(SecurityHook::new(policy.clone(), &trust));
    if !relay_tls_roots.is_empty() {
        endpoint_builder = endpoint_builder
            .ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(relay_tls_roots));
    }
    if !direct_enabled {
        endpoint_builder = endpoint_builder.clear_ip_transports();
    }
    let endpoint = endpoint_builder.bind().await?;
    Ok(Router::builder(endpoint)
        .accept(
            ENROLL_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy: policy.clone(),
                kind: ProtocolKind::Enroll,
                trust: trust.clone(),
                handshake_timeout: HANDSHAKE_TIMEOUT,
            },
        )
        .accept(
            AGENT_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy,
                kind: ProtocolKind::Agent,
                trust,
                handshake_timeout: HANDSHAKE_TIMEOUT,
            },
        )
        .spawn())
}

/// Shuts down active connections before closing the Iroh router.
///
/// # Errors
///
/// Returns an error when the Iroh router task cannot finish shutting down.
pub async fn shutdown(
    service: &IrohTransportService,
    router: Router,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    service.begin_shutdown().await;
    router.shutdown().await.map_err(Into::into)
}

#[cfg(test)]
mod tests;
