use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex, RwLock};
use std::time::Instant;

use iroh::EndpointId;
use iroh::protocol::AcceptError;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    EnrollRequest, EnrollResponse, SessionHandshake, SessionHandshakeResponse,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    AuthorizeSessionRequest, CheckEndpointRequest, ListNodeTrustRequest, NodeTrustState,
    ValidateEnrollmentRequest, trust_service_client::TrustServiceClient,
};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tonic::Response;

use crate::admission::{AttemptRejection, record_global_attempt, trust_rejection_error};
use crate::monitors::protocol_error;
use crate::{AGENT_ALPN, ENROLL_ALPN, HANDSHAKE_TIMEOUT, MAX_TRUST_SNAPSHOT_BINDINGS};

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
    attempts: StdMutex<VecDeque<Instant>>,
    checks: Arc<Semaphore>,
    accepted: AtomicU64,
    rate_rejected: AtomicU64,
    capacity_rejected: AtomicU64,
    in_flight: AtomicU64,
    limit: TrustClassLimit,
}

#[derive(Debug)]
pub(crate) struct TrustBudgetSet {
    classes: [TrustClassBudget; TrustClass::COUNT],
}

impl TrustBudgetSet {
    pub(crate) fn new(config: &TrustBudgetConfig) -> Self {
        Self {
            classes: std::array::from_fn(|index| {
                let limit = config.limits[index];
                TrustClassBudget {
                    attempts: StdMutex::new(VecDeque::with_capacity(limit.attempts_per_minute)),
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

    pub(crate) async fn acquire(
        self: &Arc<Self>,
        class: TrustClass,
    ) -> Result<TrustBudgetPermit, AttemptRejection> {
        self.acquire_at(class, Instant::now()).await
    }

    pub(crate) fn acquire_at(
        self: &Arc<Self>,
        class: TrustClass,
        now: Instant,
    ) -> std::future::Ready<Result<TrustBudgetPermit, AttemptRejection>> {
        let budget = &self.classes[class.index()];
        let Ok(permit) = budget.checks.clone().try_acquire_owned() else {
            budget.capacity_rejected.fetch_add(1, Ordering::Relaxed);
            return std::future::ready(Err(AttemptRejection::Capacity));
        };
        let mut attempts = budget
            .attempts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Err(rejection) =
            record_global_attempt(&mut attempts, now, budget.limit.attempts_per_minute)
        {
            budget.rate_rejected.fetch_add(1, Ordering::Relaxed);
            return std::future::ready(Err(rejection));
        }
        drop(attempts);
        budget.accepted.fetch_add(1, Ordering::Relaxed);
        budget.in_flight.fetch_add(1, Ordering::Relaxed);
        std::future::ready(Ok(TrustBudgetPermit {
            budgets: Arc::clone(self),
            class,
            _permit: permit,
        }))
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

    pub(crate) fn snapshot(&self, class: TrustClass) -> TrustMetricsSnapshot {
        let budget = &self.classes[class.index()];
        TrustMetricsSnapshot {
            accepted: budget.accepted.load(Ordering::Relaxed),
            rate_rejected: budget.rate_rejected.load(Ordering::Relaxed),
            capacity_rejected: budget.capacity_rejected.load(Ordering::Relaxed),
            in_flight: budget.in_flight.load(Ordering::Relaxed),
        }
    }
}

pub(crate) struct TrustBudgetPermit {
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
    approved_by_node: HashMap<Vec<u8>, EndpointId>,
    revoked: HashMap<EndpointId, Vec<u8>>,
    revisions: HashMap<EndpointId, u64>,
    synchronized: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PolicyUpdateDisposition {
    Applied,
    Stale,
    Rejected,
}

impl IdentityPolicy {
    /// Builds a policy from endpoint-to-node bindings and revoked identifiers.
    #[must_use]
    pub fn new(approved: HashMap<EndpointId, Vec<u8>>, revoked: HashSet<EndpointId>) -> Self {
        let approved_by_node = approved
            .iter()
            .map(|(endpoint, node_id)| (node_id.clone(), *endpoint))
            .collect();
        let revisions = revoked
            .iter()
            .copied()
            .map(|endpoint| (endpoint, u64::MAX))
            .collect();
        Self {
            state: Arc::new(RwLock::new(IdentityState {
                approved,
                approved_by_node,
                revoked: revoked
                    .into_iter()
                    .map(|endpoint| (endpoint, Vec::new()))
                    .collect(),
                revisions,
                synchronized: true,
            })),
        }
    }

    pub(crate) fn replace_snapshot(
        &self,
        bindings: impl IntoIterator<Item = (EndpointId, Vec<u8>, NodeTrustState, u64)>,
    ) -> Result<(), &'static str> {
        let mut approved = HashMap::new();
        let mut approved_by_node = HashMap::new();
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
                    if approved_by_node.insert(node_id.clone(), endpoint).is_some() {
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
            approved_by_node,
            revoked,
            revisions,
            synchronized: true,
        };
        Ok(())
    }

    pub(crate) fn permits(&self, endpoint: EndpointId, alpn: &[u8]) -> bool {
        let state = self
            .state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if !state.synchronized || state.revoked.contains_key(&endpoint) {
            return false;
        }
        alpn == ENROLL_ALPN || (alpn == AGENT_ALPN && state.approved.contains_key(&endpoint))
    }

    pub(crate) fn revoked(&self, endpoint: EndpointId) -> bool {
        self.state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .revoked
            .contains_key(&endpoint)
    }

    #[cfg(test)]
    pub(crate) fn revision(&self, endpoint: EndpointId) -> u64 {
        self.state
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .revisions
            .get(&endpoint)
            .copied()
            .unwrap_or(0)
    }

    #[cfg(test)]
    pub(crate) fn retained_state(&self, endpoint: EndpointId) -> NodeTrustState {
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

    #[cfg(test)]
    pub(crate) fn update(
        &self,
        endpoint: EndpointId,
        node_id: Vec<u8>,
        state: NodeTrustState,
        revision: u64,
    ) -> PolicyUpdateDisposition {
        self.update_with_status(endpoint, node_id, state, revision)
            .0
    }

    /// Atomically applies one trust-policy update and returns
    /// `(disposition, previous_revision, retained_revision, retained_state)`
    /// under a single write lock.
    pub(crate) fn update_with_status(
        &self,
        endpoint: EndpointId,
        node_id: Vec<u8>,
        state: NodeTrustState,
        revision: u64,
    ) -> (PolicyUpdateDisposition, u64, u64, NodeTrustState) {
        let mut identities = self
            .state
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let previous_revision = identities.revisions.get(&endpoint).copied().unwrap_or(0);
        let disposition =
            apply_identity_update(&mut identities, endpoint, node_id, state, revision);
        let retained_revision = identities.revisions.get(&endpoint).copied().unwrap_or(0);
        let retained_state = if identities.revoked.contains_key(&endpoint) {
            NodeTrustState::Revoked
        } else if identities.approved.contains_key(&endpoint) {
            NodeTrustState::Active
        } else {
            NodeTrustState::Unspecified
        };
        (
            disposition,
            previous_revision,
            retained_revision,
            retained_state,
        )
    }
}

fn apply_identity_update(
    identities: &mut IdentityState,
    endpoint: EndpointId,
    node_id: Vec<u8>,
    state: NodeTrustState,
    revision: u64,
) -> PolicyUpdateDisposition {
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
                .get(&endpoint)
                .is_some_and(|bound_node| bound_node != &node_id)
            {
                return PolicyUpdateDisposition::Rejected;
            }
            if identities
                .approved_by_node
                .get(&node_id)
                .is_some_and(|bound_endpoint| *bound_endpoint != endpoint)
            {
                return PolicyUpdateDisposition::Rejected;
            }
            identities
                .approved_by_node
                .insert(node_id.clone(), endpoint);
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
            if let Some(removed_node) = identities.approved.remove(&endpoint) {
                identities.approved_by_node.remove(&removed_node);
            }
            identities.revoked.insert(endpoint, node_id);
        }
        NodeTrustState::Unspecified => return PolicyUpdateDisposition::Rejected,
    }
    identities.revisions.insert(endpoint, revision);
    PolicyUpdateDisposition::Applied
}

#[derive(Clone, Debug)]
pub struct TrustAuthority {
    client: TrustServiceClient<tonic::transport::Channel>,
    pub(crate) budgets: Arc<TrustBudgetSet>,
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

    pub(crate) async fn load_snapshot(&self, policy: &IdentityPolicy) -> Result<(), AcceptError> {
        let mut client = self.client.clone();
        let response = tokio::time::timeout(
            HANDSHAKE_TIMEOUT,
            client.list_node_trust(ListNodeTrustRequest {}),
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

    pub(crate) async fn recheck_agent(
        &self,
        endpoint: EndpointId,
    ) -> Result<bool, AttemptRejection> {
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

    pub(crate) async fn enroll(
        &self,
        request: EnrollRequest,
    ) -> Result<EnrollResponse, AcceptError> {
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

    pub(crate) async fn authorize(
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
