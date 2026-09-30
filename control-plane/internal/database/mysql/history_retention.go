package mysql

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/google/uuid"
)

type historyRetentionStore struct{ database.Tx }

func (t *transaction) HistoryRetentionStore() historyretention.Store { return historyRetentionStore{t} }

const retentionEligible = `c.details_compacted_at IS NULL AND c.payload_type<>'config_plan' AND c.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND p.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND c.updated_at<? AND c.expires_at<? AND p.completed_at<?
 AND o.published_at IS NOT NULL AND o.locked_by IS NULL
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=c.id AND a.state IN ('sending','unknown'))
 AND NOT EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='unknown')
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=p.id AND u.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('consumed','expired','revoked','failed'))`

func (s historyRetentionStore) Candidates(ctx context.Context, cutoff value.Timestamp) ([]uuid.UUID, error) {
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS (SELECT 1 FROM commands c JOIN operations p ON p.id=c.operation_id WHERE c.id=o.command_id AND `+retentionEligible+`) ORDER BY o.command_id LIMIT 32 FOR UPDATE SKIP LOCKED`, cutoff, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s historyRetentionStore) LockCommand(ctx context.Context, id uuid.UUID, cutoff value.Timestamp) (v historyretention.Command, err error) {
	err = s.QueryRow(ctx, `SELECT c.id,c.node_id,c.envelope FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id WHERE c.details_compacted_at IS NULL AND c.payload_type<>'config_plan' AND c.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND p.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND c.updated_at<? AND c.expires_at<? AND p.completed_at<?
 AND o.published_at IS NOT NULL AND o.locked_by IS NULL
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=c.id AND a.state IN ('sending','unknown'))
 AND NOT EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='unknown')
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=p.id AND u.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('consumed','expired','revoked','failed')) AND c.id=? FOR UPDATE`, cutoff, cutoff, cutoff, UUIDBytes(id)).Scan(&v.ID, &v.NodeID, &v.Envelope)
	return
}
func (s historyRetentionStore) Compact(ctx context.Context, c historyretention.Command, header, digest []byte, at value.Timestamp) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=?,envelope_sha256=?,details_compacted_at=? WHERE id=?`, header, digest, at, UUIDBytes(c.ID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=?,last_error=NULL WHERE command_id=?`, header, UUIDBytes(c.ID)); err != nil {
		return err
	}
	return nil
}

func (s historyRetentionStore) CompactRetired(ctx context.Context, cutoff value.Timestamp) error {
	rows, err := s.Query(ctx, `SELECT n.id FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.status='revoked' AND k.state='revoked' AND k.revoked_at<?
 AND NOT EXISTS(SELECT 1 FROM commands c WHERE c.node_id=n.id AND c.state IN ('queued','dispatched','accepted','running','unknown'))
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=n.id)
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.node_id=n.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.state NOT IN ('succeeded','failed','rolled_back'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.node_id=n.id AND a.state NOT IN ('consumed','expired','revoked','failed'))
 AND (EXISTS(SELECT 1 FROM node_sessions s WHERE s.node_id=n.id) OR EXISTS(SELECT 1 FROM node_observed_snapshots s WHERE s.node_id=n.id AND (s.ocserv<>'{}' OR s.system<>'{}' OR s.path<>'{}'))) ORDER BY k.revoked_at,n.id LIMIT 32 FOR UPDATE SKIP LOCKED`, cutoff)
	if err != nil {
		return err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Exec(ctx, `DELETE FROM node_sessions WHERE node_id=? ORDER BY session_id LIMIT 32`, UUIDBytes(id)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, "UPDATE node_observed_snapshots SET ocserv='{}',`system`='{}',path='{}' WHERE node_id=?", UUIDBytes(id)); err != nil {
			return err
		}
	}
	return nil
}
