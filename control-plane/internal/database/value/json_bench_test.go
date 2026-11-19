package value

import (
	"testing"
)

func BenchmarkParseJSONB(b *testing.B) {
	telemetryDoc := []byte(`{"active_state":"active","sub_state":"running","memory_used_bytes":134217728,"load_avg":[0.42,0.38,0.31],"interfaces":{"vpns0":{"rx_bytes":9876543210,"tx_bytes":1234567890,"mtu":1400}},"mode":"direct","rtt_ms":12.34,"endpoint":"198.51.100.10:443"}`)
	auditDoc := []byte(`{"workspace_id":"0194f0a0-0000-7000-8000-000000000001","actor_type":"admin","action":"user.create","resource_type":"user","result":"succeeded","reason":"quarterly onboarding","metadata":{"groups":["engineering","ops","vpn-prod"],"quota_mb":51200,"enabled":true,"revision":42}}`)

	b.Run("TelemetryObject", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(telemetryDoc)))
		for b.Loop() {
			if _, err := ParseJSONB(telemetryDoc); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("AuditSummary", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(auditDoc)))
		for b.Loop() {
			if _, err := ParseJSONB(auditDoc); err != nil {
				b.Fatal(err)
			}
		}
	})
}
