package mysql

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

func (s userOperationsStore) Metrics(ctx context.Context, workspace uuid.UUID) (v userstore.Metrics, err error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	instant, err := now.Time()
	if err != nil {
		return v, err
	}
	month, err := value.FromTime(time.Date(instant.Year(), instant.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return v, err
	}
	err = s.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM desired_user_policies policy CROSS JOIN (SELECT ? AS at,? AS month_start) clock JOIN nodes node ON node.id=policy.node_id JOIN desired_users desired ON desired.node_id=policy.node_id AND BINARY desired.username=BINARY policy.username LEFT JOIN observed_users observed ON observed.node_id=policy.node_id AND BINARY observed.username=BINARY policy.username LEFT JOIN observed_user_usage consumption ON consumption.node_id=policy.node_id AND BINARY consumption.username=BINARY policy.username AND consumption.period=CASE WHEN policy.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND consumption.period_start=CASE WHEN policy.quota_period='monthly' THEN clock.month_start ELSE -946684800000000 END WHERE node.workspace_id=? AND ((desired.enabled=true AND ((policy.expires_at IS NOT NULL AND policy.expires_at<=clock.at) OR (policy.quota_period<>'none' AND CASE policy.quota_direction WHEN 'rx' THEN CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0)) WHEN 'tx' THEN CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) ELSE CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0))+CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) END>=policy.quota_bytes))) OR (EXISTS(SELECT 1 FROM user_policy_enforcements enforcement WHERE enforcement.node_id=policy.node_id AND BINARY enforcement.username=BINARY policy.username AND enforcement.policy_version=policy.version AND enforcement.operation_id IS NOT NULL) AND (observed.username IS NULL OR observed.enabled<>desired.enabled OR observed.revision<>desired.revision)))),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=? AND item.state IN('queued','submitting','submitted','offline_pending','unknown')),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=? AND item.state='submitting' AND item.lease_until<=?),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=? AND item.state='unknown')`, now, month, UUIDBytes(workspace), UUIDBytes(workspace), UUIDBytes(workspace), now, UUIDBytes(workspace)).Scan(&v.PolicyPendingTotal, &v.ActiveBatchItemTotal, &v.StaleBatchClaimTotal, &v.UnknownBatchItemTotal)
	return
}

func (s userOperationsStore) ActiveOperations(ctx context.Context) (count int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown')`).Scan(&count)
	return
}

func (s userOperationsStore) ResetCandidates(ctx context.Context, now, month value.Timestamp, limit int) ([]userstore.Candidate, error) {
	rows, err := s.Query(ctx, `SELECT p.node_id,p.username,p.version,u.version,u.enabled,prior.resulting_user_version
 FROM desired_user_policies p CROSS JOIN (SELECT ? AS month_start,? AS at) clock JOIN desired_users u ON u.node_id=p.node_id AND BINARY u.username=BINARY p.username
 LEFT JOIN observed_user_usage consumption ON consumption.node_id=p.node_id AND BINARY consumption.username=BINARY p.username AND consumption.period='monthly' AND consumption.period_start=clock.month_start
 JOIN user_policy_enforcements prior ON prior.exact_row_id=(SELECT previous.exact_row_id FROM user_policy_enforcements previous WHERE previous.node_id=p.node_id AND BINARY previous.username=BINARY p.username AND previous.policy_version=p.version AND previous.cause='quota' AND previous.period_start<clock.month_start AND previous.operation_id IS NOT NULL ORDER BY previous.period_start DESC LIMIT 1)
 WHERE p.quota_period='monthly' AND (p.expires_at IS NULL OR p.expires_at>clock.at)
 AND CASE p.quota_direction WHEN 'rx' THEN CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0)) WHEN 'tx' THEN CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) ELSE CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0))+CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) END<p.quota_bytes
 AND ((u.enabled=false AND prior.resulting_user_version=u.version) OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND BINARY pending.username=BINARY p.username AND pending.policy_version=p.version AND pending.cause='quota_reset' AND pending.period_start=clock.month_start AND (pending.source_user_version=u.version OR (u.enabled=true AND pending.source_user_version=u.version-1)) AND pending.operation_id IS NULL))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements reset WHERE reset.node_id=p.node_id AND BINARY reset.username=BINARY p.username AND reset.policy_version=p.version AND reset.cause='quota_reset' AND reset.period_start=clock.month_start AND reset.operation_id IS NOT NULL)
 ORDER BY p.node_id,BINARY p.username LIMIT ?`, month, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []userstore.Candidate
	for rows.Next() {
		v := userstore.Candidate{Cause: "quota_reset", PeriodStart: month}
		if err := rows.Scan(&v.NodeID, &v.Username, &v.PolicyVersion, &v.UserVersion, &v.Enabled, &v.EnforcedVersion); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s userOperationsStore) EnforcementCandidates(ctx context.Context, now, month value.Timestamp, limit int) ([]userstore.Candidate, error) {
	rows, err := s.Query(ctx, `SELECT p.node_id,p.username,p.version,u.version,
 CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=clock.at THEN 'expiry' ELSE 'quota' END,
 CASE WHEN p.quota_period='monthly' THEN clock.month_start ELSE -946684800000000 END,u.enabled
 FROM desired_user_policies p CROSS JOIN (SELECT ? AS at,? AS month_start) clock JOIN desired_users u ON u.node_id=p.node_id AND BINARY u.username=BINARY p.username
 LEFT JOIN observed_user_usage consumption ON consumption.node_id=p.node_id AND BINARY consumption.username=BINARY p.username AND consumption.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND consumption.period_start=CASE WHEN p.quota_period='monthly' THEN clock.month_start ELSE -946684800000000 END
 WHERE ((p.expires_at IS NOT NULL AND p.expires_at<=clock.at) OR (p.quota_period<>'none' AND CASE p.quota_direction WHEN 'rx' THEN CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0)) WHEN 'tx' THEN CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) ELSE CAST(COALESCE(consumption.rx_bytes,0) AS DECIMAL(65,0))+CAST(COALESCE(consumption.tx_bytes,0) AS DECIMAL(65,0)) END>=p.quota_bytes))
 AND (u.enabled=true OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND BINARY pending.username=BINARY p.username AND pending.policy_version=p.version AND pending.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=clock.at THEN 'expiry' ELSE 'quota' END AND pending.period_start=CASE WHEN p.quota_period='monthly' THEN clock.month_start ELSE -946684800000000 END AND pending.source_user_version IN(u.version,u.version-1) AND pending.operation_id IS NULL))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements e WHERE e.node_id=p.node_id AND BINARY e.username=BINARY p.username AND e.policy_version=p.version AND e.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=clock.at THEN 'expiry' ELSE 'quota' END AND e.period_start=CASE WHEN p.quota_period='monthly' THEN clock.month_start ELSE -946684800000000 END AND e.operation_id IS NOT NULL)
 ORDER BY p.node_id,BINARY p.username LIMIT ?`, now, month, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []userstore.Candidate
	for rows.Next() {
		var v userstore.Candidate
		if err := rows.Scan(&v.NodeID, &v.Username, &v.PolicyVersion, &v.UserVersion, &v.Cause, &v.PeriodStart, &v.Enabled); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s userOperationsStore) EnsureEnforcement(ctx context.Context, v userstore.Candidate, at value.Timestamp) error {
	// The unbounded natural key is enforced by a trigger, not a native unique
	// index. Serialize the existence check instead of swallowing trigger errors.
	if err := LockTransaction(ctx, s.Tx, "policy-enforcement:"+v.NodeID.String()); err != nil {
		return err
	}
	var exists bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_policy_enforcements WHERE node_id=? AND BINARY username=? AND policy_version=? AND cause=? AND period_start=?)`, UUIDBytes(v.NodeID), v.Username, v.PolicyVersion, v.Cause, v.PeriodStart).Scan(&exists)
	if err != nil || exists {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at)VALUES(?,?,?,?,?,?,?)`, UUIDBytes(v.NodeID), v.Username, v.PolicyVersion, v.Cause, v.PeriodStart, v.UserVersion, at)
	return err
}

func (s userOperationsStore) DeleteEnforcement(ctx context.Context, v userstore.Candidate, checkSource bool) error {
	_, err := s.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=? AND BINARY username=? AND policy_version=? AND cause=? AND period_start=? AND operation_id IS NULL AND (NOT ? OR source_user_version=?)`, UUIDBytes(v.NodeID), v.Username, v.PolicyVersion, v.Cause, v.PeriodStart, checkSource, v.UserVersion)
	return err
}

func (s userOperationsStore) CompleteEnforcement(ctx context.Context, v userstore.Candidate, operation uuid.UUID, version int64) error {
	_, err := s.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=?,resulting_user_version=? WHERE node_id=? AND BINARY username=? AND policy_version=? AND cause=? AND period_start=? AND operation_id IS NULL`, UUIDBytes(operation), version, UUIDBytes(v.NodeID), v.Username, v.PolicyVersion, v.Cause, v.PeriodStart)
	return err
}

func (s userOperationsStore) FindUserOperation(ctx context.Context, node uuid.UUID, name, key, kind string) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT op.id FROM operations op JOIN commands command ON command.operation_id=op.id WHERE op.node_id=? AND BINARY op.idempotency_key=? AND command.resource_type='user' AND BINARY command.resource_key=? AND BINARY command.payload_type=?`, UUIDBytes(node), key, name, kind).Scan(&id)
	return
}
