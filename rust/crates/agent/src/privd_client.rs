use std::io;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    PrivdRequest, PrivdResponse, PrivilegedRequestMode, ReadRequest, privd_request, privd_response,
    read_frame, write_frame,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgradeResultReport, CommandEnvelope, SessionGrantV1, command_envelope,
};
use rand::{Rng, RngExt};
use tokio::net::UnixStream;
use uuid::Uuid;

use crate::MAX_READ_CONCURRENCY;

/// Full Jitter reconnect policy.
#[derive(Clone, Copy, Debug)]
pub struct Backoff {
    base: Duration,
    cap: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            base: Duration::from_millis(250),
            // Leave half of the 30-second connection-owner takeover budget
            // for endpoint recovery, handshake, and fence persistence.
            cap: Duration::from_secs(15),
        }
    }
}

impl Backoff {
    /// Returns a uniformly jittered delay between zero and the exponential cap.
    #[must_use]
    pub fn delay(&self, attempt: u32, rng: &mut impl Rng) -> Duration {
        let exponent = attempt.min(31);
        let multiplier = 1_u32.checked_shl(exponent).unwrap_or(u32::MAX);
        let ceiling = self.base.saturating_mul(multiplier).min(self.cap);
        let max_millis = u64::try_from(ceiling.as_millis()).unwrap_or(u64::MAX);
        Duration::from_millis(rng.random_range(0..=max_millis))
    }
}

/// Fixed privd client over `AF_UNIX`.
/// Maximum command lifetime budget for `CompleteConfigApply` (up to 60s apply/rollback).
const CONFIG_APPLY_PRIVD_TIMEOUT: Duration = Duration::from_secs(75);

#[derive(Clone, Debug)]
pub struct PrivdClient {
    socket: std::path::PathBuf,
    pub(crate) timeout: Duration,
    pub(crate) desired_timeout: Duration,
}

impl PrivdClient {
    pub(crate) fn command_timeout(&self, command: &CommandEnvelope) -> Duration {
        if matches!(
            command.payload,
            Some(command_envelope::Payload::CompleteConfigApply(_))
        ) {
            self.desired_timeout.max(CONFIG_APPLY_PRIVD_TIMEOUT)
        } else {
            self.desired_timeout
        }
    }
    /// Creates a client with a bounded request deadline.
    ///
    /// # Errors
    ///
    /// Rejects zero or greater-than-30-second timeouts.
    pub fn new(socket: std::path::PathBuf, timeout: Duration) -> Result<Self, io::Error> {
        if timeout.is_zero() || timeout > Duration::from_secs(30) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "privd timeout invalid",
            ));
        }
        let desired_timeout = timeout.saturating_mul(4).min(Duration::from_secs(30));
        Ok(Self {
            socket,
            timeout,
            desired_timeout,
        })
    }

    /// Calls exactly one fixed read-only operation.
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error; structured privd failures remain in the response.
    pub async fn call(
        &self,
        operation: privd_request::Operation,
    ) -> Result<PrivdResponse, io::Error> {
        self.call_inner(
            Some(operation),
            None,
            PrivilegedRequestMode::Unspecified,
            None,
            self.timeout,
            None,
        )
        .await
    }

    /// Calls a fixed operation carrying the active Controller-signed session grant
    /// (used for artifact reads and consumption).
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error; structured privd failures remain in the response.
    pub async fn call_with_session(
        &self,
        operation: privd_request::Operation,
        session_grant: &SessionGrantV1,
    ) -> Result<PrivdResponse, io::Error> {
        self.call_inner(
            Some(operation),
            None,
            PrivilegedRequestMode::Unspecified,
            Some(session_grant),
            self.timeout,
            None,
        )
        .await
    }

    /// Sends one original Controller-signed command and active session grant for
    /// independent privd verification and execution.
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error.
    pub async fn call_command(
        &self,
        command: &CommandEnvelope,
        accepted_at: i64,
        session_grant: &SessionGrantV1,
    ) -> Result<PrivdResponse, io::Error> {
        self.call_inner(
            None,
            Some(command),
            PrivilegedRequestMode::Execute,
            Some(session_grant),
            self.command_timeout(command),
            Some(prost_types::Timestamp {
                seconds: accepted_at,
                nanos: 0,
            }),
        )
        .await
    }

    /// Sends a Controller-signed reconciliation command. Privd derives the
    /// exact effect identity from the signed typed payload.
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error.
    pub async fn call_reconcile(
        &self,
        command: &CommandEnvelope,
    ) -> Result<PrivdResponse, io::Error> {
        self.call_inner(
            None,
            Some(command),
            PrivilegedRequestMode::Reconcile,
            None,
            self.command_timeout(command),
            None,
        )
        .await
    }

    async fn call_inner(
        &self,
        operation: Option<privd_request::Operation>,
        command: Option<&CommandEnvelope>,
        mode: PrivilegedRequestMode,
        session_grant: Option<&SessionGrantV1>,
        timeout: Duration,
        accepted_at: Option<prost_types::Timestamp>,
    ) -> Result<PrivdResponse, io::Error> {
        let deadline = SystemTime::now()
            .checked_add(timeout)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?
            .duration_since(UNIX_EPOCH)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "system clock unavailable"))?;
        let deadline_unix_ms = u64::try_from(deadline.as_millis())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?;
        let request = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms,
            accepted_at,
            authorization_command: command.cloned(),
            privileged_mode: mode.into(),
            session_grant: session_grant.cloned().map(Box::new),
            operation,
        };
        let transport_deadline = tokio::time::Instant::now() + timeout;
        let mut stream =
            tokio::time::timeout_at(transport_deadline, UnixStream::connect(&self.socket))
                .await
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::TimedOut, "privd connect timed out")
                })??;
        tokio::time::timeout_at(transport_deadline, write_frame(&mut stream, &request))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd request timed out"))??;
        let response: PrivdResponse =
            tokio::time::timeout_at(transport_deadline, read_frame(&mut stream))
                .await
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::TimedOut, "privd response timed out")
                })??;
        if response.request_id != request.request_id {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "privd response correlation invalid",
            ));
        }
        Ok(response)
    }

    /// Reads the bounded most-recent durable agent upgrade outcomes through
    /// the fixed read-only privd query. Telemetry treats this as best-effort
    /// evidence: a privd build without the read reports an error the caller
    /// maps to an empty list instead of failing the heartbeat.
    ///
    /// # Errors
    ///
    /// Returns the transport or refusal error.
    pub async fn upgrade_results(&self) -> Result<Vec<AgentUpgradeResultReport>, io::Error> {
        let response = self
            .call(privd_request::Operation::UpgradeResultList(ReadRequest {}))
            .await?;
        match response.result {
            Some(privd_response::Result::UpgradeResultList(list)) => Ok(list.results),
            Some(privd_response::Result::Error(failure)) => Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                format!("privd upgrade result list refused: {}", failure.detail),
            )),
            _ => Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "privd upgrade result list response invalid",
            )),
        }
    }

    /// Reads all seven routine observations with at most four active tasks.
    ///
    /// # Errors
    ///
    /// Returns the first transport error.
    pub async fn snapshot(&self) -> Result<Vec<PrivdResponse>, io::Error> {
        let operations = [
            privd_request::Operation::ServiceStatus(ReadRequest {}),
            privd_request::Operation::OcservVersion(ReadRequest {}),
            privd_request::Operation::SessionList(ReadRequest {}),
            privd_request::Operation::IpBanList(ReadRequest {}),
            privd_request::Operation::ConfigFingerprint(ReadRequest {}),
            privd_request::Operation::UserList(ReadRequest {}),
            privd_request::Operation::GroupList(ReadRequest {}),
        ];
        let permits = std::sync::Arc::new(tokio::sync::Semaphore::new(MAX_READ_CONCURRENCY));
        let mut tasks = tokio::task::JoinSet::new();
        for operation in operations {
            let permit = std::sync::Arc::clone(&permits)
                .acquire_owned()
                .await
                .map_err(|_| io::Error::other("read supervisor closed"))?;
            let client = self.clone();
            tasks.spawn(async move {
                let _permit = permit;
                client.call(operation).await
            });
        }
        let mut responses = Vec::with_capacity(7);
        while let Some(result) = tasks.join_next().await {
            responses.push(result.map_err(|_| io::Error::other("read task failed"))??);
        }
        Ok(responses)
    }
}

/// Rejects root execution before any network or local RPC is opened.
///
/// # Errors
///
/// Returns permission denied for effective UID zero.
pub fn ensure_unprivileged(effective_uid: u32) -> Result<(), io::Error> {
    if effective_uid == 0 {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "ocservia-agent must not run as root",
        ));
    }
    Ok(())
}

/// Reads the bounded `ID` field from the fixed os-release file.
///
/// # Errors
///
/// Rejects missing, oversized, malformed, or unsupported content.
pub async fn read_os_release() -> Result<String, io::Error> {
    let bytes = tokio::fs::read("/etc/os-release").await?;
    if bytes.is_empty() || bytes.len() > 8192 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "os-release size invalid",
        ));
    }
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "os-release invalid"))?;
    let value = text
        .lines()
        .find_map(|line| line.strip_prefix("ID="))
        .map(|value| value.trim_matches('"'))
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "os-release ID missing"))?;
    if value.is_empty()
        || value.len() > 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "os-release ID invalid",
        ));
    }
    Ok(value.to_owned())
}

/// Reads the fixed Linux boot identifier with a strict bound.
///
/// # Errors
///
/// Rejects missing, oversized, malformed, or empty content.
pub async fn read_boot_id() -> Result<String, io::Error> {
    let bytes = tokio::fs::read("/proc/sys/kernel/random/boot_id").await?;
    if bytes.is_empty() || bytes.len() > 64 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "host identity value size invalid",
        ));
    }
    let value = String::from_utf8(bytes)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "host identity value invalid"))?;
    let value = value.trim();
    if value.is_empty() || value.chars().any(char::is_control) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "host identity value invalid",
        ));
    }
    Ok(value.to_owned())
}
