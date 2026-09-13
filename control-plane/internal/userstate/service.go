// Package userstate manages node-scoped desired and observed Ocserv users and groups.
package userstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationdata "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidRequest      = errors.New("user or group request is invalid")
	ErrCapacityExceeded    = errors.New("managed user or group capacity exceeded")
	ErrVersionConflict     = errors.New("desired state version is stale")
	ErrRevisionPending     = errors.New("the current desired revision is still pending")
	ErrRevisionRecovery    = errors.New("the current desired revision requires same-kind recovery")
	ErrNotFound            = errors.New("desired resource was not found")
	ErrNodeUnavailable     = errors.New("node is unavailable")
	ErrCapabilityMissing   = errors.New("node capability is unavailable")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")
	ErrBacklogExceeded     = commandlimit.ErrBacklogExceeded
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type MutationKind string

const (
	UserCreate          MutationKind = "user_create"
	UserDisable         MutationKind = "user_disable"
	UserEnable          MutationKind = "user_enable"
	UserPasswordRotate  MutationKind = "user_password_rotate"
	GroupApply          MutationKind = "group_apply"
	MaxManagedResources              = 384
)

type MutationRequest struct {
	NodeID, ActorIdentityID, ActorSessionID uuid.UUID
	Kind                                    MutationKind
	Name, IdempotencyKey                    string
	Members                                 []string
	SealedPassword                          *SealedSecret
	ExpectedVersion                         int64
	TTL                                     time.Duration
	ActorID, Reason, RequestID, Traceparent string
}

type SealedSecret struct {
	Version    uint32
	Purpose    string
	KeyID      string
	Ciphertext []byte
}

type ResourceState struct {
	Kind                     string            `json:"kind"`
	Name                     string            `json:"name"`
	DesiredEnabled           *bool             `json:"desired_enabled,omitempty"`
	ObservedEnabled          *bool             `json:"observed_enabled,omitempty"`
	DesiredMembers           []*string         `json:"desired_members,omitempty"`
	ObservedMembers          []*string         `json:"observed_members,omitempty"`
	DesiredVersion           *int64            `json:"desired_version,omitempty"`
	DesiredRevision          *int64            `json:"desired_revision,omitempty"`
	ObservedRevision         *int64            `json:"observed_revision,omitempty"`
	DesiredFingerprint       string            `json:"desired_fingerprint,omitempty"`
	ObservedFingerprint      string            `json:"observed_fingerprint,omitempty"`
	Convergence              string            `json:"convergence"`
	OperationID              *string           `json:"operation_id,omitempty"`
	OperationState           *string           `json:"operation_state,omitempty"`
	RecoveryRequired         bool              `json:"recovery_required"`
	RecoveryMutationKind     *MutationKind     `json:"recovery_mutation_kind,omitempty"`
	ObservedAt               *value.Timestamp  `json:"observed_at,omitempty"`
	DesiredMemberDimensions  []value.Dimension `json:"desired_member_dimensions,omitempty"`
	ObservedMemberDimensions []value.Dimension `json:"observed_member_dimensions,omitempty"`
}

type Service struct {
	backend database.Backend
	now     func() time.Time
	signer  *commandauth.Signer
}

func NewBackend(backend database.Backend) *Service {
	return &Service{backend: backend, now: func() time.Time { return time.Now().UTC() }}
}

func NewWithSignerBackend(backend database.Backend, signer *commandauth.Signer) *Service {
	service := NewBackend(backend)
	service.signer = signer
	return service
}

func commitFenced(ctx context.Context, tx database.Tx) error {
	if err := coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Mutate(ctx context.Context, request MutationRequest) (operationstore.Operation, bool, error) {
	if err := validateMutation(request); err != nil {
		return operationstore.Operation{}, false, err
	}
	request.Members = normalizeMembers(request.Members)
	hash := requestHash(request)
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("begin desired state transaction: %w", err)
	}
	defer rollback(tx)
	users, err := userstore.From(tx)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	operations, err := operationdata.FromTransaction(tx)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	node, err := operations.LockNode(ctx, request.NodeID)
	if errors.Is(err, database.ErrNotFound) {
		return operationstore.Operation{}, false, ErrNodeUnavailable
	}
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if node.Status != "active" && node.Status != "offline" {
		return operationstore.Operation{}, false, ErrNodeUnavailable
	}
	workspaceID := node.WorkspaceID
	if request.SealedPassword != nil {
		registered, err := users.HasSealingKey(ctx, request.NodeID, request.SealedPassword.Version, request.SealedPassword.KeyID)
		if err != nil {
			return operationstore.Operation{}, false, err
		}
		if !registered {
			return operationstore.Operation{}, false, ErrInvalidRequest
		}
	}
	if existing, same, err := findIdempotent(ctx, tx, workspaceID, request.IdempotencyKey, hash[:]); err != nil {
		return operationstore.Operation{}, false, err
	} else if existing.ID != "" {
		if !same {
			return operationstore.Operation{}, false, ErrIdempotencyConflict
		}
		if err := commitFenced(ctx, tx); err != nil {
			return operationstore.Operation{}, false, err
		}
		return existing, true, nil
	}
	approved, err := operations.HasCapability(ctx, request.NodeID, capabilityFor(request.Kind))
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if !approved {
		return operationstore.Operation{}, false, ErrCapabilityMissing
	}
	currentVersion, currentRevision, err := lockDesired(ctx, users, request)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if currentVersion != request.ExpectedVersion {
		return operationstore.Operation{}, false, ErrVersionConflict
	}
	resourceType := resourceTypeFor(request.Kind)
	replaceRevision, err := revisionReplacement(ctx, users, request.NodeID, resourceType, request.Name, request.Kind, currentRevision)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if request.Kind == UserCreate && currentVersion > 0 && !replaceRevision {
		return operationstore.Operation{}, false, ErrVersionConflict
	}
	now := s.now()
	created, err := value.FromTime(now)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := ensureMutationCapacity(ctx, users, request, currentVersion == 0, now); err != nil {
		return operationstore.Operation{}, false, err
	}
	coalesced, err := supersedePending(ctx, users, request.NodeID, resourceType, request.Name, request.Kind, currentRevision, created)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := commandlimit.ReserveBacklog(ctx, tx, workspaceID, request.NodeID); err != nil {
		return operationstore.Operation{}, false, err
	}
	nextVersion, nextRevision := currentVersion+1, currentRevision+1
	commandExpectedRevision := currentRevision
	if coalesced || replaceRevision {
		// Replacing a command that never applied must not leave a revision gap.
		nextRevision = currentRevision
		commandExpectedRevision = currentRevision - 1
	}
	fingerprint := desiredFingerprint(request.Kind, request.Name, request.Members)
	if err := users.WriteDesired(ctx, userstore.Desired{NodeID: request.NodeID, Kind: string(request.Kind), Name: request.Name, Version: nextVersion, Revision: nextRevision, Fingerprint: fingerprint[:], Members: value.TextList(request.Members), At: created}); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			err = ErrNotFound
		}
		return operationstore.Operation{}, false, err
	}
	operationID, commandID, outboxID, auditID, eventID, err := newIDs()
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	expiresAt := now.Add(request.TTL)
	expiry, err := value.FromTime(expiresAt)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	envelope, err := marshalEnvelope(request, operationID, commandID, node.AuthorizationRevision, uint64(nextRevision), now, expiresAt, s.signer)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := operations.InsertIntent(ctx, operationdata.QueuedIntent{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, CommandID: commandID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), IdempotencyKey: request.IdempotencyKey, RequestHash: hash[:], ExpiresAt: expiry, CreatedAt: created}); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("insert user operation: %w", err)
	}
	if err := operations.EnqueueCommand(ctx, operationdata.QueuedCommand{ID: commandID, OperationID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, OutboxID: outboxID, EventID: eventID, PayloadType: string(request.Kind), IdempotencyKey: request.IdempotencyKey, Traceparent: request.Traceparent, Envelope: envelope, ExpectedVersion: commandExpectedRevision, ExpiresAt: expiry, CreatedAt: created, AvailableAt: created, ResourceType: &resourceType, ResourceKey: &request.Name}); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("insert user command: %w", err)
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: actionFor(request.Kind), ResourceType: "operation", ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, At: now}); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("append desired state audit intent: %w", err)
	}
	if err := operations.NotifyOutbox(ctx, outboxID); err != nil {
		return operationstore.Operation{}, false, err
	}
	if err := commitFenced(ctx, tx); err != nil {
		return operationstore.Operation{}, false, err
	}
	nodeText, commandText := request.NodeID.String(), commandID.String()
	return operationstore.Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: created, UpdatedAt: created, ExpiresAt: &expiry}, false, nil
}

func (s *Service) List(ctx context.Context, nodeID uuid.UUID) ([]ResourceState, error) {
	result := []ResourceState{}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		users, err := userstore.From(tx)
		if err != nil {
			return err
		}
		rows, err := users.List(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("list desired observed state: %w", err)
		}
		for _, v := range rows {
			item := ResourceState{Kind: v.Kind, Name: v.Name, DesiredEnabled: v.DesiredEnabled, ObservedEnabled: v.ObservedEnabled,
				DesiredMembers: v.DesiredMembers.Elements, ObservedMembers: v.ObservedMembers.Elements,
				DesiredMemberDimensions: nonstandardDimensions(v.DesiredMembers), ObservedMemberDimensions: nonstandardDimensions(v.ObservedMembers),
				DesiredVersion: v.DesiredVersion, DesiredRevision: v.DesiredRevision, ObservedRevision: v.ObservedRevision,
				DesiredFingerprint: hex.EncodeToString(v.DesiredFingerprint), ObservedFingerprint: hex.EncodeToString(v.ObservedFingerprint), OperationState: v.OperationState}
			if v.OperationID != nil {
				id := v.OperationID.String()
				item.OperationID = &id
			}
			if v.ObservedAt.Valid {
				at := v.ObservedAt
				item.ObservedAt = &at
			}
			item.RecoveryRequired, item.RecoveryMutationKind = recoveryMetadata(v.CommandState, v.PayloadType, v.SafeRejected)
			item.Convergence = convergence(item, v.NodeStatus)
			result = append(result, item)
		}
		return nil
	})
	return result, err
}

// Ordinary membership lists retain their existing JSON shape. Dimensions are
// included only when needed to preserve a PostgreSQL array's non-list shape.
func nonstandardDimensions(a value.TextArray) []value.Dimension {
	if len(a.Dimensions) == 0 || (len(a.Dimensions) == 1 && a.Dimensions[0].LowerBound == 1) {
		return nil
	}
	return a.Dimensions
}

func validateMutation(request MutationRequest) error {
	if request.NodeID == uuid.Nil || !namePattern.MatchString(request.Name) || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || request.ExpectedVersion < 0 || request.TTL < time.Second || request.TTL > 24*time.Hour || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || request.RequestID == "" || !validTraceparent(request.Traceparent) {
		return ErrInvalidRequest
	}
	if request.Kind == UserCreate || request.Kind == UserPasswordRotate {
		if request.SealedPassword == nil || request.SealedPassword.Version != 1 || request.SealedPassword.Purpose != "user_password" || request.SealedPassword.KeyID == "" || len(request.SealedPassword.KeyID) > 128 || len(request.SealedPassword.Ciphertext) < 32 || len(request.SealedPassword.Ciphertext) > 4096 {
			return ErrInvalidRequest
		}
	} else if request.SealedPassword != nil {
		return ErrInvalidRequest
	}
	if request.Kind == GroupApply {
		if len(request.Members) > MaxManagedResources {
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

func lockDesired(ctx context.Context, users userstore.Store, request MutationRequest) (int64, int64, error) {
	version, revision, err := users.LockDesired(ctx, request.NodeID, request.Name, request.Kind == GroupApply)
	if errors.Is(err, database.ErrNotFound) {
		if request.Kind == UserCreate || request.Kind == GroupApply {
			return 0, 0, nil
		}
		return 0, 0, ErrNotFound
	}
	return version, revision, err
}

func resourceTypeFor(kind MutationKind) string {
	if kind == GroupApply {
		return "group"
	}
	return "user"
}

func revisionReplacement(ctx context.Context, users userstore.Store, nodeID uuid.UUID, resourceType, resourceKey string, kind MutationKind, currentRevision int64) (bool, error) {
	if currentRevision == 0 {
		return false, nil
	}
	previous, err := users.LastRevision(ctx, nodeID, resourceType, resourceKey, currentRevision-1)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, priorKind, safeRejected := previous.State, MutationKind(previous.Kind), previous.SafeRejected
	switch state {
	case "succeeded":
		return false, nil
	case "failed", "expired", "rolled_back":
		if priorKind != kind {
			return false, ErrRevisionRecovery
		}
		return true, nil
	case "rejected":
		if !safeRejected || priorKind != kind {
			return false, ErrRevisionRecovery
		}
		return true, nil
	case "queued":
		if priorKind == kind {
			return false, nil
		}
		return false, ErrRevisionPending
	case "dispatched", "accepted", "running", "unknown", "superseded":
		return false, ErrRevisionPending
	default:
		return false, ErrRevisionRecovery
	}
}

func recoveryMetadata(commandState, payloadType *string, safeRejected *bool) (bool, *MutationKind) {
	if commandState == nil || payloadType == nil {
		return false, nil
	}
	required := *commandState == "failed" || *commandState == "expired" || *commandState == "rolled_back" || *commandState == "rejected"
	replaceable := *commandState == "failed" || *commandState == "expired" || *commandState == "rolled_back" || (*commandState == "rejected" && safeRejected != nil && *safeRejected)
	if !replaceable {
		return required, nil
	}
	kind := MutationKind(*payloadType)
	switch kind {
	case UserCreate, UserDisable, UserEnable, UserPasswordRotate, GroupApply:
		return required, &kind
	default:
		return required, nil
	}
}

func ensureMutationCapacity(ctx context.Context, users userstore.Store, request MutationRequest, creating bool, now time.Time) error {
	fresh, err := value.FromTime(now.Add(-90 * time.Second))
	if err != nil {
		return err
	}
	if creating {
		count, err := users.CountResources(ctx, request.NodeID, request.Name, request.Kind == GroupApply, fresh)
		if err != nil {
			return err
		}
		if count >= MaxManagedResources {
			return ErrCapacityExceeded
		}
	}
	if request.Kind == GroupApply {
		count, err := users.CountMemberships(ctx, request.NodeID, request.Name, fresh)
		if err != nil {
			return err
		}
		if count+len(request.Members) > MaxManagedResources {
			return ErrCapacityExceeded
		}
	}
	return nil
}

func marshalEnvelope(request MutationRequest, operationID, commandID uuid.UUID, authorizationRevision, desiredRevision uint64, now, expires time.Time, signer *commandauth.Signer) ([]byte, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	envelope := &agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: request.NodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), ExpectedRevision: authorizationRevision, Traceparent: request.Traceparent, ActorId: request.ActorID, Reason: request.Reason, OperationId: operationID[:], Action: actionFor(request.Kind), RequiredCapability: capabilityFor(request.Kind), DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
	switch request.Kind {
	case UserCreate:
		envelope.Payload = &agentv1.CommandEnvelope_UserCreate{UserCreate: &agentv1.UserCreate{Username: request.Name, SealedPasswordV1: sealedSecretProto(request.SealedPassword), DesiredRevision: desiredRevision}}
	case UserDisable:
		envelope.Payload = &agentv1.CommandEnvelope_UserDisable{UserDisable: &agentv1.UserDisable{Username: request.Name, DesiredRevision: desiredRevision}}
	case UserEnable:
		envelope.Payload = &agentv1.CommandEnvelope_UserEnable{UserEnable: &agentv1.UserEnable{Username: request.Name, DesiredRevision: desiredRevision}}
	case UserPasswordRotate:
		envelope.Payload = &agentv1.CommandEnvelope_UserPasswordRotate{UserPasswordRotate: &agentv1.UserPasswordRotate{Username: request.Name, SealedPasswordV1: sealedSecretProto(request.SealedPassword), DesiredRevision: desiredRevision}}
	case GroupApply:
		envelope.Payload = &agentv1.CommandEnvelope_GroupApply{GroupApply: &agentv1.GroupApply{GroupName: request.Name, Members: request.Members, DesiredRevision: desiredRevision}}
	default:
		return nil, ErrInvalidRequest
	}
	if err := semanticpayload.PopulateV2(envelope); err != nil {
		return nil, err
	}
	if err := signer.Authorize(envelope); err != nil {
		return nil, fmt.Errorf("authorize desired-state command: %w", err)
	}
	return proto.Marshal(envelope)
}

func sealedSecretProto(secret *SealedSecret) *agentv1.SealedSecretV1 {
	if secret == nil {
		return nil
	}
	return &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion(secret.Version), Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: secret.KeyID, Ciphertext: append([]byte(nil), secret.Ciphertext...)}
}

func desiredFingerprint(kind MutationKind, name string, members []string) [32]byte {
	enabled := kind != UserDisable
	var encoded []byte
	if kind == GroupApply {
		encoded, _ = json.Marshal(struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}{Name: name, Members: members})
	} else {
		encoded, _ = json.Marshal(struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}{Name: name, Enabled: enabled})
	}
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
	}{request.NodeID, request.Kind, request.Name, sealedKey(request.SealedPassword), normalizeMembers(request.Members), sealedBytes(request.SealedPassword), request.ExpectedVersion, int64(request.TTL / time.Second), request.ActorID, request.Reason}
	encoded, _ := json.Marshal(value)
	return sha256.Sum256(encoded)
}

func sealedKey(secret *SealedSecret) string {
	if secret == nil {
		return ""
	}
	return fmt.Sprintf("%d:%s:%s", secret.Version, secret.Purpose, secret.KeyID)
}
func sealedBytes(secret *SealedSecret) []byte {
	if secret == nil {
		return nil
	}
	return secret.Ciphertext
}

func normalizeMembers(members []string) []string {
	if len(members) == 0 {
		return []string{}
	}
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
	case UserEnable:
		return "user.enable"
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
	if item.OperationState != nil && (*item.OperationState == "queued" || *item.OperationState == "dispatched" || *item.OperationState == "accepted" || *item.OperationState == "running") {
		if nodeStatus == "offline" {
			return "offline_pending"
		}
		return "pending"
	}
	if item.DesiredRevision != nil && item.ObservedRevision != nil && *item.DesiredRevision == *item.ObservedRevision && item.DesiredFingerprint == item.ObservedFingerprint {
		return "converged"
	}
	return "drifted"
}

func findIdempotent(ctx context.Context, tx database.Tx, workspaceID uuid.UUID, key string, hash []byte) (operationstore.Operation, bool, error) {
	store, err := operationdata.FromTransaction(tx)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	return store.FindIdempotent(ctx, workspaceID, key, hash)
}

func supersedePending(ctx context.Context, users userstore.Store, nodeID uuid.UUID, resourceType, resourceKey string, kind MutationKind, currentRevision int64, at value.Timestamp) (bool, error) {
	switch kind {
	case UserCreate:
		return false, nil
	case GroupApply, UserPasswordRotate, UserDisable, UserEnable:
		return users.SupersedePending(ctx, nodeID, resourceType, resourceKey, string(kind), currentRevision-1, at)
	default:
		return false, ErrInvalidRequest
	}
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
func rollback(tx database.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
