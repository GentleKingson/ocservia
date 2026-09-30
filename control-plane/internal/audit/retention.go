package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// A second, domain-separated MAC authenticates the retained record and the
// original authenticated hash. Successor links and checkpoints never change.
func compactionMAC(key [sha256.Size]byte, record ChainRecord, previous, hash, originalMAC []byte, at value.Timestamp) ([]byte, error) {
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Payload, Hash, OriginalMAC []byte
		At                         int64
	}{payload, hash, originalMAC, at.Micros})
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("ocservia/audit-detail-compaction/v1\x00"))
	_, _ = mac.Write(encoded)
	return mac.Sum(nil), nil
}
func (m *Manager) validCompaction(record ChainRecord, previous, hash, originalMAC []byte, at value.Timestamp, keyID *string, mac []byte) bool {
	if keyID == nil || record.Reason != "" || len(record.BeforeSummary) != 0 || len(record.AfterSummary) != 0 || record.Action == transitionActionV1 {
		return false
	}
	key, ok := m.eventKeys[*keyID]
	if !ok {
		return false
	}
	when, err := at.Time()
	if err != nil || when.Before(record.At.Add(90*24*time.Hour)) {
		return false
	}
	expected, err := compactionMAC(key, record, previous, hash, originalMAC, at)
	return err == nil && hmac.Equal(expected, mac)
}

// CompactDetails is a single bounded, fenced transaction. Legacy rows and the
// legacy/authenticated transition remain full evidence. Unverifiable rows stop
// the batch; compaction must never convert corrupt history into trusted history.
func (m *Manager) CompactDetails(ctx context.Context, days int) error {
	if days < 90 || days > 2555 {
		return errors.New("audit retention must be 90–2555 days")
	}
	return database.Within(ctx, m.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := auditstore.From(tx)
		if err != nil {
			return err
		}
		now, err := store.Clock(ctx)
		if err != nil {
			return err
		}
		at, err := value.FromTime(now)
		if err != nil {
			return err
		}
		cutoff, err := value.FromTime(now.Add(-time.Duration(days) * 24 * time.Hour))
		if err != nil {
			return err
		}
		rows, err := store.ExpiredDetails(ctx, cutoff)
		if err != nil {
			return err
		}
		type pending struct {
			id        uuid.UUID
			hash, mac []byte
		}
		updates := []pending{}
		for rows.Next() {
			var record ChainRecord
			var resourceID *uuid.UUID
			var stamp, compacted value.Timestamp
			var before, after value.JSONB
			var previous, hash, originalMAC, oldCompactMAC []byte
			var authVersion int16
			var keyID, oldCompactKey *string
			if err := rows.Scan(&record.WorkspaceID, &record.EventID, &stamp, &record.ActorType, &record.ActorID, &record.Action, &record.ResourceType, &resourceID, &record.RequestID, &record.TraceID, &record.Result, &record.Reason, &record.SessionID, &record.NodeID, &record.CommandID, &record.ApprovalID, &before, &after, &record.ErrorType, &previous, &hash, &authVersion, &keyID, &originalMAC, &compacted, &oldCompactKey, &oldCompactMAC); err != nil {
				rows.Close()
				return err
			}
			record.At, err = stamp.Time()
			if err != nil {
				rows.Close()
				return err
			}
			if resourceID != nil {
				record.ResourceID = *resourceID
			}
			record.BeforeSummary, record.AfterSummary = before.Bytes(), after.Bytes()
			payload, err := encodeChainPayload(previous, record)
			if err != nil {
				rows.Close()
				return err
			}
			digest := sha256.Sum256(payload)
			if authVersion != eventAuthVersionV1 || keyID == nil || compacted.Valid || oldCompactKey != nil || len(oldCompactMAC) != 0 || !hmac.Equal(hash, digest[:]) {
				rows.Close()
				return errors.New("audit retention: invalid original record")
			}
			key, ok := m.eventKeys[*keyID]
			if !ok || !hmac.Equal(originalMAC, signEvent(key, hash)) {
				rows.Close()
				return errors.New("audit retention: original authentication failed")
			}
			record.Reason = ""
			record.BeforeSummary = nil
			record.AfterSummary = nil
			mac, err := compactionMAC(m.current.key, record, previous, hash, originalMAC, at)
			if err != nil {
				rows.Close()
				return err
			}
			updates = append(updates, pending{record.EventID, hash, mac})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, update := range updates {
			if err := store.CompactDetails(ctx, update.id, update.hash, m.current.keyID, update.mac, at); err != nil {
				return err
			}
		}
		if err := store.CompactSecurity(ctx, cutoff); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
}
