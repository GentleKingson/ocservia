//! Unprivileged Agent runtime primitives and fixed privd client.

#![forbid(unsafe_code)]

use std::collections::HashSet;
use std::fmt;
use std::io;
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    PrivdRequest, PrivdResponse, PrivilegedRequestMode, ReadRequest, UpgradeOperationResult,
    UpgradeResultListRequest, privd_request, privd_response, read_frame, write_frame,
};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_command_journal::{
    AcceptOutcome, AppliedResourceRevision, CommandRecord, CommandState, Journal,
};
pub use ocservia_contracts::agent_upgrade::AGENT_UPGRADE_CAPABILITY;
#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::agent::v1::ConfigApplyResult;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
    SemanticPayloadHashVersion, command_envelope,
};
use prost::Message;
use rand::{Rng, RngExt};
#[cfg(test)]
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
    pub authorization_revision: u64,
    pub capabilities: HashSet<String>,
    pub session_expires_at_unix_seconds: i64,
    pub command_keys: ControllerCommandKeyring,
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
    accepted_at: i64,
}

impl ExternalCommand {
    /// Journal acceptance time that privd must bind into the receipt.
    #[must_use]
    pub const fn accepted_at(&self) -> i64 {
        self.accepted_at
    }

    fn accepted_record(&self, journal: &Journal) -> Result<CommandRecord, CommandError> {
        journal
            .command(&self.key)
            .map_err(pre_effect_failure)?
            .ok_or(CommandError::Rejected("command_not_accepted"))
    }

    fn attested_outcome(
        &self,
        before: CommandRecord,
        persisted: Result<CommandRecord, rusqlite::Error>,
    ) -> Result<CommandOutcome, CommandError> {
        let record = persisted.map_err(|source| CommandError::OutcomeUnknown {
            code: "attested_result_persistence_failed",
            record: Box::new(before),
            source: Box::new(source),
        })?;
        if record.command_id != self.command_id {
            return Err(CommandError::IdentityConflict);
        }
        Ok(CommandOutcome {
            record,
            replayed: false,
        })
    }
}

/// Result of preparing an external effect.
#[derive(Clone, Debug)]
pub enum ExternalPreparation {
    Execute(ExternalCommand),
    Replay(CommandOutcome),
}

/// Independent effect evidence used to resolve an external Unknown safely.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ExternalEffectObservation {
    AppliedExact,
    SupersededByNewerRevision,
    Absent,
    Unknown,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum DesiredRevisionFence {
    Current,
    Superseded,
    Gap,
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
        if self
            .journal
            .command(&validated.key)
            .map_err(pre_effect_failure)?
            .is_none()
        {
            self.require_current_desired_revision(envelope)?;
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
                    accepted_at: context.now_unix_seconds,
                }))
            }
        }
    }

    /// Moves an Unknown command back to Running only after privd returned an
    /// exact cached root receipt. The caller must atomically persist that proof
    /// with the terminal result next; a crash remains Unknown on replay.
    ///
    /// # Errors
    ///
    /// Rejects non-privileged, mismatched, or non-Unknown commands.
    pub fn begin_attested_recovery(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<ExternalCommand, CommandError> {
        let validated = validate_command(envelope, context)?;
        if !validated.external {
            return Err(CommandError::Rejected("external_command_required"));
        }
        let record = match self.matching_record(&validated) {
            Ok(record) => record,
            Err(CommandError::Rejected("command_not_accepted")) => {
                match self
                    .journal
                    .accept_command(
                        &validated.key,
                        &validated.command_id,
                        &validated.payload_hash,
                        validated.hash_version as i32,
                        context.now_unix_seconds,
                    )
                    .map_err(pre_effect_failure)?
                {
                    AcceptOutcome::Accepted(record) | AcceptOutcome::Replay(record) => record,
                    AcceptOutcome::PayloadConflict(_) => return Err(CommandError::PayloadConflict),
                    AcceptOutcome::IdentityConflict(_) => {
                        return Err(CommandError::IdentityConflict);
                    }
                }
            }
            Err(error) => return Err(error),
        };
        if !matches!(record.state, CommandState::Accepted | CommandState::Unknown) {
            return Err(CommandError::Rejected("reconciliation_state_invalid"));
        }
        let record = self
            .journal
            .transition_command(
                &validated.key,
                &[CommandState::Accepted, CommandState::Unknown],
                CommandState::Running,
                None,
                None,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        Ok(ExternalCommand {
            key: validated.key,
            command_id: validated.command_id,
            accepted_at: record.accepted_at,
        })
    }

    /// Persists a privd result and its opaque root-signed proof in one journal write.
    ///
    /// # Errors
    ///
    /// Fails closed when proof persistence or the terminal transition fails.
    pub fn complete_external_attested(
        &self,
        command: &ExternalCommand,
        result: Result<&[u8], &'static str>,
        proof: &[u8],
        now: i64,
    ) -> Result<CommandOutcome, CommandError> {
        let (state, bytes, code) = match result {
            Ok(bytes) if bytes.len() <= MAX_COMMAND_BYTES => {
                (CommandState::Succeeded, Some(bytes), None)
            }
            Ok(_) => (CommandState::Failed, None, Some("result_too_large")),
            Err(code) => (CommandState::Failed, None, Some(code)),
        };
        let before = command.accepted_record(&self.journal)?;
        let record = self.journal.transition_command_attested(
            &command.key,
            &[CommandState::Running],
            state,
            bytes,
            code,
            proof,
            now,
        );
        command.attested_outcome(before, record)
    }

    /// Atomically persists an applied revision with its root-signed proof.
    ///
    /// # Errors
    ///
    /// Reports unknown when result, revision, and proof cannot commit together.
    #[allow(clippy::too_many_arguments)]
    pub fn complete_external_applied_attested(
        &mut self,
        command: &ExternalCommand,
        resource_type: &str,
        resource_key: &str,
        revision: u64,
        result: &[u8],
        proof: &[u8],
        now: i64,
    ) -> Result<CommandOutcome, CommandError> {
        let before = command.accepted_record(&self.journal)?;
        let record = self.journal.complete_external_with_revision_attested(
            &command.key,
            &command.command_id,
            AppliedResourceRevision {
                resource_type,
                resource_key,
                revision,
            },
            result,
            proof,
            now,
        );
        command.attested_outcome(before, record)
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
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
        observation: ExternalEffectObservation,
    ) -> Result<CommandOutcome, CommandError> {
        let validated = validate_command(envelope, context)?;
        if !validated.external {
            return Err(CommandError::Rejected("external_command_required"));
        }
        let (record, rebuilt_journal) = match self.matching_record(&validated) {
            Ok(record) => (record, false),
            Err(CommandError::Rejected("command_not_accepted")) => {
                let record = match self
                    .journal
                    .accept_command(
                        &validated.key,
                        &validated.command_id,
                        &validated.payload_hash,
                        validated.hash_version as i32,
                        context.now_unix_seconds,
                    )
                    .map_err(pre_effect_failure)?
                {
                    AcceptOutcome::Accepted(record) | AcceptOutcome::Replay(record) => record,
                    AcceptOutcome::PayloadConflict(_) => return Err(CommandError::PayloadConflict),
                    AcceptOutcome::IdentityConflict(_) => {
                        return Err(CommandError::IdentityConflict);
                    }
                };
                (record, true)
            }
            Err(error) => return Err(error),
        };
        if matches!(record.state, CommandState::Succeeded | CommandState::Failed) {
            return Ok(CommandOutcome {
                record,
                replayed: true,
            });
        }
        let observation = match self.desired_revision_fence(envelope)? {
            DesiredRevisionFence::Current => observation,
            DesiredRevisionFence::Gap
                if rebuilt_journal && observation == ExternalEffectObservation::AppliedExact =>
            {
                observation
            }
            DesiredRevisionFence::Superseded => {
                ExternalEffectObservation::SupersededByNewerRevision
            }
            DesiredRevisionFence::Gap => return Err(CommandError::Rejected("revision_gap")),
        };
        let (state, result, code) = match observation {
            // Observation can authorize a retry after proven absence, but it
            // cannot replace the exact root receipt required for any terminal
            // privileged outcome.
            ExternalEffectObservation::AppliedExact => {
                (CommandState::Unknown, None, Some("attested_result_missing"))
            }
            ExternalEffectObservation::SupersededByNewerRevision => (
                CommandState::Unknown,
                None,
                Some("attested_result_superseded"),
            ),
            ExternalEffectObservation::Absent => {
                (CommandState::Unknown, None, Some("effect_absent"))
            }
            ExternalEffectObservation::Unknown => (
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
        self.require_current_desired_revision(envelope)?;
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
            accepted_at: record.accepted_at,
        })
    }

    fn desired_revision_fence(
        &self,
        envelope: &CommandEnvelope,
    ) -> Result<DesiredRevisionFence, CommandError> {
        let Some((resource_type, resource_key, desired_revision)) = desired_resource(envelope)
        else {
            return Ok(DesiredRevisionFence::Current);
        };
        let expected_previous = desired_revision
            .checked_sub(1)
            .ok_or(CommandError::Rejected("desired_revision_invalid"))?;
        let local_revision = self
            .journal
            .applied_revision(resource_type, resource_key)
            .map_err(pre_effect_failure)?
            .unwrap_or(0);
        Ok(match local_revision.cmp(&expected_previous) {
            std::cmp::Ordering::Equal => DesiredRevisionFence::Current,
            std::cmp::Ordering::Greater => DesiredRevisionFence::Superseded,
            std::cmp::Ordering::Less => DesiredRevisionFence::Gap,
        })
    }

    fn require_current_desired_revision(
        &self,
        envelope: &CommandEnvelope,
    ) -> Result<(), CommandError> {
        match self.desired_revision_fence(envelope)? {
            DesiredRevisionFence::Current => Ok(()),
            DesiredRevisionFence::Superseded => Err(CommandError::Rejected(
                "effect_superseded_by_newer_revision",
            )),
            DesiredRevisionFence::Gap => Err(CommandError::Rejected("revision_gap")),
        }
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
        if envelope.protocol_version
            == ocservia_command_authorization::LEGACY_COMMAND_PROTOCOL_VERSION
        {
            return self.reconcile(envelope, context);
        }
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

    #[must_use]
    pub fn journal_mut(&mut self) -> &mut Journal {
        &mut self.journal
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

fn desired_resource(envelope: &CommandEnvelope) -> Option<(&'static str, &str, u64)> {
    match envelope.payload.as_ref()? {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::GroupApply(value) => {
            Some(("group", &value.group_name, value.desired_revision))
        }
        command_envelope::Payload::CompleteConfigApply(value) => {
            Some(("config", "ocserv.conf", value.desired_revision))
        }
        _ => None,
    }
}

#[cfg(test)]
fn observed_result(envelope: &CommandEnvelope) -> Vec<u8> {
    match envelope.payload.as_ref() {
        Some(command_envelope::Payload::CompleteConfigApply(payload)) => ConfigApplyResult {
            candidate_hash: payload.candidate_hash.clone(),
            previous_hash: Vec::new(),
            observed_hash: payload.candidate_hash.clone(),
            applied_revision: payload.desired_revision,
            healthy: true,
            rolled_back: false,
            failed_critical: false,
            failure_code: String::new(),
        }
        .encode_to_vec(),
        _ => b"observed".to_vec(),
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

/// Reproduces the historical UNSPECIFIED payload hash used by synthetic Noop/Echo
/// compatibility vectors. This test helper also retains the historical typed-payload
/// cases; its Protobuf encoding is not the current canonical V1/V2 identity.
///
/// # Errors
///
/// Rejects unsupported payload types.
#[cfg(test)]
fn legacy_synthetic_payload_hash(envelope: &CommandEnvelope) -> Result<[u8; 32], CommandError> {
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
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            ("ocserv.certificate.issue", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            ("ocserv.certificate.issue", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            ("ocserv.certificate.revoke", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            ("ocserv.users.write", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            ("ocserv.users.write", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            ("ocserv.users.write", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            ("ocserv.users.write", payload.encode_to_vec())
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            ("ocserv.groups.write", payload.encode_to_vec())
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
    ocservia_command_authorization::semantic_payload_hash_v1(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))
}

/// Computes semantic hash v2, which additionally binds the `ConfigPlan` desired
/// state revision while preserving v1 as a frozen compatibility algorithm.
///
/// # Errors
///
/// Rejects malformed or unsupported typed command payloads.
pub fn semantic_payload_hash_v2(envelope: &CommandEnvelope) -> Result<[u8; 32], CommandError> {
    ocservia_command_authorization::semantic_payload_hash_v2(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))
}

fn validate_command(
    envelope: &CommandEnvelope,
    context: &CommandContext,
) -> Result<ValidatedCommand, CommandError> {
    let encoded_len = envelope.encoded_len();
    if encoded_len == 0 || encoded_len > MAX_COMMAND_BYTES {
        return Err(CommandError::Rejected("command_size_invalid"));
    }
    if envelope.protocol_version != ocservia_command_authorization::COMMAND_PROTOCOL_VERSION
        && envelope.protocol_version
            != ocservia_command_authorization::LEGACY_COMMAND_PROTOCOL_VERSION
    {
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
    if expires_at <= context.now_unix_seconds
        && envelope.delivery_mode != i32::from(CommandDeliveryMode::ReconcileOnly)
    {
        return Err(CommandError::Rejected("command_expired"));
    }
    if issued_at
        > context
            .now_unix_seconds
            .saturating_add(MAX_FUTURE_SKEW_SECONDS)
    {
        return Err(CommandError::Rejected("clock_skew"));
    }
    if context.authorization_revision == 0
        || envelope.expected_revision != context.authorization_revision
    {
        return Err(CommandError::Rejected("revision_mismatch"));
    }
    if context.session_expires_at_unix_seconds <= context.now_unix_seconds {
        return Err(CommandError::Rejected("session_grant_expired"));
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
    let hash_version = SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
        .map_err(|_| CommandError::Rejected("semantic_hash_version_unsupported"))?;
    let payload_hash = verify_semantic_payload_hash(envelope, hash_version)?;
    context
        .command_keys
        .verify(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))?;
    Ok(ValidatedCommand {
        key,
        command_id,
        payload_hash,
        hash_version,
        result,
        external,
    })
}

fn verify_semantic_payload_hash(
    envelope: &CommandEnvelope,
    version: SemanticPayloadHashVersion,
) -> Result<[u8; 32], CommandError> {
    let recomputed = match version {
        SemanticPayloadHashVersion::V1 => semantic_payload_hash_v1(envelope)?,
        SemanticPayloadHashVersion::V2 => semantic_payload_hash_v2(envelope)?,
        SemanticPayloadHashVersion::Unspecified => {
            return Err(CommandError::Rejected("semantic_hash_version_unsupported"));
        }
    };
    let expected: [u8; 32] = envelope
        .semantic_payload_sha256
        .as_slice()
        .try_into()
        .map_err(|_| CommandError::Rejected("semantic_payload_hash_missing"))?;
    if recomputed != expected {
        return Err(CommandError::Rejected("semantic_payload_hash_mismatch"));
    }
    Ok(recomputed)
}

#[allow(clippy::too_many_lines)]
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
        Some(command_envelope::Payload::CompleteConfigPlan(_)) => {
            semantic_payload_hash_v2(envelope)?;
            (
                ocservia_contracts::config_profile::PLAN_CAPABILITY,
                Vec::new(),
                true,
            )
        }
        Some(command_envelope::Payload::CompleteConfigApply(_)) => {
            semantic_payload_hash_v2(envelope)?;
            (
                ocservia_contracts::config_profile::APPLY_CAPABILITY,
                Vec::new(),
                true,
            )
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.common_name.is_empty()
                || payload.common_name.len() > 253
                || payload.dns_names.len() > 32
                || !matches!(payload.key_bits, 2048 | 3072 | 4096)
            {
                return Err(CommandError::Rejected("certificate_csr_invalid"));
            }
            ("ocserv.certificate.issue", Vec::new(), true)
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.artifact_id.len() != 16
                || payload.certificate_chain_pem.len() < 64
                || payload.certificate_chain_pem.len() > 256 * 1024
                || !payload.sealed_password.is_empty()
                || !payload.secret_key_id.is_empty()
                || payload.certificate_version == 0
                || payload.artifact_expires_at.is_none()
            {
                return Err(CommandError::Rejected("certificate_p12_invalid"));
            }
            validate_typed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::CertificateP12Password,
            )?;
            ("ocserv.certificate.issue", Vec::new(), true)
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.reason.is_empty()
                || payload.reason.len() > 128
                || payload.certificate_version == 0
            {
                return Err(CommandError::Rejected("certificate_revoke_invalid"));
            }
            ("ocserv.certificate.revoke", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            validate_name(&payload.username)?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            validate_name(&payload.username)?;
            validate_revision(payload.desired_revision)?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            validate_name(&payload.username)?;
            validate_revision(payload.desired_revision)?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            validate_name(&payload.username)?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            validate_name(&payload.group_name)?;
            validate_revision(payload.desired_revision)?;
            if payload.members.len() > ocservia_agent_protocol::MAX_MANAGED_RESOURCES {
                return Err(CommandError::Rejected("group_members_invalid"));
            }
            let mut previous = None;
            for member in &payload.members {
                validate_name(member)?;
                if previous.is_some_and(|value: &String| value >= member) {
                    return Err(CommandError::Rejected("group_members_invalid"));
                }
                previous = Some(member);
            }
            ("ocserv.groups.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            validate_agent_upgrade(payload)?;
            (
                ocservia_contracts::agent_upgrade::supported_capability(
                    &envelope.required_capability,
                )
                .ok_or(CommandError::Rejected("capability_rejected"))?,
                Vec::new(),
                true,
            )
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    })
}

/// Validates the typed upgrade release identity before any external effect.
/// The same checks are repeated independently by privd after its own
/// Controller-authorization verification.
fn validate_agent_upgrade(
    payload: &ocservia_contracts::generated::ocserv::platform::agent::v1::AgentUpgrade,
) -> Result<(), CommandError> {
    if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version) {
        return Err(CommandError::Rejected("agent_upgrade_version_invalid"));
    }
    if payload.package_sha256.len() != 32 {
        return Err(CommandError::Rejected("agent_upgrade_digest_invalid"));
    }
    if !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture) {
        return Err(CommandError::Rejected("agent_upgrade_architecture_invalid"));
    }
    if ocservia_contracts::agent_upgrade::runtime_architecture()
        != Some(payload.architecture.as_str())
    {
        return Err(CommandError::Rejected(
            "agent_upgrade_architecture_mismatch",
        ));
    }
    Ok(())
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

fn validate_name(value: &str) -> Result<(), CommandError> {
    if value.is_empty()
        || value.len() > 64
        || !value.as_bytes().iter().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'_' | b'.' | b'-'))
        })
    {
        return Err(CommandError::Rejected("name_invalid"));
    }
    Ok(())
}

fn validate_revision(value: u64) -> Result<(), CommandError> {
    if value == 0 {
        return Err(CommandError::Rejected("revision_invalid"));
    }
    Ok(())
}
fn validate_sealed_secret(
    secret: Option<&SealedSecretV1>,
    legacy_secret: &[u8],
    legacy_key_id: &str,
    revision: u64,
) -> Result<(), CommandError> {
    validate_revision(revision)?;
    if !legacy_secret.is_empty() || !legacy_key_id.is_empty() {
        return Err(CommandError::Rejected("sealed_secret_invalid"));
    }
    validate_typed_secret(secret, SealedSecretPurpose::UserPassword)
}

fn validate_typed_secret(
    secret: Option<&SealedSecretV1>,
    purpose: SealedSecretPurpose,
) -> Result<(), CommandError> {
    let secret = secret.ok_or(CommandError::Rejected("sealed_secret_invalid"))?;
    if SealedSecretVersion::try_from(secret.version).ok() != Some(SealedSecretVersion::V1)
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(CommandError::Rejected("sealed_secret_invalid"));
    }
    Ok(())
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
            // Leave half of the 30-second connection-owner takeover budget
            // for endpoint recovery, handshake, and fence persistence.
            cap: Duration::from_secs(15),
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
    desired_timeout: Duration,
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
            self.timeout,
            None,
        )
        .await
    }

    /// Sends one original Controller-signed command for independent privd
    /// verification and execution.
    ///
    /// # Errors
    ///
    /// Returns an I/O or deadline error.
    pub async fn call_command(
        &self,
        command: &CommandEnvelope,
        accepted_at: i64,
    ) -> Result<PrivdResponse, io::Error> {
        self.call_inner(
            None,
            Some(command),
            PrivilegedRequestMode::Execute,
            self.desired_timeout,
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
            self.desired_timeout,
            None,
        )
        .await
    }

    async fn call_inner(
        &self,
        operation: Option<privd_request::Operation>,
        command: Option<&CommandEnvelope>,
        mode: PrivilegedRequestMode,
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
    pub async fn upgrade_results(&self) -> Result<Vec<UpgradeOperationResult>, io::Error> {
        let response = self
            .call(privd_request::Operation::UpgradeResultList(
                UpgradeResultListRequest {},
            ))
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
#[path = "lib_tests.rs"]
mod tests;
