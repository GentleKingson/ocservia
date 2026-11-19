use std::collections::{HashMap, VecDeque};
use std::time::{Duration, Instant};

use super::{
    AGENT_ALPN, AcceptError, AfterHandshakeOutcome, Arc, Connection, ENROLL_ALPN, EndpointHooks,
    EndpointId, IdentityPolicy, MAX_AGENT_CONNECTIONS, MAX_ATTEMPTS_PER_MINUTE,
    MAX_ENROLLMENT_CONNECTIONS, Mutex, TrustAuthority, TrustBudgetSet, TrustClass, VarInt,
    protocol_error,
};

#[derive(Debug)]
pub(super) struct SecurityHook {
    policy: IdentityPolicy,
    agent_attempts: Mutex<HashMap<EndpointId, VecDeque<Instant>>>,
    enrollment_attempts: Mutex<HashMap<EndpointId, VecDeque<Instant>>>,
    budgets: Arc<TrustBudgetSet>,
}

impl SecurityHook {
    pub(super) fn new(policy: IdentityPolicy, trust: &TrustAuthority) -> Self {
        Self {
            policy,
            agent_attempts: Mutex::new(HashMap::new()),
            enrollment_attempts: Mutex::new(HashMap::new()),
            budgets: Arc::clone(&trust.budgets),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum AttemptRejection {
    Rate,
    Capacity,
}

pub(super) fn trust_rejection_error(rejection: AttemptRejection) -> AcceptError {
    match rejection {
        AttemptRejection::Rate => protocol_error("trust authority rate exceeded"),
        AttemptRejection::Capacity => protocol_error("trust authority capacity reached"),
    }
}

pub(super) fn record_attempt(
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

pub(super) fn record_global_attempt(
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
