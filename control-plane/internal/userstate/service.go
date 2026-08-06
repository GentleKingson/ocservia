// Package userstate manages node-scoped desired and observed Ocserv users and groups.
package userstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidRequest      = errors.New("user or group request is invalid")
	ErrVersionConflict     = errors.New("desired state version is stale")
	ErrNotFound            = errors.New("desired resource was not found")
	ErrNodeUnavailable     = errors.New("node is unavailable")
	ErrCapabilityMissing   = errors.New("node capability is unavailable")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type MutationKind string

const (
	UserCreate         MutationKind = "user_create"
	UserDisable        MutationKind = "user_disable"
	UserPasswordRotate MutationKind = "user_password_rotate"
	GroupApply         MutationKind = "group_apply"
)

type MutationRequest struct {
	NodeID, ActorIdentityID, ActorSessionID uuid.UUID
	Kind                                    MutationKind
	Name, SecretKeyID, IdempotencyKey       string
	Members                                 []string
	SealedPassword                          []byte
	ExpectedVersion                         int64
	TTL                                     time.Duration
	ActorID, Reason, RequestID, Traceparent string
}

type ResourceState struct {
	Kind                string     `json:"kind"`
	Name                string     `json:"name"`
	DesiredEnabled      *bool      `json:"desired_enabled,omitempty"`
	ObservedEnabled     *bool      `json:"observed_enabled,omitempty"`
	DesiredMembers      []string   `json:"desired_members,omitempty"`
	ObservedMembers     []string   `json:"observed_members,omitempty"`
	DesiredVersion      *int64     `json:"desired_version,omitempty"`
	DesiredRevision     *int64     `json:"desired_revision,omitempty"`
	ObservedRevision    *int64     `json:"observed_revision,omitempty"`
	DesiredFingerprint  string     `json:"desired_fingerprint,omitempty"`
	ObservedFingerprint string     `json:"observed_fingerprint,omitempty"`
	Convergence         string     `json:"convergence"`
	OperationID         *string    `json:"operation_id,omitempty"`
	OperationState      *string    `json:"operation_state,omitempty"`
	ObservedAt          *time.Time `json:"observed_at,omitempty"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Mutate(ctx context.Context, request MutationRequest) (operationstore.Operation, bool, error) {
	if err := validateMutation(request); err != nil {
		return operationstore.Operation{}, false, err
	}
	request.Members = normalizeMembers(request.Members)
	hash := requestHash(request)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("begin desired state transaction: %w", err)
	}
	defer rollback(tx)

	var workspaceID uuid.UUID
	var nodeStatus string
	if err := tx.QueryRow(ctx, `SELECT workspace_id,status FROM nodes WHERE id=$1 FOR UPDATE`, request.NodeID).Scan(&workspaceID, &nodeStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return operationstore.Operation{}, false, ErrNodeUnavailable
		}
		return operationstore.Operation{}, false, err
	}
	if nodeStatus != "active" && nodeStatus != "offline" {
		return operationstore.Operation{}, false, ErrNodeUnavailable
	}
	if existing, same, err := findIdempotent(ctx, tx, workspaceID, request.IdempotencyKey, hash[:]); err != nil {
		return operationstore.Operation{}, false, err
	} else if existing.ID != "" {
		if !same {
			return operationstore.Operation{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return operationstore.Operation{}, false, err
		}
		return existing, true, nil
	}
	capability := capabilityFor(request.Kind)
	var approved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability=$2 AND approved=true)`, request.NodeID, capability).Scan(&approved); err != nil {
		return operationstore.Operation{}, false, err
	}
	if !approved {
		return operationstore.Operation{}, false, ErrCapabilityMissing
	}

	currentVersion, currentRevision, err := lockDesired(ctx, tx, request)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if currentVersion != request.ExpectedVersion {
		return operationstore.Operation{}, false, ErrVersionConflict
	}
	nextVersion, nextRevision := currentVersion+1, currentRevision+1
	now := s.now()
	fingerprint := desiredFingerprint(request.Kind, request.Name, request.Members)
	if err := writeDesired(ctx, tx, request, nextVersion, nextRevision, fingerprint[:], now); err != nil {
		return operationstore.Operation{}, false, err
	}

	operationID, commandID, outboxID, auditID, eventID, err := newIDs()
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	expiresAt := now.Add(request.TTL)
	envelope, err := marshalEnvelope(request, operationID, commandID, uint64(currentRevision), uint64(nextRevision), now, expiresAt)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	resourceType := "user"
	if request.Kind == GroupApply {
		resourceType = "group"
	}
	if err := supersedePending(ctx, tx, request.NodeID, resourceType, request.Name, now); err != nil {
		return operationstore.Operation{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,version,request_id,trace_id,idempotency_key,request_hash,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'queued',1,$5,$6,$7,$8,$9,$10,$10)`, operationID, workspaceID, request.NodeID, commandID, request.RequestID, traceID(request.Traceparent), request.IdempotencyKey, hash[:], expiresAt, now); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("insert user operation: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at,resource_type,resource_key) VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)`, commandID, operationID, workspaceID, request.NodeID, request.Kind, envelope, request.IdempotencyKey, currentRevision, request.Traceparent, expiresAt, now, resourceType, request.Name); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("insert user command: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,created_at) VALUES($1,$2,'command.dispatch',$3,$4,$4)`, outboxID, commandID, envelope, now); err != nil {
		return operationstore.Operation{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,'queued',$3)`, eventID, operationID, now); err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: actionFor(request.Kind), ResourceType: resourceType, ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, At: now}); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("append desired state audit intent: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, outboxID.String()); err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return operationstore.Operation{}, false, err
	}
	nodeText, commandText := request.NodeID.String(), commandID.String()
	return operationstore.Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiresAt}, false, nil
}

func (s *Service) List(ctx context.Context, nodeID uuid.UUID) ([]ResourceState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind,name,desired_enabled,observed_enabled,desired_members,observed_members,
		       desired_version,desired_revision,observed_revision,encode(desired_fingerprint,'hex'),
		       encode(observed_fingerprint,'hex'),node_status,operation_id::text,operation_state,observed_at
		FROM (
		 SELECT 'user'::text kind,COALESCE(d.username,o.username) name,d.enabled desired_enabled,o.enabled observed_enabled,
		        NULL::text[] desired_members,NULL::text[] observed_members,d.version desired_version,d.revision desired_revision,
		        o.revision observed_revision,d.fingerprint desired_fingerprint,o.fingerprint observed_fingerprint,n.status node_status,
		        latest.operation_id,latest.operation_state,o.observed_at
		 FROM desired_users d FULL JOIN observed_users o USING(node_id,username)
		 JOIN nodes n ON n.id=COALESCE(d.node_id,o.node_id)
		 LEFT JOIN LATERAL (SELECT op.id operation_id,op.state operation_state FROM commands c JOIN operations op ON op.command_id=c.id WHERE c.node_id=n.id AND c.resource_type='user' AND c.resource_key=COALESCE(d.username,o.username) ORDER BY c.created_at DESC LIMIT 1) latest ON true
		 WHERE COALESCE(d.node_id,o.node_id)=$1
		 UNION ALL
		 SELECT 'group',COALESCE(d.group_name,o.group_name),NULL,NULL,d.members,o.members,d.version,d.revision,o.revision,
		        d.fingerprint,o.fingerprint,n.status,latest.operation_id,latest.operation_state,o.observed_at
		 FROM desired_groups d FULL JOIN observed_groups o USING(node_id,group_name)
		 JOIN nodes n ON n.id=COALESCE(d.node_id,o.node_id)
		 LEFT JOIN LATERAL (SELECT op.id operation_id,op.state operation_state FROM commands c JOIN operations op ON op.command_id=c.id WHERE c.node_id=n.id AND c.resource_type='group' AND c.resource_key=COALESCE(d.group_name,o.group_name) ORDER BY c.created_at DESC LIMIT 1) latest ON true
		 WHERE COALESCE(d.node_id,o.node_id)=$1
		) state ORDER BY kind,name`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list desired observed state: %w", err)
	}
	defer rows.Close()
	result := []ResourceState{}
	for rows.Next() {
		var item ResourceState
		var nodeStatus string
		if err := rows.Scan(&item.Kind, &item.Name, &item.DesiredEnabled, &item.ObservedEnabled, &item.DesiredMembers, &item.ObservedMembers, &item.DesiredVersion, &item.DesiredRevision, &item.ObservedRevision, &item.DesiredFingerprint, &item.ObservedFingerprint, &nodeStatus, &item.OperationID, &item.OperationState, &item.ObservedAt); err != nil {
			return nil, err
		}
		item.Convergence = convergence(item, nodeStatus)
		result = append(result, item)
	}
	return result, rows.Err()
}

func validateMutation(request MutationRequest) error {
	if request.NodeID == uuid.Nil || !namePattern.MatchString(request.Name) || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || request.ExpectedVersion < 0 || request.TTL < time.Second || request.TTL > 24*time.Hour || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || request.RequestID == "" || !validTraceparent(request.Traceparent) {
		return ErrInvalidRequest
	}
	if request.Kind == UserCreate || request.Kind == UserPasswordRotate {
		if len(request.SealedPassword) < 32 || len(request.SealedPassword) > 4096 || request.SecretKeyID == "" || len(request.SecretKeyID) > 128 {
			return ErrInvalidRequest
		}
	} else if len(request.SealedPassword) != 0 || request.SecretKeyID != "" {
		return ErrInvalidRequest
	}
	if request.Kind == GroupApply {
		if len(request.Members) > 4096 {
			return ErrInvalidRequest
		}
		for _, member := range request.Members {
			if !namePattern.MatchString(member) {
				return ErrInvalidRequest
			}
		}
	} else if len(request.Members) != 0 {
		return ErrInvalidRequest
	}
	return nil
}

func lockDesired(ctx context.Context, tx pgx.Tx, request MutationRequest) (int64, int64, error) {
	query := `SELECT version,revision FROM desired_users WHERE node_id=$1 AND username=$2 FOR UPDATE`
	if request.Kind == GroupApply {
		query = `SELECT version,revision FROM desired_groups WHERE node_id=$1 AND group_name=$2 FOR UPDATE`
	}
	var version, revision int64
	err := tx.QueryRow(ctx, query, request.NodeID, request.Name).Scan(&version, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		if request.Kind == UserCreate || request.Kind == GroupApply {
			return 0, 0, nil
		}
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	if request.Kind == UserCreate {
		return 0, 0, ErrVersionConflict
	}
	return version, revision, nil
}

func writeDesired(ctx context.Context, tx pgx.Tx, request MutationRequest, version, revision int64, fingerprint []byte, now time.Time) error {
	switch request.Kind {
	case UserCreate:
		_, err := tx.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES($1,$2,true,$3,$4,$5,$6,$6)`, request.NodeID, request.Name, version, revision, fingerprint, now)
		return err
	case UserDisable:
		result, err := tx.Exec(ctx, `UPDATE desired_users SET enabled=false,version=$3,revision=$4,fingerprint=$5,updated_at=$6 WHERE node_id=$1 AND username=$2`, request.NodeID, request.Name, version, revision, fingerprint, now)
		if err == nil && result.RowsAffected() != 1 {
			return ErrNotFound
		}
		return err
	case UserPasswordRotate:
		result, err := tx.Exec(ctx, `UPDATE desired_users SET version=$3,revision=$4,updated_at=$5 WHERE node_id=$1 AND username=$2`, request.NodeID, request.Name, version, revision, now)
		if err == nil && result.RowsAffected() != 1 {
			return ErrNotFound
		}
		return err
	case GroupApply:
		_, err := tx.Exec(ctx, `INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7) ON CONFLICT(node_id,group_name) DO UPDATE SET members=EXCLUDED.members,version=EXCLUDED.version,revision=EXCLUDED.revision,fingerprint=EXCLUDED.fingerprint,updated_at=EXCLUDED.updated_at`, request.NodeID, request.Name, request.Members, version, revision, fingerprint, now)
		return err
	default:
		return ErrInvalidRequest
	}
}

func marshalEnvelope(request MutationRequest, operationID, commandID uuid.UUID, expectedRevision, desiredRevision uint64, now, expires time.Time) ([]byte, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	envelope := &agentv1.CommandEnvelope{ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: request.NodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), ExpectedRevision: expectedRevision, Traceparent: request.Traceparent, ActorId: request.ActorID, Reason: request.Reason, DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
	switch request.Kind {
	case UserCreate:
		envelope.Payload = &agentv1.CommandEnvelope_UserCreate{UserCreate: &agentv1.UserCreate{Username: request.Name, SealedPassword: request.SealedPassword, SecretKeyId: request.SecretKeyID, DesiredRevision: desiredRevision}}
	case UserDisable:
		envelope.Payload = &agentv1.CommandEnvelope_UserDisable{UserDisable: &agentv1.UserDisable{Username: request.Name, DesiredRevision: desiredRevision}}
	case UserPasswordRotate:
		envelope.Payload = &agentv1.CommandEnvelope_UserPasswordRotate{UserPasswordRotate: &agentv1.UserPasswordRotate{Username: request.Name, SealedPassword: request.SealedPassword, SecretKeyId: request.SecretKeyID, DesiredRevision: desiredRevision}}
	case GroupApply:
		envelope.Payload = &agentv1.CommandEnvelope_GroupApply{GroupApply: &agentv1.GroupApply{GroupName: request.Name, Members: request.Members, DesiredRevision: desiredRevision}}
	default:
		return nil, ErrInvalidRequest
	}
	if err := semanticpayload.PopulateV1(envelope); err != nil {
		return nil, err
	}
	return proto.Marshal(envelope)
}

func desiredFingerprint(kind MutationKind, name string, members []string) [32]byte {
	enabled := kind != UserDisable
	value := struct {
		Name    string   `json:"name"`
		Enabled *bool    `json:"enabled,omitempty"`
		Members []string `json:"members,omitempty"`
	}{Name: name}
	if kind == GroupApply {
		value.Members = members
	} else {
		value.Enabled = &enabled
	}
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(encoded)
}

func requestHash(request MutationRequest) [32]byte {
	value := struct {
		Node          uuid.UUID
		Kind          MutationKind
		Name, Key     string
		Members       []string
		Secret        []byte
		Version       int64
		TTL           int64
		Actor, Reason string
	}{request.NodeID, request.Kind, request.Name, request.SecretKeyID, normalizeMembers(request.Members), request.SealedPassword, request.ExpectedVersion, int64(request.TTL / time.Second), request.ActorID, request.Reason}
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(encoded)
}

func normalizeMembers(members []string) []string {
	result := slices.Clone(members)
	slices.Sort(result)
	return slices.Compact(result)
}
func capabilityFor(kind MutationKind) string {
	if kind == GroupApply {
		return "ocserv.groups.write"
	}
	return "ocserv.users.write"
}
func actionFor(kind MutationKind) string {
	switch kind {
	case UserCreate:
		return "user.create"
	case UserDisable:
		return "user.disable"
	case UserPasswordRotate:
		return "user.password.rotate"
	default:
		return "group.apply"
	}
}

func convergence(item ResourceState, nodeStatus string) string {
	if item.DesiredVersion == nil {
		return "drifted"
	}
	if item.ObservedRevision != nil && item.DesiredFingerprint == item.ObservedFingerprint {
		return "converged"
	}
	if nodeStatus == "offline" {
		return "offline_pending"
	}
	if item.OperationState != nil && (*item.OperationState == "queued" || *item.OperationState == "dispatched" || *item.OperationState == "accepted" || *item.OperationState == "running") {
		return "pending"
	}
	return "drifted"
}

func findIdempotent(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, key string, hash []byte) (operationstore.Operation, bool, error) {
	var op operationstore.Operation
	var nodeID, commandID *string
	var same bool
	err := tx.QueryRow(ctx, `SELECT id::text,state,node_id::text,command_id::text,version,created_at,updated_at,expires_at,request_hash=$3 FROM operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, key, hash).Scan(&op.ID, &op.State, &nodeID, &commandID, &op.Version, &op.CreatedAt, &op.UpdatedAt, &op.ExpiresAt, &same)
	if errors.Is(err, pgx.ErrNoRows) {
		return operationstore.Operation{}, false, nil
	}
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	op.NodeID = nodeID
	op.CommandID = commandID
	return op, same, nil
}

func supersedePending(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, resourceType, resourceKey string, now time.Time) error {
	rows, err := tx.Query(ctx, `UPDATE commands c SET state='superseded',updated_at=$4 FROM outbox_events o WHERE c.node_id=$1 AND c.resource_type=$2 AND c.resource_key=$3 AND c.state='queued' AND o.command_id=c.id AND o.locked_by IS NULL AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id) RETURNING c.id,c.operation_id`, nodeID, resourceType, resourceKey, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	type old struct{ command, operation uuid.UUID }
	var olds []old
	for rows.Next() {
		var value old
		if err := rows.Scan(&value.command, &value.operation); err != nil {
			return err
		}
		olds = append(olds, value)
	}
	for _, value := range olds {
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1 AND state='queued'`, value.operation, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=$2,last_error='superseded by newer desired revision' WHERE command_id=$1`, value.command, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'superseded',$3)`, eventID, value.operation, now); err != nil {
			return err
		}
	}
	return rows.Err()
}

func newIDs() (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
		}
		ids[i] = id
	}
	return ids[0], ids[1], ids[2], ids[3], ids[4], nil
}
func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
func traceID(value string) string { return value[3:35] }
func validTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, part := range parts[1:] {
		for _, c := range part {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
