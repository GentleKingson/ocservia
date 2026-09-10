package postgres

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	"github.com/google/uuid"
)

type userStateStore struct{ database.Tx }

func (t *transaction) UserStateStore() userstore.Store { return userStateStore{t} }

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

const userSafeRejected = `c.state='rejected'
 AND EXISTS(SELECT 1 FROM agent_command_results result WHERE result.command_id=c.id AND result.state='rejected')
 AND NOT EXISTS(SELECT 1 FROM agent_command_results result WHERE result.command_id=c.id AND (result.state='unknown' OR result.accepted_at IS NOT NULL))
 AND (SELECT count(*) FROM command_attempts attempt WHERE attempt.command_id=c.id)<=1`

func (s userStateStore) LastRevision(ctx context.Context, node uuid.UUID, resource, key string, expected int64) (v userstore.Revision, err error) {
	err = s.QueryRow(ctx, `SELECT c.state,c.payload_type,`+userSafeRejected+` FROM commands c WHERE c.node_id=$1 AND c.resource_type=$2 AND c.resource_key=$3 AND c.expected_version=$4 ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, node, resource, key, expected).Scan(&v.State, &v.Kind, &v.SafeRejected)
	return
}

func (s userStateStore) CountResources(ctx context.Context, node uuid.UUID, name string, group bool, fresh value.Timestamp) (count int, err error) {
	if group {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT group_name FROM desired_groups WHERE node_id=$1 AND group_name<>$2 UNION SELECT o.group_name FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND o.group_name<>$2 AND s.last_heartbeat_at>=$3) resources`, node, name, fresh).Scan(&count)
	} else {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT username FROM desired_users WHERE node_id=$1 UNION SELECT o.username FROM observed_users o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND s.last_heartbeat_at>=$2) resources`, node, fresh).Scan(&count)
	}
	return
}

func (s userStateStore) CountMemberships(ctx context.Context, node uuid.UUID, name string, fresh value.Timestamp) (count int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT d.group_name,unnest(d.members) member FROM desired_groups d WHERE d.node_id=$1 AND d.group_name<>$2 UNION SELECT o.group_name,unnest(o.members) member FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=$1 AND o.group_name<>$2 AND s.last_heartbeat_at>=$3) memberships`, node, name, fresh).Scan(&count)
	return
}

func (s userStateStore) WriteDesired(ctx context.Context, v userstore.Desired) error {
	var n int64
	var err error
	switch v.Kind {
	case "user_create":
		_, err = s.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,true,$3,$4,$5,$6,$6) ON CONFLICT(node_id,username) DO UPDATE SET enabled=true,version=EXCLUDED.version,revision=EXCLUDED.revision,fingerprint=EXCLUDED.fingerprint,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Name, v.Version, v.Revision, v.Fingerprint, v.At)
	case "group_apply":
		_, err = s.Exec(ctx, `INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$7) ON CONFLICT(node_id,group_name) DO UPDATE SET members=EXCLUDED.members,version=EXCLUDED.version,revision=EXCLUDED.revision,fingerprint=EXCLUDED.fingerprint,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Name, v.Members, v.Version, v.Revision, v.Fingerprint, v.At)
	case "user_enable", "user_disable":
		n, err = s.Exec(ctx, `UPDATE desired_users SET enabled=$3,version=$4,revision=$5,fingerprint=$6,updated_at=$7 WHERE node_id=$1 AND username=$2`, v.NodeID, v.Name, v.Kind == "user_enable", v.Version, v.Revision, v.Fingerprint, v.At)
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

func (s userStateStore) SupersedePending(ctx context.Context, node uuid.UUID, resource, key, kind string, expected int64, at value.Timestamp) (bool, error) {
	// Dispatch also locks outbox before command. Recheck eligibility after the
	// outbox lock so a concurrent claim cannot be superseded while in flight.
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE o.locked_by IS NULL AND EXISTS(SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.node_id=$1 AND c.resource_type=$2 AND c.resource_key=$3 AND c.payload_type=$4 AND c.expected_version=$5 AND c.state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)) ORDER BY o.id FOR UPDATE`, node, resource, key, kind, expected)
	if err != nil {
		return false, err
	}
	var commands []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		commands = append(commands, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	changed := false
	for _, command := range commands {
		var operation uuid.UUID
		err := s.QueryRow(ctx, `SELECT operation_id FROM commands WHERE id=$1 AND state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=commands.id) FOR UPDATE`, command).Scan(&operation)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE commands SET state='superseded',updated_at=$2 WHERE id=$1`, command, at); err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1 AND state='queued'`, operation, at); err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE outbox_events SET published_at=$2,last_error='superseded by newer desired revision' WHERE command_id=$1`, command, at); err != nil {
			return false, err
		}
		event, err := uuid.NewV7()
		if err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'superseded',$3)`, event, operation, at); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func (s userStateStore) List(ctx context.Context, node uuid.UUID) ([]userstore.Resource, error) {
	rows, err := s.Query(ctx, `WITH resources AS (
 SELECT 'user' kind,node_id,username name FROM desired_users WHERE node_id=$1
 UNION SELECT 'user',node_id,username FROM observed_users WHERE node_id=$1
 UNION SELECT 'group',node_id,group_name FROM desired_groups WHERE node_id=$1
 UNION SELECT 'group',node_id,group_name FROM observed_groups WHERE node_id=$1
 ) SELECT r.kind,r.name,n.status,du.enabled,ou.enabled,dg.members,og.members,
 COALESCE(du.version,dg.version),COALESCE(du.revision,dg.revision),COALESCE(ou.revision,og.revision),
 COALESCE(du.fingerprint,dg.fingerprint),COALESCE(ou.fingerprint,og.fingerprint),
 op.id,op.state,c.state,c.payload_type,`+userSafeRejected+`,COALESCE(ou.observed_at,og.observed_at)
 FROM resources r JOIN nodes n ON n.id=r.node_id
 LEFT JOIN desired_users du ON r.kind='user' AND du.node_id=r.node_id AND du.username=r.name
 LEFT JOIN observed_users ou ON r.kind='user' AND ou.node_id=r.node_id AND ou.username=r.name
 LEFT JOIN desired_groups dg ON r.kind='group' AND dg.node_id=r.node_id AND dg.group_name=r.name
 LEFT JOIN observed_groups og ON r.kind='group' AND og.node_id=r.node_id AND og.group_name=r.name
 LEFT JOIN commands c ON c.id=(SELECT latest.id FROM commands latest JOIN operations operation ON operation.command_id=latest.id WHERE latest.node_id=r.node_id AND latest.resource_type=r.kind AND latest.resource_key=r.name ORDER BY latest.created_at DESC,latest.id DESC LIMIT 1)
 LEFT JOIN operations op ON op.command_id=c.id ORDER BY r.kind,r.name`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []userstore.Resource{}
	for rows.Next() {
		var v userstore.Resource
		if err := rows.Scan(&v.Kind, &v.Name, &v.NodeStatus, &v.DesiredEnabled, &v.ObservedEnabled, &v.DesiredMembers, &v.ObservedMembers, &v.DesiredVersion, &v.DesiredRevision, &v.ObservedRevision, &v.DesiredFingerprint, &v.ObservedFingerprint, &v.OperationID, &v.OperationState, &v.CommandState, &v.PayloadType, &v.SafeRejected, &v.ObservedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
