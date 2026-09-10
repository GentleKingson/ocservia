package mysql

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
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sealing_keys WHERE node_id=? AND purpose=1 AND version=? AND BINARY key_id=?)`, UUIDBytes(node), version, key).Scan(&v)
	return
}

func (s userStateStore) LockDesired(ctx context.Context, node uuid.UUID, name string, group bool) (version, revision int64, err error) {
	q := `SELECT version,revision FROM desired_users WHERE node_id=? AND BINARY username=? FOR UPDATE`
	if group {
		q = `SELECT version,revision FROM desired_groups WHERE node_id=? AND BINARY group_name=? FOR UPDATE`
	}
	err = s.QueryRow(ctx, q, UUIDBytes(node), name).Scan(&version, &revision)
	return
}

const userSafeRejected = `c.state='rejected'
 AND EXISTS(SELECT 1 FROM agent_command_results result WHERE result.command_id=c.id AND result.state='rejected')
 AND NOT EXISTS(SELECT 1 FROM agent_command_results result WHERE result.command_id=c.id AND (result.state='unknown' OR result.accepted_at IS NOT NULL))
 AND (SELECT count(*) FROM command_attempts attempt WHERE attempt.command_id=c.id)<=1`

func (s userStateStore) LastRevision(ctx context.Context, node uuid.UUID, resource, key string, expected int64) (v userstore.Revision, err error) {
	err = s.QueryRow(ctx, `SELECT c.state,c.payload_type,`+userSafeRejected+` FROM commands c WHERE c.node_id=? AND BINARY c.resource_type=? AND BINARY c.resource_key=? AND c.expected_version=? ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, UUIDBytes(node), resource, key, expected).Scan(&v.State, &v.Kind, &v.SafeRejected)
	return
}

func (s userStateStore) CountResources(ctx context.Context, node uuid.UUID, name string, group bool, fresh value.Timestamp) (count int, err error) {
	if group {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT BINARY group_name FROM desired_groups WHERE node_id=? AND BINARY group_name<>? UNION SELECT BINARY o.group_name FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=? AND BINARY o.group_name<>? AND s.last_heartbeat_at>=?) resources`, UUIDBytes(node), name, UUIDBytes(node), name, fresh).Scan(&count)
	} else {
		err = s.QueryRow(ctx, `SELECT count(*) FROM (SELECT BINARY username FROM desired_users WHERE node_id=? UNION SELECT BINARY o.username FROM observed_users o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=? AND s.last_heartbeat_at>=?) resources`, UUIDBytes(node), UUIDBytes(node), fresh).Scan(&count)
	}
	return
}

func (s userStateStore) CountMemberships(ctx context.Context, node uuid.UUID, name string, fresh value.Timestamp) (int, error) {
	rows, err := s.Query(ctx, `SELECT group_name,members FROM desired_groups WHERE node_id=? AND BINARY group_name<>? UNION ALL SELECT o.group_name,o.members FROM observed_groups o JOIN node_observed_snapshots s ON s.node_id=o.node_id WHERE o.node_id=? AND BINARY o.group_name<>? AND s.last_heartbeat_at>=?`, UUIDBytes(node), name, UUIDBytes(node), name, fresh)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	// Match PostgreSQL UNION over unnest exactly, including NULL, arbitrary
	// text, and flattened multidimensional arrays; never cast to bounded text.
	type membership struct {
		group, member string
		null          bool
	}
	seen := map[membership]struct{}{}
	for rows.Next() {
		var group string
		var members value.TextArray
		if err := rows.Scan(&group, &members); err != nil {
			return 0, err
		}
		for _, member := range members.Elements {
			key := membership{group: group, null: member == nil}
			if member != nil {
				key.member = *member
			}
			seen[key] = struct{}{}
		}
	}
	return len(seen), rows.Err()
}

func (s userStateStore) WriteDesired(ctx context.Context, v userstore.Desired) error {
	var n int64
	var err error
	switch v.Kind {
	case "user_create":
		_, err = s.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,true,?,?,?,?,?) ON DUPLICATE KEY UPDATE enabled=true,version=VALUES(version),revision=VALUES(revision),fingerprint=VALUES(fingerprint),updated_at=VALUES(updated_at)`, UUIDBytes(v.NodeID), v.Name, v.Version, v.Revision, v.Fingerprint, v.At, v.At)
	case "group_apply":
		_, err = s.Exec(ctx, `INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at)VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE members=VALUES(members),version=VALUES(version),revision=VALUES(revision),fingerprint=VALUES(fingerprint),updated_at=VALUES(updated_at)`, UUIDBytes(v.NodeID), v.Name, v.Members, v.Version, v.Revision, v.Fingerprint, v.At, v.At)
	case "user_enable", "user_disable":
		n, err = s.Exec(ctx, `UPDATE desired_users SET enabled=?,version=?,revision=?,fingerprint=?,updated_at=? WHERE node_id=? AND BINARY username=?`, v.Kind == "user_enable", v.Version, v.Revision, v.Fingerprint, v.At, UUIDBytes(v.NodeID), v.Name)
		if err == nil && n == 0 {
			// A no-op UPDATE still matches an existing row on PostgreSQL.
			_, _, err = s.LockDesired(ctx, v.NodeID, v.Name, false)
		}
	case "user_password_rotate":
		n, err = s.Exec(ctx, `UPDATE desired_users SET version=?,revision=?,updated_at=? WHERE node_id=? AND BINARY username=?`, v.Version, v.Revision, v.At, UUIDBytes(v.NodeID), v.Name)
		if err == nil && n == 0 {
			_, _, err = s.LockDesired(ctx, v.NodeID, v.Name, false)
		}
	default:
		return database.ErrUnsupported
	}
	return err
}

func (s userStateStore) SupersedePending(ctx context.Context, node uuid.UUID, resource, key, kind string, expected int64, at value.Timestamp) (bool, error) {
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE o.locked_by IS NULL AND EXISTS(SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.node_id=? AND BINARY c.resource_type=? AND BINARY c.resource_key=? AND BINARY c.payload_type=? AND c.expected_version=? AND c.state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)) ORDER BY o.id FOR UPDATE`, UUIDBytes(node), resource, key, kind, expected)
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
		err := s.QueryRow(ctx, `SELECT operation_id FROM commands WHERE id=? AND state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=commands.id) FOR UPDATE`, UUIDBytes(command)).Scan(&operation)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE commands SET state='superseded',updated_at=? WHERE id=?`, at, UUIDBytes(command)); err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=?,completed_at=? WHERE id=? AND state='queued'`, at, at, UUIDBytes(operation)); err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `UPDATE outbox_events SET published_at=?,last_error='superseded by newer desired revision' WHERE command_id=?`, at, UUIDBytes(command)); err != nil {
			return false, err
		}
		event, err := uuid.NewV7()
		if err != nil {
			return false, err
		}
		if _, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'superseded',?)`, UUIDBytes(event), UUIDBytes(operation), at); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func (s userStateStore) List(ctx context.Context, node uuid.UUID) ([]userstore.Resource, error) {
	rows, err := s.Query(ctx, `WITH resources AS (
 SELECT 'user' kind,node_id,BINARY username name FROM desired_users WHERE node_id=?
 UNION SELECT 'user',node_id,BINARY username FROM observed_users WHERE node_id=?
 UNION SELECT 'group',node_id,BINARY group_name FROM desired_groups WHERE node_id=?
 UNION SELECT 'group',node_id,BINARY group_name FROM observed_groups WHERE node_id=?
 ) SELECT r.kind,r.name,n.status,du.enabled,ou.enabled,dg.members,og.members,
 COALESCE(du.version,dg.version),COALESCE(du.revision,dg.revision),COALESCE(ou.revision,og.revision),
 COALESCE(du.fingerprint,dg.fingerprint),COALESCE(ou.fingerprint,og.fingerprint),
 op.id,op.state,c.state,c.payload_type,`+userSafeRejected+`,COALESCE(ou.observed_at,og.observed_at)
 FROM resources r JOIN nodes n ON n.id=r.node_id
 LEFT JOIN desired_users du ON r.kind='user' AND du.node_id=r.node_id AND BINARY du.username=r.name
 LEFT JOIN observed_users ou ON r.kind='user' AND ou.node_id=r.node_id AND BINARY ou.username=r.name
 LEFT JOIN desired_groups dg ON r.kind='group' AND dg.node_id=r.node_id AND BINARY dg.group_name=r.name
 LEFT JOIN observed_groups og ON r.kind='group' AND og.node_id=r.node_id AND BINARY og.group_name=r.name
 LEFT JOIN commands c ON c.id=(SELECT latest.id FROM commands latest JOIN operations operation ON operation.command_id=latest.id WHERE latest.node_id=r.node_id AND BINARY latest.resource_type=BINARY r.kind AND BINARY latest.resource_key=r.name ORDER BY latest.created_at DESC,latest.id DESC LIMIT 1)
 LEFT JOIN operations op ON op.command_id=c.id ORDER BY r.kind,r.name`, UUIDBytes(node), UUIDBytes(node), UUIDBytes(node), UUIDBytes(node))
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
