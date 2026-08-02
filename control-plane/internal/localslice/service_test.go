package localslice

import (
	"testing"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

func TestValidTraceparent(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if !validTraceparent(valid) {
		t.Fatal("validTraceparent rejected a valid value")
	}
	for _, value := range []string{
		"", "00-0123-0123-01",
		"00-00000000000000000000000000000000-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",
		"00-0123456789abcdef0123456789abcdef-0123456789abcdef-zz",
	} {
		if validTraceparent(value) {
			t.Fatalf("validTraceparent accepted %q", value)
		}
	}
}

func TestEventNamesRejectUnspecified(t *testing.T) {
	if _, err := eventName(transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_UNSPECIFIED); err == nil {
		t.Fatal("eventName accepted an unspecified event")
	}
}
