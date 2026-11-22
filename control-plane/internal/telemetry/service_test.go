package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testBatch(nodeID uuid.UUID, sequence uint64, observed time.Time) Batch {
	return Batch{
		ID: uuid.Must(uuid.NewV7()), NodeID: nodeID, Sequence: sequence, Kind: "current_health",
		Snapshot: Snapshot{ObservedAt: observed, BootID: "boot-a", AgentInstance: uuid.Must(uuid.NewV7()), AgentVersion: "0.1.0", OcservVersion: "1.3.0", OSRelease: "debian", Ocserv: json.RawMessage(`{"active_state":"active"}`), System: json.RawMessage(`{"memory_used_bytes":42}`), Path: json.RawMessage(`{"mode":"direct","rtt_ms":12}`)},
		Sessions: []Session{{ID: "session-a", Username: "alice", ClientIP: "192.0.2.1", ConnectedAt: observed.Add(-time.Minute), BytesIn: 10, BytesOut: 20}},
		IPBans:   []IPBan{{IP: "192.0.2.9"}},
		Samples:  []Sample{{SampledAt: observed, Metric: "connection_rtt_ms", Value: 12}},
	}
}

func TestValidateBatchRejectsInvalidInputs(t *testing.T) {
	now := time.Now().UTC()
	remaining := uint64(math.MaxUint64)
	cases := []struct {
		name   string
		mutate func(*Batch)
	}{
		{"non-canonical IP ban", func(b *Batch) { b.IPBans[0].IP = "2001:0db8::1" }},
		{"high-cardinality metric", func(b *Batch) { b.Samples[0].Metric = "session_019fc0a4" }},
		{"bigint overflow dropped", func(b *Batch) { b.Snapshot.Dropped.Security = uint64(math.MaxInt64) + 1 }},
		{"bigint overflow IP ban", func(b *Batch) { b.IPBans[0].SecondsRemaining = &remaining }},
		{"duplicate session", func(b *Batch) { b.Sessions = append(b.Sessions, b.Sessions[0]) }},
		{"duplicate IP ban", func(b *Batch) { b.IPBans = append(b.IPBans, b.IPBans[0]) }},
		{"duplicate user", func(b *Batch) {
			u := User{Username: "alice", Enabled: true, Revision: 1}
			b.Users = []User{u, u}
		}},
		{"duplicate group", func(b *Batch) {
			g := Group{Name: "staff", Members: []string{"alice"}, Revision: 1}
			b.Groups = []Group{g, g}
		}},
		{"oversized username", func(b *Batch) { b.Sessions[0].Username = strings.Repeat("a", 65) }},
		{"NUL in boot ID", func(b *Batch) { b.Snapshot.BootID = "boot\x00poison" }},
		{"NUL in session ID", func(b *Batch) { b.Sessions[0].ID = "session\x00poison" }},
		{"NUL in security type", func(b *Batch) {
			b.Security = []SecurityEvent{{ID: uuid.Must(uuid.NewV7()), ObservedAt: now, Severity: "warning", Type: "ev\x00", Detail: json.RawMessage(`{}`)}}
		}},
		{"invalid UTF-8 JSON", func(b *Batch) {
			b.Snapshot.Path = json.RawMessage([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
		}},
		{"stale snapshot", func(b *Batch) { b.Snapshot.ObservedAt = now.Add(-MaxTelemetryAge - time.Second) }},
		{"future sample", func(b *Batch) { b.Samples[0].SampledAt = now.Add(MaxTelemetrySkew + time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := testBatch(uuid.Must(uuid.NewV7()), 1, now)
			tc.mutate(&b)
			if err := validateBatch(b, now); err == nil {
				t.Fatalf("expected %s to fail validation", tc.name)
			}
		})
	}
}

func TestValidObjectUnicodeHandling(t *testing.T) {
	for _, doc := range []json.RawMessage{
		json.RawMessage(`{"label":"replacement "}`),
		json.RawMessage(`{"label":"\ud83d\ude00"}`),
		json.RawMessage(`{"escaped":"\\u0000"}`),
		json.RawMessage(`{"label":"\u0000"}`),
		json.RawMessage(`{"outside_postgres_numeric":1e131072}`),
	} {
		if !validObject(doc) {
			t.Fatalf("validObject rejected %s", doc)
		}
	}
}

func TestMaximumUserGroupSnapshotFitsWireAndValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	maxName := func(prefix string, i int) string {
		base := fmt.Sprintf("%s%06d", prefix, i)
		return base + strings.Repeat("x", 64-len(base))
	}
	batchID, nodeID, instID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wire := &agentv1.TelemetryBatch{
		BatchId: batchID[:], NodeId: nodeID[:], Sequence: 1,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
		Snapshot: &agentv1.ObservedSnapshot{ObservedAt: timestamppb.New(now), BootId: "boot", AgentInstanceId: instID[:], AgentVersion: "0.1.0", OcservVersion: "1.3.0", OsRelease: "debian", OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`)},
	}
	for i := range MaxManagedResources {
		u := maxName("u", i)
		wire.Users = append(wire.Users, &agentv1.UserObservation{Username: u, Enabled: true, Revision: 1})
		wire.Groups = append(wire.Groups, &agentv1.GroupObservation{GroupName: maxName("g", i), Members: []string{u}, Revision: 1})
	}
	for i := MaxManagedResources; i < MaxReportedGroups; i++ {
		wire.Groups = append(wire.Groups, &agentv1.GroupObservation{GroupName: maxName("g", i), Revision: 1})
	}
	payload, err := proto.Marshal(wire)
	testutil.NoError(t, err)
	if len(payload) > MaxBatchBytes {
		t.Fatalf("wire telemetry %d > %d", len(payload), MaxBatchBytes)
	}
	batch, err := decodeWire(payload)
	testutil.NoError(t, err)
	testutil.NoError(t, validateBatch(batch, now))
	encoded, err := json.Marshal(batch)
	if err != nil || len(encoded) > MaxBatchBytes {
		t.Fatalf("internal telemetry %d bytes: %v", len(encoded), err)
	}
	batch.Users = append(batch.Users, batch.Users[0])
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("oversized user snapshot accepted")
	}
}
