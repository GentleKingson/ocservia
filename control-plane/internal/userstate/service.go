// Package userstate manages node-scoped desired and observed Ocserv users and groups.
package userstate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationdata "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	"github.com/google/uuid"
)

var (
	ErrInvalidRequest      = errors.New("user or group request is invalid")
	ErrCapacityExceeded    = errors.New("managed user or group capacity exceeded")
	ErrVersionConflict     = errors.New("desired state version is stale")
	ErrRevisionPending     = errors.New("the current desired revision is still pending")
	ErrRevisionRecovery    = errors.New("the current desired revision requires same-kind recovery")
	ErrNotFound            = errors.New("desired resource was not found")
	ErrNodeUnavailable     = errors.New("node is unavailable")
	ErrCapabilityMissing   = operations.ErrCapabilityMissing
	ErrIdempotencyConflict = operations.ErrIdempotencyConflict
	ErrBacklogExceeded     = operationdata.ErrBacklogExceeded
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
	Kind                 string        `json:"kind"`
	Name                 string        `json:"name"`
	DesiredEnabled       *bool         `json:"desired_enabled,omitempty"`
	ObservedEnabled      *bool         `json:"observed_enabled,omitempty"`
	DesiredMembers       []string      `json:"desired_members,omitempty"`
	ObservedMembers      []string      `json:"observed_members,omitempty"`
	DesiredVersion       *int64        `json:"desired_version,omitempty"`
	DesiredRevision      *int64        `json:"desired_revision,omitempty"`
	ObservedRevision     *int64        `json:"observed_revision,omitempty"`
	Convergence          string        `json:"convergence"`
	OperationID          *string       `json:"operation_id,omitempty"`
	OperationState       *string       `json:"operation_state,omitempty"`
	RecoveryRequired     bool          `json:"recovery_required"`
	RecoveryMutationKind *MutationKind `json:"recovery_mutation_kind,omitempty"`
	ObservedAt           *time.Time    `json:"observed_at,omitempty"`
}

type Service struct {
	backend database.Backend
	now     func() time.Time
	signer  *commandauth.Signer
	queue   *operations.Service
}

func New(backend database.Backend) *Service {
	return NewWithOperations(backend, nil, operations.New(backend, 0, nil))
}

func NewWithSigner(backend database.Backend, signer *commandauth.Signer) *Service {
	return NewWithOperations(backend, signer, operations.New(backend, 0, signer))
}

func NewWithOperations(backend database.Backend, signer *commandauth.Signer, queue *operations.Service) *Service {
	return &Service{backend: backend, signer: signer, queue: queue, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Mutate(ctx context.Context, request MutationRequest) (operations.Operation, bool, error) {
	return s.mutate(ctx, request, true)
}

// MutatePolicyEnforcement is the scheduler-only entry point for policy-driven
// enable and disable commands. Browser mutations must use Mutate so a missing
// session can never become an implicit trusted-principal bypass.
func (s *Service) MutatePolicyEnforcement(ctx context.Context, request MutationRequest) (operations.Operation, bool, error) {
	if request.ActorID != "scheduler" || request.ActorIdentityID != uuid.Nil || request.ActorSessionID != uuid.Nil || (request.Kind != UserEnable && request.Kind != UserDisable) {
		return operations.Operation{}, false, ErrInvalidRequest
	}
	return s.mutate(ctx, request, false)
}

func (s *Service) mutate(ctx context.Context, request MutationRequest, authorizeSession bool) (operations.Operation, bool, error) {
	if err := validateMutation(request); err != nil {
		return operations.Operation{}, false, err
	}
	request.Members = normalizeMembers(request.Members)
	digest := requestDigest(request)
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return operations.Operation{}, false, fmt.Errorf("begin desired state transaction: %w", err)
	}
	defer rollback(tx)
	users := userstore.From(tx)
	operationStore := operationdata.FromTransaction(tx)
	node, err := operationStore.LockNode(ctx, request.NodeID)
	if errors.Is(err, database.ErrNotFound) {
		return operations.Operation{}, false, ErrNodeUnavailable
	}
	if err != nil {
		return operations.Operation{}, false, err
	}
	if node.Status != "active" && node.Status != "offline" {
		return operations.Operation{}, false, ErrNodeUnavailable
	}
	workspaceID := node.WorkspaceID
	if authorizeSession {
		if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, workspaceID); err != nil {
			return operations.Operation{}, false, err
		}
	}
	if request.SealedPassword != nil {
		registered, err := users.HasSealingKey(ctx, request.NodeID, request.SealedPassword.Version, request.SealedPassword.KeyID)
		if err != nil {
			return operations.Operation{}, false, err
		}
		if !registered {
			return operations.Operation{}, false, ErrInvalidRequest
		}
	}
	if existing, err := operations.ReplayTx(ctx, tx, operationStore, workspaceID, request.IdempotencyKey, digest); err != nil || existing.ID != "" {
		return existing, err == nil, err
	}
	if err := operations.RequireAttestation(ctx, operationStore, request.NodeID); err != nil {
		return operations.Operation{}, false, err
	}
	approved, err := operationStore.HasCapability(ctx, request.NodeID, capabilityFor(request.Kind))
	if err != nil {
		return operations.Operation{}, false, err
	}
	if !approved {
		return operations.Operation{}, false, ErrCapabilityMissing
	}
	currentVersion, currentRevision, err := lockDesired(ctx, users, request)
	if err != nil {
		return operations.Operation{}, false, err
	}
	if currentVersion != request.ExpectedVersion {
		return operations.Operation{}, false, ErrVersionConflict
	}
	resourceType := resourceTypeFor(request.Kind)
	replaceRevision, err := revisionReplacement(ctx, users, request.NodeID, resourceType, request.Name, request.Kind, currentRevision)
	if err != nil {
		return operations.Operation{}, false, err
	}
	if request.Kind == UserCreate && currentVersion > 0 && !replaceRevision {
		return operations.Operation{}, false, ErrVersionConflict
	}
	now := s.now().UTC()
	if err := ensureMutationCapacity(ctx, users, request, currentVersion == 0, now); err != nil {
		return operations.Operation{}, false, err
	}
	coalesced, err := supersedePending(ctx, users, request.NodeID, resourceType, request.Name, request.Kind, currentRevision, now)
	if err != nil {
		return operations.Operation{}, false, err
	}
	nextVersion, nextRevision := currentVersion+1, currentRevision+1
	commandExpectedRevision := currentRevision
	if coalesced || replaceRevision {
		// Replacing a command that never applied must not leave a revision gap.
		nextRevision = currentRevision
		commandExpectedRevision = currentRevision - 1
	}
	expiresAt := now.Add(request.TTL)
	op, err := s.queue.EnqueueTx(ctx, tx, operationStore, operations.QueueIntent{
		WorkspaceID: workspaceID, NodeID: request.NodeID, Key: request.IdempotencyKey, RequestID: request.RequestID,
		Traceparent: request.Traceparent, Digest: digest, Now: now, Expires: expiresAt, Available: now,
		ExpectedVersion: commandExpectedRevision, ResourceType: &resourceType, ResourceKey: &request.Name,
	}, func(ids operations.QueueIDs) ([]byte, string, error) {
		envelope, err := marshalEnvelope(request, ids.Operation, ids.Command, node.AuthorizationRevision, uint64(nextRevision), now, expiresAt, s.signer)
		return envelope, string(request.Kind), err
	}, func(operations.QueueIDs) error {
		err := users.WriteDesired(ctx, userstore.Desired{NodeID: request.NodeID, Kind: string(request.Kind), Name: request.Name, Version: nextVersion, Revision: nextRevision, Members: request.Members, At: now})
		if errors.Is(err, database.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}, func(ids operations.QueueIDs) error {
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{EventID: ids.Audit, WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: actionFor(request.Kind), ResourceType: "operation", ResourceID: ids.Operation, NodeID: &request.NodeID, CommandID: &ids.Command, RequestID: request.RequestID, TraceID: tracecontext.TraceID(request.Traceparent), Reason: request.Reason, At: now})
	})
	return op, false, err
}

func (s *Service) List(ctx context.Context, nodeID uuid.UUID) ([]ResourceState, error) {
	result := []ResourceState{}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		rows, err := userstore.From(tx).List(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("list desired observed state: %w", err)
		}
		for _, v := range rows {
			item := ResourceState{Kind: v.Kind, Name: v.Name, DesiredEnabled: v.DesiredEnabled, ObservedEnabled: v.ObservedEnabled,
				DesiredMembers: v.DesiredMembers, ObservedMembers: v.ObservedMembers,
				DesiredVersion: v.DesiredVersion, DesiredRevision: v.DesiredRevision, ObservedRevision: v.ObservedRevision,
				OperationState: v.OperationState,
				ObservedAt:     v.ObservedAt}
			if v.OperationID != nil {
				id := v.OperationID.String()
				item.OperationID = &id
			}
			item.RecoveryRequired, item.RecoveryMutationKind = recoveryMetadata(v.CommandState, v.PayloadType)
			item.Convergence = convergence(item, v.NodeStatus)
			result = append(result, item)
		}
		return nil
	})
	return result, err
}

func validateMutation(request MutationRequest) error {
	if request.NodeID == uuid.Nil || !namePattern.MatchString(request.Name) || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || request.ExpectedVersion < 0 || request.TTL < time.Second || request.TTL > 24*time.Hour || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || request.RequestID == "" || tracecontext.TraceID(request.Traceparent) == "" {
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
	state, priorKind := previous.State, MutationKind(previous.Kind)
	switch state {
	case "succeeded":
		return false, nil
	case "failed", "expired", "rolled_back":
		if priorKind != kind {
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

func recoveryMetadata(commandState, payloadType *string) (bool, *MutationKind) {
	if commandState == nil || payloadType == nil {
		return false, nil
	}
	replaceable := *commandState == "failed" || *commandState == "expired" || *commandState == "rolled_back"
	required := replaceable || *commandState == "rejected"
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
	fresh := now.Add(-90 * time.Second).UTC()
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
	envelope, err := operations.NewCommandEnvelope(request.NodeID, operationID, commandID, authorizationRevision, now, expires, request.Traceparent, request.ActorID, actionFor(request.Kind), request.Reason)
	if err != nil {
		return nil, err
	}
	envelope.RequiredCapability = capabilityFor(request.Kind)
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
	return operations.SignCommandEnvelope(envelope, signer)
}

func sealedSecretProto(secret *SealedSecret) *agentv1.SealedSecretV1 {
	if secret == nil {
		return nil
	}
	return &agentv1.SealedSecretV1{Version: secret.Version, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: secret.KeyID, Ciphertext: append([]byte(nil), secret.Ciphertext...)}
}

func requestDigest(request MutationRequest) canonicalcbor.IntentDigest {
	var key string
	var secret []byte
	if request.SealedPassword != nil {
		key = fmt.Sprintf("%d:%s:%s", request.SealedPassword.Version, request.SealedPassword.Purpose, request.SealedPassword.KeyID)
		secret = request.SealedPassword.Ciphertext
	}
	value := struct {
		Node          uuid.UUID
		Kind          MutationKind
		Name, Key     string
		Members       []string
		Secret        []byte
		Version       int64
		TTL           int64
		Actor, Reason string
	}{request.NodeID, request.Kind, request.Name, key, request.Members, secret, request.ExpectedVersion, int64(request.TTL / time.Second), request.ActorID, request.Reason}
	return canonicalcbor.NewIntentDigest("desired-state", struct {
		Intent                      any
		ActorIdentity, ActorSession uuid.UUID
	}{value, request.ActorIdentityID, request.ActorSessionID})
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
	if item.DesiredRevision != nil && item.ObservedRevision != nil && *item.DesiredRevision == *item.ObservedRevision {
		if item.Kind == "group" && slices.Equal(item.DesiredMembers, item.ObservedMembers) {
			return "converged"
		}
		if item.Kind == "user" && item.DesiredEnabled != nil && item.ObservedEnabled != nil && *item.DesiredEnabled == *item.ObservedEnabled {
			return "converged"
		}
	}
	return "drifted"
}

func supersedePending(ctx context.Context, users userstore.Store, nodeID uuid.UUID, resourceType, resourceKey string, kind MutationKind, currentRevision int64, at time.Time) (bool, error) {
	switch kind {
	case UserCreate:
		return false, nil
	case GroupApply, UserPasswordRotate, UserDisable, UserEnable:
		return users.SupersedePending(ctx, nodeID, resourceType, resourceKey, string(kind), currentRevision-1, at)
	default:
		return false, ErrInvalidRequest
	}
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func rollback(tx database.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
