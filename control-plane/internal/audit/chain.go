package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

const (
	chainResultIntent        = "intent"
	eventAuthVersionV1       = int16(1)
	eventAuthDomainV1        = "ocservia/audit-event/v1\x00"
	transitionActionV1       = "audit.auth.transition"
	transitionActorTypeV1    = "controller"
	transitionActorIDV1      = "audit-auth-v1"
	transitionResourceTypeV1 = "audit_chain"
	authenticityMigrationV1  = int64(21)
)

var eventKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type eventAuthenticator struct {
	keyID string
	key   [sha256.Size]byte
}

var processEventAuth struct {
	sync.Mutex
	auth   *eventAuthenticator
	frozen bool
}

type ChainRecord struct {
	EventID                                  uuid.UUID
	WorkspaceID                              uuid.UUID
	ActorType, ActorID, Action, ResourceType string
	ResourceID                               uuid.UUID
	RequestID, TraceID, Reason               string
	Result, ErrorType                        string
	SessionID, NodeID, CommandID, ApprovalID *uuid.UUID
	BeforeSummary, AfterSummary              json.RawMessage
	At                                       time.Time
}

type Verification struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Events      int64     `json:"events"`
	Valid       bool      `json:"valid"`
	Checkpoint  bool      `json:"checkpoint_valid"`
}

type Manager struct {
	backend       database.Backend
	checkpointKey []byte
	eventKeys     map[string][sha256.Size]byte
	current       eventAuthenticator
}

func NewBackendManager(backend database.Backend, checkpointKey []byte) *Manager {
	auth := currentEventAuthenticator()
	return newManager(backend, checkpointKey, auth)
}

func NewBackendManagerWithEventKey(backend database.Backend, checkpointKey []byte, keyID string, key []byte) (*Manager, error) {
	auth, err := configureEventAuthenticator(keyID, key)
	if err != nil {
		return nil, err
	}
	return newManager(backend, checkpointKey, auth), nil
}

func newManager(backend database.Backend, checkpointKey []byte, auth eventAuthenticator) *Manager {
	return &Manager{
		backend: backend, checkpointKey: append([]byte(nil), checkpointKey...), current: auth,
		eventKeys: map[string][sha256.Size]byte{auth.keyID: auth.key},
	}
}

func configureEventAuthenticator(keyID string, key []byte) (eventAuthenticator, error) {
	if !eventKeyIDPattern.MatchString(keyID) || len(key) != sha256.Size {
		return eventAuthenticator{}, errors.New("audit event key ID or key is invalid")
	}
	var configured eventAuthenticator
	configured.keyID = keyID
	copy(configured.key[:], key)
	processEventAuth.Lock()
	defer processEventAuth.Unlock()
	if processEventAuth.frozen {
		if processEventAuth.auth == nil || processEventAuth.auth.keyID != configured.keyID || subtle.ConstantTimeCompare(processEventAuth.auth.key[:], configured.key[:]) != 1 {
			return eventAuthenticator{}, errors.New("audit event authenticator is already in use")
		}
		return *processEventAuth.auth, nil
	}
	processEventAuth.auth = &configured
	processEventAuth.frozen = true
	return configured, nil
}

func currentEventAuthenticator() eventAuthenticator {
	processEventAuth.Lock()
	defer processEventAuth.Unlock()
	if processEventAuth.auth == nil {
		var key [sha256.Size]byte
		keyID := ""
		if os.Getenv("OCSERV_ENVIRONMENT") == "test" && os.Getenv("OCSERV_TEST_AUDIT_EVENT_KEY_HEX") != "" {
			decoded, err := hex.DecodeString(os.Getenv("OCSERV_TEST_AUDIT_EVENT_KEY_HEX"))
			if err != nil || len(decoded) != sha256.Size {
				panic("initialize test audit event authenticator: invalid key")
			}
			copy(key[:], decoded)
			keyID = os.Getenv("OCSERV_AUDIT_EVENT_KEY_ID")
			if keyID == "" {
				keyID = "test-audit-event-v1"
			}
		} else if _, err := rand.Read(key[:]); err != nil {
			panic(fmt.Sprintf("initialize audit event authenticator: %v", err))
		}
		if keyID == "" {
			digest := sha256.Sum256(key[:])
			keyID = "ephemeral-" + hex.EncodeToString(digest[:8])
		}
		if !eventKeyIDPattern.MatchString(keyID) {
			panic("initialize audit event authenticator: invalid key ID")
		}
		processEventAuth.auth = &eventAuthenticator{keyID: keyID, key: key}
	}
	processEventAuth.frozen = true
	return *processEventAuth.auth
}

func signEvent(key [sha256.Size]byte, eventHash []byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(eventAuthDomainV1))
	_, _ = mac.Write(eventHash)
	return mac.Sum(nil)
}

func LockChainTx(ctx context.Context, tx database.Tx, workspaceID uuid.UUID) error {
	s, err := auditstore.From(tx)
	if err != nil {
		return err
	}
	if err := s.Lock(ctx, workspaceID); err != nil {
		return fmt.Errorf("lock audit chain: %w", err)
	}
	return nil
}

func AppendChainTx(ctx context.Context, tx database.Tx, record ChainRecord) error {
	s, err := auditstore.From(tx)
	if err != nil {
		return err
	}
	auth := currentEventAuthenticator()
	if record.Result == "" {
		record.Result = chainResultIntent
	}
	if record.Result != "intent" && record.Result != "succeeded" && record.Result != "failed" {
		return errors.New("invalid audit result")
	}
	if err := LockChainTx(ctx, tx, record.WorkspaceID); err != nil {
		return err
	}
	record.At, err = s.Clock(ctx)
	if err != nil {
		return fmt.Errorf("assign audit order: %w", err)
	}
	// Hash the persisted microsecond value, never a higher-precision clock value.
	at, err := value.FromTime(record.At)
	if err != nil {
		return err
	}
	record.At, err = at.Time()
	if err != nil {
		return err
	}
	previous, err := s.Previous(ctx, record.WorkspaceID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return fmt.Errorf("read audit chain: %w", err)
	}
	if record.EventID == uuid.Nil {
		record.EventID = uuid.Must(uuid.NewV7())
	}
	before, err := summaryValue(record.BeforeSummary)
	if err != nil {
		return err
	}
	after, err := summaryValue(record.AfterSummary)
	if err != nil {
		return err
	}
	// Feed the unchanged canonicalizer the same logical JSON it will read back.
	// In particular, whitespace around a top-level null must not add a field.
	record.BeforeSummary, record.AfterSummary = before.Bytes(), after.Bytes()
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return fmt.Errorf("encode audit payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	var traceID any
	if record.TraceID != "" {
		traceID = record.TraceID
	}
	err = s.Append(ctx, []any{record.EventID, record.WorkspaceID, at, record.ActorType, record.ActorID, record.SessionID, record.Action, record.ResourceType, nullableID(record.ResourceID), record.NodeID, record.RequestID, traceID, record.CommandID, record.ApprovalID, record.Result, nullableString(record.Reason), before, after, nullableString(record.ErrorType), previous, digest[:], eventAuthVersionV1, auth.keyID, signEvent(auth.key, digest[:])})
	if err != nil {
		return fmt.Errorf("append audit intent: %w", err)
	}
	return nil
}

func encodeChainPayload(previous []byte, record ChainRecord) ([]byte, error) {
	before, err := canonicalJSON(record.BeforeSummary)
	if err != nil {
		return nil, err
	}
	after, err := canonicalJSON(record.AfterSummary)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Previous                                 []byte          `json:"previous"`
		EventID                                  uuid.UUID       `json:"event_id"`
		WorkspaceID                              uuid.UUID       `json:"workspace_id"`
		OccurredAt                               time.Time       `json:"occurred_at"`
		ActorType                                string          `json:"actor_type"`
		ActorID                                  string          `json:"actor_id"`
		Action                                   string          `json:"action"`
		ResourceType                             string          `json:"resource_type"`
		ResourceID                               uuid.UUID       `json:"resource_id"`
		RequestID                                string          `json:"request_id"`
		TraceID                                  string          `json:"trace_id"`
		Result                                   string          `json:"result"`
		Reason                                   string          `json:"reason"`
		SessionID, NodeID, CommandID, ApprovalID *uuid.UUID      `json:",omitempty"`
		BeforeSummary, AfterSummary              json.RawMessage `json:",omitempty"`
		ErrorType                                string          `json:"error_type,omitempty"`
	}{
		Previous: previous, EventID: record.EventID, WorkspaceID: record.WorkspaceID, OccurredAt: record.At.UTC(),
		ActorType: record.ActorType, ActorID: record.ActorID, Action: record.Action,
		ResourceType: record.ResourceType, ResourceID: record.ResourceID, RequestID: record.RequestID,
		TraceID: record.TraceID, Result: record.Result, Reason: record.Reason,
		SessionID: record.SessionID, NodeID: record.NodeID, CommandID: record.CommandID, ApprovalID: record.ApprovalID,
		BeforeSummary: before, AfterSummary: after, ErrorType: record.ErrorType,
	})
}

func canonicalJSON(input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 || string(input) == "null" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(input, &decoded); err != nil {
		// Preserve every previously signable v1 payload byte. Only extend the
		// formerly rejected domain for valid JSONB numbers outside float64.
		if logical, parseErr := value.ParseJSONB(input); parseErr == nil {
			return json.RawMessage(logical.Bytes()), nil
		}
		return nil, err
	}
	encoded, err := json.Marshal(decoded)
	return json.RawMessage(encoded), err
}

type checkpointRecord struct {
	eventID   uuid.UUID
	eventHash []byte
	signature []byte
}

type chainVerification struct {
	Verification
	legacyOnly bool
	lastID     uuid.UUID
	lastHash   []byte
}

func (m *Manager) Verify(ctx context.Context, workspaceID uuid.UUID) (Verification, error) {
	var verified chainVerification
	err := database.Within(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		var err error
		verified, err = m.verifyTx(ctx, tx, workspaceID, false)
		return err
	})
	return verified.Verification, err
}

func (m *Manager) verifyTx(ctx context.Context, tx database.Tx, workspaceID uuid.UUID, allowLegacyOnly bool) (chainVerification, error) {
	checkpoints, latest, err := m.readCheckpoints(ctx, tx, workspaceID)
	if err != nil {
		return chainVerification{}, err
	}
	s, err := auditstore.From(tx)
	if err != nil {
		return chainVerification{}, err
	}
	rows, err := s.Events(ctx, workspaceID)
	if err != nil {
		return chainVerification{}, err
	}
	defer rows.Close()
	verified := chainVerification{Verification: Verification{WorkspaceID: workspaceID, Valid: true}}
	var previous []byte
	var previousID uuid.UUID
	eventHashes := make(map[uuid.UUID][]byte)
	seenAuthenticated := false
	for rows.Next() {
		record := ChainRecord{WorkspaceID: workspaceID}
		var resourceID *uuid.UUID
		var storedPrevious, storedHash, eventMAC []byte
		var authVersion int16
		var eventKeyID *string
		var at value.Timestamp
		var before, after value.JSONB
		if err := rows.Scan(&record.EventID, &at, &record.ActorType, &record.ActorID, &record.Action, &record.ResourceType, &resourceID, &record.RequestID, &record.TraceID, &record.Result, &record.Reason, &record.SessionID, &record.NodeID, &record.CommandID, &record.ApprovalID, &before, &after, &record.ErrorType, &storedPrevious, &storedHash, &authVersion, &eventKeyID, &eventMAC); err != nil {
			return chainVerification{}, err
		}
		record.At, err = at.Time()
		if err != nil {
			return chainVerification{}, err
		}
		record.BeforeSummary, record.AfterSummary = before.Bytes(), after.Bytes()
		if resourceID != nil {
			record.ResourceID = *resourceID
		}
		payload, err := encodeChainPayload(previous, record)
		if err != nil {
			return chainVerification{}, err
		}
		digest := sha256.Sum256(payload)
		if subtle.ConstantTimeCompare(storedPrevious, previous) != 1 || subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
			verified.Valid = false
		}
		switch authVersion {
		case 0:
			if seenAuthenticated || eventKeyID != nil || len(eventMAC) != 0 {
				verified.Valid = false
			}
		case eventAuthVersionV1:
			if !seenAuthenticated && verified.Events > 0 {
				checkpoint, ok := checkpoints[previousID]
				if !ok || !m.validCheckpoint(workspaceID, checkpoint) || subtle.ConstantTimeCompare(checkpoint.eventHash, previous) != 1 || !isTransitionRecord(record) {
					verified.Valid = false
				}
			}
			seenAuthenticated = true
			if eventKeyID == nil {
				verified.Valid = false
				break
			}
			key, ok := m.eventKeys[*eventKeyID]
			if !ok || !hmac.Equal(eventMAC, signEvent(key, storedHash)) {
				verified.Valid = false
			}
		default:
			verified.Valid = false
		}
		previous = append(previous[:0], storedHash...)
		previousID = record.EventID
		verified.lastID = record.EventID
		verified.lastHash = append(verified.lastHash[:0], storedHash...)
		eventHashes[record.EventID] = append([]byte(nil), storedHash...)
		verified.Events++
	}
	if err := rows.Err(); err != nil {
		return chainVerification{}, err
	}
	verified.legacyOnly = verified.Events > 0 && !seenAuthenticated
	if verified.legacyOnly && !allowLegacyOnly {
		verified.Valid = false
	}
	if latest == nil {
		verified.Checkpoint = verified.Events == 0
	} else {
		checkpoint, encountered := checkpoints[latest.eventID]
		storedHash, eventEncountered := eventHashes[latest.eventID]
		verified.Checkpoint = encountered && eventEncountered && subtle.ConstantTimeCompare(storedHash, checkpoint.eventHash) == 1 && m.validCheckpoint(workspaceID, checkpoint)
	}
	return verified, nil
}

func (m *Manager) readCheckpoints(ctx context.Context, tx database.Tx, workspaceID uuid.UUID) (map[uuid.UUID]checkpointRecord, *checkpointRecord, error) {
	s, err := auditstore.From(tx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.Checkpoints(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	checkpoints := make(map[uuid.UUID]checkpointRecord)
	var latest *checkpointRecord
	for rows.Next() {
		var checkpoint checkpointRecord
		if err := rows.Scan(&checkpoint.eventID, &checkpoint.eventHash, &checkpoint.signature); err != nil {
			return nil, nil, err
		}
		checkpoints[checkpoint.eventID] = checkpoint
		copy := checkpoint
		latest = &copy
	}
	return checkpoints, latest, rows.Err()
}

func (m *Manager) validCheckpoint(workspaceID uuid.UUID, checkpoint checkpointRecord) bool {
	return len(m.checkpointKey) >= sha256.Size && hmac.Equal(checkpoint.signature, signCheckpoint(m.checkpointKey, workspaceID, checkpoint.eventID, checkpoint.eventHash))
}

func isTransitionRecord(record ChainRecord) bool {
	return record.ActorType == transitionActorTypeV1 && record.ActorID == transitionActorIDV1 && record.Action == transitionActionV1 && record.ResourceType == transitionResourceTypeV1 && record.ResourceID == record.WorkspaceID && record.Result == "succeeded"
}

func (m *Manager) Checkpoint(ctx context.Context, workspaceID uuid.UUID) error {
	if len(m.checkpointKey) < sha256.Size {
		return errors.New("audit checkpoint key is unavailable")
	}
	return database.Within(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		if err := LockChainTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		verified, err := m.verifyTx(ctx, tx, workspaceID, false)
		if err != nil {
			return err
		}
		if !verified.Valid || verified.Events == 0 {
			return errors.New("audit chain is not authenticated")
		}
		s, err := auditstore.From(tx)
		if err != nil {
			return err
		}
		err = s.AddCheckpoint(ctx, uuid.Must(uuid.NewV7()), workspaceID, verified.lastID, verified.lastHash, signCheckpoint(m.checkpointKey, workspaceID, verified.lastID, verified.lastHash))
		if err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
}

func (m *Manager) CheckpointAll(ctx context.Context) error {
	if len(m.checkpointKey) == 0 {
		return nil
	}
	s, err := auditstore.From(m.backend)
	if err != nil {
		return err
	}
	rows, err := s.Workspaces(ctx)
	if err != nil {
		return err
	}
	var workspaces []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range workspaces {
		if err := m.Checkpoint(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) EnsureAuthenticity(ctx context.Context) error {
	s, err := auditstore.From(m.backend)
	if err != nil {
		return err
	}
	rows, err := s.Workspaces(ctx)
	if err != nil {
		return fmt.Errorf("list audit workspaces: %w", err)
	}
	var workspaces []uuid.UUID
	for rows.Next() {
		var workspaceID uuid.UUID
		if err := rows.Scan(&workspaceID); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, workspaceID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, workspaceID := range workspaces {
		if err := m.ensureWorkspaceAuthenticity(ctx, workspaceID); err != nil {
			return fmt.Errorf("initialize audit authenticity for workspace %s: %w", workspaceID, err)
		}
	}
	return nil
}

func (m *Manager) PreflightAuthenticityMigration(ctx context.Context, tx database.Tx, version int64) error {
	if version != authenticityMigrationV1 {
		return nil
	}
	store, err := auditstore.From(tx)
	if err != nil {
		return err
	}
	legacy, ok := store.(auditstore.LegacyPreflight)
	if !ok {
		return database.ErrUnsupported
	}
	if err := legacy.LockLegacy(ctx); err != nil {
		return fmt.Errorf("lock legacy audit history: %w", err)
	}
	rows, err := store.Workspaces(ctx)
	if err != nil {
		return fmt.Errorf("list legacy audit workspaces: %w", err)
	}
	var workspaces []uuid.UUID
	for rows.Next() {
		var workspaceID uuid.UUID
		if err := rows.Scan(&workspaceID); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, workspaceID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, workspaceID := range workspaces {
		if err := m.preflightLegacyWorkspace(ctx, tx, legacy, workspaceID); err != nil {
			return fmt.Errorf("preflight audit authenticity for workspace %s: %w", workspaceID, err)
		}
	}
	return nil
}

func (m *Manager) preflightLegacyWorkspace(ctx context.Context, tx database.Tx, legacy auditstore.LegacyPreflight, workspaceID uuid.UUID) error {
	checkpoints, _, err := m.readCheckpoints(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	rows, err := legacy.LegacyEvents(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var previous, lastHash []byte
	var lastID uuid.UUID
	var events int64
	for rows.Next() {
		record := ChainRecord{WorkspaceID: workspaceID}
		var resourceID *uuid.UUID
		var storedPrevious, storedHash []byte
		if err := rows.Scan(&record.EventID, &record.At, &record.ActorType, &record.ActorID, &record.Action, &record.ResourceType, &resourceID, &record.RequestID, &record.TraceID, &record.Result, &record.Reason, &record.SessionID, &record.NodeID, &record.CommandID, &record.ApprovalID, &record.BeforeSummary, &record.AfterSummary, &record.ErrorType, &storedPrevious, &storedHash); err != nil {
			return err
		}
		if resourceID != nil {
			record.ResourceID = *resourceID
		}
		payload, err := encodeChainPayload(previous, record)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		if subtle.ConstantTimeCompare(storedPrevious, previous) != 1 || subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
			return errors.New("legacy audit chain integrity is invalid")
		}
		previous = append(previous[:0], storedHash...)
		lastHash = append(lastHash[:0], storedHash...)
		lastID = record.EventID
		events++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if events == 0 {
		return nil
	}
	checkpoint, ok := checkpoints[lastID]
	if !ok || !m.validCheckpoint(workspaceID, checkpoint) || subtle.ConstantTimeCompare(checkpoint.eventHash, lastHash) != 1 {
		return errors.New("legacy audit tail is not checkpointed")
	}
	return nil
}

func (m *Manager) ensureWorkspaceAuthenticity(ctx context.Context, workspaceID uuid.UUID) error {
	return database.Within(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		if err := LockChainTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		verified, err := m.verifyTx(ctx, tx, workspaceID, true)
		if err != nil {
			return err
		}
		if !verified.Valid {
			return errors.New("audit chain integrity or origin authentication is invalid")
		}
		if !verified.legacyOnly {
			return nil
		}
		if !verified.Checkpoint {
			return errors.New("legacy audit tail is not checkpointed")
		}
		s, err := auditstore.From(tx)
		if err != nil {
			return err
		}
		checkpointHash, err := s.CheckpointHash(ctx, workspaceID, verified.lastID)
		if err != nil {
			return errors.New("legacy audit checkpoint does not cover the tail")
		}
		if subtle.ConstantTimeCompare(checkpointHash, verified.lastHash) != 1 {
			return errors.New("legacy audit checkpoint hash does not match the tail")
		}
		if err := AppendChainTx(ctx, tx, ChainRecord{
			WorkspaceID: workspaceID, ActorType: transitionActorTypeV1, ActorID: transitionActorIDV1,
			Action: transitionActionV1, ResourceType: transitionResourceTypeV1, ResourceID: workspaceID,
			RequestID: "audit-auth-transition-v1", Result: "succeeded", Reason: "checkpoint-anchored audit event authentication transition",
		}); err != nil {
			return err
		}
		return nil
	})
}

func signCheckpoint(key []byte, workspaceID, eventID uuid.UUID, hash []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(workspaceID[:])
	_, _ = mac.Write(eventID[:])
	_, _ = mac.Write(hash)
	return mac.Sum(nil)
}

func nullableID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func summaryValue(raw json.RawMessage) (value.JSONB, error) {
	if len(raw) == 0 {
		return value.JSONB{}, nil
	}
	return value.ParseJSONB(raw)
}
