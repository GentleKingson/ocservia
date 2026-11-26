package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

// A second, domain-separated MAC authenticates the retained record and the
// original authenticated hash. Successor links and checkpoints never change.
func compactionMAC(key [sha256.Size]byte, record ChainRecord, previous, hash, originalMAC []byte, at time.Time) ([]byte, error) {
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Payload, Hash, OriginalMAC []byte
		At                         int64
	}{payload, hash, originalMAC, at.UTC().Truncate(time.Microsecond).UnixMicro()})
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("ocservia/audit-detail-compaction/v1\x00"))
	_, _ = mac.Write(encoded)
	return mac.Sum(nil), nil
}
func (m *Manager) validCompaction(record ChainRecord, previous, hash, originalMAC []byte, at time.Time, keyID *string, mac []byte) bool {
	if keyID == nil || record.Reason != "" || len(record.BeforeSummary) != 0 || len(record.AfterSummary) != 0 {
		return false
	}
	key, ok := m.eventKeys[*keyID]
	if !ok {
		return false
	}
	when := at.UTC().Truncate(time.Microsecond)
	if when.IsZero() || when.Before(record.At.Add(90*24*time.Hour)) {
		return false
	}
	expected, err := compactionMAC(key, record, previous, hash, originalMAC, when)
	return err == nil && hmac.Equal(expected, mac)
}

// CompactDetails is a single bounded transaction. Unverifiable rows stop the
// batch; compaction must never convert corrupt history into trusted history.
func (m *Manager) CompactDetails(ctx context.Context, days int) error {
	if days < 90 || days > 2555 {
		return errors.New("audit retention must be 90–2555 days")
	}
	return database.Within(ctx, m.backend, database.ReadCommitted, func(tx database.Tx) error {
		store := auditstore.Of(tx)
		now, err := store.Clock(ctx)
		if err != nil {
			return err
		}
		at := now.UTC().Truncate(time.Microsecond)
		cutoff := at.Add(-time.Duration(days) * 24 * time.Hour)
		expired, err := store.ExpiredDetails(ctx, cutoff)
		if err != nil {
			return err
		}
		type pending struct {
			id        uuid.UUID
			hash, mac []byte
		}
		updates := make([]pending, 0, len(expired))
		for _, event := range expired {
			record := recordFromEventRow(event)
			hashValid, err := m.validEventHash(event, record, event.PreviousHash)
			if err != nil {
				return err
			}
			if event.AuthVersion != eventAuthVersionV1 || event.CompactedAt != nil || !hashValid {
				return errors.New("audit retention: invalid original record")
			}
			if !m.validEventMAC(event) {
				return errors.New("audit retention: original authentication failed")
			}
			record.Reason = ""
			record.BeforeSummary = nil
			record.AfterSummary = nil
			mac, err := compactionMAC(m.current.key, record, event.PreviousHash, event.EventHash, event.EventMAC, at)
			if err != nil {
				return err
			}
			updates = append(updates, pending{record.EventID, event.EventHash, mac})
		}
		for _, update := range updates {
			if err := store.CompactDetails(ctx, update.id, update.hash, m.current.keyID, update.mac, at); err != nil {
				return err
			}
		}
		if err := store.CompactSecurity(ctx, cutoff); err != nil {
			return err
		}
		return nil
	})
}
