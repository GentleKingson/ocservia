package postgres

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	userstatestore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	"github.com/google/uuid"
)

type userOperationsStore struct{ *transaction }

func (t *transaction) UserOperationsStore() userstore.Store { return userOperationsStore{t} }

func (s userOperationsStore) LockUser(ctx context.Context, node uuid.UUID, name string) (workspace uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id FROM desired_users u JOIN nodes n ON n.id=u.node_id WHERE u.node_id=$1 AND u.username=$2 AND n.status IN('active','offline') FOR UPDATE OF u`, node, name).Scan(&workspace)
	return
}

func (s userOperationsStore) Mutation(ctx context.Context, workspace uuid.UUID, key string) (v userstore.Mutation, err error) {
	err = s.QueryRow(ctx, `SELECT policy_version,request_hash FROM user_policy_mutations WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, key).Scan(&v.Version, &v.Hash)
	return
}

func (s userOperationsStore) LockPolicy(ctx context.Context, node uuid.UUID, name string) (version int64, err error) {
	err = s.QueryRow(ctx, `SELECT version FROM desired_user_policies WHERE node_id=$1 AND username=$2 FOR UPDATE`, node, name).Scan(&version)
	return
}

func (s userOperationsStore) PutPolicy(ctx context.Context, v userstore.Policy) error {
	_, err := s.Exec(ctx, `INSERT INTO desired_user_policies(node_id,username,quota_period,quota_direction,quota_bytes,expires_at,version,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(node_id,username) DO UPDATE SET quota_period=EXCLUDED.quota_period,quota_direction=EXCLUDED.quota_direction,quota_bytes=EXCLUDED.quota_bytes,expires_at=EXCLUDED.expires_at,version=EXCLUDED.version,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Username, v.QuotaPeriod, v.QuotaDirection, v.QuotaBytes, v.ExpiresAt, v.Version, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s userOperationsStore) InsertMutation(ctx context.Context, v userstore.Mutation) error {
	_, err := s.Exec(ctx, `INSERT INTO user_policy_mutations(id,workspace_id,node_id,username,idempotency_key,request_hash,policy_version,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, v.ID, v.WorkspaceID, v.NodeID, v.Username, v.IdempotencyKey, v.Hash, v.Version, v.At)
	return err
}

func (s userOperationsStore) Policy(ctx context.Context, node uuid.UUID, name string, month time.Time) (v userstore.PolicyState, err error) {
	v.NodeID, v.Username = node, name
	err = s.QueryRow(ctx, `SELECT p.quota_period,p.quota_direction,p.quota_bytes,p.expires_at,p.version,p.created_at,p.updated_at,
 CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,
 COALESCE(u.rx_bytes,0),COALESCE(u.tx_bytes,0),u.observed_at,n.status,d.enabled,d.revision,o.enabled,o.revision,latest.state
 FROM desired_user_policies p JOIN nodes n ON n.id=p.node_id JOIN desired_users d ON d.node_id=p.node_id AND d.username=p.username
 LEFT JOIN observed_users o ON o.node_id=p.node_id AND o.username=p.username
 LEFT JOIN observed_user_usage u ON u.node_id=p.node_id AND u.username=p.username AND u.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND u.period_start=CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
 LEFT JOIN LATERAL (SELECT op.state FROM commands command JOIN operations op ON op.id=command.operation_id WHERE command.node_id=p.node_id AND command.resource_type='user' AND command.resource_key=p.username ORDER BY command.created_at DESC,command.id DESC LIMIT 1) latest ON true
 WHERE p.node_id=$1 AND p.username=$2`, node, name, month).Scan(&v.QuotaPeriod, &v.QuotaDirection, &v.QuotaBytes, &v.ExpiresAt, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.PeriodStart, &v.ObservedRXBytes, &v.ObservedTXBytes, &v.ObservedAt, &v.NodeStatus, &v.DesiredEnabled, &v.DesiredRevision, &v.ObservedEnabled, &v.ObservedRevision, &v.OperationState)
	return
}

func (s userOperationsStore) ActiveOperations(ctx context.Context) (count int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown')`).Scan(&count)
	return
}

func (s userOperationsStore) ResetCandidates(ctx context.Context, now, month time.Time, limit int) ([]userstore.Candidate, error) {
	return collectRows(ctx, s.store, `SELECT p.node_id,p.username,p.version,u.version,u.enabled,prior.resulting_user_version
 FROM desired_user_policies p JOIN nodes node ON node.id=p.node_id AND node.status IN('active','offline') JOIN desired_users u USING(node_id,username)
 LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period='monthly' AND usage.period_start=$1
 JOIN LATERAL (SELECT resulting_user_version FROM user_policy_enforcements prior WHERE prior.node_id=p.node_id AND prior.username=p.username AND prior.policy_version=p.version AND prior.cause='quota' AND prior.period_start<$1 AND prior.operation_id IS NOT NULL ORDER BY prior.period_start DESC LIMIT 1) prior ON true
 WHERE p.quota_period='monthly' AND (p.expires_at IS NULL OR p.expires_at>$2)
 AND (u.enabled OR (`+attestationReadyForNode+`))
 AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END<p.quota_bytes::numeric
 AND ((u.enabled=false AND prior.resulting_user_version=u.version) OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause='quota_reset' AND pending.period_start=$1 AND (pending.source_user_version=u.version OR (u.enabled=true AND pending.source_user_version=u.version-1)) AND pending.operation_id IS NULL))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements reset WHERE reset.node_id=p.node_id AND reset.username=p.username AND reset.policy_version=p.version AND reset.cause='quota_reset' AND reset.period_start=$1 AND reset.operation_id IS NOT NULL)
 ORDER BY p.node_id,p.username LIMIT $3`, func(r Rows) (userstore.Candidate, error) {
		v := userstore.Candidate{Cause: "quota_reset", PeriodStart: month}
		err := r.Scan(&v.NodeID, &v.Username, &v.PolicyVersion, &v.UserVersion, &v.Enabled, &v.EnforcedVersion)
		return v, err
	}, month, now, limit)
}

func (s userOperationsStore) EnforcementCandidates(ctx context.Context, now, month time.Time, limit int) ([]userstore.Candidate, error) {
	return collectRows(ctx, s.store, `SELECT p.node_id,p.username,p.version,u.version,
 CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END,
 CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,u.enabled
 FROM desired_user_policies p JOIN nodes node ON node.id=p.node_id AND node.status IN('active','offline') JOIN desired_users u USING(node_id,username)
 LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND usage.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
 WHERE ((p.expires_at IS NOT NULL AND p.expires_at<=$1) OR (p.quota_period<>'none' AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END>=p.quota_bytes::numeric))
 AND ((u.enabled AND (`+attestationReadyForNode+`)) OR (NOT u.enabled AND EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND pending.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND pending.source_user_version IN(u.version,u.version-1) AND pending.operation_id IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements e WHERE e.node_id=p.node_id AND e.username=p.username AND e.policy_version=p.version AND e.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND e.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND e.operation_id IS NOT NULL)
 ORDER BY p.node_id,p.username LIMIT $3`, func(r Rows) (v userstore.Candidate, err error) {
		err = r.Scan(&v.NodeID, &v.Username, &v.PolicyVersion, &v.UserVersion, &v.Cause, &v.PeriodStart, &v.Enabled)
		return
	}, now, month, limit)
}

func (s userOperationsStore) EnsureEnforcement(ctx context.Context, v userstore.Candidate, at time.Time) error {
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

type userStateStore struct{ *transaction }

func (t *transaction) UserStateStore() userstatestore.Store { return userStateStore{t} }

func (s userStateStore) HasSealingKey(ctx context.Context, node uuid.UUID, version uint32, key string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sealing_keys WHERE node_id=$1 AND purpose=1 AND version=$2 AND key_id=$3)`, node, version, key).Scan(&v)
	return
}

func (s userStateStore) LockDesired(ctx context.Context, node uuid.UUID, name string, group bool) (version, revision int64, err error) {
	q := `SELECT version,revision FROM desired_users WHERE node_id=$1 AND username=$2 FOR UPDATE`
	if group {
		q = `SELECT version,revision FROM desired_groups WHERE node_id=$1 AND group_name=$2 FOR UPDATE`
	}
	err = s.QueryRow(ctx, q, node, name).Scan(&version, &revision)
	return
}

func (s userStateStore) LastRevision(ctx context.Context, node uuid.UUID, resource, key string, expected int64) (v userstatestore.Revision, err error) {
	err = s.QueryRow(ctx, `SELECT c.state,c.payload_type FROM commands c WHERE c.node_id=$1 AND c.resource_type=$2 AND c.resource_key=$3 AND c.expected_version=$4 ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, node, resource, key, expected).Scan(&v.State, &v.Kind)
	return
}

func (s userStateStore) CountResources(ctx context.Context, node uuid.UUID, name string, group bool, fresh time.Time) (count int, err error) {
	if group {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT group_name FROM desired_groups WHERE node_id=$1 AND group_name<>$2 UNION SELECT o.group_name FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND o.group_name<>$2 AND s.last_heartbeat_at>=$3) resources`, node, name, fresh).Scan(&count)
	} else {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT username FROM desired_users WHERE node_id=$1 UNION SELECT o.username FROM observed_users o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND s.last_heartbeat_at>=$2) resources`, node, fresh).Scan(&count)
	}
	return
}

func (s userStateStore) CountMemberships(ctx context.Context, node uuid.UUID, name string, fresh time.Time) (count int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT d.group_name,unnest(d.members) member FROM desired_groups d WHERE d.node_id=$1 AND d.group_name<>$2 UNION SELECT o.group_name,unnest(o.members) member FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND o.group_name<>$2 AND s.last_heartbeat_at>=$3) memberships`, node, name, fresh).Scan(&count)
	return
}

func (s userStateStore) WriteDesired(ctx context.Context, v userstatestore.Desired) error {
	var n int64
	var err error
	switch v.Kind {
	case "user_create":
		_, err = s.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,created_at,updated_at)VALUES($1,$2,true,$3,$4,$5,$5) ON CONFLICT(node_id,username) DO UPDATE SET enabled=true,version=EXCLUDED.version,revision=EXCLUDED.revision,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Name, v.Version, v.Revision, v.At)
	case "group_apply":
		_, err = s.Exec(ctx, `INSERT INTO desired_groups(node_id,group_name,members,version,revision,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$6) ON CONFLICT(node_id,group_name) DO UPDATE SET members=EXCLUDED.members,version=EXCLUDED.version,revision=EXCLUDED.revision,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Name, v.Members, v.Version, v.Revision, v.At)
	case "user_enable", "user_disable":
		n, err = s.Exec(ctx, `UPDATE desired_users SET enabled=$3,version=$4,revision=$5,updated_at=$6 WHERE node_id=$1 AND username=$2`, v.NodeID, v.Name, v.Kind == "user_enable", v.Version, v.Revision, v.At)
		if err == nil && n != 1 {
			return database.ErrNotFound
		}
	case "user_password_rotate":
		n, err = s.Exec(ctx, `UPDATE desired_users SET version=$3,revision=$4,updated_at=$5 WHERE node_id=$1 AND username=$2`, v.NodeID, v.Name, v.Version, v.Revision, v.At)
		if err == nil && n != 1 {
			return database.ErrNotFound
		}
	default:
		return database.ErrUnsupported
	}
	return err
}

func (s userStateStore) SupersedePending(ctx context.Context, node uuid.UUID, resource, key, kind string, expected int64, at time.Time) (bool, error) {
	// Dispatch also locks outbox before command. Recheck eligibility after the
	// outbox lock so a concurrent claim cannot be superseded while in flight.
	commands, err := collectColumn[uuid.UUID](ctx, s.store, `SELECT o.command_id FROM outbox_events o WHERE o.locked_by IS NULL AND EXISTS(SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.node_id=$1 AND c.resource_type=$2 AND c.resource_key=$3 AND c.payload_type=$4 AND c.expected_version=$5 AND c.state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)) ORDER BY o.id FOR UPDATE`, node, resource, key, kind, expected)
	if err != nil {
		return false, err
	}
	return supersedeQueuedCommands(ctx, s.store, commands, at, "superseded by newer desired revision")
}

func (s userStateStore) List(ctx context.Context, node uuid.UUID) ([]userstatestore.Resource, error) {
	return collectRows(ctx, s.store, `WITH resources AS (
 SELECT 'user' kind,node_id,username name FROM desired_users WHERE node_id=$1
 UNION SELECT 'user',node_id,username FROM observed_users WHERE node_id=$1
 UNION SELECT 'group',node_id,group_name FROM desired_groups WHERE node_id=$1
 UNION SELECT 'group',node_id,group_name FROM observed_groups WHERE node_id=$1
 ) SELECT r.kind,r.name,n.status,du.enabled,ou.enabled,dg.members,og.members,
 COALESCE(du.version,dg.version),COALESCE(du.revision,dg.revision),COALESCE(ou.revision,og.revision),
 op.id,op.state,c.state,c.payload_type,COALESCE(ou.observed_at,og.observed_at)
 FROM resources r JOIN nodes n ON n.id=r.node_id
 LEFT JOIN desired_users du ON r.kind='user' AND du.node_id=r.node_id AND du.username=r.name
 LEFT JOIN observed_users ou ON r.kind='user' AND ou.node_id=r.node_id AND ou.username=r.name
 LEFT JOIN desired_groups dg ON r.kind='group' AND dg.node_id=r.node_id AND dg.group_name=r.name
 LEFT JOIN observed_groups og ON r.kind='group' AND og.node_id=r.node_id AND og.group_name=r.name
 LEFT JOIN commands c ON c.id=(SELECT latest.id FROM commands latest JOIN operations operation ON operation.id=latest.operation_id WHERE latest.node_id=r.node_id AND latest.resource_type=r.kind AND latest.resource_key=r.name ORDER BY latest.created_at DESC,latest.id DESC LIMIT 1)
 LEFT JOIN operations op ON op.id=c.operation_id ORDER BY r.kind,r.name`, func(r Rows) (v userstatestore.Resource, err error) {
		err = r.Scan(&v.Kind, &v.Name, &v.NodeStatus, &v.DesiredEnabled, &v.ObservedEnabled, &v.DesiredMembers, &v.ObservedMembers, &v.DesiredVersion, &v.DesiredRevision, &v.ObservedRevision, &v.OperationID, &v.OperationState, &v.CommandState, &v.PayloadType, &v.ObservedAt)
		return
	}, node)
}
