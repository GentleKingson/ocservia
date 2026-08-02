package app

import (
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func TestOperationAuthIsNotEnabledBySimulator(t *testing.T) {
	if operationAuthEnabled(config.Config{LocalSimulator: true}) {
		t.Fatal("local simulator enabled operation authentication")
	}
	if !operationAuthEnabled(config.Config{DevAuth: true, LocalSimulator: true}) {
		t.Fatal("development authentication was not enabled")
	}
}
