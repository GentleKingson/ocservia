package postgres

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
	err = s.QueryRow(ctx, `SELECT id FROM outbox_events WHERE command_id=$1 FOR UPDATE`, command).Scan(&outbox)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return v, err
	}
	// A separate statement establishes outbox-before-command ordering and a
	// fresh snapshot after waiting for a concurrent dispatch completion.
	err = s.QueryRow(ctx, `SELECT c.envelope,c.state,c.created_at,f.attempt_id IS NOT NULL,
		COALESCE(f.attempt_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(f.lease_token,'00000000-0000-0000-0000-000000000000'::uuid)
		FROM commands c JOIN operations p ON p.command_id=c.id
		LEFT JOIN LATERAL (SELECT a.id AS attempt_id,l.lease_token FROM outbox_events o
		 JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
		 JOIN command_attempts a ON a.command_id=l.command_id AND a.outbox_event_id=o.id AND a.worker_id=l.worker_id
		 WHERE o.command_id=c.id AND l.leased_until>clock_timestamp() AND o.locked_by=l.worker_id AND o.locked_until>clock_timestamp()
		 AND a.attempt_number=o.attempts AND a.state='sending' AND a.finished_at IS NULL LIMIT 1) f ON true
		WHERE c.id=$1 AND c.node_id=$2`, command, node).Scan(&v.Envelope, &v.State, &v.CreatedAt, &v.DispatchInFlight, &v.AttemptID, &v.LeaseToken)
	return
}

func (s commandResultStore) Receipt(ctx context.Context, key string, effect []byte, sequence uint64) (id uuid.UUID, digest []byte, err error) {
	err = s.QueryRow(ctx, `SELECT command_id,receipt_sha256 FROM agent_command_results WHERE privd_attestation_key_id=$1 AND effect_record_id=$2 AND effect_sequence=$3 AND receipt_verification_status='verified'`, key, effect, sequence).Scan(&id, &digest)
	return
}

func (s commandResultStore) InsertResult(ctx context.Context, v resultstore.Result) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,error_code,accepted_at,completed_at,replayed,created_at,receipt_verification_status,receipt_failure_reason,privd_attestation_key_id,effect_record_id,effect_sequence,receipt_sha256,privileged_result_proof)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,clock_timestamp(),$12,$13,$14,$15,$16,$17,$18)`, v.EventID, v.CommandID, v.IdempotencyKey, v.PayloadHash, v.HashVersion, v.State, v.Bytes, v.ErrorCode, v.AcceptedAt, v.CompletedAt, v.Replayed, v.VerificationStatus, v.FailureReason, v.KeyID, v.EffectRecordID, v.EffectSequence, v.ReceiptHash, v.Proof)
	return err
}

func (s commandResultStore) Workspace(ctx context.Context, node uuid.UUID) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1`, node).Scan(&id)
	return
}
func (s commandResultStore) Alert(ctx context.Context, id, workspace uuid.UUID, severity, kind string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,created_at)VALUES($1,$2,$3,$4,$5)`, id, workspace, severity, kind, at)
	return err
}
func (s commandResultStore) UpdateCommand(ctx context.Context, id uuid.UUID, state string, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE commands SET state=$2,updated_at=GREATEST(updated_at,$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, id, state, at)
	return n != 0, err
}
func (s commandResultStore) UpdateOperation(ctx context.Context, command uuid.UUID, state string, terminal bool, at value.Timestamp) (v resultstore.ResultOperation, err error) {
	err = s.QueryRow(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=GREATEST(updated_at,$3),completed_at=CASE WHEN $4::boolean THEN GREATEST(COALESCE(completed_at,$3),$3) ELSE NULL END WHERE command_id=$1 AND state IN ('queued','dispatched','accepted','running','unknown') RETURNING id,workspace_id,request_id,COALESCE(trace_id,'')`, command, state, at, terminal).Scan(&v.ID, &v.WorkspaceID, &v.RequestID, &v.TraceID)
	return
}
func (s commandResultStore) CompleteOutbox(ctx context.Context, command uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, command, at)
	return err
}
func (s commandResultStore) CloseDispatch(ctx context.Context, node, command, attempt, lease uuid.UUID) error {
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=clock_timestamp() WHERE id=$1 AND command_id=$2 AND state='sending' AND finished_at IS NULL`, attempt, command)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=$1 AND command_id=$2 AND lease_token=$3`, node, command, lease)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s commandResultStore) ConfigOutcome(ctx context.Context, v resultstore.ConfigOutcome) (bool, error) {
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state=$2,failure_code=$3,updated_at=$4 WHERE operation_id=$1`, v.OperationID, v.State, v.FailureCode, v.At); err != nil {
		return false, err
	}
	if v.State == "succeeded" {
		n, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,candidate_hash,redacted_config,automation_locked,last_apply_operation_id,updated_at)
		 SELECT x.node_id,$2,$2,$3,p.candidate_redacted,false,$1,$4 FROM config_apply_operations x JOIN config_plans p ON p.id=x.plan_id WHERE x.operation_id=$1 AND x.node_id=$5
		 ON CONFLICT(node_id) DO UPDATE SET revision=EXCLUDED.revision,desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),candidate_hash=EXCLUDED.candidate_hash,redacted_config=EXCLUDED.redacted_config,automation_locked=false,automation_lock_reason=NULL,last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, v.OperationID, v.Revision, v.CandidateHash, v.At, v.NodeID)
		return n == 1, err
	}
	if v.State == "failed_critical" {
		if _, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,automation_locked,automation_lock_reason,last_apply_operation_id,updated_at)
		 VALUES($1,0,$4,'',true,'config_apply_rollback_failed',$2,$3) ON CONFLICT(node_id) DO UPDATE SET desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),automation_locked=true,automation_lock_reason='config_apply_rollback_failed',last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, v.NodeID, v.OperationID, v.At, v.Revision); err != nil {
			return false, err
		}
		return true, s.Alert(ctx, v.AlertID, v.WorkspaceID, "critical", "config_apply.rollback_failed", v.At)
	}
	return true, nil
}
func (s commandResultStore) CSROutcome(ctx context.Context, v resultstore.CSROutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,csr_der=COALESCE($3,csr_der),public_key_sha256=COALESCE($4,public_key_sha256),csr_receipt_verified_at=COALESCE($7,csr_receipt_verified_at),csr_receipt_sha256=COALESCE($8,csr_receipt_sha256),csr_privd_attestation_key_id=COALESCE($9,csr_privd_attestation_key_id),csr_effect_record_id=COALESCE($10,csr_effect_record_id),csr_der_sha256=COALESCE($11,csr_der_sha256),csr_requested_subject_sha256=COALESCE($12,csr_requested_subject_sha256),updated_at=$5 WHERE id=$1 AND operation_id=$6`, v.CertificateID, v.State, v.DER, v.PublicHash, v.At, v.OperationID, v.VerifiedAt, v.ReceiptHash, v.KeyID, v.EffectRecordID, v.CSRHash, v.SubjectHash)
	return err
}
func (s commandResultStore) RevokeOutcome(ctx context.Context, v resultstore.RevokeOutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,revoked_at=COALESCE($3,revoked_at),revocation_reason=CASE WHEN $2='revoked' THEN $4 ELSE revocation_reason END,updated_at=$5 WHERE id=$1 AND node_id=$6`, v.CertificateID, v.State, v.RevokedAt, v.Reason, v.At, v.NodeID)
	return err
}
func (s commandResultStore) ArtifactOutcome(ctx context.Context, v resultstore.ArtifactOutcome) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state=$2,content_sha256=COALESCE($3,content_sha256),content_size=COALESCE($4,content_size),updated_at=$5 WHERE id=$1 AND operation_id=$6`, v.ArtifactID, v.State, v.Digest, v.Size, v.At, v.OperationID)
	return err
}
func (s commandResultStore) UpgradeOutcome(ctx context.Context, operation uuid.UUID, state string, scheduled, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,scheduled_at=COALESCE($3,scheduled_at),updated_at=$4 WHERE operation_id=$1`, operation, state, scheduled, at)
	return err
}
func (s commandResultStore) AppendEvent(ctx context.Context, id, operation uuid.UUID, state string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,$3,$4)`, id, operation, state, at)
	return err
}
func (s commandResultStore) ScheduleRecovery(ctx context.Context, command uuid.UUID, payload []byte, expires, at value.Timestamp) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,expires_at=$3 WHERE id=$1 AND state='unknown'`, command, payload, expires); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET expires_at=$2 WHERE command_id=$1 AND state='unknown'`, command, expires); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error=NULL WHERE command_id=$1`, command, payload, at)
	return err
}
