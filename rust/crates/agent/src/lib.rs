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
    CommandDeliveryMode, CommandEnvelope, SemanticPayloadHashVersion, command_envelope,
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
    /// Locally authoritative revision when one exists. Production node commands
    /// rely on the Controller's transactional revision check and bind that
    /// revision into the semantic hash instead of inventing a local value.
    pub observed_revision: Option<u64>,
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

/// Durable authorization to execute one external typed effect.
#[derive(Clone, Debug)]
pub struct ExternalCommand {
    key: [u8; 16],
    command_id: [u8; 16],
}

/// Result of preparing an external effect.
#[derive(Clone, Debug)]
pub enum ExternalPreparation {
    Execute(ExternalCommand),
    Replay(CommandOutcome),
}

/// Refusal, storage failure, or an intentional fault boundary.
#[derive(Debug)]
pub enum CommandError {
    Rejected(&'static str),
    IdentityConflict,
    PayloadConflict,
    PreEffectJournalFailure(Box<rusqlite::Error>),
    OutcomeUnknown {
        code: &'static str,
        record: Box<CommandRecord>,
        source: Box<rusqlite::Error>,
    },
    InjectedCrash(CrashPoint),
}

impl fmt::Display for CommandError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Rejected(code) => write!(formatter, "command rejected: {code}"),
            Self::IdentityConflict => formatter.write_str("command identity conflict"),
            Self::PayloadConflict => formatter.write_str("command payload conflict"),
            Self::PreEffectJournalFailure(_) => {
                formatter.write_str("command journal unavailable before effect")
            }
            Self::OutcomeUnknown { code, .. } => {
                write!(formatter, "command outcome unknown: {code}")
            }
            Self::InjectedCrash(point) => write!(formatter, "injected crash at {point:?}"),
        }
    }
}

impl std::error::Error for CommandError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::PreEffectJournalFailure(error) => Some(error.as_ref()),
            Self::OutcomeUnknown { source, .. } => Some(source.as_ref()),
            _ => None,
        }
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
        let acceptance = self
            .journal
            .accept_command(
                &validated.key,
                &validated.command_id,
                &validated.payload_hash,
                validated.hash_version as i32,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        match acceptance {
            AcceptOutcome::PayloadConflict(_) => {
                return Err(CommandError::PayloadConflict);
            }
            AcceptOutcome::IdentityConflict(_) => {
                return Err(CommandError::IdentityConflict);
            }
            AcceptOutcome::Replay(record) => {
                return self.reconcile_replay(record, &validated, context, false);
            }
            AcceptOutcome::Accepted(_) => {}
        }
        inject(crash, CrashPoint::AfterAccepted)?;
        self.journal
            .transition_command(
                &validated.key,
                &[CommandState::Accepted],
                CommandState::Running,
                None,
                None,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        inject(crash, CrashPoint::BeforeSideEffect)?;
        let record = self
            .journal
            .execute_and_complete_synthetic(
                &validated.key,
                &validated.command_id,
                &validated.payload_hash,
                &validated.result,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        inject(crash, CrashPoint::AfterSideEffect)?;
        inject(crash, CrashPoint::AfterResult)?;
        Ok(CommandOutcome {
            record,
            replayed: false,
        })
    }

    /// Executes the delivery intent carried by a production command frame.
    ///
    /// # Errors
    ///
    /// Rejects an unspecified mode and propagates execution or reconciliation failures.
    pub fn deliver(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<CommandOutcome, CommandError> {
        match CommandDeliveryMode::try_from(envelope.delivery_mode)
            .unwrap_or(CommandDeliveryMode::Unspecified)
        {
            CommandDeliveryMode::ExecuteOrReplay => self.execute(envelope, context, None),
            CommandDeliveryMode::ReconcileOnly => self.reconcile(envelope, context),
            CommandDeliveryMode::RetryIfEffectAbsent => self.retry_unknown(envelope, context),
            CommandDeliveryMode::Unspecified => {
                Err(CommandError::Rejected("delivery_mode_invalid"))
            }
        }
    }

    /// Validates and durably records an external command before its privd call.
    ///
    /// # Errors
    ///
    /// Rejects invalid or conflicting commands and fails closed on journal errors.
    pub fn prepare_external(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<ExternalPreparation, CommandError> {
        let validated = validate_command(envelope, context)?;
        if !validated.external {
            return Err(CommandError::Rejected("external_command_required"));
        }
        let acceptance = self
            .journal
            .accept_command(
                &validated.key,
                &validated.command_id,
                &validated.payload_hash,
                validated.hash_version as i32,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        match acceptance {
            AcceptOutcome::PayloadConflict(_) => Err(CommandError::PayloadConflict),
            AcceptOutcome::IdentityConflict(_) => Err(CommandError::IdentityConflict),
            AcceptOutcome::Replay(record) => {
                let terminal_or_reconciling =
                    matches!(record.state, CommandState::Succeeded | CommandState::Failed)
                        || (record.state == CommandState::Unknown
                            && record.error_code.as_deref()
                                == Some("outcome_requires_reconciliation"));
                let record = if terminal_or_reconciling {
                    record
                } else {
                    self.journal
                        .transition_command(
                            &validated.key,
                            &[
                                CommandState::Accepted,
                                CommandState::Running,
                                CommandState::Unknown,
                            ],
                            CommandState::Unknown,
                            None,
                            Some("outcome_requires_reconciliation"),
                            context.now_unix_seconds,
                        )
                        .map_err(pre_effect_failure)?
                };
                Ok(ExternalPreparation::Replay(CommandOutcome {
                    record,
                    replayed: true,
                }))
            }
            AcceptOutcome::Accepted(_) => {
                self.journal
                    .transition_command(
                        &validated.key,
                        &[CommandState::Accepted],
                        CommandState::Running,
                        None,
                        None,
                        context.now_unix_seconds,
                    )
                    .map_err(pre_effect_failure)?;
                Ok(ExternalPreparation::Execute(ExternalCommand {
                    key: validated.key,
                    command_id: validated.command_id,
                }))
            }
        }
    }

    /// Persists the bounded result of an external privd call.
    ///
    /// # Errors
    ///
    /// Fails closed on missing identity, identity conflict, or journal failure.
    pub fn complete_external(
        &self,
        command: &ExternalCommand,
        result: Result<&[u8], &'static str>,
        now: i64,
    ) -> Result<CommandOutcome, CommandError> {
        let (state, bytes, code) = match result {
            Ok(bytes) if bytes.len() <= MAX_COMMAND_BYTES => {
                (CommandState::Succeeded, Some(bytes), None)
            }
            Ok(_) => (CommandState::Failed, None, Some("result_too_large")),
            Err(code) => (CommandState::Failed, None, Some(code)),
        };
        let before = self
            .journal
            .command(&command.key)
            .map_err(pre_effect_failure)?
            .ok_or(CommandError::Rejected("command_not_accepted"))?;
        let record = self
            .journal
            .transition_command(
                &command.key,
                &[CommandState::Running],
                state,
                bytes,
                code,
                now,
            )
            .map_err(|source| CommandError::OutcomeUnknown {
                code: "result_persistence_failed",
                record: Box::new(before),
                source: Box::new(source),
            })?;
        if record.command_id != command.command_id {
            return Err(CommandError::IdentityConflict);
        }
        Ok(CommandOutcome {
            record,
            replayed: false,
        })
    }

    /// Persists an uncertain external outcome without permitting an automatic retry.
    ///
    /// # Errors
    ///
    /// Fails closed on identity conflict or journal failure.
    pub fn mark_external_unknown(
        &self,
        command: &ExternalCommand,
        code: &'static str,
        now: i64,
    ) -> Result<CommandOutcome, CommandError> {
        let record = self
            .journal
            .transition_command(
                &command.key,
                &[CommandState::Running],
                CommandState::Unknown,
                None,
                Some(code),
                now,
            )
            .map_err(pre_effect_failure)?;
        if record.command_id != command.command_id {
            return Err(CommandError::IdentityConflict);
        }
        Ok(CommandOutcome {
            record,
            replayed: false,
        })
    }

    /// Resolves an external Unknown from an independent typed observation.
    ///
    /// # Errors
    ///
    /// Rejects invalid or mismatched commands and propagates journal failures.
    pub fn reconcile_external(
        &self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
        applied: Option<bool>,
    ) -> Result<CommandOutcome, CommandError> {
        let validated = validate_command(envelope, context)?;
        if !validated.external {
            return Err(CommandError::Rejected("external_command_required"));
        }
        let record = self.matching_record(&validated)?;
        if matches!(record.state, CommandState::Succeeded | CommandState::Failed) {
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let (state, result, code) = match applied {
            Some(true) => (CommandState::Succeeded, Some(b"observed".as_slice()), None),
            Some(false) => (CommandState::Unknown, None, Some("effect_absent")),
            None => (
                CommandState::Unknown,
                None,
                Some("manual_reconciliation_required"),
            ),
        };
        let record = self
            .journal
            .transition_command(
                &validated.key,
                &[
                    CommandState::Accepted,
                    CommandState::Running,
                    CommandState::Unknown,
                ],
                state,
                result,
                code,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        Ok(CommandOutcome {
            record,
            replayed: true,
        })
    }

    /// Authorizes one retry only after an explicit effect-absent observation.
    ///
    /// # Errors
    ///
    /// Rejects commands without persisted absence proof and propagates journal failures.
    pub fn retry_external(
        &self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<ExternalCommand, CommandError> {
        let validated = validate_command(envelope, context)?;
        if !validated.external {
            return Err(CommandError::Rejected("external_command_required"));
        }
        let record = self.matching_record(&validated)?;
        if record.state != CommandState::Unknown
            || record.error_code.as_deref() != Some("effect_absent")
        {
            return Err(CommandError::Rejected("reconciliation_required"));
        }
        self.journal
            .transition_command(
                &validated.key,
                &[CommandState::Unknown],
                CommandState::Running,
                None,
                None,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        Ok(ExternalCommand {
            key: validated.key,
            command_id: validated.command_id,
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
        let record = self.matching_record(&validated)?;
        if record.state != CommandState::Unknown {
            return Err(CommandError::Rejected("command_not_unknown"));
        }
        if record.error_code.as_deref() != Some("effect_absent") {
            return Err(CommandError::Rejected("reconciliation_required"));
        }
        if self
            .journal
            .synthetic_effect(&validated.key)
            .map_err(pre_effect_failure)?
            .is_some()
        {
            return self.reconcile_replay(record, &validated, context, false);
        }
        self.journal
            .transition_command(
                &validated.key,
                &[CommandState::Unknown],
                CommandState::Running,
                None,
                None,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        let record = self
            .journal
            .execute_and_complete_synthetic(
                &validated.key,
                &validated.command_id,
                &validated.payload_hash,
                &validated.result,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        Ok(CommandOutcome {
            record,
            replayed: true,
        })
    }

    #[must_use]
    pub fn journal(&self) -> &Journal {
        &self.journal
    }

    /// Observes and persists reconciliation state without executing an effect.
    ///
    /// # Errors
    ///
    /// Rejects an identity mismatch and reports journal failures without executing an effect.
    pub fn reconcile(
        &self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<CommandOutcome, CommandError> {
        let validated = validate_command(envelope, context)?;
        let record = self.matching_record(&validated)?;
        self.reconcile_replay(record, &validated, context, true)
    }

    fn matching_record(&self, validated: &ValidatedCommand) -> Result<CommandRecord, CommandError> {
        let by_key = self
            .journal
            .command(&validated.key)
            .map_err(pre_effect_failure)?;
        let by_command = self
            .journal
            .command_by_id(&validated.command_id)
            .map_err(pre_effect_failure)?;
        match (by_key, by_command) {
            (Some(by_key), Some(by_command))
                if by_key.idempotency_key == by_command.idempotency_key
                    && by_key.command_id == validated.command_id =>
            {
                if by_key.payload_hash_version != validated.hash_version as i32 {
                    return Err(CommandError::Rejected("semantic_hash_version_conflict"));
                }
                if by_key.payload_sha256 == validated.payload_hash {
                    Ok(by_key)
                } else {
                    Err(CommandError::PayloadConflict)
                }
            }
            (None, None) => Err(CommandError::Rejected("command_not_accepted")),
            _ => Err(CommandError::IdentityConflict),
        }
    }

    fn reconcile_replay(
        &self,
        record: CommandRecord,
        validated: &ValidatedCommand,
        context: &CommandContext,
        explicit: bool,
    ) -> Result<CommandOutcome, CommandError> {
        if matches!(record.state, CommandState::Succeeded | CommandState::Failed) {
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let effect = self
            .journal
            .synthetic_effect(&validated.key)
            .map_err(pre_effect_failure)?;
        if let Some(effect) = effect {
            if effect.payload_sha256 != validated.payload_hash {
                return Err(CommandError::Rejected("idempotency_payload_conflict"));
            }
            let record = self
                .journal
                .transition_command(
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
                )
                .map_err(|source| CommandError::OutcomeUnknown {
                    code: "result_persistence_failed",
                    record: Box::new(record.clone()),
                    source: Box::new(source),
                })?;
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let error_code = if explicit {
            "effect_absent"
        } else {
            "outcome_requires_reconciliation"
        };
        let record = if record.state == CommandState::Unknown
            && record.error_code.as_deref() == Some(error_code)
        {
            record
        } else {
            self.journal
                .transition_command(
                    &validated.key,
                    &[
                        CommandState::Accepted,
                        CommandState::Running,
                        CommandState::Unknown,
                    ],
                    CommandState::Unknown,
                    None,
                    Some(error_code),
                    context.now_unix_seconds,
                )
                .map_err(pre_effect_failure)?
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
    hash_version: SemanticPayloadHashVersion,
    result: Vec<u8>,
    external: bool,
}

/// Computes the cross-language semantic payload identity for a supported command.
///
/// Delivery metadata is intentionally excluded because reconciliation intent does not change the
/// side effect. The capability, node, revision, and canonical typed payload are all bound.
///
/// # Errors
///
/// Rejects unsupported payload types.
pub fn semantic_payload_hash(envelope: &CommandEnvelope) -> Result<[u8; 32], CommandError> {
    let (capability, payload_bytes) = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(payload)) => {
            ("synthetic.noop", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            ("synthetic.echo", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            ("ocserv.session.disconnect", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            ("ocserv.session.terminate", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            ("ocserv.ip_ban.remove", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::ServiceReload(payload)) => {
            ("ocserv.service.reload", payload.encode_to_vec())
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    };
    let mut hash = Sha256::new();
    hash.update(capability.as_bytes());
    hash.update(&envelope.node_id);
    hash.update(envelope.expected_revision.to_be_bytes());
    hash.update(payload_bytes);
    Ok(hash.finalize().into())
}

/// Domain separator for v1 canonical semantic hashes.
///
/// Includes the trailing `0x00` terminator required by
/// `docs/development/command-semantic-hash-v1.md`.
const SEMANTIC_HASH_V1_DOMAIN_SEPARATOR: &[u8] = b"ocservia.command.semantic-hash.v1\0";

/// Computes the versioned canonical semantic hash (v1) for a command envelope.
///
/// Follows `docs/development/command-semantic-hash-v1.md`. The preimage is
/// hand-specified canonical bytes, never Protobuf wire encoding, so it is stable
/// across language runtimes regardless of unknown-field retention.
///
/// # Errors
///
/// Rejects unsupported payload types.
pub fn semantic_payload_hash_v1(envelope: &CommandEnvelope) -> Result<[u8; 32], CommandError> {
    let (payload_kind, canonical_payload) = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => (107_u32, Vec::new()),
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            let utf8 = payload.message.as_bytes();
            let mut out = Vec::with_capacity(4 + utf8.len());
            out.extend_from_slice(
                &u32::try_from(utf8.len())
                    .map_err(|_| CommandError::Rejected("payload_size_invalid"))?
                    .to_be_bytes(),
            );
            out.extend_from_slice(utf8);
            (108_u32, out)
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => (
            100_u32,
            canonical_session_payload(&payload.session_id, &payload.boot_id)?,
        ),
        Some(command_envelope::Payload::ServiceReload(_)) => (105_u32, Vec::new()),
        Some(command_envelope::Payload::SessionTerminate(payload)) => (
            112_u32,
            canonical_session_payload(&payload.session_id, &payload.boot_id)?,
        ),
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let utf8 = payload.ip.as_bytes();
            let mut out = Vec::with_capacity(4 + utf8.len());
            out.extend_from_slice(
                &u32::try_from(utf8.len())
                    .map_err(|_| CommandError::Rejected("payload_size_invalid"))?
                    .to_be_bytes(),
            );
            out.extend_from_slice(utf8);
            (113_u32, out)
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    };
    let mut hash = Sha256::new();
    hash.update(SEMANTIC_HASH_V1_DOMAIN_SEPARATOR);
    hash.update(&envelope.node_id);
    hash.update(envelope.expected_revision.to_be_bytes());
    hash.update(payload_kind.to_be_bytes());
    hash.update(&canonical_payload);
    Ok(hash.finalize().into())
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
    if context
        .observed_revision
        .is_some_and(|revision| envelope.expected_revision != revision)
    {
        return Err(CommandError::Rejected("revision_mismatch"));
    }
    let (capability, result, external) = validate_payload(envelope)?;
    if !context.capabilities.contains(capability) {
        return Err(CommandError::Rejected("capability_rejected"));
    }
    let key = fixed::<16>(&envelope.idempotency_key, "idempotency_key_invalid")?;
    let command_id = fixed::<16>(&envelope.command_id, "command_id_invalid")?;
    if Uuid::from_bytes(key).get_version_num() != 7 {
        return Err(CommandError::Rejected("idempotency_key_invalid"));
    }
    if Uuid::from_bytes(command_id).get_version_num() != 7 {
        return Err(CommandError::Rejected("command_id_invalid"));
    }
    let version = SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
        .map_err(|_| CommandError::Rejected("semantic_hash_version_unsupported"))?;
    let (payload_hash, hash_version) = match version {
        SemanticPayloadHashVersion::Unspecified => {
            // Compatibility path: the Controller has not upgraded to v1 (pre-Commit 4).
            // Use the legacy hash and do not verify envelope.semantic_payload_sha256.
            (
                semantic_payload_hash(envelope)?,
                SemanticPayloadHashVersion::Unspecified,
            )
        }
        SemanticPayloadHashVersion::V1 => {
            let recomputed = semantic_payload_hash_v1(envelope)?;
            if envelope.semantic_payload_sha256.len() != 32 {
                return Err(CommandError::Rejected("semantic_payload_hash_missing"));
            }
            let expected: [u8; 32] = envelope
                .semantic_payload_sha256
                .as_slice()
                .try_into()
                .map_err(|_| CommandError::Rejected("semantic_payload_hash_malformed"))?;
            if recomputed != expected {
                return Err(CommandError::Rejected("semantic_payload_hash_mismatch"));
            }
            (recomputed, SemanticPayloadHashVersion::V1)
        }
    };
    Ok(ValidatedCommand {
        key,
        command_id,
        payload_hash,
        hash_version,
        result,
        external,
    })
}

fn validate_payload(
    envelope: &CommandEnvelope,
) -> Result<(&'static str, Vec<u8>, bool), CommandError> {
    Ok(match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => {
            ("synthetic.noop", b"noop:completed".to_vec(), false)
        }
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            if payload.message.len() > 4096 {
                return Err(CommandError::Rejected("payload_size_invalid"));
            }
            ("synthetic.echo", payload.message.as_bytes().to_vec(), false)
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            ("ocserv.session.disconnect", Vec::new(), true)
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            ("ocserv.session.terminate", Vec::new(), true)
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let canonical = payload
                .ip
                .parse::<std::net::IpAddr>()
                .map_err(|_| CommandError::Rejected("ip_invalid"))?
                .to_string();
            if canonical != payload.ip {
                return Err(CommandError::Rejected("ip_not_canonical"));
            }
            ("ocserv.ip_ban.remove", Vec::new(), true)
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            ("ocserv.service.reload", Vec::new(), true)
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    })
}

fn validate_session_payload(session_id: &str, boot_id: &str) -> Result<(), CommandError> {
    let parsed = session_id
        .parse::<u64>()
        .map_err(|_| CommandError::Rejected("session_id_invalid"))?;
    if parsed == 0 || parsed.to_string() != session_id {
        return Err(CommandError::Rejected("session_id_invalid"));
    }
    if Uuid::parse_str(boot_id).is_err() {
        return Err(CommandError::Rejected("boot_id_invalid"));
    }
    Ok(())
}

fn canonical_session_payload(session_id: &str, boot_id: &str) -> Result<Vec<u8>, CommandError> {
    validate_session_payload(session_id, boot_id)?;
    let mut out = Vec::with_capacity(8 + session_id.len() + boot_id.len());
    for value in [session_id.as_bytes(), boot_id.as_bytes()] {
        out.extend_from_slice(
            &u32::try_from(value.len())
                .map_err(|_| CommandError::Rejected("payload_size_invalid"))?
                .to_be_bytes(),
        );
        out.extend_from_slice(value);
    }
    Ok(out)
}

fn fixed<const N: usize>(value: &[u8], code: &'static str) -> Result<[u8; N], CommandError> {
    value.try_into().map_err(|_| CommandError::Rejected(code))
}

fn pre_effect_failure(source: rusqlite::Error) -> CommandError {
    CommandError::PreEffectJournalFailure(Box::new(source))
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
        IpBanRemove, ServiceReload, SessionDisconnect, SessionTerminate, SyntheticEcho,
        SyntheticNoop, command_envelope,
    };
    use prost_types::Timestamp;
    use rand::{SeedableRng, rngs::StdRng};
    use std::os::unix::process::ExitStatusExt as _;
    use std::process::Command;

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

    #[test]
    fn semantic_payload_hash_matches_cross_language_golden_vectors() {
        let node = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
        let key = *Uuid::now_v7().as_bytes();
        let mut envelope = command(node, key, "", 100);
        let cases = [
            (
                "",
                "2d6daaae892285c786fba378aff37d4d0436dae76699061500b548e939782433",
            ),
            (
                "hello",
                "a7299856cb7fa4e266b1234614361677e1d1b2466608d014e71b4a547c804397",
            ),
            (
                "你好",
                "10f1ae9994bde9ea69de8fb965bf5029e97ad8b1bf1f00834d47831f60adf38d",
            ),
        ];
        for (message, expected) in cases {
            envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: message.to_owned(),
            }));
            assert_eq!(
                hex::encode(semantic_payload_hash(&envelope).unwrap()),
                expected
            );
        }
        envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: "x".repeat(4096),
        }));
        assert_eq!(
            hex::encode(semantic_payload_hash(&envelope).unwrap()),
            "d6f3a8f8d0fc7ccbbfe6fec9f93bcad544d98fec0f3501aeef8be2a5d2b78daa"
        );
        envelope.payload = Some(command_envelope::Payload::SyntheticNoop(
            SyntheticNoop::default(),
        ));
        assert_eq!(
            hex::encode(semantic_payload_hash(&envelope).unwrap()),
            "2e5b198f3c3a2718113a4dbf2a552c730ddede13567ee448c6118459ccfa0d98"
        );
        envelope.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: "hello".to_owned(),
        }));
        envelope.expected_revision = 2;
        assert_eq!(
            hex::encode(semantic_payload_hash(&envelope).unwrap()),
            "79659b4e1819080191867174096d8aa5d01a43cb634cab9c51b113391643c343"
        );
        envelope.expected_revision = 1;
        envelope.node_id = (1_u8..=16).collect();
        assert_eq!(
            hex::encode(semantic_payload_hash(&envelope).unwrap()),
            "a45222e4babe147a02b9274937f09c337ac56764a56c61a9ebfd901d4fec7afe"
        );
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
            delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
            payload: Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: message.to_owned(),
            })),
            semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
            semantic_payload_sha256: Vec::new(),
        }
    }

    /// Builds a v1 command envelope with a correctly computed semantic hash.
    ///
    /// The `semantic_payload_sha256` is filled from `semantic_payload_hash_v1`
    /// so that `validate_command` passes the strict wire-schema check.
    fn command_v1(node_id: [u8; 16], key: [u8; 16], message: &str, now: i64) -> CommandEnvelope {
        let mut envelope = command(node_id, key, message, now);
        envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
        envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
            .expect("v1 hash")
            .to_vec();
        envelope
    }

    fn context(node_id: [u8; 16], now: i64) -> CommandContext {
        CommandContext {
            node_id,
            observed_revision: Some(1),
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

    #[test]
    fn external_command_is_durable_and_duplicate_delivery_replays() {
        let path = temporary_journal("external");
        let node = [0x41; 16];
        let key = *Uuid::now_v7().as_bytes();
        let mut envelope = command(node, key, "", 100);
        envelope.payload = Some(command_envelope::Payload::SessionDisconnect(
            SessionDisconnect {
                session_id: "42".to_owned(),
                boot_id: Uuid::now_v7().to_string(),
            },
        ));
        envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
        envelope.semantic_payload_sha256 = semantic_payload_hash_v1(&envelope)
            .expect("session hash")
            .to_vec();
        let mut command_context = context(node, 100);
        command_context.capabilities = HashSet::from(["ocserv.session.disconnect"]);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("journal"));
        let ExternalPreparation::Execute(command) = executor
            .prepare_external(&envelope, &command_context)
            .expect("prepare")
        else {
            panic!("first delivery must execute");
        };
        let completed = executor
            .complete_external(&command, Ok(b"applied"), 101)
            .expect("complete");
        assert_eq!(completed.record.state, CommandState::Succeeded);
        let ExternalPreparation::Replay(replayed) = executor
            .prepare_external(&envelope, &command_context)
            .expect("replay")
        else {
            panic!("duplicate delivery must not execute");
        };
        assert!(replayed.replayed);
        assert_eq!(replayed.record.state, CommandState::Succeeded);
        cleanup_journal(&path);
    }

    fn cleanup_journal(path: &Path) {
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }

    fn abort_matrix_command() -> (CommandEnvelope, CommandContext) {
        let node_id = [0x11; 16];
        let mut key = [0x22; 16];
        key[6] = 0x72;
        key[8] = 0x82;
        let mut command_id = [0x33; 16];
        command_id[6] = 0x73;
        command_id[8] = 0x83;
        let mut envelope = command(node_id, key, "once", 100);
        envelope.command_id = command_id.to_vec();
        (envelope, context(node_id, 100))
    }

    #[test]
    fn abort_crash_child() {
        let Ok(point) = std::env::var("OCSERVIA_ABORT_CRASH_POINT") else {
            return;
        };
        let path = std::env::var_os("OCSERVIA_ABORT_JOURNAL")
            .map(std::path::PathBuf::from)
            .expect("child journal path");
        let crash = match point.as_str() {
            "after-accepted" => CrashPoint::AfterAccepted,
            "before-side-effect" => CrashPoint::BeforeSideEffect,
            "after-side-effect" => CrashPoint::AfterSideEffect,
            "after-result" => CrashPoint::AfterResult,
            _ => panic!("unknown crash point"),
        };
        let (envelope, command_context) = abort_matrix_command();
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("child journal"));
        assert!(matches!(
            executor.execute(&envelope, &command_context, Some(crash)),
            Err(CommandError::InjectedCrash(point)) if point == crash
        ));
        std::process::abort();
    }

    #[test]
    fn process_abort_crash_matrix_recovers_exactly_once() {
        for point in [
            "after-accepted",
            "before-side-effect",
            "after-side-effect",
            "after-result",
        ] {
            let path = temporary_journal(point);
            let status = Command::new(std::env::current_exe().expect("test executable"))
                .args(["--exact", "tests::abort_crash_child", "--nocapture"])
                .env("OCSERVIA_ABORT_CRASH_POINT", point)
                .env("OCSERVIA_ABORT_JOURNAL", &path)
                .status()
                .expect("spawn crash child");
            assert_eq!(status.signal(), Some(6), "{point}: {status}");

            let (envelope, command_context) = abort_matrix_command();
            let mut executor = CommandExecutor::new(Journal::open(&path).expect("reopen"));
            let replayed = executor
                .execute(&envelope, &command_context, None)
                .expect("ordinary replay");
            let reconciled = if replayed.record.state == CommandState::Unknown {
                executor
                    .reconcile(&envelope, &command_context)
                    .expect("explicit reconcile")
            } else {
                replayed
            };
            let completed = if reconciled.record.state == CommandState::Unknown {
                executor
                    .retry_unknown(&envelope, &command_context)
                    .expect("safe retry")
            } else {
                reconciled
            };
            assert_eq!(completed.record.state, CommandState::Succeeded, "{point}");
            assert_eq!(
                executor
                    .journal()
                    .synthetic_execution_count()
                    .expect("effect count"),
                1,
                "{point}"
            );
            executor
                .journal()
                .integrity_check()
                .expect("integrity check");
            drop(executor);
            cleanup_journal(&path);
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
            let replayed = executor
                .execute(&envelope, &command_context, None)
                .expect("ordinary replay");
            let reconciled = if replayed.record.state == CommandState::Unknown {
                assert_eq!(
                    replayed.record.error_code.as_deref(),
                    Some("outcome_requires_reconciliation")
                );
                executor
                    .reconcile(&envelope, &command_context)
                    .expect("explicit reconcile")
            } else {
                replayed
            };
            let completed = if reconciled.record.state == CommandState::Unknown {
                assert_eq!(
                    reconciled.record.error_code.as_deref(),
                    Some("effect_absent")
                );
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
            ("idempotency-key-version", {
                let mut value = base.clone();
                value.idempotency_key = [0_u8; 16].to_vec();
                value
            }),
            ("command-id-version", {
                let mut value = base.clone();
                value.command_id = [0_u8; 16].to_vec();
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
    fn v1_command_with_missing_semantic_payload_hash_is_rejected() {
        let path = temporary_journal("v1-missing-hash");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        // Declare v1 but omit the expected hash bytes.
        let mut envelope = command(node_id, key, "hello", 100);
        envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
        assert!(matches!(
            executor.execute(&envelope, &command_context, None),
            Err(CommandError::Rejected("semantic_payload_hash_missing"))
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
    fn v1_command_with_mismatched_semantic_payload_hash_is_rejected() {
        let path = temporary_journal("v1-hash-mismatch");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        // Declare v1 with a wrong hash (32 bytes of zeros).
        let mut envelope = command(node_id, key, "hello", 100);
        envelope.semantic_payload_hash_version = SemanticPayloadHashVersion::V1 as i32;
        envelope.semantic_payload_sha256 = vec![0_u8; 32];
        assert!(matches!(
            executor.execute(&envelope, &command_context, None),
            Err(CommandError::Rejected("semantic_payload_hash_mismatch"))
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
    fn v1_command_with_unknown_hash_version_is_rejected() {
        let path = temporary_journal("v1-unknown-version");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        // Use an unsupported version number.
        let mut envelope = command(node_id, key, "hello", 100);
        envelope.semantic_payload_hash_version = 99;
        assert!(matches!(
            executor.execute(&envelope, &command_context, None),
            Err(CommandError::Rejected("semantic_hash_version_unsupported"))
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
    fn v1_command_with_correct_hash_is_accepted() {
        let path = temporary_journal("v1-correct");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        let envelope = command_v1(node_id, key, "hello", 100);
        let outcome = executor
            .execute(&envelope, &command_context, None)
            .expect("accept");
        assert!(!outcome.replayed);
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
    fn same_key_different_payload_is_rejected_without_second_effect() {
        let path = temporary_journal("conflict");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        let first = command(node_id, key, "first", 100);
        executor
            .execute(&first, &command_context, None)
            .expect("first");
        let mut conflicting = first.clone();
        conflicting.payload = Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
            message: "second".to_owned(),
        }));
        assert!(matches!(
            executor.execute(&conflicting, &command_context, None),
            Err(CommandError::PayloadConflict)
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

    #[test]
    fn identity_conflicts_never_execute_a_second_effect() {
        let path = temporary_journal("identity-conflict");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let first = command(node_id, key, "first", 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        executor
            .execute(&first, &command_context, None)
            .expect("first");

        let mut different_command = first.clone();
        different_command.command_id = Uuid::now_v7().as_bytes().to_vec();
        assert!(matches!(
            executor.execute(&different_command, &command_context, None),
            Err(CommandError::IdentityConflict)
        ));

        let mut different_key = first.clone();
        different_key.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
        assert!(matches!(
            executor.execute(&different_key, &command_context, None),
            Err(CommandError::IdentityConflict)
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

    #[test]
    fn reconcile_and_retry_reject_stored_hash_version_conflict() {
        let path = temporary_journal("stored-version-conflict");
        let node_id = *Uuid::now_v7().as_bytes();
        let key = *Uuid::now_v7().as_bytes();
        let command_context = context(node_id, 100);
        let envelope = command_v1(node_id, key, "once", 100);
        {
            let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
            assert!(matches!(
                executor.execute(&envelope, &command_context, Some(CrashPoint::AfterAccepted)),
                Err(CommandError::InjectedCrash(CrashPoint::AfterAccepted))
            ));
            executor
                .deliver(&envelope, &command_context)
                .expect("ordinary replay records uncertainty");
        }
        let connection = rusqlite::Connection::open(&path).expect("open journal for mutation");
        connection
            .execute(
                "UPDATE command_journal SET payload_hash_version=99 WHERE idempotency_key=?1",
                [key.as_slice()],
            )
            .expect("install incompatible stored hash version");
        drop(connection);

        let mut executor = CommandExecutor::new(Journal::open(&path).expect("reopen"));
        assert!(matches!(
            executor.reconcile(&envelope, &command_context),
            Err(CommandError::Rejected("semantic_hash_version_conflict"))
        ));
        let mut retry = envelope;
        retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
        assert!(matches!(
            executor.retry_unknown(&retry, &command_context),
            Err(CommandError::Rejected("semantic_hash_version_conflict"))
        ));
        assert_eq!(
            executor
                .journal()
                .synthetic_execution_count()
                .expect("effect count"),
            0
        );
        drop(executor);
        cleanup_journal(&path);
    }

    #[test]
    fn safe_retry_requires_explicit_effect_absence_reconciliation() {
        let path = temporary_journal("reconcile-required");
        let node_id = *Uuid::now_v7().as_bytes();
        let envelope = command(node_id, *Uuid::now_v7().as_bytes(), "once", 100);
        let command_context = context(node_id, 100);
        let mut executor = CommandExecutor::new(Journal::open(&path).expect("open"));
        assert!(matches!(
            executor.execute(&envelope, &command_context, Some(CrashPoint::AfterAccepted)),
            Err(CommandError::InjectedCrash(CrashPoint::AfterAccepted))
        ));
        assert!(matches!(
            executor.retry_unknown(&envelope, &command_context),
            Err(CommandError::Rejected("command_not_unknown"))
        ));
        executor
            .deliver(&envelope, &command_context)
            .expect("ordinary replay records uncertainty");
        let mut retry = envelope.clone();
        retry.delivery_mode = CommandDeliveryMode::RetryIfEffectAbsent.into();
        assert!(matches!(
            executor.deliver(&retry, &command_context),
            Err(CommandError::Rejected("reconciliation_required"))
        ));
        let mut reconcile = envelope.clone();
        reconcile.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
        executor
            .deliver(&reconcile, &command_context)
            .expect("observe effect absence");
        executor
            .deliver(&retry, &command_context)
            .expect("safe retry");
        assert_eq!(executor.journal().synthetic_execution_count().unwrap(), 1);
        drop(executor);
        cleanup_journal(&path);
    }

    // ---- Canonical Semantic Payload Hash v1 ----
    //
    // The tests below pin the v1 algorithm defined in
    // `docs/development/command-semantic-hash-v1.md` against the shared golden
    // fixture `testdata/semantic-payload-hash-v1.json`. The inlined
    // `compute_v1_canonical` mirror is an independent reference that writes each
    // field by hand (no Protobuf). The production `semantic_payload_hash_v1`
    // function must produce identical bytes.

    /// V1 canonical preimage domain separator: ASCII label plus a NUL terminator.
    const V1_DOMAIN_SEPARATOR: &[u8] = b"ocservia.command.semantic-hash.v1\0";

    /// Independent reference mirror of the v1 canonical hash.
    ///
    /// This intentionally does not use Protobuf serialization. It is the test
    /// reference; the production `semantic_payload_hash_v1` must match it.
    fn compute_v1_canonical(
        node_id: &[u8],
        expected_revision: u64,
        payload_kind: u32,
        canonical_payload: &[u8],
    ) -> [u8; 32] {
        let mut hash = Sha256::new();
        hash.update(V1_DOMAIN_SEPARATOR);
        hash.update(node_id);
        hash.update(expected_revision.to_be_bytes());
        hash.update(payload_kind.to_be_bytes());
        hash.update(canonical_payload);
        hash.finalize().into()
    }

    fn canonical_payload_for(kind: u32, message: &str) -> Vec<u8> {
        match kind {
            107 => Vec::new(), // SyntheticNoop
            108 => {
                // SyntheticEcho: u32_be(len(utf8)) || utf8
                let utf8 = message.as_bytes();
                let mut out = Vec::with_capacity(4 + utf8.len());
                out.extend_from_slice(&(u32::try_from(utf8.len()).unwrap()).to_be_bytes());
                out.extend_from_slice(utf8);
                out
            }
            _ => panic!("unsupported payload_kind in test: {kind}"),
        }
    }

    fn fixture_string_payload(values: &[&str]) -> Vec<u8> {
        let mut out = Vec::new();
        for value in values {
            out.extend_from_slice(&u32::try_from(value.len()).unwrap().to_be_bytes());
            out.extend_from_slice(value.as_bytes());
        }
        out
    }

    fn canonical_payload_from_fixture(kind: u32, payload: &serde_json::Value) -> Vec<u8> {
        let field = |name| {
            payload
                .get(name)
                .and_then(serde_json::Value::as_str)
                .unwrap_or("")
        };
        match kind {
            100 | 112 => fixture_string_payload(&[field("session_id"), field("boot_id")]),
            105 => Vec::new(),
            107 | 108 => canonical_payload_for(kind, field("message")),
            113 => fixture_string_payload(&[field("ip")]),
            _ => panic!("unsupported payload_kind in fixture: {kind}"),
        }
    }

    fn load_v1_fixture() -> serde_json::Value {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/semantic-payload-hash-v1.json");
        let raw = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read v1 fixture {}: {e}", path.display()));
        serde_json::from_str(&raw)
            .unwrap_or_else(|e| panic!("parse v1 fixture {}: {e}", path.display()))
    }

    /// Builds a v1 envelope from fixture vector fields for hash verification.
    fn envelope_from_fixture_vector(
        node_id: &[u8],
        expected_revision: u64,
        payload_kind: u32,
        message: &str,
    ) -> CommandEnvelope {
        let payload = match payload_kind {
            107 => Some(command_envelope::Payload::SyntheticNoop(SyntheticNoop {})),
            108 => Some(command_envelope::Payload::SyntheticEcho(SyntheticEcho {
                message: message.to_owned(),
            })),
            _ => panic!("unsupported payload_kind in test: {payload_kind}"),
        };
        CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            message_id: Vec::new(),
            command_id: Vec::new(),
            idempotency_key: Vec::new(),
            node_id: node_id.to_vec(),
            sequence: 0,
            issued_at: None,
            expires_at: None,
            expected_revision,
            traceparent: String::new(),
            actor_id: String::new(),
            reason: String::new(),
            delivery_mode: CommandDeliveryMode::Unspecified.into(),
            payload,
            semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
            semantic_payload_sha256: Vec::new(),
        }
    }

    fn envelope_from_fixture_payload(
        node_id: &[u8],
        expected_revision: u64,
        payload_kind: u32,
        payload: &serde_json::Value,
    ) -> CommandEnvelope {
        let field = |name| {
            payload
                .get(name)
                .and_then(serde_json::Value::as_str)
                .unwrap_or("")
                .to_owned()
        };
        let message = field("message");
        let mut envelope = envelope_from_fixture_vector(
            node_id,
            expected_revision,
            if matches!(payload_kind, 107 | 108) {
                payload_kind
            } else {
                107
            },
            &message,
        );
        envelope.payload = Some(match payload_kind {
            100 => command_envelope::Payload::SessionDisconnect(SessionDisconnect {
                session_id: field("session_id"),
                boot_id: field("boot_id"),
            }),
            105 => command_envelope::Payload::ServiceReload(ServiceReload {}),
            107 | 108 => return envelope,
            112 => command_envelope::Payload::SessionTerminate(SessionTerminate {
                session_id: field("session_id"),
                boot_id: field("boot_id"),
            }),
            113 => command_envelope::Payload::IpBanRemove(IpBanRemove { ip: field("ip") }),
            _ => panic!("unsupported payload_kind in fixture: {payload_kind}"),
        });
        envelope
    }

    #[test]
    fn canonical_semantic_hash_v1_matches_shared_fixture() {
        let fixture = load_v1_fixture();
        for vector in fixture
            .get("vectors")
            .and_then(serde_json::Value::as_array)
            .expect("vectors array")
        {
            let name = vector
                .get("name")
                .and_then(serde_json::Value::as_str)
                .unwrap_or("<unnamed>");
            let node_id = hex::decode(
                vector
                    .get("node_id_hex")
                    .and_then(serde_json::Value::as_str)
                    .expect("node_id_hex"),
            )
            .expect("node_id hex");
            let expected_revision = vector
                .get("expected_revision")
                .and_then(serde_json::Value::as_u64)
                .expect("expected_revision");
            let payload_kind = vector
                .get("payload_kind")
                .and_then(serde_json::Value::as_u64)
                .expect("payload_kind");
            let payload_kind = u32::try_from(payload_kind).expect("payload_kind fits u32");
            let payload_fields = vector.get("payload").expect("payload");
            let expected = vector
                .get("expected_sha256")
                .and_then(serde_json::Value::as_str)
                .expect("expected_sha256");

            // Cross-check: the test-only mirror must agree with the fixture.
            let payload = canonical_payload_from_fixture(payload_kind, payload_fields);
            let mirror = compute_v1_canonical(&node_id, expected_revision, payload_kind, &payload);
            assert_eq!(
                hex::encode(mirror),
                expected,
                "vector {name:?} mirror digest mismatch"
            );

            // Production function must agree with the mirror.
            let envelope = envelope_from_fixture_payload(
                &node_id,
                expected_revision,
                payload_kind,
                payload_fields,
            );
            let production = semantic_payload_hash_v1(&envelope).expect("v1 hash");
            assert_eq!(
                hex::encode(production),
                expected,
                "vector {name:?} production digest mismatch"
            );
        }
    }

    #[test]
    fn canonical_semantic_hash_v1_excludes_delivery_metadata() {
        let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
        let message = "hello";
        // Build two envelopes that differ only in delivery/audit metadata.
        let mut a = envelope_from_fixture_vector(&node, 1, 108, message);
        let mut b = envelope_from_fixture_vector(&node, 1, 108, message);
        a.message_id = vec![1_u8; 16];
        a.command_id = vec![2_u8; 16];
        a.idempotency_key = vec![3_u8; 16];
        a.sequence = 42;
        a.traceparent = "00-aaa".to_owned();
        a.actor_id = "alice".to_owned();
        a.reason = "ticket-1".to_owned();
        a.delivery_mode = CommandDeliveryMode::ExecuteOrReplay.into();
        b.message_id = vec![9_u8; 16];
        b.command_id = vec![8_u8; 16];
        b.idempotency_key = vec![7_u8; 16];
        b.sequence = 99;
        b.traceparent = "00-bbb".to_owned();
        b.actor_id = "bob".to_owned();
        b.reason = "ticket-2".to_owned();
        b.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
        // Only node_id, revision, kind, and payload bytes enter the preimage,
        // so changing delivery/audit fields cannot affect the digest.
        assert_eq!(
            semantic_payload_hash_v1(&a).expect("v1 hash a"),
            semantic_payload_hash_v1(&b).expect("v1 hash b"),
        );
    }

    #[test]
    fn canonical_semantic_hash_v1_changes_on_semantic_input() {
        let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
        let node_shifted = [1_u8, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16];

        let hello = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "hello"))
            .expect("v1 hash");
        // message change
        let world = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "world"))
            .expect("v1 hash");
        assert_ne!(hello, world);
        // node_id change
        let shifted = semantic_payload_hash_v1(&envelope_from_fixture_vector(
            &node_shifted,
            1,
            108,
            "hello",
        ))
        .expect("v1 hash");
        assert_ne!(hello, shifted);
        // revision change
        let rev2 = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 2, 108, "hello"))
            .expect("v1 hash");
        assert_ne!(hello, rev2);
        // payload kind change (noop vs echo)
        let noop = semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 107, ""))
            .expect("v1 hash");
        assert_ne!(hello, noop);
    }

    #[test]
    fn canonical_semantic_hash_v1_rejects_unicode_normalization() {
        let node = [0_u8, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15];
        // Precomposed é (U+00E9) vs decomposed e + combining acute (U+0301).
        let composed =
            semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "\u{00e9}"))
                .expect("v1 hash");
        let decomposed =
            semantic_payload_hash_v1(&envelope_from_fixture_vector(&node, 1, 108, "e\u{0301}"))
                .expect("v1 hash");
        assert_ne!(
            composed, decomposed,
            "no Unicode normalization: different bytes must hash differently"
        );
    }
}
