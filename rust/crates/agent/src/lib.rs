//! Unprivileged Agent runtime primitives and fixed privd client.

#![forbid(unsafe_code)]

use std::collections::HashSet;
use std::fmt;
use std::io;
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    PrivdRequest, PrivdResponse, ReadRequest, privd_request, read_frame, write_frame,
};
use ocservia_command_journal::{AcceptOutcome, CommandRecord, CommandState, Journal};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, command_envelope,
};
use prost::Message;
use rand::Rng;
use sha2::{Digest, Sha256};
use tokio::net::UnixStream;
use uuid::Uuid;

/// Maximum concurrent read-only collection tasks per Agent.
pub const MAX_READ_CONCURRENCY: usize = 4;
/// Maximum queued mutating commands. Execution itself is strictly serial.
pub const MAX_WRITE_QUEUE: usize = 8;
/// Maximum accepted command frame.
pub const MAX_COMMAND_BYTES: usize = 1024 * 1024;
const MAX_FUTURE_SKEW_SECONDS: i64 = 300;

/// Validation inputs owned by the local Agent session.
#[derive(Clone, Debug)]
pub struct CommandContext {
    pub node_id: [u8; 16],
    pub observed_revision: u64,
    pub capabilities: HashSet<&'static str>,
    pub now_unix_seconds: i64,
    pub cancelled: bool,
}

/// Deterministic crash boundaries used by the fault-injection harness.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CrashPoint {
    AfterAccepted,
    BeforeSideEffect,
    AfterSideEffect,
    AfterResult,
}

/// Persisted result plus whether it came from a duplicate delivery or reconciliation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommandOutcome {
    pub record: CommandRecord,
    pub replayed: bool,
}

/// Refusal, storage failure, or an intentional fault boundary.
#[derive(Debug)]
pub enum CommandError {
    Rejected(&'static str),
    Journal(rusqlite::Error),
    InjectedCrash(CrashPoint),
}

impl fmt::Display for CommandError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Rejected(code) => write!(formatter, "command rejected: {code}"),
            Self::Journal(_) => formatter.write_str("command journal unavailable"),
            Self::InjectedCrash(point) => write!(formatter, "injected crash at {point:?}"),
        }
    }
}

impl std::error::Error for CommandError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Journal(error) => Some(error),
            _ => None,
        }
    }
}

impl From<rusqlite::Error> for CommandError {
    fn from(value: rusqlite::Error) -> Self {
        Self::Journal(value)
    }
}

/// Serial command executor implementing validate, persist, execute, persist, acknowledge.
#[derive(Debug)]
pub struct CommandExecutor {
    journal: Journal,
}

impl CommandExecutor {
    #[must_use]
    pub fn new(journal: Journal) -> Self {
        Self { journal }
    }

    /// Executes or replays one typed synthetic command.
    ///
    /// # Errors
    ///
    /// Rejects invalid envelopes before persistence, refuses payload conflicts, and fails closed
    /// when `SQLite` cannot durably record the next state.
    pub fn execute(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
        crash: Option<CrashPoint>,
    ) -> Result<CommandOutcome, CommandError> {
        let validated = validate_command(envelope, context)?;
        let acceptance = self.journal.accept_command(
            &validated.key,
            &validated.command_id,
            &validated.payload_hash,
            context.now_unix_seconds,
        )?;
        match acceptance {
            AcceptOutcome::PayloadConflict(_) => {
                return Err(CommandError::Rejected("idempotency_payload_conflict"));
            }
            AcceptOutcome::Replay(record) => {
                return self.reconcile_replay(record, &validated, context);
            }
            AcceptOutcome::Accepted(_) => {}
        }
        inject(crash, CrashPoint::AfterAccepted)?;
        self.journal.transition_command(
            &validated.key,
            &[CommandState::Accepted],
            CommandState::Running,
            None,
            None,
            context.now_unix_seconds,
        )?;
        inject(crash, CrashPoint::BeforeSideEffect)?;
        self.journal.execute_synthetic_effect(
            &validated.key,
            &validated.payload_hash,
            &validated.result,
            context.now_unix_seconds,
        )?;
        inject(crash, CrashPoint::AfterSideEffect)?;
        let record = self.journal.transition_command(
            &validated.key,
            &[CommandState::Running],
            CommandState::Succeeded,
            Some(&validated.result),
            None,
            context.now_unix_seconds,
        )?;
        inject(crash, CrashPoint::AfterResult)?;
        Ok(CommandOutcome {
            record,
            replayed: false,
        })
    }

    /// Explicitly retries an Unknown synthetic command only after reconciliation proved absence.
    ///
    /// # Errors
    ///
    /// Refuses non-Unknown records and any effect already observed.
    pub fn retry_unknown(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<CommandOutcome, CommandError> {
        let validated = validate_command(envelope, context)?;
        let record = self
            .journal
            .command(&validated.key)?
            .ok_or(CommandError::Rejected("command_not_accepted"))?;
        if record.payload_sha256 != validated.payload_hash {
            return Err(CommandError::Rejected("idempotency_payload_conflict"));
        }
        if record.state != CommandState::Unknown {
            return Err(CommandError::Rejected("command_not_unknown"));
        }
        if self.journal.synthetic_effect(&validated.key)?.is_some() {
            return self.reconcile_replay(record, &validated, context);
        }
        self.journal.transition_command(
            &validated.key,
            &[CommandState::Unknown],
            CommandState::Running,
            None,
            None,
            context.now_unix_seconds,
        )?;
        self.journal.execute_synthetic_effect(
            &validated.key,
            &validated.payload_hash,
            &validated.result,
            context.now_unix_seconds,
        )?;
        let record = self.journal.transition_command(
            &validated.key,
            &[CommandState::Running],
            CommandState::Succeeded,
            Some(&validated.result),
            None,
            context.now_unix_seconds,
        )?;
        Ok(CommandOutcome {
            record,
            replayed: true,
        })
    }

    #[must_use]
    pub fn journal(&self) -> &Journal {
        &self.journal
    }

    fn reconcile_replay(
        &self,
        record: CommandRecord,
        validated: &ValidatedCommand,
        context: &CommandContext,
    ) -> Result<CommandOutcome, CommandError> {
        if matches!(record.state, CommandState::Succeeded | CommandState::Failed) {
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let effect = self.journal.synthetic_effect(&validated.key)?;
        if let Some(effect) = effect {
            if effect.payload_sha256 != validated.payload_hash {
                return Err(CommandError::Rejected("idempotency_payload_conflict"));
            }
            let record = self.journal.transition_command(
                &validated.key,
                &[
                    CommandState::Accepted,
                    CommandState::Running,
                    CommandState::Unknown,
                ],
                CommandState::Succeeded,
                Some(&effect.result),
                None,
                context.now_unix_seconds,
            )?;
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let record = if record.state == CommandState::Unknown {
            record
        } else {
            self.journal.transition_command(
                &validated.key,
                &[CommandState::Accepted, CommandState::Running],
                CommandState::Unknown,
                None,
                Some("outcome_requires_reconciliation"),
                context.now_unix_seconds,
            )?
        };
        Ok(CommandOutcome {
            record,
            replayed: true,
        })
    }
}

struct ValidatedCommand {
    key: [u8; 16],
    command_id: [u8; 16],
    payload_hash: [u8; 32],
    result: Vec<u8>,
}

fn validate_command(
    envelope: &CommandEnvelope,
    context: &CommandContext,
) -> Result<ValidatedCommand, CommandError> {
    if envelope.encoded_len() == 0 || envelope.encoded_len() > MAX_COMMAND_BYTES {
        return Err(CommandError::Rejected("command_size_invalid"));
    }
    if envelope.protocol_version != "1.0" {
        return Err(CommandError::Rejected("protocol_version_unsupported"));
    }
    let node_id = fixed::<16>(&envelope.node_id, "node_id_invalid")?;
    if node_id != context.node_id {
        return Err(CommandError::Rejected("node_id_mismatch"));
    }
    if context.cancelled {
        return Err(CommandError::Rejected("command_cancelled"));
    }
    let issued_at = envelope
        .issued_at
        .as_ref()
        .ok_or(CommandError::Rejected("issued_at_missing"))?
        .seconds;
    let expires_at = envelope
        .expires_at
        .as_ref()
        .ok_or(CommandError::Rejected("expires_at_missing"))?
        .seconds;
    if expires_at <= context.now_unix_seconds {
        return Err(CommandError::Rejected("command_expired"));
    }
    if issued_at
        > context
            .now_unix_seconds
            .saturating_add(MAX_FUTURE_SKEW_SECONDS)
    {
        return Err(CommandError::Rejected("clock_skew"));
    }
    if envelope.expected_revision != context.observed_revision {
        return Err(CommandError::Rejected("revision_mismatch"));
    }
    let (capability, result, payload_bytes) = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(payload)) => (
            "synthetic.noop",
            b"noop:completed".to_vec(),
            payload.encode_to_vec(),
        ),
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            if payload.message.len() > 4096 {
                return Err(CommandError::Rejected("payload_size_invalid"));
            }
            (
                "synthetic.echo",
                payload.message.as_bytes().to_vec(),
                payload.encode_to_vec(),
            )
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    };
    if !context.capabilities.contains(capability) {
        return Err(CommandError::Rejected("capability_rejected"));
    }
    let key = fixed::<16>(&envelope.idempotency_key, "idempotency_key_invalid")?;
    let command_id = fixed::<16>(&envelope.command_id, "command_id_invalid")?;
    let mut hash = Sha256::new();
    hash.update(capability.as_bytes());
    hash.update(context.node_id);
    hash.update(envelope.expected_revision.to_be_bytes());
    hash.update(payload_bytes);
    let payload_hash: [u8; 32] = hash.finalize().into();
    Ok(ValidatedCommand {
        key,
        command_id,
        payload_hash,
        result,
    })
}

fn fixed<const N: usize>(value: &[u8], code: &'static str) -> Result<[u8; N], CommandError> {
    value.try_into().map_err(|_| CommandError::Rejected(code))
}

fn inject(selected: Option<CrashPoint>, point: CrashPoint) -> Result<(), CommandError> {
    if selected == Some(point) {
        Err(CommandError::InjectedCrash(point))
    } else {
        Ok(())
    }
}

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
            cap: Duration::from_secs(30),
        }
    }
}

impl Backoff {
    /// Creates a bounded Full Jitter policy.
    ///
    /// # Errors
    ///
    /// Rejects zero or inverted bounds.
    pub fn new(base: Duration, cap: Duration) -> Result<Self, io::Error> {
        if base.is_zero() || cap < base {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "backoff bounds invalid",
            ));
        }
        Ok(Self { base, cap })
    }

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
#[derive(Clone, Debug)]
pub struct PrivdClient {
    socket: std::path::PathBuf,
    timeout: Duration,
}

impl PrivdClient {
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
        Ok(Self { socket, timeout })
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
        let deadline = SystemTime::now()
            .checked_add(self.timeout)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?
            .duration_since(UNIX_EPOCH)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "system clock unavailable"))?;
        let deadline_unix_ms = u64::try_from(deadline.as_millis())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "privd deadline overflow"))?;
        let request = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms,
            operation: Some(operation),
        };
        let mut stream = tokio::time::timeout(self.timeout, UnixStream::connect(&self.socket))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd connect timed out"))??;
        tokio::time::timeout(self.timeout, write_frame(&mut stream, &request))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd request timed out"))??;
        let response: PrivdResponse = tokio::time::timeout(self.timeout, read_frame(&mut stream))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd response timed out"))??;
        if response.request_id != request.request_id {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "privd response correlation invalid",
            ));
        }
        Ok(response)
    }

    /// Reads all five I06 observations with at most four active tasks.
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
        let mut responses = Vec::with_capacity(5);
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

/// Reads a bounded, fixed host identity file.
///
/// # Errors
///
/// Rejects missing, oversized, empty, or control-character-containing content.
async fn read_host_value(path: &Path, max_bytes: usize) -> Result<String, io::Error> {
    let bytes = tokio::fs::read(path).await?;
    if bytes.is_empty() || bytes.len() > max_bytes {
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
    read_host_value(Path::new("/proc/sys/kernel/random/boot_id"), 64).await
}

#[cfg(test)]
mod tests {
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        SyntheticEcho, command_envelope,
    };
    use prost_types::Timestamp;
    use rand::{SeedableRng, rngs::StdRng};

    use super::*;

    #[test]
    fn root_is_refused() {
        assert_eq!(
            ensure_unprivileged(0).expect_err("root refused").kind(),
            io::ErrorKind::PermissionDenied
        );
        ensure_unprivileged(65532).expect("service UID allowed");
    }

    #[test]
    fn full_jitter_is_bounded_and_not_constant() {
        let policy = Backoff::default();
        let mut rng = StdRng::seed_from_u64(7);
        let values = (0..64)
            .map(|_| policy.delay(20, &mut rng))
            .collect::<Vec<_>>();
        assert!(values.iter().all(|delay| *delay <= Duration::from_secs(30)));
        assert!(values.windows(2).any(|pair| pair[0] != pair[1]));
    }

    fn command(node_id: [u8; 16], key: [u8; 16], message: &str, now: i64) -> CommandEnvelope {
        CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: key.to_vec(),
            node_id: node_id.to_vec(),
            sequence: 1,
            issued_at: Some(Timestamp {
                seconds: now,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: now + 60,
                nanos: 0,
            }),
            expected_revision: 1,
            traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01".to_owned(),
            actor_id: "test".to_owned(),
            reason: "fault matrix".to_owned(),
            payload: Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: message.to_owned(),
            })),
        }
    }

    fn context(node_id: [u8; 16], now: i64) -> CommandContext {
        CommandContext {
            node_id,
            observed_revision: 1,
            capabilities: HashSet::from(["synthetic.noop", "synthetic.echo"]),
            now_unix_seconds: now,
            cancelled: false,
        }
    }

    fn temporary_journal(label: &str) -> std::path::PathBuf {
        std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("ocservia-agent-{label}-{}.db", Uuid::now_v7()))
    }

    fn cleanup_journal(path: &Path) {
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }

    #[test]
    fn duplicate_delivery_ack_loss_and_restart_execute_once() {
        let path = temporary_journal("duplicates");
        let node_id = *Uuid::now_v7().as_bytes();
        let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
        let command_context = context(node_id, 100);
        {
            let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
            let first = executor
                .execute(&envelope, &command_context, None)
                .expect("first");
            assert!(!first.replayed);
        }
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("restart"));
        for _ in 0..100 {
            let replay = executor
                .execute(&envelope, &command_context, None)
                .expect("ack-loss replay");
            assert!(replay.replayed);
            assert_eq!(replay.record.state, CommandState::Succeeded);
            assert_eq!(replay.record.result.as_deref(), Some(b"once".as_slice()));
        }
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("count"),
            1
        );
        drop(executor);
        cleanup_journal(&path);
    }

    #[test]
    fn every_crash_boundary_reconciles_before_safe_retry() {
        for crash in [
            CrashPoint::AfterAccepted,
            CrashPoint::BeforeSideEffect,
            CrashPoint::AfterSideEffect,
            CrashPoint::AfterResult,
        ] {
            let path = temporary_journal("crash");
            let node_id = *Uuid::now_v7().as_bytes();
            let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
            let command_context = context(node_id, 100);
            let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
            assert!(matches!(
                executor.execute(&envelope, &command_context, Some(crash)),
                Err(CommandError::InjectedCrash(point)) if point == crash
            ));
            drop(executor);

            let mut executor = CommandExecutor::new(Journal::open(&path).expect("restart"));
            let reconciled = executor
                .execute(&envelope, &command_context, None)
                .expect("reconcile");
            let completed = if reconciled.record.state == CommandState::Unknown {
                executor
                    .retry_unknown(&envelope, &command_context)
                    .expect("explicit safe retry")
            } else {
                reconciled
            };
            assert_eq!(completed.record.state, CommandState::Succeeded);
            assert_eq!(
                executor
                    .journal()
                    .synthetic_execution_count()
                    .expect("count"),
                1
            );
            drop(executor);
            cleanup_journal(&path);
        }
    }

    #[test]
    fn command_validation_fails_before_side_effect() {
        let path = temporary_journal("validation");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let base = command(node_id, key, "valid", 100);
        let base_context = context(node_id, 100);
        let cases = [
            ("node", {
                let mut value = base.clone();
                value.node_id = Uuid::now_v7().as_bytes().to_vec();
                value
            }),
            ("expired", {
                let mut value = base.clone();
                value.expires_at = Some(Timestamp {
                    seconds: 100,
                    nanos: 0,
                });
                value
            }),
            ("clock", {
                let mut value = base.clone();
                value.issued_at = Some(Timestamp {
                    seconds: 401,
                    nanos: 0,
                });
                value
            }),
            ("revision", {
                let mut value = base.clone();
                value.expected_revision = 2;
                value
            }),
        ];
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        for (label, value) in cases {
            assert!(
                matches!(
                    executor.execute(&value, &base_context, None),
                    Err(CommandError::Rejected(_))
                ),
                "{label}"
            );
        }
        let mut unsupported = base.clone();
        unsupported.payload = None;
        assert!(matches!(
            executor.execute(&unsupported, &base_context, None),
            Err(CommandError::Rejected("capability_rejected"))
        ));
        let mut no_capability = base_context.clone();
        no_capability.capabilities.clear();
        assert!(matches!(
            executor.execute(&base, &no_capability, None),
            Err(CommandError::Rejected("capability_rejected"))
        ));
        let mut oversized = base.clone();
        oversized.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: "x".repeat(4097),
        }));
        assert!(matches!(
            executor.execute(&oversized, &base_context, None),
            Err(CommandError::Rejected("payload_size_invalid"))
        ));
        let mut cancelled = base_context.clone();
        cancelled.cancelled = true;
        assert!(matches!(
            executor.execute(&base, &cancelled, None),
            Err(CommandError::Rejected("command_cancelled"))
        ));
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("count"),
            0
        );
        drop(executor);
        cleanup_journal(&path);
    }

    #[test]
    fn same_key_different_payload_is_rejected_without_second_effect() {
        let path = temporary_journal("conflict");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        executor
            .execute(&command(node_id, key, "first", 100), &command_context, None)
            .expect("first");
        assert!(matches!(
            executor.execute(
                &command(node_id, key, "second", 100),
                &command_context,
                None
            ),
            Err(CommandError::Rejected("idempotency_payload_conflict"))
        ));
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("count"),
            1
        );
        drop(executor);
        cleanup_journal(&path);
    }
}
