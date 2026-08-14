//! Iroh endpoint ownership, protocol routing, and the typed UDS transport service.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock, Weak};
use std::time::{Duration, Instant, SystemTime};

use futures_util::StreamExt as _;
use iroh::endpoint::{
    AfterHandshakeOutcome, Connection, EndpointHooks, PathEvent, QuicTransportConfig, RelayMode,
    VarInt, presets,
};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, EndpointId, RelayMap, SecretKey, Watcher as _};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, ArtifactChunk,
    ArtifactConsumeRequest as AgentArtifactConsumeRequest,
    ArtifactConsumeResponse as AgentArtifactConsumeResponse, ArtifactFetchRequest, CommandEnvelope,
    CommandResult, CommandResultState, EnrollRequest, EnrollResponse, HandshakeResult,
    SessionHandshake, SessionHandshakeResponse, TelemetryBatch, command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, CloseNodeRequest, CloseNodeResponse,
    ConnectionPath, ConsumeArtifactRequest, ConsumeArtifactResponse, FetchArtifactRequest,
    GetNodeConnectionRequest, HealthRequest, HealthResponse, HealthStatus, NodeConnection,
    NodeTrustState, SendCommandRequest, SendCommandResponse, TransportEvent, TransportEventType,
    TrustUpdateDisposition, UpdateNodeTrustRequest, UpdateNodeTrustResponse,
    ValidateEnrollmentRequest, WatchEventsRequest, transport_service_server::TransportService,
    trust_service_client::TrustServiceClient,
};
use ocservia_contracts::session::{
    READ_ONLY_SESSION_CAPABILITIES, is_read_only_session_capability,
};
use prost::Message;
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore, mpsc, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use uuid::Uuid;

/// ALPN for one-time enrollment connections.
pub const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
/// ALPN for approved agent sessions.
pub const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";

const ARTIFACT_FETCH_FRAME: u32 = 1 << 31;
const ARTIFACT_CONSUME_FRAME: u32 = 3 << 30;

/// Temporarily removes a failed home relay so Iroh selects another member of
/// the configured dedicated set instead of waiting for a periodic net report.
pub fn spawn_dedicated_relay_failover(endpoint: Endpoint, configured: RelayMap) {
    if configured.len() < 2 {
        return;
    }
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
const MAX_RESPONSE_FRAMES: usize = 128;

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

    fn matches_node(&self, endpoint: EndpointId, node_id: &[u8]) -> bool {
        self.state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .approved
            .get(&endpoint)
            .is_some_and(|approved_node| approved_node == node_id)
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

#[derive(Debug)]
struct SecurityHook {
    policy: IdentityPolicy,
    agent_attempts: Mutex<HashMap<EndpointId, VecDeque<Instant>>>,
    enrollment_attempts: Mutex<HashMap<EndpointId, VecDeque<Instant>>>,
    budgets: Arc<TrustBudgetSet>,
}

impl SecurityHook {
    fn new(policy: IdentityPolicy, trust: Option<&TrustAuthority>) -> Self {
        let budgets = trust.map_or_else(
            || Arc::new(TrustBudgetSet::new(&TrustBudgetConfig::default())),
            |authority| Arc::clone(&authority.budgets),
        );
        Self {
            policy,
            agent_attempts: Mutex::new(HashMap::new()),
            enrollment_attempts: Mutex::new(HashMap::new()),
            budgets,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum AttemptRejection {
    Rate,
    Capacity,
}

fn trust_rejection_error(rejection: AttemptRejection) -> AcceptError {
    match rejection {
        AttemptRejection::Rate => protocol_error("trust authority rate exceeded"),
        AttemptRejection::Capacity => protocol_error("trust authority capacity reached"),
    }
}

fn record_attempt(
    attempts: &mut HashMap<EndpointId, VecDeque<Instant>>,
    endpoint: EndpointId,
    now: Instant,
    identity_capacity: usize,
) -> Result<(), AttemptRejection> {
    attempts.retain(|_, endpoint_attempts| {
        while endpoint_attempts
            .front()
            .is_some_and(|at| now.duration_since(*at) >= Duration::from_mins(1))
        {
            endpoint_attempts.pop_front();
        }
        !endpoint_attempts.is_empty()
    });
    if !attempts.contains_key(&endpoint) && attempts.len() >= identity_capacity {
        return Err(AttemptRejection::Capacity);
    }
    let endpoint_attempts = attempts.entry(endpoint).or_default();
    if endpoint_attempts.len() >= MAX_ATTEMPTS_PER_MINUTE {
        return Err(AttemptRejection::Rate);
    }
    endpoint_attempts.push_back(now);
    Ok(())
}

fn record_global_attempt(
    attempts: &mut VecDeque<Instant>,
    now: Instant,
    limit: usize,
) -> Result<(), AttemptRejection> {
    while attempts
        .front()
        .is_some_and(|at| now.duration_since(*at) >= Duration::from_mins(1))
    {
        attempts.pop_front();
    }
    if attempts.len() >= limit {
        return Err(AttemptRejection::Rate);
    }
    attempts.push_back(now);
    Ok(())
}

impl EndpointHooks for SecurityHook {
    async fn after_handshake(&self, connection: &Connection) -> AfterHandshakeOutcome {
        let endpoint = connection.remote_id();
        let alpn = connection.alpn();
        if alpn != ENROLL_ALPN && alpn != AGENT_ALPN {
            return reject(0x100, b"unsupported protocol");
        }
        if self.policy.revoked(endpoint)
            || (alpn == AGENT_ALPN && !self.policy.permits(endpoint, alpn))
        {
            return reject(0x101, b"endpoint not permitted");
        }
        let (attempts, identity_capacity) = if alpn == ENROLL_ALPN {
            (&self.enrollment_attempts, MAX_ENROLLMENT_CONNECTIONS)
        } else {
            (&self.agent_attempts, MAX_AGENT_CONNECTIONS)
        };
        let mut attempts = attempts.lock().await;
        if record_attempt(&mut attempts, endpoint, Instant::now(), identity_capacity).is_err() {
            return reject(0x103, b"connection rate exceeded");
        }
        drop(attempts);
        let class = if alpn == ENROLL_ALPN {
            TrustClass::UnknownEnrollmentPreAuth
        } else {
            TrustClass::KnownAgentHandshake
        };
        let _budget = match self.budgets.acquire(class).await {
            Ok(permit) => permit,
            Err(AttemptRejection::Capacity) => {
                return reject(0x102, b"connection class capacity reached");
            }
            Err(AttemptRejection::Rate) => {
                return reject(0x103, b"connection class rate exceeded");
            }
        };
        // Enrollment has no token at this hook. It is admitted locally only;
        // the first bounded application frame performs a pre-auth credential
        // check, and only a valid credential reaches EnrollmentCompletion.
        AfterHandshakeOutcome::Accept
    }
}

fn reject(code: u32, reason: &[u8]) -> AfterHandshakeOutcome {
    AfterHandshakeOutcome::Reject {
        error_code: VarInt::from_u32(code),
        reason: reason.to_vec(),
    }
}

#[derive(Clone)]
struct Shared {
    inner: Arc<Inner>,
}

struct Inner {
    connections: Mutex<HashMap<Vec<u8>, RegisteredConnection>>,
    registration_tokens: Mutex<HashMap<Vec<u8>, Weak<()>>>,
    events: Mutex<EventState>,
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
    session_mode: TransportSessionMode,
    authorization_revision: u64,
    session_expires_at: Option<SystemTime>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TransportSessionMode {
    ReadOnly,
    AuthorizedV11,
}

struct NegotiatedSession {
    handshake: SessionHandshake,
    negotiated_capabilities: HashSet<String>,
    session_mode: TransportSessionMode,
    authorization_revision: u64,
    session_expires_at: Option<SystemTime>,
}

struct EventState {
    retained: VecDeque<TransportEvent>,
    subscribers: Vec<mpsc::Sender<Result<TransportEvent, Status>>>,
}

impl Shared {
    fn new(event_capacity: usize) -> Self {
        let capacity = event_capacity.clamp(1, MAX_CONNECTIONS);
        let (shutdown, _) = watch::channel(false);
        Self {
            inner: Arc::new(Inner {
                connections: Mutex::new(HashMap::new()),
                registration_tokens: Mutex::new(HashMap::new()),
                events: Mutex::new(EventState {
                    retained: VecDeque::with_capacity(capacity),
                    subscribers: Vec::new(),
                }),
                event_capacity: capacity,
                agent_connection_permits: Arc::new(Semaphore::new(MAX_AGENT_CONNECTIONS)),
                enrollment_connection_permits: Arc::new(Semaphore::new(MAX_ENROLLMENT_CONNECTIONS)),
                shutdown,
            }),
        }
    }

    async fn publish(&self, event: TransportEvent) {
        if *self.inner.shutdown.borrow() {
            return;
        }
        let mut state = self.inner.events.lock().await;
        if state.retained.len() == self.inner.event_capacity {
            state.retained.pop_front();
        }
        state.retained.push_back(event.clone());
        state
            .subscribers
            .retain(|sender| matches!(sender.try_send(Ok(event.clone())), Ok(())));
    }

    async fn remove(&self, node_id: &[u8], reason: &[u8]) -> Option<RegisteredConnection> {
        // Invalidate any live trust decision made before this close attempt,
        // including when the requested node has not registered yet.
        self.inner.registration_tokens.lock().await.remove(node_id);
        let mut registry = self.inner.connections.lock().await;
        let registered = registry.remove(node_id);
        if let Some(registered) = &registered {
            registered.connection.close(VarInt::from_u32(0x104), reason);
            self.publish(event(
                node_id,
                &registered.metadata.endpoint_id,
                TransportEventType::Disconnected,
                reason.to_vec(),
            ))
            .await;
        }
        drop(registry);
        registered
    }

    async fn registration_token(&self, node_id: &[u8]) -> Arc<()> {
        let mut tokens = self.inner.registration_tokens.lock().await;
        tokens.retain(|_, token| token.strong_count() > 0);
        if let Some(token) = tokens.get(node_id).and_then(Weak::upgrade) {
            return token;
        }
        let token = Arc::new(());
        tokens.insert(node_id.to_vec(), Arc::downgrade(&token));
        token
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
    /// Creates the service and its bounded connection registry.
    #[must_use]
    pub fn new(event_capacity: usize) -> Self {
        Self::new_with_policy(event_capacity, IdentityPolicy::default())
    }

    /// Creates the service with a shared mutable endpoint policy.
    #[must_use]
    pub fn new_with_policy(event_capacity: usize, policy: IdentityPolicy) -> Self {
        Self {
            shared: Shared::new(event_capacity),
            policy,
        }
    }

    /// Stops active connections and terminates all long-lived event streams.
    pub async fn begin_shutdown(&self) {
        self.shared.shutdown().await;
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
        let (
            connection,
            max_message_size,
            negotiated_capabilities,
            session_mode,
            authorization_revision,
            session_expires_at,
        ) = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get_mut(&node_id)
            .map(|entry| {
                (
                    entry.connection.clone(),
                    entry.max_message_size,
                    entry.negotiated_capabilities.clone(),
                    entry.session_mode,
                    entry.authorization_revision,
                    entry.session_expires_at,
                )
            })
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if session_mode == TransportSessionMode::ReadOnly {
            return Err(Status::permission_denied(
                "read-only session does not permit command dispatch",
            ));
        }
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
        if request.command_envelope.len() > max_message_size {
            return Err(Status::resource_exhausted(
                "command exceeds the agent's negotiated message size",
            ));
        }
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
        let (connection, session_mode) = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id)
            .map(|entry| (entry.connection.clone(), entry.session_mode))
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if session_mode == TransportSessionMode::ReadOnly {
            return Err(Status::permission_denied(
                "read-only session does not permit artifact fetch",
            ));
        }
        let payload = ArtifactFetchRequest {
            artifact_id,
            purpose: request.purpose,
            max_bytes: request.max_bytes,
            grant: request.grant,
        }
        .encode_to_vec();
        let (sender, receiver) = mpsc::channel::<Result<ArtifactChunk, Status>>(8);
        tokio::spawn(async move {
            let outcome = async {
                let (mut send, mut recv) = connection
                    .open_bi()
                    .await
                    .map_err(|_| Status::unavailable("failed to open artifact stream"))?;
                let length = u32::try_from(payload.len())
                    .map_err(|_| Status::resource_exhausted("artifact request is too large"))?
                    | ARTIFACT_FETCH_FRAME;
                send.write_all(&length.to_be_bytes())
                    .await
                    .map_err(|_| Status::unavailable("failed to write artifact request length"))?;
                send.write_all(&payload)
                    .await
                    .map_err(|_| Status::unavailable("failed to write artifact request"))?;
                send.finish()
                    .map_err(|_| Status::unavailable("failed to finish artifact request"))?;
                let mut expected_offset = 0_u64;
                loop {
                    let mut size = [0_u8; 4];
                    tokio::time::timeout(Duration::from_secs(30), recv.read_exact(&mut size))
                        .await
                        .map_err(|_| Status::deadline_exceeded("artifact stream timed out"))?
                        .map_err(|_| Status::unavailable("artifact stream ended"))?;
                    let size = u32::from_be_bytes(size) as usize;
                    if size == 0 || size > MAX_FRAME_BYTES {
                        return Err(Status::resource_exhausted("artifact chunk is invalid"));
                    }
                    let mut bytes = vec![0; size];
                    recv.read_exact(&mut bytes)
                        .await
                        .map_err(|_| Status::unavailable("artifact chunk ended"))?;
                    let chunk = ArtifactChunk::decode(bytes.as_slice())
                        .map_err(|_| Status::data_loss("artifact chunk protobuf is invalid"))?;
                    if chunk.artifact_id != request.artifact_id
                        || chunk.offset != expected_offset
                        || chunk.data.len() > 256 * 1024
                        || expected_offset.saturating_add(chunk.data.len() as u64)
                            > request.max_bytes
                        || (!chunk.eof && !chunk.sha256.is_empty())
                        || (chunk.eof && chunk.sha256.len() != 32)
                    {
                        return Err(Status::data_loss("artifact chunk is inconsistent"));
                    }
                    expected_offset = expected_offset.saturating_add(chunk.data.len() as u64);
                    let eof = chunk.eof;
                    sender
                        .send(Ok(chunk))
                        .await
                        .map_err(|_| Status::cancelled("artifact consumer disconnected"))?;
                    if eof {
                        return Ok(());
                    }
                }
            }
            .await;
            if let Err(error) = outcome {
                let _ = sender.send(Err(error)).await;
            }
        });
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
        let (connection, session_mode) = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id)
            .map(|entry| (entry.connection.clone(), entry.session_mode))
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if session_mode == TransportSessionMode::ReadOnly {
            return Err(Status::permission_denied(
                "read-only session does not permit artifact consumption",
            ));
        }
        let payload = AgentArtifactConsumeRequest {
            grant: request.grant,
            sha256: request.sha256,
            size: request.size,
            confirm_only,
        }
        .encode_to_vec();
        let response = tokio::time::timeout(STREAM_TIMEOUT, async move {
            let (mut send, mut recv) = connection
                .open_bi()
                .await
                .map_err(|_| Status::unavailable("failed to open artifact finalize stream"))?;
            let length = u32::try_from(payload.len()).map_err(|_| {
                Status::resource_exhausted("artifact finalize request is too large")
            })? | ARTIFACT_CONSUME_FRAME;
            send.write_all(&length.to_be_bytes())
                .await
                .map_err(|_| Status::unavailable("failed to write artifact finalize length"))?;
            send.write_all(&payload)
                .await
                .map_err(|_| Status::unavailable("failed to write artifact finalize request"))?;
            send.finish()
                .map_err(|_| Status::unavailable("failed to finish artifact finalize request"))?;
            let mut size = [0_u8; 4];
            recv.read_exact(&mut size)
                .await
                .map_err(|_| Status::unavailable("artifact finalize response ended"))?;
            let size = u32::from_be_bytes(size) as usize;
            if size == 0 || size > 64 * 1024 {
                return Err(Status::data_loss("artifact finalize response is invalid"));
            }
            let mut bytes = vec![0; size];
            recv.read_exact(&mut bytes)
                .await
                .map_err(|_| Status::unavailable("artifact finalize response ended"))?;
            let response = AgentArtifactConsumeResponse::decode(bytes.as_slice())
                .map_err(|_| Status::data_loss("artifact finalize response is invalid"))?;
            if response.artifact_id != expected_artifact_id
                || response.grant_id != expected_grant_id
                || (!confirm_only && !response.consumed)
            {
                return Err(Status::data_loss(
                    "artifact finalize response is inconsistent",
                ));
            }
            Ok::<_, Status>(response)
        })
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
        let mut state = self.shared.inner.events.lock().await;
        state.subscribers.retain(|sender| !sender.is_closed());
        if !after.is_empty() && !state.retained.iter().any(|item| item.event_id == after) {
            return Err(Status::out_of_range("event cursor is outside retention"));
        }
        if state.subscribers.len() == self.shared.inner.event_capacity {
            return Err(Status::resource_exhausted(
                "event subscriber capacity reached",
            ));
        }
        let backlog = state
            .retained
            .iter()
            .skip_while(|item| !after.is_empty() && item.event_id != after)
            .skip(usize::from(!after.is_empty()))
            .cloned()
            .collect::<Vec<_>>();
        let (sender, receiver) = mpsc::channel(self.shared.inner.event_capacity);
        for item in backlog {
            sender
                .try_send(Ok(item))
                .map_err(|_| Status::internal("retained event queue overflow"))?;
        }
        state.subscribers.push(sender);
        drop(state);
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
            let should_close = self
                .shared
                .inner
                .connections
                .lock()
                .await
                .get(&node_id)
                .is_some_and(|entry| entry.authorization_revision < retained_revision);
            if should_close {
                let reason = if state == NodeTrustState::Revoked {
                    b"node revoked".as_slice()
                } else {
                    b"node authorization changed".as_slice()
                };
                let _ = self.shared.remove(&node_id, reason).await;
            }
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
    trust: Option<TrustAuthority>,
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
        if matches!(self.kind, ProtocolKind::Enroll) && self.trust.is_some() {
            let request = read_enrollment(recv).await?;
            validate_enrollment(&request, connection.remote_id())?;
            let trust = self
                .trust
                .as_ref()
                .ok_or_else(|| protocol_error("trust authority unavailable"))?;
            let response = trust.enroll(request).await?;
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
        if self.trust.is_none() {
            validate_protocol_version(&handshake)?;
        }
        let response = match self.kind {
            ProtocolKind::Enroll => local_handshake_response(
                HandshakeResult::PendingApproval,
                handshake.max_message_size,
            ),
            ProtocolKind::Agent if self.trust.is_some() => {
                self.trust
                    .as_ref()
                    .ok_or_else(|| protocol_error("trust authority unavailable"))?
                    .authorize(connection.remote_id(), handshake.clone())
                    .await?
            }
            ProtocolKind::Agent
                if self
                    .policy
                    .matches_node(connection.remote_id(), &handshake.node_id) =>
            {
                static_read_only_handshake_response(&handshake)
            }
            ProtocolKind::Agent => {
                return Err(protocol_error("endpoint is not bound to the claimed node"));
            }
        };
        write_message(send, &response).await?;
        let result =
            HandshakeResult::try_from(response.result).unwrap_or(HandshakeResult::Unspecified);
        if matches!(self.kind, ProtocolKind::Enroll) || result != HandshakeResult::Accepted {
            wait_for_delivery(send).await?;
            let reason = if matches!(self.kind, ProtocolKind::Enroll) {
                b"enrollment pending".as_slice()
            } else {
                b"session rejected".as_slice()
            };
            connection.close(VarInt::from_u32(0x101), reason);
            return Ok(None);
        }
        if response.max_message_size == 0
            || response.max_message_size > handshake.max_message_size
            || response.max_message_size as usize > MAX_FRAME_BYTES
        {
            return Err(protocol_error("negotiated message size is invalid"));
        }
        let negotiated_capabilities = validate_negotiated_session(
            &handshake,
            connection.remote_id(),
            &response,
            self.trust.is_some(),
        )?;
        let session_mode = if response.protocol_minor == 0 {
            TransportSessionMode::ReadOnly
        } else {
            TransportSessionMode::AuthorizedV11
        };
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
        let mut handshake = handshake;
        handshake.max_message_size = response.max_message_size;
        Ok(Some(NegotiatedSession {
            handshake,
            negotiated_capabilities,
            session_mode,
            authorization_revision,
            session_expires_at,
        }))
    }
}

impl ProtocolHandler for SessionHandler {
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        let permits = match self.kind {
            ProtocolKind::Enroll => &self.shared.inner.enrollment_connection_permits,
            ProtocolKind::Agent => &self.shared.inner.agent_connection_permits,
        };
        let permit = permits
            .clone()
            .try_acquire_owned()
            .map_err(|_| protocol_error("connection capacity reached"))?;
        let (mut send, mut recv) = tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.accept_bi())
            .await
            .map_err(|_| protocol_error("handshake stream timed out"))?
            .map_err(|_| protocol_error("handshake stream failed"))?;
        let Some(session) = self.negotiate(&connection, &mut send, &mut recv).await? else {
            drop(permit);
            return Ok(());
        };
        let max_message_size = session.handshake.max_message_size as usize;

        connection.set_max_concurrent_bi_streams(VarInt::from_u32(MAX_STREAMS));
        connection.set_max_concurrent_uni_streams(VarInt::from_u32(2));
        let metadata = metadata(&session, &connection).await;
        let node_id = metadata.node_id.clone();
        let registration_token = self.shared.registration_token(&node_id).await;
        if let Some(trust) = &self.trust {
            match trust.recheck_agent(connection.remote_id()).await {
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
        }
        let mut registry = self.shared.inner.connections.lock().await;
        // Trust updates change policy before taking the registry lock. Checking
        // both local policy and this node's close token makes a concurrent revoke
        // either reject this session here or close it after registration.
        if self.policy.revoked(connection.remote_id())
            || !Arc::ptr_eq(
                &self.shared.registration_token(&node_id).await,
                &registration_token,
            )
        {
            connection.close(VarInt::from_u32(0x101), b"session revoked");
            return Err(protocol_error("endpoint was revoked during handshake"));
        }
        let replaced = registry.insert(
            node_id.clone(),
            RegisteredConnection {
                metadata: metadata.clone(),
                connection: connection.clone(),
                max_message_size,
                negotiated_capabilities: session.negotiated_capabilities,
                session_mode: session.session_mode,
                authorization_revision: session.authorization_revision,
                session_expires_at: session.session_expires_at,
            },
        );
        if let Some(previous) = replaced {
            previous
                .connection
                .close(VarInt::from_u32(0x106), b"superseded connection");
            self.shared
                .publish(event(
                    &node_id,
                    &previous.metadata.endpoint_id,
                    TransportEventType::Disconnected,
                    b"connection superseded".to_vec(),
                ))
                .await;
        }
        self.shared
            .publish(event(
                &node_id,
                &metadata.endpoint_id,
                TransportEventType::Connected,
                metadata.path_detail.as_bytes().to_vec(),
            ))
            .await;
        drop(registry);

        let monitors = (
            monitor_paths(self.shared.clone(), node_id.clone(), connection.clone()),
            monitor_telemetry(
                self.shared.clone(),
                node_id.clone(),
                connection.clone(),
                max_message_size,
            ),
        );
        let _closed = connection.closed().await;
        finish_session(&self.shared, &node_id, &connection, monitors, permit).await;
        Ok(())
    }
}

async fn finish_session(
    shared: &Shared,
    node_id: &[u8],
    connection: &Connection,
    monitors: (tokio::task::JoinHandle<()>, tokio::task::JoinHandle<()>),
    _permit: OwnedSemaphorePermit,
) {
    monitors.0.abort();
    monitors.1.abort();
    let mut registry = shared.inner.connections.lock().await;
    if registry
        .get(node_id)
        .is_some_and(|entry| entry.connection.stable_id() == connection.stable_id())
    {
        registry.remove(node_id);
        shared
            .publish(event(
                node_id,
                connection.remote_id().as_bytes(),
                TransportEventType::Disconnected,
                b"connection closed".to_vec(),
            ))
            .await;
    }
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
            shared
                .publish(event(
                    &node_id,
                    connection.remote_id().as_bytes(),
                    TransportEventType::Telemetry,
                    payload,
                ))
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
    for _ in 0..MAX_RESPONSE_FRAMES {
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
            publish_command_unknown(event_context, &command, "agent response protobuf invalid")
                .await;
            return;
        };
        let event_type = match AgentEventType::try_from(agent_event.r#type) {
            Ok(AgentEventType::CommandResult) => TransportEventType::CommandResult,
            Ok(AgentEventType::Heartbeat) => TransportEventType::Heartbeat,
            Ok(AgentEventType::Error) => TransportEventType::Error,
            _ => {
                publish_command_unknown(event_context, &command, "agent response type invalid")
                    .await;
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
            if !valid_privileged_result_proof(&command, &result) {
                publish_command_unknown(
                    event_context,
                    &command,
                    "privd result receipt missing or malformed",
                )
                .await;
                return;
            }
        }
        let terminal = matches!(
            event_type,
            TransportEventType::CommandResult | TransportEventType::Error
        );
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
        if terminal {
            return;
        }
    }
    publish_command_unknown(
        event_context,
        &command,
        "agent response frame limit exceeded",
    )
    .await;
}

async fn publish_command_unknown(
    (shared, node_id, traceparent, connection): (&Shared, &[u8], &str, &Connection),
    command: &CommandEnvelope,
    _message: &str,
) {
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
        Ok(CommandResultState::Succeeded | CommandResultState::Failed)
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
        && receipt.command_id.len() == 16
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
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| protocol_error(&format!("{kind} length timed out")))?
        .map_err(|_| protocol_error(&format!("{kind} length invalid")))?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_HANDSHAKE_BYTES {
        return Err(protocol_error(&format!("{kind} size invalid")));
    }
    let mut bytes = vec![0_u8; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| protocol_error(&format!("{kind} body timed out")))?
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
    let stopped = tokio::time::timeout(HANDSHAKE_TIMEOUT, send.stopped())
        .await
        .map_err(|_| protocol_error("handshake response acknowledgement timed out"))?
        .map_err(|_| protocol_error("handshake response acknowledgement failed"))?;
    if stopped.is_some() {
        return Err(protocol_error("peer rejected handshake response"));
    }
    Ok(())
}

fn local_handshake_response(
    result: HandshakeResult,
    max_message_size: u32,
) -> SessionHandshakeResponse {
    SessionHandshakeResponse {
        result: result.into(),
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: PROTOCOL_MINOR,
        max_message_size,
        controller_version: env!("CARGO_PKG_VERSION").to_owned(),
        negotiated_capabilities: Vec::new(),
        session_grant: None,
    }
}

fn static_read_only_handshake_response(handshake: &SessionHandshake) -> SessionHandshakeResponse {
    let advertised = handshake
        .capabilities
        .iter()
        .map(String::as_str)
        .collect::<HashSet<_>>();
    let negotiated_capabilities = READ_ONLY_SESSION_CAPABILITIES
        .iter()
        .filter(|capability| advertised.contains(**capability))
        .map(|capability| (*capability).to_owned())
        .collect();
    SessionHandshakeResponse {
        result: HandshakeResult::Accepted.into(),
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: 0,
        max_message_size: handshake.max_message_size,
        controller_version: env!("CARGO_PKG_VERSION").to_owned(),
        negotiated_capabilities,
        session_grant: None,
    }
}

fn validate_negotiated_session(
    handshake: &SessionHandshake,
    endpoint: EndpointId,
    response: &SessionHandshakeResponse,
    controller_authoritative: bool,
) -> Result<HashSet<String>, AcceptError> {
    if response.protocol_major != PROTOCOL_MAJOR
        || response.protocol_minor > PROTOCOL_MINOR
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
    if response.protocol_minor == 0 {
        if response
            .negotiated_capabilities
            .iter()
            .any(|capability| !is_read_only_session_capability(capability))
        {
            return Err(protocol_error(
                "legacy sessions may negotiate read-only capabilities only",
            ));
        }
        if response.session_grant.is_some() {
            return Err(protocol_error("legacy session grant is invalid"));
        }
        return Ok(response.negotiated_capabilities.iter().cloned().collect());
    }
    if !controller_authoritative {
        return Err(protocol_error(
            "mutation-capable sessions require Controller authority",
        ));
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
    if request.token.len() != 43 || request.nonce.len() < 16 || request.nonce.len() > 64 {
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
    if handshake.protocol_major != PROTOCOL_MAJOR || handshake.protocol_minor > PROTOCOL_MINOR {
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

/// Builds the single controller endpoint and registers exactly the two platform ALPNs.
///
/// # Errors
///
/// Returns an error when the initial fail-closed trust snapshot is unavailable
/// or Iroh endpoint initialization fails.
pub async fn build_router(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    service: &IrohTransportService,
) -> Result<Router, iroh::endpoint::BindError> {
    build_router_with_direct(secret_key, relay_mode, policy, None, service, true).await
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
    trust.load_snapshot(&policy).await?;
    build_router_with_direct(secret_key, relay_mode, policy, Some(trust), service, true)
        .await
        .map_err(|_| protocol_error("iroh transport bind failed"))
}

async fn build_router_with_direct(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: Option<TrustAuthority>,
    service: &IrohTransportService,
    direct_enabled: bool,
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
        .transport_config(transport)
        .hooks(SecurityHook::new(policy.clone(), trust.as_ref()));
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
            },
        )
        .accept(
            AGENT_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy,
                kind: ProtocolKind::Agent,
                trust,
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
mod tests {
    use ed25519_dalek::SigningKey;
    use iroh::{TransportAddr, Watcher as _, tls::CaTlsConfig};
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        CommandAuthorizationProof, CommandAuthorizationVersion, CommandDeliveryMode,
        GroupObservation, PrivdReceiptVersion, PrivdResultReceiptV1, PrivilegedCommandKind,
        PrivilegedResultKind, SealedSecretPurpose, SealedSecretVersion, SealingKeyDescriptorV1,
        SemanticPayloadHashVersion, ServiceReload, UserObservation,
    };
    use ocservia_privd_attestation::{key_id, sign_receipt, verify_receipt};
    use sha2::{Digest as _, Sha256};

    use super::*;

    const RELAYED_AUTHORIZATION_SIGNATURE: &[u8] = &[0xa5; 64];

    fn sealing_keys() -> Vec<SealingKeyDescriptorV1> {
        vec![
            SealingKeyDescriptorV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::UserPassword as i32,
                key_id: "fixture-user-key-v1".into(),
                public_key_sha256: vec![0x11; 32],
            },
            SealingKeyDescriptorV1 {
                version: SealedSecretVersion::V1 as i32,
                purpose: SealedSecretPurpose::CertificateP12Password as i32,
                key_id: "fixture-p12-key-v1".into(),
                public_key_sha256: vec![0x22; 32],
            },
        ]
    }

    fn handshake(key: &SecretKey) -> SessionHandshake {
        SessionHandshake {
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            agent_version: "test".to_owned(),
            controller_version: String::new(),
            node_id: Uuid::now_v7().as_bytes().to_vec(),
            endpoint_id: key.public().as_bytes().to_vec(),
            capabilities: vec!["ocserv.status.read".to_owned()],
            ocserv_version: "1.3".to_owned(),
            os_release: "test".to_owned(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            supported_compressions: Vec::new(),
            max_message_size: MAX_FRAME_BYTES_U32,
            time: Some(now_timestamp()),
            nonce: vec![7; 32],
            sealing_keys: sealing_keys(),
        }
    }

    fn identity_policy(key: &SecretKey, handshake: &SessionHandshake) -> IdentityPolicy {
        IdentityPolicy::new(
            HashMap::from([(key.public(), handshake.node_id.clone())]),
            HashSet::new(),
        )
    }

    fn command_envelope(node_id: &[u8], traceparent: &str, reason: String) -> Vec<u8> {
        CommandEnvelope {
            protocol_version: "1.1".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            node_id: node_id.to_vec(),
            sequence: 1,
            issued_at: Some(now_timestamp()),
            expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
            expected_revision: 0,
            traceparent: traceparent.to_owned(),
            actor_id: "test".to_owned(),
            required_capability: "ocserv.status.read".to_owned(),
            reason,
            authorization: Some(CommandAuthorizationProof {
                version: CommandAuthorizationVersion::V1.into(),
                key_id: "ed25519-sha256:test-transport-relay-only".to_owned(),
                signature: RELAYED_AUTHORIZATION_SIGNATURE.to_vec(),
            }),
            delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
            payload: Some(command_envelope::Payload::ServiceReload(ServiceReload {})),
            semantic_payload_hash_version: SemanticPayloadHashVersion::V1 as i32,
            semantic_payload_sha256: vec![0x11; 32],
            operation_id: Uuid::now_v7().as_bytes().to_vec(),
            ..CommandEnvelope::default()
        }
        .encode_to_vec()
    }

    async fn send_handshake(
        connection: &Connection,
        handshake: &SessionHandshake,
    ) -> SessionHandshakeResponse {
        let bytes = handshake.encode_to_vec();
        let (mut send, mut recv) = connection.open_bi().await.expect("open handshake stream");
        let request_len = u32::try_from(bytes.len()).expect("test handshake length fits u32");
        send.write_all(&request_len.to_be_bytes())
            .await
            .expect("write handshake length");
        send.write_all(&bytes).await.expect("write handshake");
        send.finish().expect("finish handshake request");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read response length");
        let mut response = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut response).await.expect("read response");
        SessionHandshakeResponse::decode(response.as_slice()).expect("decode response")
    }

    fn privileged_command_result(
        command: &CommandEnvelope,
        include_receipt: bool,
    ) -> CommandResult {
        let now = now_timestamp();
        let result_bytes = b"completed".to_vec();
        let signing_key = SigningKey::from_bytes(&[7; 32]);
        let proof = include_receipt.then(|| {
            sign_receipt(
                PrivdResultReceiptV1 {
                    receipt_version: PrivdReceiptVersion::V1.into(),
                    node_id: command.node_id.clone(),
                    privd_attestation_key_id: key_id(&signing_key.verifying_key()),
                    command_id: command.command_id.clone(),
                    operation_id: command.operation_id.clone(),
                    idempotency_key: command.idempotency_key.clone(),
                    semantic_payload_hash_version: command.semantic_payload_hash_version,
                    semantic_payload_sha256: command.semantic_payload_sha256.clone(),
                    command_kind: PrivilegedCommandKind::ServiceReload.into(),
                    result_kind: PrivilegedResultKind::Mutation.into(),
                    terminal_state: CommandResultState::Succeeded.into(),
                    result_bytes_sha256: Sha256::digest(&result_bytes).to_vec(),
                    error_code_sha256: Sha256::digest([]).to_vec(),
                    effect_record_id: Uuid::now_v7().as_bytes().to_vec(),
                    effect_sequence: 1,
                    accepted_at: Some(now),
                    completed_at: Some(now),
                    replayed: false,
                    certificate: None,
                },
                &signing_key,
            )
            .expect("sign root result receipt")
        });
        CommandResult {
            command_id: command.command_id.clone(),
            idempotency_key: command.idempotency_key.clone(),
            payload_sha256: command.semantic_payload_sha256.clone(),
            state: CommandResultState::Succeeded.into(),
            result: result_bytes,
            error_code: String::new(),
            accepted_at: Some(now),
            completed_at: Some(now),
            replayed: false,
            semantic_payload_hash_version: command.semantic_payload_hash_version,
            privileged_result_proof: proof,
        }
    }

    async fn respond_to_command(connection: Connection, include_receipt: bool) {
        let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read command length");
        let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut command).await.expect("read command");
        let decoded = CommandEnvelope::decode(command.as_slice()).expect("decode command");
        assert_eq!(
            decoded
                .authorization
                .as_ref()
                .expect("relayed authorization")
                .signature,
            RELAYED_AUTHORIZATION_SIGNATURE
        );
        let response = AgentEvent {
            r#type: AgentEventType::CommandResult.into(),
            payload: privileged_command_result(&decoded, include_receipt).encode_to_vec(),
        }
        .encode_to_vec();
        let response_len = u32::try_from(response.len()).expect("response length fits u32");
        send.write_all(&response_len.to_be_bytes())
            .await
            .expect("write response length");
        send.write_all(&response).await.expect("write response");
        send.finish().expect("finish response");
    }

    #[test]
    fn privileged_terminal_result_requires_a_well_formed_receipt() {
        let node_id = Uuid::now_v7();
        let command = CommandEnvelope::decode(
            command_envelope(node_id.as_bytes(), &new_traceparent(), String::new()).as_slice(),
        )
        .expect("decode command");
        let valid = privileged_command_result(&command, true);
        assert!(valid_privileged_result_proof(&command, &valid));

        let missing = privileged_command_result(&command, false);
        assert!(!valid_privileged_result_proof(&command, &missing));

        let mut malformed = valid;
        malformed
            .privileged_result_proof
            .as_mut()
            .expect("proof")
            .version = PrivdReceiptVersion::Unspecified.into();
        assert!(!valid_privileged_result_proof(&command, &malformed));
    }

    async fn end_command_without_response(connection: Connection) {
        let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read command length");
        let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut command).await.expect("read command");
        let decoded = CommandEnvelope::decode(command.as_slice()).expect("decode command");
        assert_eq!(
            decoded
                .authorization
                .expect("relayed authorization")
                .signature,
            RELAYED_AUTHORIZATION_SIGNATURE
        );
        send.finish().expect("finish empty response");
    }

    async fn next_event(
        events: &mut EventStream,
        event_type: TransportEventType,
    ) -> TransportEvent {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let event = events
                    .next()
                    .await
                    .expect("event stream remains open")
                    .expect("event is valid");
                if event.r#type == i32::from(event_type) {
                    break event;
                }
            }
        })
        .await
        .expect("event arrives")
    }

    async fn wait_until_registered(service: &IrohTransportService, node_id: &[u8]) {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .contains_key(node_id)
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("connection registered");
    }

    async fn last_seen(
        service: &IrohTransportService,
        node_id: &[u8],
    ) -> Option<prost_types::Timestamp> {
        service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: node_id.to_vec(),
            }))
            .await
            .expect("query connection")
            .into_inner()
            .last_seen
    }

    #[test]
    fn policy_rejects_unknown_agent_and_revoked_enrollment() {
        let approved = SecretKey::generate().public();
        let revoked = SecretKey::generate().public();
        let approved_node = Uuid::now_v7().as_bytes().to_vec();
        let policy = IdentityPolicy::new(
            HashMap::from([(approved, approved_node.clone())]),
            HashSet::from([revoked]),
        );
        assert!(policy.permits(approved, AGENT_ALPN));
        assert!(policy.matches_node(approved, &approved_node));
        assert!(!policy.matches_node(approved, Uuid::now_v7().as_bytes()));
        assert!(!policy.permits(revoked, ENROLL_ALPN));
        assert!(!policy.permits(SecretKey::generate().public(), AGENT_ALPN));
        let dynamic = SecretKey::generate().public();
        let dynamic_node = Uuid::now_v7().as_bytes().to_vec();
        assert_eq!(
            policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Active, 1),
            PolicyUpdateDisposition::Applied
        );
        assert!(policy.matches_node(dynamic, &dynamic_node));
        assert_eq!(
            policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Revoked, 2),
            PolicyUpdateDisposition::Applied
        );
        assert_eq!(
            policy.update(dynamic, dynamic_node.clone(), NodeTrustState::Active, 1),
            PolicyUpdateDisposition::Stale
        );
        assert_eq!(
            policy.update(dynamic, dynamic_node, NodeTrustState::Active, 3),
            PolicyUpdateDisposition::Rejected
        );
        assert!(!policy.permits(dynamic, AGENT_ALPN));
        assert!(policy.revoked(dynamic));

        let restored_revocation = SecretKey::generate().public();
        let restored = IdentityPolicy::new(HashMap::new(), HashSet::from([restored_revocation]));
        assert_eq!(
            restored.update(
                restored_revocation,
                Uuid::now_v7().as_bytes().to_vec(),
                NodeTrustState::Active,
                u64::MAX,
            ),
            PolicyUpdateDisposition::Rejected
        );
        assert!(restored.revoked(restored_revocation));
    }

    #[tokio::test]
    async fn trust_update_reports_stale_and_rejects_tombstone_reactivation() {
        let service = IrohTransportService::new(8);
        let endpoint = SecretKey::generate().public();
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let update = |state: NodeTrustState, revision: u64| UpdateNodeTrustRequest {
            node_id: node_id.clone(),
            endpoint_id: endpoint.as_bytes().to_vec(),
            state: state.into(),
            reason: "trust transition fixture".into(),
            revision,
        };
        let active = service
            .update_node_trust(Request::new(update(NodeTrustState::Active, 2)))
            .await
            .expect("activate endpoint")
            .into_inner();
        assert_eq!(
            active.disposition,
            i32::from(TrustUpdateDisposition::Applied)
        );
        let stale = service
            .update_node_trust(Request::new(update(NodeTrustState::Active, 1)))
            .await
            .expect("report stale update")
            .into_inner();
        assert_eq!(stale.disposition, i32::from(TrustUpdateDisposition::Stale));
        assert_eq!(stale.retained_revision, 2);
        assert_eq!(stale.retained_state, i32::from(NodeTrustState::Active));

        let revoked = service
            .update_node_trust(Request::new(update(NodeTrustState::Revoked, 3)))
            .await
            .expect("revoke endpoint")
            .into_inner();
        assert_eq!(
            revoked.disposition,
            i32::from(TrustUpdateDisposition::Applied)
        );
        let reactivation = service
            .update_node_trust(Request::new(update(NodeTrustState::Active, 4)))
            .await
            .expect("report rejected reactivation")
            .into_inner();
        assert_eq!(
            reactivation.disposition,
            i32::from(TrustUpdateDisposition::Rejected)
        );
        assert_eq!(reactivation.retained_revision, 3);
        assert_eq!(
            reactivation.retained_state,
            i32::from(NodeTrustState::Revoked)
        );
    }

    #[tokio::test]
    async fn close_attempt_invalidates_only_target_registration() {
        let shared = Shared::new(8);
        let target = Uuid::now_v7().as_bytes().to_vec();
        let unrelated = Uuid::now_v7().as_bytes().to_vec();
        let target_token = shared.registration_token(&target).await;
        let unrelated_token = shared.registration_token(&unrelated).await;
        assert!(shared.remove(&target, b"revoked").await.is_none());
        assert!(!Arc::ptr_eq(
            &shared.registration_token(&target).await,
            &target_token
        ));
        assert!(Arc::ptr_eq(
            &shared.registration_token(&unrelated).await,
            &unrelated_token
        ));
    }

    #[test]
    fn enrollment_request_binds_the_authenticated_endpoint() {
        let key = SecretKey::generate();
        let mut request = EnrollRequest {
            token: "a".repeat(43),
            endpoint_id: key.public().as_bytes().to_vec(),
            agent_version: "test".to_owned(),
            os_release: "test".to_owned(),
            ocserv_version: "test".to_owned(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            capabilities: vec!["ocserv.status.read".to_owned()],
            environment: "test".to_owned(),
            nonce: vec![0; 16],
            time: Some(now_timestamp()),
            enrollment_protocol_major: 0,
            enrollment_protocol_minor: 0,
            proof: None,
            sealing_keys: sealing_keys(),
        };
        ocservia_agent_identity::authorize_enrollment(&mut request, &key)
            .expect("sign enrollment request");
        assert!(validate_enrollment(&request, key.public()).is_ok());
        assert!(validate_enrollment(&request, SecretKey::generate().public()).is_err());
        let mut unicode = request;
        unicode.capabilities = vec!["界".repeat(128)];
        assert!(validate_enrollment(&unicode, key.public()).is_ok());
        unicode.capabilities = vec!["界".repeat(129)];
        assert!(validate_enrollment(&unicode, key.public()).is_err());
    }

    #[test]
    fn enrollment_identity_churn_refuses_new_identity_without_evicting_oldest() {
        let now = Instant::now();
        let mut enrollment_attempts = HashMap::new();
        let oldest = SecretKey::generate().public();
        record_attempt(
            &mut enrollment_attempts,
            oldest,
            now,
            MAX_ENROLLMENT_CONNECTIONS,
        )
        .expect("record oldest enrollment identity");
        for _ in 1..MAX_ENROLLMENT_CONNECTIONS {
            record_attempt(
                &mut enrollment_attempts,
                SecretKey::generate().public(),
                now + Duration::from_millis(1),
                MAX_ENROLLMENT_CONNECTIONS,
            )
            .expect("fill enrollment identity capacity");
        }
        let newcomer = SecretKey::generate().public();
        assert_eq!(
            record_attempt(
                &mut enrollment_attempts,
                newcomer,
                now + Duration::from_millis(2),
                MAX_ENROLLMENT_CONNECTIONS,
            ),
            Err(AttemptRejection::Capacity)
        );
        assert_eq!(enrollment_attempts.len(), MAX_ENROLLMENT_CONNECTIONS);
        assert!(enrollment_attempts.contains_key(&oldest));
        assert!(!enrollment_attempts.contains_key(&newcomer));

        let mut agent_attempts = HashMap::new();
        assert_eq!(
            record_attempt(
                &mut agent_attempts,
                SecretKey::generate().public(),
                now,
                MAX_AGENT_CONNECTIONS,
            ),
            Ok(())
        );
    }

    #[test]
    fn trust_attempt_rate_is_global_and_non_evictable() {
        let now = Instant::now();
        let mut attempts = VecDeque::new();
        assert_eq!(record_global_attempt(&mut attempts, now, 2), Ok(()));
        assert_eq!(
            record_global_attempt(&mut attempts, now + Duration::from_millis(1), 2),
            Ok(())
        );
        assert_eq!(
            record_global_attempt(&mut attempts, now + Duration::from_millis(2), 2),
            Err(AttemptRejection::Rate)
        );
        assert_eq!(
            record_global_attempt(&mut attempts, now + Duration::from_mins(1), 2),
            Ok(())
        );
    }

    #[tokio::test]
    async fn trust_authority_clones_share_the_global_limits() {
        let channel = tonic::transport::Endpoint::from_static("http://[::1]:50051").connect_lazy();
        let trust = TrustAuthority::new(channel);
        let clone = trust.clone();
        assert!(Arc::ptr_eq(&trust.budgets, &clone.budgets));
        for class in [
            TrustClass::KnownAgentHandshake,
            TrustClass::AgentAuthorization,
            TrustClass::AgentRegistrationRecheck,
            TrustClass::UnknownEnrollmentPreAuth,
            TrustClass::EnrollmentCompletion,
        ] {
            assert_eq!(trust.metrics(class), TrustMetricsSnapshot::default());
            assert!(!class.label().contains("endpoint"));
        }
    }

    #[tokio::test]
    async fn unknown_enrollment_flood_cannot_consume_reserved_agent_budgets() {
        let config = TrustBudgetConfig::default()
            .with_limit(
                TrustClass::UnknownEnrollmentPreAuth,
                TrustClassLimit {
                    attempts_per_minute: 20,
                    concurrency: 20,
                },
            )
            .expect("valid test limit")
            .with_limit(
                TrustClass::KnownAgentHandshake,
                TrustClassLimit {
                    attempts_per_minute: 3,
                    concurrency: 3,
                },
            )
            .expect("valid test limit")
            .with_limit(
                TrustClass::AgentAuthorization,
                TrustClassLimit {
                    attempts_per_minute: 3,
                    concurrency: 3,
                },
            )
            .expect("valid test limit")
            .with_limit(
                TrustClass::AgentRegistrationRecheck,
                TrustClassLimit {
                    attempts_per_minute: 3,
                    concurrency: 3,
                },
            )
            .expect("valid test limit")
            .with_limit(
                TrustClass::EnrollmentCompletion,
                TrustClassLimit {
                    attempts_per_minute: 3,
                    concurrency: 3,
                },
            )
            .expect("valid test limit");
        let budgets = Arc::new(TrustBudgetSet::new(&config));
        let now = Instant::now();
        let mut enrollment = Vec::new();
        for _ in 0..20 {
            enrollment.push(
                budgets
                    .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                    .await
                    .expect("unknown enrollment receives only its own permits"),
            );
        }
        assert_eq!(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .err(),
            Some(AttemptRejection::Capacity)
        );
        for class in [
            TrustClass::KnownAgentHandshake,
            TrustClass::AgentAuthorization,
            TrustClass::AgentRegistrationRecheck,
            TrustClass::EnrollmentCompletion,
        ] {
            let permit = budgets
                .acquire_at(class, now)
                .await
                .expect("approved class retains independent capacity during enrollment flood");
            assert_eq!(budgets.snapshot(class).accepted, 1);
            drop(permit);
        }
        drop(enrollment);
        assert_eq!(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .err(),
            Some(AttemptRejection::Rate)
        );
        let recovered = budgets
            .acquire_at(
                TrustClass::UnknownEnrollmentPreAuth,
                now + Duration::from_mins(1),
            )
            .await
            .expect("rolling window recovers deterministically");
        drop(recovered);
    }

    #[tokio::test]
    async fn baseline_budget_parameters_remain_partitioned() {
        let shared_limit = TrustClassLimit {
            attempts_per_minute: 600,
            concurrency: 16,
        };
        let mut config = TrustBudgetConfig::default();
        for class in [
            TrustClass::KnownAgentHandshake,
            TrustClass::AgentAuthorization,
            TrustClass::AgentRegistrationRecheck,
            TrustClass::UnknownEnrollmentPreAuth,
            TrustClass::EnrollmentCompletion,
        ] {
            config = config
                .with_limit(class, shared_limit)
                .expect("baseline-compatible trust limit");
        }
        let budgets = Arc::new(TrustBudgetSet::new(&config));
        let now = Instant::now();
        for _ in 0..shared_limit.attempts_per_minute {
            drop(
                budgets
                    .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                    .await
                    .expect("enrollment consumes only its own rolling window"),
            );
        }
        assert_eq!(
            budgets
                .acquire_at(TrustClass::UnknownEnrollmentPreAuth, now)
                .await
                .err(),
            Some(AttemptRejection::Rate)
        );
        for class in [
            TrustClass::KnownAgentHandshake,
            TrustClass::AgentAuthorization,
            TrustClass::AgentRegistrationRecheck,
        ] {
            drop(
                budgets
                    .acquire_at(class, now)
                    .await
                    .expect("Agent reserve survives enrollment exhaustion"),
            );
        }
    }

    #[tokio::test]
    async fn router_restart_retains_identity_and_direct_connectivity() {
        let controller_key = SecretKey::generate();
        let controller_id = controller_key.public();
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let node_id = handshake.node_id.clone();

        for _ in 0..2 {
            let service = IrohTransportService::new(8);
            let router = build_router(
                controller_key.clone(),
                RelayMode::Disabled,
                identity_policy(&agent_key, &handshake),
                &service,
            )
            .await
            .expect("build direct router");
            assert_eq!(router.endpoint().id(), controller_id);
            let client = Endpoint::builder(presets::N0)
                .secret_key(agent_key.clone())
                .relay_mode(RelayMode::Disabled)
                .bind()
                .await
                .expect("build direct client");
            let connection = client
                .connect(router.endpoint().addr(), AGENT_ALPN)
                .await
                .expect("connect after router start");
            let response = send_handshake(&connection, &handshake).await;
            assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
            wait_until_registered(&service, &node_id).await;
            connection.close(VarInt::from_u32(0), b"restart fixture");
            client.close().await;
            shutdown(&service, router)
                .await
                .expect("shutdown direct router");
        }
    }

    #[test]
    fn command_response_wait_honors_envelope_expiration() {
        let command = CommandEnvelope {
            expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
            ..CommandEnvelope::default()
        };
        let deadline = command_response_deadline(&command).expect("valid command deadline");
        assert!(deadline > tokio::time::Instant::now() + STREAM_TIMEOUT);

        let expired = CommandEnvelope {
            expires_at: Some((SystemTime::now() - Duration::from_secs(1)).into()),
            ..CommandEnvelope::default()
        };
        assert_eq!(
            command_response_deadline(&expired)
                .expect_err("expired command rejected")
                .code(),
            tonic::Code::DeadlineExceeded
        );
    }

    #[test]
    fn handshake_limits_reject_identity_and_version_mismatch() {
        let remote_key = SecretKey::generate();
        let mut handshake = SessionHandshake {
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            agent_version: "test".to_owned(),
            controller_version: String::new(),
            node_id: Uuid::now_v7().as_bytes().to_vec(),
            endpoint_id: remote_key.public().as_bytes().to_vec(),
            capabilities: Vec::new(),
            ocserv_version: String::new(),
            os_release: String::new(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            supported_compressions: Vec::new(),
            max_message_size: MAX_FRAME_BYTES_U32,
            time: Some(now_timestamp()),
            nonce: vec![7; 32],
            sealing_keys: sealing_keys(),
        };
        assert!(validate_handshake(&handshake, remote_key.public()).is_ok());
        assert!(validate_protocol_version(&handshake).is_ok());
        handshake.protocol_major = 2;
        assert!(validate_handshake(&handshake, remote_key.public()).is_ok());
        assert!(validate_protocol_version(&handshake).is_err());
        handshake.protocol_major = PROTOCOL_MAJOR;
        assert!(validate_handshake(&handshake, SecretKey::generate().public()).is_err());
    }

    #[test]
    fn static_handshake_is_protocol_v1_0_with_explicit_read_only_capabilities() {
        let key = SecretKey::generate();
        let mut request = handshake(&key);
        request.capabilities = vec![
            "synthetic.noop".to_owned(),
            "future.feature.read".to_owned(),
            "ocserv.status.read".to_owned(),
            "ocserv.sessions.read".to_owned(),
        ];
        let response = static_read_only_handshake_response(&request);
        assert_eq!(response.protocol_major, 1);
        assert_eq!(response.protocol_minor, 0);
        assert!(response.session_grant.is_none());
        assert_eq!(
            response.negotiated_capabilities,
            vec!["ocserv.sessions.read", "ocserv.status.read"]
        );
        assert!(validate_negotiated_session(&request, key.public(), &response, false).is_ok());

        let mut grantless_v1_1 = response.clone();
        grantless_v1_1.protocol_minor = 1;
        assert!(
            validate_negotiated_session(&request, key.public(), &grantless_v1_1, false).is_err()
        );
        let mut unknown_read = response;
        unknown_read.negotiated_capabilities = vec!["future.feature.read".to_owned()];
        assert!(validate_negotiated_session(&request, key.public(), &unknown_read, false).is_err());
    }

    #[test]
    fn queues_are_bounded() {
        let service = IrohTransportService::new(usize::MAX);
        assert_eq!(service.shared.inner.event_capacity, MAX_CONNECTIONS);
        assert_eq!(
            service
                .shared
                .inner
                .agent_connection_permits
                .available_permits(),
            MAX_AGENT_CONNECTIONS
        );
        assert_eq!(
            service
                .shared
                .inner
                .enrollment_connection_permits
                .available_permits(),
            MAX_ENROLLMENT_CONNECTIONS
        );
    }

    #[test]
    fn enrollment_capacity_cannot_exhaust_agent_capacity() {
        let service = IrohTransportService::new(8);
        let enrollment_permits = service
            .shared
            .inner
            .enrollment_connection_permits
            .clone()
            .try_acquire_many_owned(
                u32::try_from(MAX_ENROLLMENT_CONNECTIONS).expect("enrollment capacity fits u32"),
            )
            .expect("reserve all enrollment permits");

        assert_eq!(
            service
                .shared
                .inner
                .enrollment_connection_permits
                .available_permits(),
            0
        );
        assert_eq!(
            service
                .shared
                .inner
                .agent_connection_permits
                .available_permits(),
            MAX_AGENT_CONNECTIONS
        );
        drop(enrollment_permits);
    }

    #[test]
    fn transport_events_have_a_valid_root_traceparent() {
        let traceparent = event(
            Uuid::now_v7().as_bytes(),
            SecretKey::generate().public().as_bytes(),
            TransportEventType::Connected,
            Vec::new(),
        )
        .traceparent;
        let parts = traceparent.split('-').collect::<Vec<_>>();
        assert_eq!(parts.len(), 4);
        assert_eq!(parts[0], "00");
        assert_eq!(parts[1].len(), 32);
        assert_eq!(parts[2].len(), 16);
        assert_eq!(parts[3], "01");
        assert!(parts[1..=2].iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        }));
    }

    #[tokio::test]
    async fn shutdown_terminates_event_streams() {
        let service = IrohTransportService::new(8);
        let mut stream = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("open event stream")
            .into_inner();
        service.begin_shutdown().await;
        assert!(
            tokio::time::timeout(Duration::from_secs(1), stream.next())
                .await
                .expect("stream terminates promptly")
                .is_none()
        );
    }

    #[tokio::test]
    async fn router_rejects_wrong_alpn_and_unknown_agent() {
        let service = IrohTransportService::new(8);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            IdentityPolicy::default(),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let address = router.endpoint().addr();

        assert!(
            client
                .connect(address.clone(), b"wrong/alpn/1")
                .await
                .is_err()
        );
        let unknown = client
            .connect(address, AGENT_ALPN)
            .await
            .expect("TLS identity is established before the endpoint hook rejects it");
        tokio::time::timeout(Duration::from_secs(2), unknown.closed())
            .await
            .expect("unknown endpoint closed before application data");

        client.close().await;
        shutdown(&service, router).await.expect("shutdown router");
    }

    #[tokio::test]
    async fn newer_authority_revision_closes_the_retained_session() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let policy = identity_policy(&agent_key, &handshake);
        let service = IrohTransportService::new_with_policy(8, policy.clone());
        let router = build_router(SecretKey::generate(), RelayMode::Disabled, policy, &service)
            .await
            .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .contains_key(&handshake.node_id)
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("connection registered");
        let metadata = service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: handshake.node_id,
            }))
            .await
            .expect("query connection")
            .into_inner();
        assert_eq!(metadata.endpoint_id, agent_key.public().as_bytes());
        assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

        service
            .update_node_trust(Request::new(UpdateNodeTrustRequest {
                node_id: metadata.node_id,
                endpoint_id: agent_key.public().as_bytes().to_vec(),
                state: NodeTrustState::Active.into(),
                reason: "capability authority changed".to_owned(),
                revision: 2,
            }))
            .await
            .expect("advance connected node authority");
        tokio::time::timeout(Duration::from_secs(2), connection.closed())
            .await
            .expect("connection closed after authority revision changed");

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    async fn telemetry_uni_stream_is_identity_checked_and_published() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(8);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect agent");
        send_handshake(&connection, &handshake).await;
        wait_until_registered(&service, &handshake.node_id).await;
        let mut events = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("watch events")
            .into_inner();
        let maximum_name = |prefix: &str, index: usize| {
            format!("{prefix}{index:06}{}", "x".repeat(64 - prefix.len() - 6))
        };
        let users = (0..384)
            .map(|index| UserObservation {
                username: maximum_name("u", index),
                enabled: true,
                revision: 1,
                fingerprint_sha256: vec![1; 32],
            })
            .collect::<Vec<_>>();
        let groups = (0..768)
            .map(|index| GroupObservation {
                group_name: maximum_name("g", index),
                members: if index < 384 {
                    vec![maximum_name("u", index)]
                } else {
                    Vec::new()
                },
                revision: 1,
                fingerprint_sha256: vec![2; 32],
            })
            .collect::<Vec<_>>();
        let batch = TelemetryBatch {
            batch_id: Uuid::now_v7().as_bytes().to_vec(),
            node_id: handshake.node_id.clone(),
            sequence: 1,
            priority: 2,
            snapshot: None,
            sessions: Vec::new(),
            samples: Vec::new(),
            security_events: Vec::new(),
            ip_bans: Vec::new(),
            users,
            groups,
        };
        let payload = batch.encode_to_vec();
        assert!(payload.len() <= 512 * 1024);
        let mut send = connection.open_uni().await.expect("open telemetry stream");
        send.write_all(&u32::try_from(payload.len()).unwrap().to_be_bytes())
            .await
            .expect("write telemetry length");
        send.write_all(&payload)
            .await
            .expect("write telemetry batch");
        send.finish().expect("finish telemetry stream");

        let event = next_event(&mut events, TransportEventType::Telemetry).await;
        assert_eq!(event.node_id, handshake.node_id);
        assert_eq!(event.payload, payload);

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    async fn replacement_connection_publishes_disconnect_before_connect() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(8);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let mut events = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("watch events")
            .into_inner();

        let first = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect first agent session");
        send_handshake(&first, &handshake).await;
        wait_until_registered(&service, &handshake.node_id).await;
        let second = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect replacement agent session");
        send_handshake(&second, &handshake).await;

        let mut connectivity = Vec::new();
        tokio::time::timeout(Duration::from_secs(2), async {
            while connectivity.len() < 3 {
                let event = events
                    .next()
                    .await
                    .expect("event stream remains open")
                    .expect("event is valid");
                if event.r#type == i32::from(TransportEventType::Connected)
                    || event.r#type == i32::from(TransportEventType::Disconnected)
                {
                    connectivity.push(event.r#type);
                }
            }
        })
        .await
        .expect("replacement connectivity events arrive");
        assert_eq!(
            connectivity,
            vec![
                i32::from(TransportEventType::Connected),
                i32::from(TransportEventType::Disconnected),
                i32::from(TransportEventType::Connected),
            ]
        );
        tokio::time::timeout(Duration::from_secs(2), first.closed())
            .await
            .expect("superseded connection closes");

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)]
    async fn command_responses_are_published_with_negotiated_limits() {
        let agent_key = SecretKey::generate();
        let mut handshake = handshake(&agent_key);
        handshake.max_message_size = 512;
        let service = IrohTransportService::new(16);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
        wait_until_registered(&service, &handshake.node_id).await;
        let metadata = service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: handshake.node_id.clone(),
            }))
            .await
            .expect("query negotiated session")
            .into_inner();
        assert_eq!(metadata.negotiated_capabilities, handshake.capabilities);
        service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get_mut(&handshake.node_id)
            .expect("registered session")
            .session_mode = TransportSessionMode::AuthorizedV11;

        let mut unapproved = CommandEnvelope::decode(
            command_envelope(&handshake.node_id, &new_traceparent(), String::new()).as_slice(),
        )
        .expect("decode unapproved command");
        unapproved.required_capability = "ocserv.service.reload".to_owned();
        let error = service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: unapproved.encode_to_vec(),
            }))
            .await
            .expect_err("unnegotiated capability rejected before transport");
        assert_eq!(error.code(), tonic::Code::PermissionDenied);

        {
            let mut connections = service.shared.inner.connections.lock().await;
            let registered = connections
                .get_mut(&handshake.node_id)
                .expect("registered session");
            registered.authorization_revision = 7;
        }
        let error = service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: command_envelope(
                    &handshake.node_id,
                    &new_traceparent(),
                    String::new(),
                ),
            }))
            .await
            .expect_err("stale command revision rejected before transport");
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        {
            let mut connections = service.shared.inner.connections.lock().await;
            let registered = connections
                .get_mut(&handshake.node_id)
                .expect("registered session");
            registered.authorization_revision = 0;
            registered.session_expires_at = Some(SystemTime::UNIX_EPOCH);
        }
        let error = service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: command_envelope(
                    &handshake.node_id,
                    &new_traceparent(),
                    String::new(),
                ),
            }))
            .await
            .expect_err("expired session grant rejected before transport");
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        service
            .shared
            .inner
            .connections
            .lock()
            .await
            .get_mut(&handshake.node_id)
            .expect("registered session")
            .session_expires_at = None;

        let traceparent = new_traceparent();
        let baseline_last_seen = last_seen(&service, &handshake.node_id).await;
        let oversized = command_envelope(&handshake.node_id, &traceparent, "x".repeat(512));
        let error = service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: oversized,
            }))
            .await
            .expect_err("negotiated limit enforced");
        assert_eq!(error.code(), tonic::Code::ResourceExhausted);
        assert_eq!(
            last_seen(&service, &handshake.node_id).await,
            baseline_last_seen
        );

        tokio::time::sleep(Duration::from_millis(2)).await;
        let agent = tokio::spawn(respond_to_command(connection.clone(), true));
        let mut events = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("watch events")
            .into_inner();
        let command = command_envelope(&handshake.node_id, &traceparent, String::new());
        let sent_command =
            CommandEnvelope::decode(command.as_slice()).expect("decode sent command");
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: command,
            }))
            .await
            .expect("send command");
        agent.await.expect("agent response task");
        let event = next_event(&mut events, TransportEventType::CommandResult).await;
        assert_eq!(event.node_id, handshake.node_id);
        assert_eq!(event.traceparent, traceparent);
        let result = CommandResult::decode(event.payload.as_slice()).expect("command result");
        assert_eq!(
            CommandResultState::try_from(result.state).expect("result state"),
            CommandResultState::Succeeded
        );
        assert_eq!(result.result, b"completed");
        let proof = result
            .privileged_result_proof
            .as_ref()
            .expect("root result receipt");
        let signing_key = SigningKey::from_bytes(&[7; 32]);
        let receipt =
            verify_receipt(proof, &signing_key.verifying_key()).expect("valid root receipt");
        assert_eq!(receipt.node_id, sent_command.node_id);
        assert_eq!(receipt.command_id, sent_command.command_id);
        assert_eq!(receipt.operation_id, sent_command.operation_id);
        assert_eq!(receipt.idempotency_key, sent_command.idempotency_key);
        assert_eq!(
            receipt.result_bytes_sha256,
            Sha256::digest(b"completed").to_vec()
        );
        assert_ne!(
            last_seen(&service, &handshake.node_id).await,
            baseline_last_seen
        );

        let missing_receipt_traceparent = new_traceparent();
        let agent = tokio::spawn(respond_to_command(connection.clone(), false));
        let missing_receipt_command_bytes = command_envelope(
            &handshake.node_id,
            &missing_receipt_traceparent,
            String::new(),
        );
        let missing_receipt_command =
            CommandEnvelope::decode(missing_receipt_command_bytes.as_slice())
                .expect("decode missing-receipt command");
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: missing_receipt_command_bytes,
            }))
            .await
            .expect("send command with missing receipt");
        agent.await.expect("missing-receipt response task");
        let event = next_event(&mut events, TransportEventType::CommandResult).await;
        assert_eq!(event.traceparent, missing_receipt_traceparent);
        let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
        assert_eq!(
            CommandResultState::try_from(result.state).expect("result state"),
            CommandResultState::Unknown
        );
        assert_eq!(result.error_code, "outcome_requires_reconciliation");
        assert_eq!(result.command_id, missing_receipt_command.command_id);
        assert_eq!(
            result.idempotency_key,
            missing_receipt_command.idempotency_key
        );

        let eof_traceparent = new_traceparent();
        let agent = tokio::spawn(end_command_without_response(connection.clone()));
        let eof_command_bytes =
            command_envelope(&handshake.node_id, &eof_traceparent, String::new());
        let eof_command =
            CommandEnvelope::decode(eof_command_bytes.as_slice()).expect("decode sent EOF command");
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: eof_command_bytes,
            }))
            .await
            .expect("send command with empty response");
        agent.await.expect("empty response task");
        let event = next_event(&mut events, TransportEventType::CommandResult).await;
        assert_eq!(event.traceparent, eof_traceparent);
        let result = CommandResult::decode(event.payload.as_slice()).expect("unknown result");
        assert_eq!(
            CommandResultState::try_from(result.state).expect("result state"),
            CommandResultState::Unknown
        );
        assert_eq!(result.error_code, "outcome_requires_reconciliation");
        assert_eq!(result.command_id, eof_command.command_id);
        assert_eq!(result.idempotency_key, eof_command.idempotency_key);
        assert_eq!(result.payload_sha256, eof_command.semantic_payload_sha256);
        assert_eq!(
            result.semantic_payload_hash_version,
            eof_command.semantic_payload_hash_version
        );

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    #[ignore = "requires access to the configured public Iroh relay"]
    async fn relay_only_connection_and_disabled_relay_failure() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(8);
        let router = build_router_with_direct(
            SecretKey::generate(),
            RelayMode::Default,
            identity_policy(&agent_key, &handshake),
            None,
            &service,
            false,
        )
        .await
        .expect("build router");
        tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
            .await
            .expect("controller endpoint online");
        let full_address = router.endpoint().addr();
        let relay_url = full_address
            .relay_urls()
            .next()
            .expect("controller selected a home relay")
            .clone();
        let relay_only = iroh::EndpointAddr::new(full_address.id).with_relay_url(relay_url);

        let disabled = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build relay-disabled client");
        let disabled_attempt = tokio::time::timeout(
            Duration::from_secs(5),
            disabled.connect(relay_only.clone(), AGENT_ALPN),
        )
        .await;
        assert!(!matches!(disabled_attempt, Ok(Ok(_))));
        disabled.close().await;

        let client = Endpoint::builder(presets::N0)
            .secret_key(agent_key.clone())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build relay client");
        let connection = tokio::time::timeout(
            Duration::from_secs(30),
            client.connect(relay_only, AGENT_ALPN),
        )
        .await
        .expect("relay connection completed")
        .expect("connect over relay");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let path = service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .get(&handshake.node_id)
                    .map(|entry| entry.metadata.path);
                if path == Some(i32::from(ConnectionPath::Relay)) {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("relay path registered");

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)]
    async fn dedicated_relay_failure_moves_traffic_to_second_relay() {
        const TEST_ALPN: &[u8] = b"ocservia/relay-failover-test/1";
        const RELAY_TOKEN: &str = "i18-dedicated-relay-token";

        #[derive(Debug)]
        struct TokenAccess;

        impl iroh_relay::server::AccessControl for TokenAccess {
            async fn on_connect(
                &self,
                request: &iroh_relay::server::ClientRequest,
            ) -> iroh_relay::server::Access {
                if request.auth_token().as_deref() == Some(RELAY_TOKEN) {
                    iroh_relay::server::Access::Allow
                } else {
                    iroh_relay::server::Access::Deny { reason: None }
                }
            }
        }

        let access = Arc::new(TokenAccess);
        let (relay_map, relay_url, relay_one) =
            iroh::test_utils::run_relay_server_with_access(false, access.clone())
                .await
                .expect("start first dedicated relay");
        let (second_map, second_url, relay_two) =
            iroh::test_utils::run_relay_server_with_access(false, access)
                .await
                .expect("start second dedicated relay");
        relay_map.extend(&second_map);
        assert_eq!(relay_map.len(), 2);
        let unauthenticated = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Custom(relay_map.clone()))
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build unauthenticated relay endpoint");
        assert!(
            tokio::time::timeout(Duration::from_secs(2), unauthenticated.online())
                .await
                .is_err(),
            "dedicated relays accepted a client without the configured token"
        );
        unauthenticated.close().await;
        let wrong_token = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Custom(
                relay_map.clone().with_auth_token("wrong-token"),
            ))
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build wrong-token relay endpoint");
        assert!(
            tokio::time::timeout(Duration::from_secs(2), wrong_token.online())
                .await
                .is_err(),
            "dedicated relays accepted a client with the wrong token"
        );
        wrong_token.close().await;
        let relay_map = relay_map.with_auth_token(RELAY_TOKEN);
        let client_key = SecretKey::generate();
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(client_key.clone())
            .relay_mode(RelayMode::Custom(relay_map.clone()))
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build relay-only client");
        let server = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Custom(relay_map.clone()))
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .alpns(vec![TEST_ALPN.to_vec()])
            .bind()
            .await
            .expect("build relay-only server");
        spawn_dedicated_relay_failover(server.clone(), relay_map.clone());

        let echo = tokio::spawn({
            let server = server.clone();
            async move {
                for _ in 0..2 {
                    let connection = server
                        .accept()
                        .await
                        .expect("incoming relay connection")
                        .await
                        .expect("complete relay handshake");
                    let (mut send, mut recv) = connection.accept_bi().await.expect("accept stream");
                    let bytes = recv.read_to_end(1024).await.expect("read relay payload");
                    send.write_all(&bytes).await.expect("echo relay payload");
                    send.finish().expect("finish echo");
                    connection.closed().await;
                }
            }
        });

        server.online().await;
        let mut address = server.addr();
        let active_url = address
            .relay_urls()
            .next()
            .expect("server selected a dedicated relay")
            .clone();
        address
            .addrs
            .retain(|address| !matches!(address, TransportAddr::Ip(_)));
        let connection = client
            .connect(address, TEST_ALPN)
            .await
            .expect("connect through first relay");
        let (mut send, mut recv) = connection.open_bi().await.expect("open first stream");
        send.write_all(b"relay-one")
            .await
            .expect("write first payload");
        send.finish().expect("finish first payload");
        assert_eq!(
            recv.read_to_end(1024).await.expect("read first echo"),
            b"relay-one"
        );
        connection.close(0_u32.into(), b"first relay complete");
        connection.closed().await;
        client.close().await;

        let mut addresses = server.watch_addr().stream();
        let mut relay_status = server.home_relay_status();
        let (failed_relay, surviving_relay, surviving_url) = if active_url == relay_url {
            (relay_one, relay_two, second_url)
        } else {
            (relay_two, relay_one, relay_url)
        };
        failed_relay
            .shutdown()
            .await
            .expect("stop selected dedicated relay");

        let mut address = tokio::time::timeout(Duration::from_secs(20), async {
            loop {
                let address = addresses.next().await.expect("address update");
                if address.relay_urls().any(|url| url == &surviving_url) {
                    break address;
                }
            }
        })
        .await
        .expect("publish second relay address");
        tokio::time::timeout(Duration::from_secs(20), async {
            loop {
                if relay_status
                    .get()
                    .iter()
                    .any(|status| status.url() == &surviving_url && status.is_connected())
                {
                    break;
                }
                relay_status.updated().await.expect("relay status update");
            }
        })
        .await
        .expect("connect server to surviving relay");
        address
            .addrs
            .retain(|address| !matches!(address, TransportAddr::Ip(_)));
        let replacement = Endpoint::builder(presets::Minimal)
            .secret_key(client_key)
            .relay_mode(RelayMode::Custom(relay_map.clone()))
            .ca_tls_config(CaTlsConfig::insecure_skip_verify())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("rebuild relay-only client after outage");
        spawn_dedicated_relay_failover(replacement.clone(), relay_map);
        tokio::time::timeout(Duration::from_secs(20), replacement.online())
            .await
            .expect("replacement client selected surviving relay");
        let connection = tokio::time::timeout(
            Duration::from_secs(20),
            replacement.connect(address, TEST_ALPN),
        )
        .await
        .expect("second relay connection completed")
        .expect("connect through second relay");
        let (mut send, mut recv) = connection.open_bi().await.expect("open second stream");
        send.write_all(b"relay-two")
            .await
            .expect("write second payload");
        send.finish().expect("finish second payload");
        assert_eq!(
            recv.read_to_end(1024).await.expect("read second echo"),
            b"relay-two"
        );
        connection.close(0_u32.into(), b"second relay complete");

        tokio::time::timeout(Duration::from_secs(5), echo)
            .await
            .expect("echo task completed")
            .expect("echo task");
        replacement.close().await;
        server.close().await;
        surviving_relay
            .shutdown()
            .await
            .expect("stop surviving relay");
    }

    #[tokio::test]
    async fn unsigned_artifact_requests_fail_before_connection_lookup() {
        let service = IrohTransportService::new(8);
        let node_id = Uuid::now_v7();
        let artifact_id = Uuid::now_v7();
        let Err(fetch) = service
            .fetch_artifact(Request::new(FetchArtifactRequest {
                node_id: node_id.as_bytes().to_vec(),
                artifact_id: artifact_id.as_bytes().to_vec(),
                purpose: "certificate_p12".to_owned(),
                max_bytes: 32,
                grant: None,
            }))
            .await
        else {
            panic!("unsigned artifact fetch must fail");
        };
        assert_eq!(fetch.code(), tonic::Code::PermissionDenied);
        let consume = service
            .consume_artifact(Request::new(ConsumeArtifactRequest {
                node_id: node_id.as_bytes().to_vec(),
                grant: None,
                sha256: vec![0; 32],
                size: 32,
                confirm_only: false,
            }))
            .await
            .expect_err("unsigned artifact consume must fail");
        assert_eq!(consume.code(), tonic::Code::PermissionDenied);
    }

    #[tokio::test]
    #[ignore = "requires access to the configured public Iroh relay"]
    async fn relay_and_direct_paths_converge_to_direct() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(16);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Default,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
            .await
            .expect("controller endpoint online");
        let client = Endpoint::builder(presets::N0)
            .secret_key(agent_key.clone())
            .bind()
            .await
            .expect("build client");
        let connection = tokio::time::timeout(
            Duration::from_secs(30),
            client.connect(router.endpoint().addr(), AGENT_ALPN),
        )
        .await
        .expect("connection completed")
        .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

        tokio::time::timeout(Duration::from_secs(30), async {
            loop {
                let paths = connection.paths();
                let has_relay = paths.iter().any(|path| path.is_relay());
                let direct_selected = paths.iter().any(|path| path.is_ip() && path.is_selected());
                if has_relay && direct_selected {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .expect("relay fallback and selected direct path observed");
        let metadata = service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: handshake.node_id,
            }))
            .await
            .expect("query connection")
            .into_inner();
        assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }
}
