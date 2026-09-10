package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

func (s userOperationsStore) Metrics(ctx context.Context, workspace uuid.UUID) (v userstore.Metrics, err error) {
	err = s.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM desired_user_policies policy JOIN nodes node ON node.id=policy.node_id JOIN desired_users desired USING(node_id,username) LEFT JOIN observed_users observed USING(node_id,username) LEFT JOIN observed_user_usage usage ON usage.node_id=policy.node_id AND usage.username=policy.username AND usage.period=CASE WHEN policy.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND usage.period_start=CASE WHEN policy.quota_period='monthly' THEN date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' ELSE '1970-01-01T00:00:00Z'::timestamptz END WHERE node.workspace_id=$1 AND ((desired.enabled=true AND ((policy.expires_at IS NOT NULL AND policy.expires_at<=now()) OR (policy.quota_period<>'none' AND CASE policy.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END>=policy.quota_bytes::numeric))) OR (EXISTS(SELECT 1 FROM user_policy_enforcements enforcement WHERE enforcement.node_id=policy.node_id AND enforcement.username=policy.username AND enforcement.policy_version=policy.version AND enforcement.operation_id IS NOT NULL) AND (observed.username IS NULL OR observed.enabled<>desired.enabled OR observed.revision<>desired.revision)))),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state IN('queued','submitting','submitted','offline_pending','unknown')),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state='submitting' AND item.lease_until<=now()),
 (SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state='unknown')`, workspace).Scan(&v.PolicyPendingTotal, &v.ActiveBatchItemTotal, &v.StaleBatchClaimTotal, &v.UnknownBatchItemTotal)
	return
}

func (s userOperationsStore) ActiveOperations(ctx context.Context) (count int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown')`).Scan(&count)
	return
}

func (s userOperationsStore) ResetCandidates(ctx context.Context, now, month value.Timestamp, limit int) ([]userstore.Candidate, error) {
	rows, err := s.Query(ctx, `SELECT p.node_id,p.username,p.version,u.version,u.enabled,prior.resulting_user_version
 FROM desired_user_policies p JOIN desired_users u USING(node_id,username)
 LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period='monthly' AND usage.period_start=$1
 JOIN LATERAL (SELECT resulting_user_version FROM user_policy_enforcements prior WHERE prior.node_id=p.node_id AND prior.username=p.username AND prior.policy_version=p.version AND prior.cause='quota' AND prior.period_start<$1 AND prior.operation_id IS NOT NULL ORDER BY prior.period_start DESC LIMIT 1) prior ON true
 WHERE p.quota_period='monthly' AND (p.expires_at IS NULL OR p.expires_at>$2)
 AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END<p.quota_bytes::numeric
 AND ((u.enabled=false AND prior.resulting_user_version=u.version) OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause='quota_reset' AND pending.period_start=$1 AND (pending.source_user_version=u.version OR (u.enabled=true AND pending.source_user_version=u.version-1)) AND pending.operation_id IS NULL))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements reset WHERE reset.node_id=p.node_id AND reset.username=p.username AND reset.policy_version=p.version AND reset.cause='quota_reset' AND reset.period_start=$1 AND reset.operation_id IS NOT NULL)
 ORDER BY p.node_id,p.username LIMIT $3`, month, now, limit)
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
 CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END,
 CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,u.enabled
 FROM desired_user_policies p JOIN desired_users u USING(node_id,username)
 LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND usage.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
 WHERE ((p.expires_at IS NOT NULL AND p.expires_at<=$1) OR (p.quota_period<>'none' AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END>=p.quota_bytes::numeric))
 AND (u.enabled=true OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND pending.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND pending.source_user_version IN(u.version,u.version-1) AND pending.operation_id IS NULL))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements e WHERE e.node_id=p.node_id AND e.username=p.username AND e.policy_version=p.version AND e.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND e.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND e.operation_id IS NOT NULL)
 ORDER BY p.node_id,p.username LIMIT $3`, now, month, limit)
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
	_, err := s.Exec(ctx, `INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at)VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, v.NodeID, v.Username, v.PolicyVersion, v.Cause, v.PeriodStart, v.UserVersion, at)
	return err
}

func (s userOperationsStore) DeleteEnforcement(ctx context.Context, v userstore.Candidate, checkSource bool) error {
	_, err := s.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND operation_id IS NULL AND (NOT $6::boolean OR source_user_version=$7)`, v.NodeID, v.Username, v.PolicyVersion, v.Cause, v.PeriodStart, checkSource, v.UserVersion)
	return err
}

func (s userOperationsStore) CompleteEnforcement(ctx context.Context, v userstore.Candidate, operation uuid.UUID, version int64) error {
	_, err := s.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=$6,resulting_user_version=$7 WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND operation_id IS NULL`, v.NodeID, v.Username, v.PolicyVersion, v.Cause, v.PeriodStart, operation, version)
	return err
}

func (s userOperationsStore) FindUserOperation(ctx context.Context, node uuid.UUID, name, key, kind string) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT op.id FROM operations op JOIN commands command ON command.operation_id=op.id WHERE op.node_id=$1 AND op.idempotency_key=$2 AND command.resource_type='user' AND command.resource_key=$3 AND command.payload_type=$4`, node, key, name, kind).Scan(&id)
	return
}
