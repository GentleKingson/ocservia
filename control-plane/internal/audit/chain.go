package audit

import (
	"bytes"
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
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

const (
	chainResultIntent  = "intent"
	eventAuthVersionV1 = int16(1)
	eventAuthDomainV1  = "ocservia/audit-event/v1\x00"
)

var eventKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ErrVerificationBusy rejects overlapping public full-history scans.
var ErrVerificationBusy = errors.New("audit: verification already running")

// Public full-history scans share one slot across managers and workspaces.
var verificationAdmission sync.Mutex

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
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	Events          int64     `json:"events"`
	CompactedEvents int64     `json:"compacted_events"`
	Valid           bool      `json:"valid"`
	Checkpoint      bool      `json:"checkpoint_valid"`
}

type Manager struct {
	backend       database.Backend
	checkpointKey []byte
	eventKeys     map[string][sha256.Size]byte
	current       eventAuthenticator
}

func NewManager(backend database.Backend, checkpointKey []byte) *Manager {
	auth := currentEventAuthenticator()
	return newManager(backend, checkpointKey, auth)
}

func NewManagerWithEventKey(backend database.Backend, checkpointKey []byte, keyID string, key []byte) (*Manager, error) {
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
			var label [8]byte
			rand.Read(label[:])
			keyID = "ephemeral-" + hex.EncodeToString(label[:])
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
	if err := auditstore.Of(tx).Lock(ctx, workspaceID); err != nil {
		return fmt.Errorf("lock audit chain: %w", err)
	}
	return nil
}

func AppendChainTx(ctx context.Context, tx database.Tx, record ChainRecord) error {
	s := auditstore.Of(tx)
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
	var err error
	record.At, err = s.Clock(ctx)
	if err != nil {
		return fmt.Errorf("assign audit order: %w", err)
	}
	// Hash the persisted microsecond value, never a higher-precision clock value.
	at := record.At.UTC().Truncate(time.Microsecond)
	record.At = at
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
	record.BeforeSummary, record.AfterSummary = before, after
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return fmt.Errorf("encode audit payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	err = s.Append(ctx, auditstore.EventRow{
		ID:            record.EventID,
		WorkspaceID:   record.WorkspaceID,
		At:            at,
		ActorType:     record.ActorType,
		ActorID:       record.ActorID,
		SessionID:     record.SessionID,
		Action:        record.Action,
		ResourceType:  record.ResourceType,
		ResourceID:    record.ResourceID,
		NodeID:        record.NodeID,
		RequestID:     record.RequestID,
		TraceID:       record.TraceID,
		CommandID:     record.CommandID,
		ApprovalID:    record.ApprovalID,
		Result:        record.Result,
		Reason:        record.Reason,
		BeforeSummary: before,
		AfterSummary:  after,
		ErrorType:     record.ErrorType,
		PreviousHash:  previous,
		EventHash:     digest[:],
		AuthVersion:   eventAuthVersionV1,
		EventKeyID:    &auth.keyID,
		EventMAC:      signEvent(auth.key, digest[:]),
	})
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
	lastID   uuid.UUID
	lastHash []byte
}

func (m *Manager) Verify(ctx context.Context, workspaceID uuid.UUID) (Verification, error) {
	if !verificationAdmission.TryLock() {
		return Verification{}, ErrVerificationBusy
	}
	defer verificationAdmission.Unlock()
	var verified chainVerification
	err := database.Within(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		var err error
		verified, err = m.verifyTx(ctx, tx, workspaceID)
		return err
	})
	return verified.Verification, err
}

func recordFromEventRow(ev auditstore.EventRow) ChainRecord {
	return ChainRecord{
		EventID:       ev.ID,
		WorkspaceID:   ev.WorkspaceID,
		ActorType:     ev.ActorType,
		ActorID:       ev.ActorID,
		Action:        ev.Action,
		ResourceType:  ev.ResourceType,
		ResourceID:    ev.ResourceID,
		RequestID:     ev.RequestID,
		TraceID:       ev.TraceID,
		Reason:        ev.Reason,
		Result:        ev.Result,
		ErrorType:     ev.ErrorType,
		SessionID:     ev.SessionID,
		NodeID:        ev.NodeID,
		CommandID:     ev.CommandID,
		ApprovalID:    ev.ApprovalID,
		BeforeSummary: ev.BeforeSummary,
		AfterSummary:  ev.AfterSummary,
		At:            ev.At.UTC().Truncate(time.Microsecond),
	}
}

func (m *Manager) validEventHash(event auditstore.EventRow, record ChainRecord, previous []byte) (bool, error) {
	if !bytes.Equal(event.PreviousHash, previous) {
		return false, nil
	}
	if event.CompactedAt != nil {
		return event.AuthVersion == eventAuthVersionV1 && m.validCompaction(record, event.PreviousHash, event.EventHash, event.EventMAC, *event.CompactedAt, event.CompactionKeyID, event.CompactionMAC), nil
	}
	if event.CompactionKeyID != nil || len(event.CompactionMAC) != 0 {
		return false, nil
	}
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(payload)
	return bytes.Equal(event.EventHash, digest[:]), nil
}

func (m *Manager) validEventMAC(event auditstore.EventRow) bool {
	if event.EventKeyID == nil {
		return false
	}
	key, ok := m.eventKeys[*event.EventKeyID]
	return ok && hmac.Equal(event.EventMAC, signEvent(key, event.EventHash))
}

func (m *Manager) verifyTx(ctx context.Context, tx database.Tx, workspaceID uuid.UUID) (chainVerification, error) {
	s := auditstore.Of(tx)
	latestID, latestHash, latestSignature, _, _, err := s.LatestCheckpoint(ctx, workspaceID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return chainVerification{}, err
	}
	verified := chainVerification{Verification: Verification{WorkspaceID: workspaceID, Valid: true}}
	var previous []byte
	var checkpointHash []byte
	err = s.Events(ctx, workspaceID, func(event auditstore.EventRow) error {
		event.WorkspaceID = workspaceID
		record := recordFromEventRow(event)
		hashValid, err := m.validEventHash(event, record, previous)
		if err != nil {
			return err
		}
		if event.CompactedAt != nil {
			verified.CompactedEvents++
		}
		if !hashValid || event.AuthVersion != eventAuthVersionV1 || !m.validEventMAC(event) {
			verified.Valid = false
		}
		previous = append(previous[:0], event.EventHash...)
		verified.lastID = record.EventID
		verified.lastHash = append(verified.lastHash[:0], event.EventHash...)
		if record.EventID == latestID {
			checkpointHash = append([]byte(nil), event.EventHash...)
		}
		verified.Events++
		return nil
	})
	if err != nil {
		return chainVerification{}, err
	}
	if latestID == uuid.Nil {
		verified.Checkpoint = verified.Events == 0
	} else {
		verified.Checkpoint = len(checkpointHash) != 0 && bytes.Equal(checkpointHash, latestHash) &&
			m.validCheckpoint(workspaceID, checkpointRecord{eventID: latestID, eventHash: latestHash, signature: latestSignature})
	}
	return verified, nil
}

func (m *Manager) validCheckpoint(workspaceID uuid.UUID, checkpoint checkpointRecord) bool {
	return len(m.checkpointKey) >= sha256.Size && hmac.Equal(checkpoint.signature, signCheckpoint(m.checkpointKey, workspaceID, checkpoint.eventID, checkpoint.eventHash))
}

func (m *Manager) Checkpoint(ctx context.Context, workspaceID uuid.UUID) error {
	if len(m.checkpointKey) < sha256.Size {
		return errors.New("audit checkpoint key is unavailable")
	}
	// Lease renewal can invalidate this snapshot before the final fence check.
	// Retry only a rolled-back transaction, rechecking the chain and the fence.
	return database.WithinRetry(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		if err := LockChainTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		s := auditstore.Of(tx)
		lastID, lastHash, alreadyCheckpointed, err := m.verifyCheckpointDeltaTx(ctx, tx, s, workspaceID, true)
		if err != nil {
			return err
		}
		if !alreadyCheckpointed {
			if err := s.AddCheckpoint(ctx, uuid.Must(uuid.NewV7()), workspaceID, lastID, lastHash, signCheckpoint(m.checkpointKey, workspaceID, lastID, lastHash)); err != nil {
				return err
			}
		}
		return nil
	})
}

// verifyCheckpointDeltaTx validates the authenticated checkpoint anchor and
// the appended suffix under the workspace advisory lock. If the chain has yet
// to be anchored, the caller can request a full genesis scan.
func (m *Manager) verifyCheckpointDeltaTx(ctx context.Context, tx database.Tx, s auditstore.Store, workspaceID uuid.UUID, verifyGenesis bool) (uuid.UUID, []byte, bool, error) {
	tailID, tailHash, err := s.LatestEvent(ctx, workspaceID)
	if errors.Is(err, database.ErrNotFound) && !verifyGenesis {
		return uuid.Nil, nil, false, nil
	}
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	cpEventID, cpHash, cpSignature, cpOccurredAt, cpAuthVersion, err := s.LatestCheckpoint(ctx, workspaceID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return uuid.Nil, nil, false, err
	}
	if err == nil {
		if cpOccurredAt.IsZero() {
			return uuid.Nil, nil, false, errors.New("audit checkpoint anchor is missing")
		}
		if cpAuthVersion != eventAuthVersionV1 || !m.validCheckpoint(workspaceID, checkpointRecord{eventID: cpEventID, eventHash: cpHash, signature: cpSignature}) {
			return uuid.Nil, nil, false, errors.New("audit checkpoint is not authenticated")
		}
		var previous []byte
		var lastID uuid.UUID
		var deltaCount int64
		valid := true
		err = s.EventsFrom(ctx, workspaceID, cpOccurredAt, cpEventID, func(event auditstore.EventRow) error {
			event.WorkspaceID = workspaceID
			record := recordFromEventRow(event)
			if deltaCount == 0 {
				if record.EventID != cpEventID || !bytes.Equal(cpHash, event.EventHash) {
					return errors.New("audit checkpoint anchor is missing")
				}
				previous = bytes.Clone(event.PreviousHash)
			}
			hashValid, err := m.validEventHash(event, record, previous)
			if err != nil {
				return err
			}
			if !hashValid || event.AuthVersion != eventAuthVersionV1 || !m.validEventMAC(event) {
				valid = false
			}
			previous = append(previous[:0], event.EventHash...)
			lastID = record.EventID
			deltaCount++
			return nil
		})
		if err != nil {
			return uuid.Nil, nil, false, err
		}
		if deltaCount == 0 || !valid || lastID != tailID || !bytes.Equal(previous, tailHash) {
			return uuid.Nil, nil, false, errors.New("audit chain is not authenticated")
		}
		return lastID, previous, deltaCount == 1, nil
	}
	if !verifyGenesis {
		return uuid.Nil, nil, false, nil
	}
	verified, err := m.verifyTx(ctx, tx, workspaceID)
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	if !verified.Valid || verified.Events == 0 {
		return uuid.Nil, nil, false, errors.New("audit chain is not authenticated")
	}
	return verified.lastID, verified.lastHash, false, nil
}

func (m *Manager) CheckpointAll(ctx context.Context) error {
	if len(m.checkpointKey) == 0 {
		return nil
	}
	workspaces, err := auditstore.Of(m.backend).Workspaces(ctx)
	if err != nil {
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
	workspaces, err := auditstore.Of(m.backend).Workspaces(ctx)
	if err != nil {
		return fmt.Errorf("list audit workspaces: %w", err)
	}
	for _, workspaceID := range workspaces {
		if err := m.ensureWorkspaceAuthenticity(ctx, workspaceID); err != nil {
			return fmt.Errorf("initialize audit authenticity for workspace %s: %w", workspaceID, err)
		}
	}
	return nil
}

func (m *Manager) ensureWorkspaceAuthenticity(ctx context.Context, workspaceID uuid.UUID) error {
	return database.Within(ctx, m.backend, database.RepeatableRead, func(tx database.Tx) error {
		if err := LockChainTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		s := auditstore.Of(tx)
		if lastID, _, _, err := m.verifyCheckpointDeltaTx(ctx, tx, s, workspaceID, false); err != nil || lastID != uuid.Nil {
			return err
		}
		verified, err := m.verifyTx(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if !verified.Valid {
			return errors.New("audit chain integrity or origin authentication is invalid")
		}
		if verified.Events == 0 || len(m.checkpointKey) < sha256.Size {
			return nil
		}
		return s.AddCheckpoint(ctx, uuid.Must(uuid.NewV7()), workspaceID, verified.lastID, verified.lastHash, signCheckpoint(m.checkpointKey, workspaceID, verified.lastID, verified.lastHash))
	})
}

func signCheckpoint(key []byte, workspaceID, eventID uuid.UUID, hash []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(workspaceID[:])
	_, _ = mac.Write(eventID[:])
	_, _ = mac.Write(hash)
	return mac.Sum(nil)
}

func summaryValue(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return canonicalJSON(raw)
}
