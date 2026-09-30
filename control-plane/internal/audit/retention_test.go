package audit

import (
	"bytes"
	"crypto/sha256"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestCompactionBindsRetainedRecordAndOriginalProof(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, _ := value.FromTime(now)
	record := ChainRecord{EventID: uuid.New(), WorkspaceID: uuid.New(), ActorType: "user", ActorID: "operator", Action: "node.revoke", ResourceType: "node", ResourceID: uuid.New(), RequestID: uuid.NewString(), At: now.Add(-400 * 24 * time.Hour), Result: "succeeded"}
	previous, hash, originalMAC := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	key := sha256.Sum256([]byte("test-only"))
	keyID := "key-1"
	manager := &Manager{eventKeys: map[string][32]byte{keyID: key}}
	mac, err := compactionMAC(key, record, previous, hash, originalMAC, stamp)
	if err != nil {
		t.Fatal(err)
	}
	valid := func(r ChainRecord, p, h, original []byte, at value.Timestamp) bool {
		return manager.validCompaction(r, p, h, original, at, &keyID, mac)
	}
	if !valid(record, previous, hash, originalMAC, stamp) {
		t.Fatal("valid compaction refused")
	}
	forged := record
	forged.ActorID = "other"
	if valid(forged, previous, hash, originalMAC, stamp) {
		t.Fatal("retained actor is not authenticated")
	}
	for _, which := range []string{"previous", "hash", "original", "time"} {
		p, h, o, at := previous, hash, originalMAC, stamp
		switch which {
		case "previous":
			p = nil
		case "hash":
			h = nil
		case "original":
			o = nil
		case "time":
			at.Micros++
		}
		if valid(record, p, h, o, at) {
			t.Fatal("unbound compaction evidence", which)
		}
	}
	record.BeforeSummary = []byte(`{}`)
	if valid(record, previous, hash, originalMAC, stamp) {
		t.Fatal("detail restored without authentication")
	}
}
