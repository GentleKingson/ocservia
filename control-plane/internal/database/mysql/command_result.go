package mysql

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

type commandResultStore struct{ database.Tx }

func (t *transaction) CommandResultStore() resultstore.ResultStore { return commandResultStore{t} }

func (s commandResultStore) LoadCommand(ctx context.Context, command, node uuid.UUID) (v resultstore.ResultCommand, err error) {
	var outbox uuid.UUID
	err = s.QueryRow(ctx, `SELECT id FROM outbox_events WHERE command_id=? FOR UPDATE`, UUIDBytes(command)).Scan(&outbox)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return v, err
	}
	// The outer ingress transaction owns the node. Lock the outbox separately
	// before reading dispatch state, matching completion and reaping order.
	err = s.QueryRow(ctx, `SELECT c.envelope,c.state,c.created_at,a.id IS NOT NULL,
		COALESCE(a.id,UNHEX(REPEAT('00',16))),IF(a.id IS NULL,UNHEX(REPEAT('00',16)),l.lease_token)
		FROM commands c JOIN operations p ON p.command_id=c.id
		LEFT JOIN outbox_events o ON o.command_id=c.id
		LEFT JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
		 AND l.leased_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))
		 AND o.locked_by=l.worker_id AND o.locked_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))
		LEFT JOIN command_attempts a ON a.command_id=l.command_id AND a.outbox_event_id=o.id AND a.worker_id=l.worker_id
		 AND a.attempt_number=o.attempts AND a.state='sending' AND a.finished_at IS NULL
		WHERE c.id=? AND c.node_id=?`, UUIDBytes(command), UUIDBytes(node)).Scan(&v.Envelope, &v.State, &v.CreatedAt, &v.DispatchInFlight, &v.AttemptID, &v.LeaseToken)
	return
}
func (s commandResultStore) Receipt(ctx context.Context, key string, effect []byte, sequence uint64) (id uuid.UUID, digest []byte, err error) {
	err = s.QueryRow(ctx, `SELECT command_id,receipt_sha256 FROM agent_command_results WHERE BINARY privd_attestation_key_id=BINARY ? AND effect_record_id=? AND effect_sequence=? AND receipt_verification_status='verified'`, key, effect, sequence).Scan(&id, &digest)
	return
}
func (s commandResultStore) InsertResult(ctx context.Context, v resultstore.Result) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,error_code,accepted_at,completed_at,replayed,created_at,receipt_verification_status,receipt_failure_reason,privd_attestation_key_id,effect_record_id,effect_sequence,receipt_sha256,privileged_result_proof)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)),?,?,?,?,?,?,?)`, UUIDBytes(v.EventID), UUIDBytes(v.CommandID), UUIDBytes(v.IdempotencyKey), v.PayloadHash, v.HashVersion, v.State, v.Bytes, v.ErrorCode, v.AcceptedAt, v.CompletedAt, v.Replayed, v.VerificationStatus, v.FailureReason, v.KeyID, v.EffectRecordID, v.EffectSequence, v.ReceiptHash, v.Proof)
	return err
}
func (s commandResultStore) Workspace(ctx context.Context, node uuid.UUID) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=?`, UUIDBytes(node)).Scan(&id)
	return
}
func (s commandResultStore) Alert(ctx context.Context, id, workspace uuid.UUID, severity, kind string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,created_at)VALUES(?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), severity, kind, at)
	return err
}
func (s commandResultStore) UpdateCommand(ctx context.Context, id uuid.UUID, state string, at value.Timestamp) (bool, error) {
	// A matched Unknown row may have an infinite updated_at and unchanged
	// state. MySQL's changed-row count must not discard that valid result.
	var current string
	err := s.QueryRow(ctx, `SELECT state FROM commands WHERE id=? AND state IN ('queued','dispatched','accepted','running','unknown') FOR UPDATE`, UUIDBytes(id)).Scan(&current)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE commands SET state=?,updated_at=GREATEST(updated_at,?) WHERE id=?`, state, at, UUIDBytes(id))
	return err == nil, err
}
func (s commandResultStore) UpdateOperation(ctx context.Context, command uuid.UUID, state string, terminal bool, at value.Timestamp) (v resultstore.ResultOperation, err error) {
	err = s.QueryRow(ctx, `SELECT id,workspace_id,request_id,COALESCE(trace_id,'') FROM operations WHERE command_id=? AND state IN ('queued','dispatched','accepted','running','unknown') FOR UPDATE`, UUIDBytes(command)).Scan(&v.ID, &v.WorkspaceID, &v.RequestID, &v.TraceID)
	if err != nil {
		return v, err
	}
	_, err = s.Exec(ctx, `UPDATE operations SET state=?,version=version+1,updated_at=GREATEST(updated_at,?),completed_at=CASE WHEN ? THEN GREATEST(COALESCE(completed_at,?),?) ELSE NULL END WHERE id=?`, state, at, terminal, at, at, UUIDBytes(v.ID))
	return
}
func (s commandResultStore) CompleteOutbox(ctx context.Context, command uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,?),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=?`, at, UUIDBytes(command))
	return err
}
func (s commandResultStore) CloseDispatch(ctx context.Context, node, command, attempt, lease uuid.UUID) error {
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)) WHERE id=? AND command_id=? AND state='sending' AND finished_at IS NULL`, UUIDBytes(attempt), UUIDBytes(command))
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=? AND command_id=? AND lease_token=?`, UUIDBytes(node), UUIDBytes(command), UUIDBytes(lease))
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}
func (s commandResultStore) ConfigOutcome(ctx context.Context, v resultstore.ConfigOutcome) (bool, error) {
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state=?,failure_code=?,updated_at=? WHERE operation_id=?`, v.State, v.FailureCode, v.At, UUIDBytes(v.OperationID)); err != nil {
		return false, err
	}
	if v.State == "succeeded" {
		var redacted string
		err := s.QueryRow(ctx, `SELECT p.candidate_redacted FROM config_apply_operations x JOIN config_plans p ON p.id=x.plan_id WHERE x.operation_id=? AND x.node_id=?`, UUIDBytes(v.OperationID), UUIDBytes(v.NodeID)).Scan(&redacted)
		if errors.Is(err, database.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_, err = s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,candidate_hash,redacted_config,automation_locked,last_apply_operation_id,updated_at)
		 VALUES(?,?,?,?,?,false,?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision),desired_revision=GREATEST(desired_revision,VALUES(desired_revision)),candidate_hash=VALUES(candidate_hash),redacted_config=VALUES(redacted_config),automation_locked=false,automation_lock_reason=NULL,last_apply_operation_id=VALUES(last_apply_operation_id),updated_at=VALUES(updated_at)`, UUIDBytes(v.NodeID), v.Revision, v.Revision, v.CandidateHash, redacted, UUIDBytes(v.OperationID), v.At)
		return err == nil, err
	}
	if v.State == "failed_critical" {
		if _, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,automation_locked,automation_lock_reason,last_apply_operation_id,updated_at)
		 VALUES(?,0,?,'',true,'config_apply_rollback_failed',?,?) ON DUPLICATE KEY UPDATE desired_revision=GREATEST(desired_revision,VALUES(desired_revision)),automation_locked=true,automation_lock_reason='config_apply_rollback_failed',last_apply_operation_id=VALUES(last_apply_operation_id),updated_at=VALUES(updated_at)`, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(v.OperationID), v.At); err != nil {
			return false, err
		}
		return true, s.Alert(ctx, v.AlertID, v.WorkspaceID, "critical", "config_apply.rollback_failed", v.At)
	}
	return true, nil
}
func (s commandResultStore) CSROutcome(ctx context.Context, v resultstore.CSROutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=?,version=version+1,csr_der=COALESCE(?,csr_der),public_key_sha256=COALESCE(?,public_key_sha256),csr_receipt_verified_at=COALESCE(?,csr_receipt_verified_at),csr_receipt_sha256=COALESCE(?,csr_receipt_sha256),csr_privd_attestation_key_id=COALESCE(?,csr_privd_attestation_key_id),csr_effect_record_id=COALESCE(?,csr_effect_record_id),csr_der_sha256=COALESCE(?,csr_der_sha256),csr_requested_subject_sha256=COALESCE(?,csr_requested_subject_sha256),updated_at=? WHERE id=? AND operation_id=?`, v.State, v.DER, v.PublicHash, v.VerifiedAt, v.ReceiptHash, v.KeyID, v.EffectRecordID, v.CSRHash, v.SubjectHash, v.At, UUIDBytes(v.CertificateID), UUIDBytes(v.OperationID))
	return err
}
func (s commandResultStore) RevokeOutcome(ctx context.Context, v resultstore.RevokeOutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=?,version=version+1,revoked_at=COALESCE(?,revoked_at),revocation_reason=CASE WHEN ?='revoked' THEN ? ELSE revocation_reason END,updated_at=? WHERE id=? AND node_id=?`, v.State, v.RevokedAt, v.State, v.Reason, v.At, UUIDBytes(v.CertificateID), UUIDBytes(v.NodeID))
	return err
}
func (s commandResultStore) ArtifactOutcome(ctx context.Context, v resultstore.ArtifactOutcome) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state=?,content_sha256=COALESCE(?,content_sha256),content_size=COALESCE(?,content_size),updated_at=? WHERE id=? AND operation_id=?`, v.State, v.Digest, v.Size, v.At, UUIDBytes(v.ArtifactID), UUIDBytes(v.OperationID))
	return err
}
func (s commandResultStore) UpgradeOutcome(ctx context.Context, operation uuid.UUID, state string, scheduled, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_upgrade_operations SET state=?,scheduled_at=COALESCE(?,scheduled_at),updated_at=? WHERE operation_id=?`, state, scheduled, at, UUIDBytes(operation))
	return err
}
func (s commandResultStore) AppendEvent(ctx context.Context, id, operation uuid.UUID, state string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,?,?)`, UUIDBytes(id), UUIDBytes(operation), state, at)
	return err
}
func (s commandResultStore) ScheduleRecovery(ctx context.Context, command uuid.UUID, payload []byte, expires, at value.Timestamp) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=?,expires_at=? WHERE id=? AND state='unknown'`, payload, expires, UUIDBytes(command)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET expires_at=? WHERE command_id=? AND state='unknown'`, expires, UUIDBytes(command)); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error=NULL WHERE command_id=?`, payload, at, UUIDBytes(command))
	return err
}
