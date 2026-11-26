//! Unprivileged Agent runtime primitives and fixed privd client.

#![forbid(unsafe_code)]

use std::collections::HashSet;
use std::fmt;

use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_command_journal::{
    AcceptOutcome, AppliedResourceRevision, CommandRecord, CommandState, Journal,
};
pub use ocservia_contracts::agent_upgrade::AGENT_UPGRADE_CAPABILITY;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope,
};

/// Maximum concurrent read-only collection tasks per Agent.
pub const MAX_READ_CONCURRENCY: usize = 4;
/// Maximum queued mutating commands. Execution itself is strictly serial.
pub const MAX_WRITE_QUEUE: usize = 8;
/// Maximum accepted command frame.
pub const MAX_COMMAND_BYTES: usize = 1024 * 1024;
pub(crate) const MAX_FUTURE_SKEW_SECONDS: i64 = 300;

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

    /// Replays a settled command or resumes an unfinished command after privd
    /// returns its cached root receipt. For a resumed command the caller must
    /// atomically persist that proof with the terminal result.
    ///
    /// # Errors
    ///
    /// Rejects non-privileged or mismatched commands and propagates journal failures.
    pub fn begin_attested_recovery(
        &mut self,
        envelope: &CommandEnvelope,
        context: &CommandContext,
    ) -> Result<ExternalPreparation, CommandError> {
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
        if matches!(record.state, CommandState::Succeeded | CommandState::Failed)
            && record.privileged_result_proof.is_some()
        {
            return Ok(ExternalPreparation::Replay(CommandOutcome {
                record,
                replayed: true,
            }));
        }
        if !matches!(
            record.state,
            CommandState::Accepted | CommandState::Unknown | CommandState::Running
        ) {
            return Err(CommandError::Rejected("reconciliation_state_invalid"));
        }
        let record = self
            .journal
            .transition_command(
                &validated.key,
                &[record.state],
                CommandState::Running,
                None,
                None,
                context.now_unix_seconds,
            )
            .map_err(pre_effect_failure)?;
        Ok(ExternalPreparation::Execute(ExternalCommand {
            key: validated.key,
            command_id: validated.command_id,
            accepted_at: record.accepted_at,
        }))
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

mod privd_client;
mod validation;

pub use crate::privd_client::{
    Backoff, PrivdClient, ensure_unprivileged, read_boot_id, read_os_release,
};
use crate::validation::{ValidatedCommand, validate_command};
pub use crate::validation::{desired_resource, semantic_payload_hash_v2};

#[cfg(test)]
#[path = "lib_tests.rs"]
mod tests;
